// refreshes.go — Ship 1 (live-refresh-coherence, option A).
//
// GET /refreshes is the per-subject live-refresh SSE stream (design §3). A
// browser opens ONE multiplexed EventSource per tab; it arms the widgets it
// has mounted by sending their resource coordinates (?sub=). When the
// refresher commits a fresh L1 entry for an armed key (resolve_populate.go:291
// -> cache.PublishRefresh), this stream emits `event: refresh\ndata: <l1Key>`.
// The frontend then refetches /call and reads the freshly-committed L1 as a
// HIT — no apiserver read (the cache-respecting invariant, falsifier 9.1).
//
// SIGNAL-ONLY, no payload: data carries just the l1Key. The body that flows
// to the client comes from the subsequent /call, which re-applies per-user
// RBAC gating at serve time. So a shared (widgetContent) signal never leaks
// another subject's row-level visibility (falsifier 9.4b).
//
// AUTH: middleware.RefreshAuth (refreshauth.go) places UserInfo on ctx from a
// cookie-or-header JWT. ISOLATION: every armed key is re-derived UNDER that
// connection's identity via dispatchers.DeriveSubscriptionKey (forgery-proof;
// design §5). ZERO apiserver reads: the only external touch is the in-process
// RBAC snapshot inside the derivation.
//
// TRANSPARENT FALLBACK (project_cache_off_is_transparent_fallback): when the
// SSE layer is disabled (REFRESH_SSE_ENABLED=false) or cache is off
// (CACHE_ENABLED=false), the endpoint serves a clean idle stream — heartbeats
// only, zero refresh events — so a connected client degrades to its own 5s
// throttle and never hangs or errors.

package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/dispatchers"
)

const (
	// refreshHeartbeatInterval keeps intermediaries from idling the
	// connection and lets the client detect a dead link. 20s beats the
	// server IdleTimeout (30s, main.go:905) with margin; the per-connection
	// SetWriteDeadline(0) defeats the 300s WriteTimeout (main.go:904).
	refreshHeartbeatInterval = 20 * time.Second

	// refreshSubParamMaxBytes caps the decoded ?sub= payload to avoid a
	// memory-amplification subscribe vector (extras can be large; design §6
	// tradeoff). A connection sending a larger payload is rejected 400.
	refreshSubParamMaxBytes = 16 * 1024

	// refreshSubMaxEntries caps how many widgets one connection may arm in a
	// single subscribe. Defence-in-depth alongside the byte cap.
	refreshSubMaxEntries = 512
)

// subRequest is one widget's arm request inside ?sub=. It is the JSON form of
// dispatchers.SubscriptionCoordinates. Identity is NOT carried here — it
// comes from the connection's authenticated ctx (the forgery-proof property).
type subRequest struct {
	Class     string         `json:"class"`
	Group     string         `json:"group"`
	Version   string         `json:"version"`
	Resource  string         `json:"resource"`
	Namespace string         `json:"namespace"`
	Name      string         `json:"name"`
	PerPage   int            `json:"perPage"`
	Page      int            `json:"page"`
	Extras    map[string]any `json:"extras"`
}

// Refreshes returns the GET /refreshes SSE handler. It takes no verification
// key: identity is already resolved onto ctx by middleware.RefreshAuth and the
// handler never re-validates the token. (It previously accepted the signing key
// "for symmetry and future use" and ignored it; the asymmetric-signing
// migration replaced that key with a fetched-and-cached JWKS key source, and
// threading an unused one through here would only invite the reader to think
// this handler verifies something. It does not.)
func Refreshes() http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		log := xcontext.Logger(req.Context())

		// (1) Disabled / cache-off -> clean idle stream (transparent
		// fallback). No subscription validation, no apiserver touch; the
		// client gets heartbeats only and falls back to its own throttle.
		if !cache.RefreshSSEEnabled() {
			serveIdleSSE(wri, req)
			return
		}

		// (2) Identity is already on ctx (RefreshAuth). Defence in depth.
		ui, err := xcontext.UserInfo(req.Context())
		if err != nil {
			response.Unauthorized(wri, err)
			return
		}

		// (3) Validate + re-derive the requested key-set UNDER THIS subject.
		armed, derr := validateSubscription(req)
		if derr != nil {
			http.Error(wri, derr.Error(), http.StatusBadRequest)
			return
		}
		if len(armed) == 0 {
			// #68: distinguish a TRANSIENT empty (the post-redeploy warmup
			// window — informer/RBAC snapshot not yet synced, so
			// DeriveSubscriptionKey skips every coord) from a GENUINELY empty
			// one (warm, but all coords NotFound / RBAC-denied — the blessed
			// C64-1 honest-400). During warmup, serve the documented idle
			// stream (keepalives only; the browser stays connected and starts
			// delivering once warm) instead of a 400 the frontend must back off
			// from. The gate keys STRICTLY on warmup readiness
			// (refreshWarmupIncomplete = !IsPhase1Done || RBACGen==0), NOT on
			// the armed count or WHY coords were skipped — so a warm
			// all-NotFound/all-denied subscription still gets the correct 400
			// (the divert cannot mask "armed nothing valid" once warm).
			if refreshWarmupIncomplete() {
				serveIdleSSE(wri, req)
				return
			}
			http.Error(wri, "no valid subscription keys", http.StatusBadRequest)
			return
		}

		// (4) SSE headers + defeat the per-connection write deadline so the
		// 300s WriteTimeout does not kill a long-lived stream (design §3.2,
		// §6). SetWriteDeadline(zero) clears the deadline for THIS connection
		// only; Go 1.20+ ResponseController, supported on go 1.25.x (go.mod).
		rc := http.NewResponseController(wri)
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			// Some ResponseWriters (test recorders) don't support deadlines;
			// that's non-fatal — the stream still works under the test server.
			log.Debug("refreshes: SetWriteDeadline unsupported; continuing",
				slog.String("subsystem", "cache"), slog.Any("err", err))
		}
		wri.Header().Set("Content-Type", "text/event-stream")
		wri.Header().Set("Cache-Control", "no-cache")
		wri.Header().Set("Connection", "keep-alive")
		wri.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
		wri.WriteHeader(http.StatusOK)
		// Track 2 (gzip) C-1: emit a `: connected` SSE comment BEFORE the
		// first flush so response headers commit at connect. Under the gzhttp
		// wrapper (middleware.Gzip) WriteHeader only saves the status and a
		// Flush with ZERO buffered body bytes returns without touching the
		// underlying writer (the ExceptContentTypes exclusion engages at the
		// FIRST body write). Without a body byte, an armed-but-quiet stream
		// would withhold headers up to the 20s heartbeat, stranding EventSource
		// in CONNECTING and tripping intermediary time-to-first-header
		// timeouts. A `:`-prefixed comment line is a non-empty write (→ the
		// content-type filter runs → startPlain → headers commit) yet is
		// invisible to EventSource clients per the spec — byte-neutral to the
		// consumer, unconditional so the fix holds with or without the wrapper.
		fmt.Fprint(wri, ": connected\n\n")
		_ = rc.Flush()

		// (5) Subscribe + stream until the client disconnects.
		ch, unsub := cache.SubscribeRefresh(armed)
		defer unsub()

		log.Info("refreshes: subscribed",
			slog.String("subsystem", "cache"),
			slog.String("user", ui.Username),
			slog.Int("armed_keys", len(armed)),
		)

		heartbeat := time.NewTicker(refreshHeartbeatInterval)
		defer heartbeat.Stop()
		for {
			select {
			case <-req.Context().Done():
				return // client gone
			case k, ok := <-ch:
				if !ok {
					// Hub closed the channel (disabled mid-stream) — degrade
					// to idle; the client falls back to its throttle.
					return
				}
				if _, werr := fmt.Fprintf(wri, "event: refresh\ndata: %s\n\n", k); werr != nil {
					return // client write failed — disconnect
				}
				_ = rc.Flush()
			case <-heartbeat.C:
				if _, werr := fmt.Fprint(wri, ": keepalive\n\n"); werr != nil {
					return
				}
				_ = rc.Flush()
			}
		}
	}
}

// refreshWarmupIncomplete reports whether the pod is still in its post-(re)deploy
// warmup window — the ~4-min span where the informer/RBAC substrate is not yet
// synced, so DeriveSubscriptionKey legitimately skips every coord (#68). It is
// the disjunction of the two readiness gates already used elsewhere:
//   - !cache.IsPhase1Done() — the Tag-B startup warmup gate (the /readyz signal,
//     phase1.go); false until the prewarm walk completes.
//   - cache.RBACGen() == 0 — no RBAC snapshot published yet (rbac_snapshot.go);
//     EvaluateRBAC fails closed until the first publish, so every identity-bound
//     coord derives the empty/denied key pre-publish.
// BOTH disjuncts are required: phase1 can complete while the RBAC snapshot is
// momentarily unpublished (and vice-versa) — either alone leaves
// DeriveSubscriptionKey unable to produce a real armed key.
//
// It deliberately does NOT consult the armed count or the skip reason — so a
// WARM pod (both gates satisfied) with a genuinely-empty/all-denied
// subscription still falls through to the honest 400 (C64-1), not the idle
// stream. The divert is STRICTLY warmup-gated by construction.
func refreshWarmupIncomplete() bool {
	return !cache.IsPhase1Done() || cache.RBACGen() == 0
}

// serveIdleSSE serves a heartbeat-only SSE stream for the disabled / cache-off
// path (transparent fallback). It opens the stream, defeats the write
// deadline, and emits only `: keepalive` comment frames until the client
// disconnects. Never emits a refresh event (there is no broadcaster).
func serveIdleSSE(wri http.ResponseWriter, req *http.Request) {
	rc := http.NewResponseController(wri)
	_ = rc.SetWriteDeadline(time.Time{})
	wri.Header().Set("Content-Type", "text/event-stream")
	wri.Header().Set("Cache-Control", "no-cache")
	wri.Header().Set("Connection", "keep-alive")
	wri.Header().Set("X-Accel-Buffering", "no")
	wri.WriteHeader(http.StatusOK)
	// Track 2 (gzip) C-1: emit a `: connected` SSE comment BEFORE the first
	// flush so headers commit at connect under the gzhttp wrapper. This idle
	// path is the more critical of the two sites — serveIdleSSE serves the
	// disabled/cache-off fallback AND the #68 warmup window, both of which
	// idle indefinitely with no event, so without the preamble headers would
	// never commit until the first 20s heartbeat. See the armed-path comment
	// in Refreshes for the full mechanism (gzhttp WriteHeader saves status,
	// zero-byte Flush is a no-op; a comment byte forces startPlain).
	fmt.Fprint(wri, ": connected\n\n")
	_ = rc.Flush()

	heartbeat := time.NewTicker(refreshHeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(wri, ": keepalive\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// validateSubscription decodes the ?sub= base64-JSON coordinate array and
// re-derives each key UNDER the request's authenticated identity (the
// connection ctx), returning the validated armed-key set. A coordinate that
// fails derivation (foreign identity / unknown class / cache-off) is silently
// skipped (fail-closed) — only keys the connection's OWN identity legitimately
// produces are armed (design §5.2). Returns an error only on a malformed or
// oversized ?sub= payload (a client protocol error), never on a per-entry
// derivation failure.
func validateSubscription(req *http.Request) (map[string]struct{}, error) {
	raw := req.URL.Query().Get("sub")
	if raw == "" {
		return nil, fmt.Errorf("missing 'sub' query parameter")
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// Tolerate URL-safe base64 too (EventSource URLs are query-encoded).
		decoded, err = base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid 'sub' encoding: not base64")
		}
	}
	if len(decoded) > refreshSubParamMaxBytes {
		return nil, fmt.Errorf("'sub' payload too large (%d bytes; max %d)", len(decoded), refreshSubParamMaxBytes)
	}

	var reqs []subRequest
	if err := json.Unmarshal(decoded, &reqs); err != nil {
		return nil, fmt.Errorf("invalid 'sub' payload: not a JSON coordinate array")
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("'sub' payload is an empty array")
	}
	if len(reqs) > refreshSubMaxEntries {
		return nil, fmt.Errorf("too many subscription entries (%d; max %d)", len(reqs), refreshSubMaxEntries)
	}

	// #101: the /refreshes arming reads MUST be informer-only. The route is
	// authed by middleware.RefreshAuth (UserInfo but DELIBERATELY no UserConfig),
	// so subscriptionKeyExtras's per-coord objects.Get cannot reach the apiserver
	// fall-through without dying at the endpoint read (378-error storm). The
	// marker makes an informer GET-miss a quiet NotFound-shaped skip instead.
	armCtx := cache.WithInformerOnlyReads(req.Context())

	armed := make(map[string]struct{}, len(reqs))
	var skippedInformerMiss, skippedOther int
	for _, s := range reqs {
		key, ok, reason := dispatchers.DeriveSubscriptionKeyWithReason(armCtx, dispatchers.SubscriptionCoordinates{
			Class:     s.Class,
			Group:     s.Group,
			Version:   s.Version,
			Resource:  s.Resource,
			Namespace: s.Namespace,
			Name:      s.Name,
			PerPage:   s.PerPage,
			Page:      s.Page,
			Extras:    s.Extras,
		})
		if !ok {
			// fail-closed: skip keys this identity can't produce.
			switch reason {
			case dispatchers.SubscriptionSkipInformerMiss:
				skippedInformerMiss++
			default:
				skippedOther++
			}
			continue
		}
		armed[key] = struct{}{}
	}

	// #101 ARM condition: per-subscription INFO summary REPLACING the per-coord
	// ERROR storm. ONE line per /refreshes connection with the armed count and
	// the skip breakdown — so a "did admin's dashboard arm?" question is
	// answerable from a single grep, and the informer-miss coords (the transient
	// post-storm state) are visible as a count, not 378 ERROR lines. INFO, not
	// ERROR: an informer-miss skip is expected/self-healing, not a fault.
	xcontext.Logger(req.Context()).Info("refreshes.subscription.summary",
		slog.String("subsystem", "cache"),
		slog.Int("requested", len(reqs)),
		slog.Int("armed", len(armed)),
		slog.Int("skipped_informer_miss", skippedInformerMiss),
		slog.Int("skipped_other", skippedOther),
	)
	return armed, nil
}
