package dispatchers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/api"
	"github.com/krateoplatformops/snowplow/internal/resolvers/restactions/l1cache"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// errRefilterPanic wraps a recovered panic from RefilterRESTAction so the
// fall-through logic at the L1-hit branch can treat panics identically to
// explicit refilter errors. NEVER serve the cached bytes if refilter
// panicked — same trust-boundary discipline as the error path.
type errRefilterPanic struct {
	panicValue any
}

func (e errRefilterPanic) Error() string {
	return fmt.Sprintf("v3 refilter panic: %v", e.panicValue)
}

var restactionTracer = otel.Tracer("snowplow/dispatchers")

// RESTAction builds the HTTP /call dispatcher for the restactions GVR.
// snowplowEndpointFn is the elevated-call provider plumbed through to
// l1cache.Input.SnowplowEndpoint; pass nil when not running in-cluster
// (userAccessFilter calls will be rejected at the resolver with an explicit
// log line).
//
// Q-RBAC-DECOUPLE C(d) v6 — Path B (audit 2026-05-04): snowplowK8sClient
// is the in-cluster dynamic K8s client used for SA dispatch. Replaces
// httpcall.Do for the SA branch, structurally avoiding plumbing's
// tlsConfigFor TLS handshake bug. Both fields are nil-safe; the resolver
// emits a runtime error log if neither is configured but a UAF api[]
// entry tries to dispatch.
func RESTAction(snowplowEndpointFn func() (*endpoints.Endpoint, error), snowplowK8sClient dynamic.Client) http.Handler {
	return &restActionHandler{
		authnNS:           env.String("AUTHN_NAMESPACE", ""),
		verbose:           env.True("DEBUG"),
		snowplowEndpoint:  snowplowEndpointFn,
		snowplowK8sClient: snowplowK8sClient,
	}
}

type restActionHandler struct {
	authnNS           string
	verbose           bool
	snowplowEndpoint  func() (*endpoints.Endpoint, error)
	snowplowK8sClient dynamic.Client
}

var _ http.Handler = (*restActionHandler)(nil)

func (r *restActionHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())
	start := time.Now()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	// ── Resolved-output cache ─────────────────────────────────────────────────
	// Cache the fully-resolved RESTAction JSON keyed per user + resource.
	// This eliminates both the HTTP fan-out AND all JQ evaluations on repeated
	// requests. Only unpaginated requests are cached (extras excluded for now).
	perPage, page := paginationInfo(log, req)
	c := cache.FromContext(req.Context())

	var resolvedKey string
	if c != nil && len(extras) == 0 {
		gvr, gerr := util.ParseGVR(req)
		nsn, nerr := util.ParseNamespacedName(req)
		if gerr == nil && nerr == nil {
			user, uerr := xcontext.UserInfo(req.Context())
			if uerr == nil {
				identity := cache.CacheIdentity(req.Context(), user.Username)
				resolvedKey = cache.ResolvedKey(identity, gvr, nsn.Namespace, nsn.Name, page, perPage)
				lookupCtx, lookupSpan := restactionTracer.Start(req.Context(), "cache.lookup",
					trace.WithAttributes(
						attribute.String("cache.layer", "l1"),
						attribute.String("cache.key", resolvedKey),
					))
				raw, hit, _ := c.GetRaw(lookupCtx, resolvedKey)
				if lookupSpan.IsRecording() {
					lookupSpan.SetAttributes(attribute.Bool("cache.hit", hit))
				}
				lookupSpan.End()
				if hit {
					// Q-RBAC-DECOUPLE C(d) v3 — refilter the cached
					// (binding-identity-shared) entry per the requesting
					// user. The trust-boundary contract is non-negotiable:
					// if RefilterRESTAction returns ANY error, we MUST
					// fall through to the miss path. Serving the cached
					// bytes verbatim would re-introduce Q-RBACC-DEFECT-1
					// (silent RBAC leak between users in the same binding
					// group). The miss path is correct; just slower.
					_, refilterSpan := restactionTracer.Start(req.Context(), "restaction.refilter",
						trace.WithAttributes(
							attribute.String("cache.key", resolvedKey),
							attribute.String("user", user.Username),
						))
					refiltered, refErr := func() (out []byte, err error) {
						defer func() {
							if r := recover(); r != nil {
								err = errRefilterPanic{panicValue: r}
								out = nil
							}
						}()
						// Q-RBAC-DECOUPLE C(d) v3 §2.6 — singleflight
						// dedup. 80 burst requests for the same
						// (l1key, user, groups) coalesce into one
						// RefilterRESTAction execution; followers
						// share the result. Cuts ~2.4 GB transient
						// alloc + ~4 s CPU peak under cluster-restart
						// thundering-herd.
						return api.RefilterRESTActionDeduped(req.Context(), c, raw, resolvedKey)
					}()
					if refErr != nil {
						refilterSpan.RecordError(refErr)
						refilterSpan.End()
						log.Warn("L1 refilter failed; falling through to miss path",
							slog.String("key", resolvedKey),
							slog.String("user", user.Username),
							slog.Any("err", refErr))
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawMisses, "raw_misses")
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Misses, "l1_misses")
					} else {
						refilterSpan.SetAttributes(
							attribute.Int("response.bytes", len(refiltered)),
						)
						refilterSpan.End()
						if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
							httpSpan.AddEvent("cache.hit", trace.WithAttributes(
								attribute.String("cache.key", resolvedKey),
								attribute.String("cache.layer", "l1"),
							))
						}
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawHits, "raw_hits")
						cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Hits, "l1_hits")
						cache.TouchKey(cache.ResolvedKeyBase(identity, gvr, nsn.Namespace, nsn.Name))
						log.Info("RESTAction resolved from cache",
							slog.String("key", resolvedKey),
							slog.String("user", user.Username),
							slog.String("resource", gvr.Resource),
							slog.String("name", nsn.Name),
							slog.String("namespace", nsn.Namespace),
							slog.String("source", "L1-cache"),
							slog.String("duration", util.ETA(start)))
						wri.Header().Set("Content-Type", "application/json")
						wri.WriteHeader(http.StatusOK)
						_, writeSpan := restactionTracer.Start(req.Context(), "http.write",
							trace.WithAttributes(
								attribute.Bool("cache.hit", true),
								attribute.Int("http.response.body.size", len(refiltered)),
							))
						_, _ = wri.Write(refiltered)
						writeSpan.End()
						return
					}
					// fall-through to miss path (resolveAndCache below)
				} else {
					if httpSpan := trace.SpanFromContext(req.Context()); httpSpan.IsRecording() {
						httpSpan.AddEvent("cache.miss", trace.WithAttributes(
							attribute.String("cache.key", resolvedKey),
							attribute.String("cache.layer", "l1"),
						))
					}
					cache.GlobalMetrics.Inc(&cache.GlobalMetrics.RawMisses, "raw_misses")
					cache.GlobalMetrics.Inc(&cache.GlobalMetrics.L1Misses, "l1_misses")
					log.Info("restaction: L1 miss", slog.String("key", resolvedKey))
				}
			}
		}
	}
	// ── End resolved-output cache ─────────────────────────────────────────────

	// Fetch the K8s object (needs HTTP request for query params).
	_, fetchSpan := restactionTracer.Start(req.Context(), "restaction.fetch_object")
	got := fetchObject(req)
	fetchSpan.End()
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}

	// ── Resolve via the shared l1cache package ─────────────────────────────
	// The shared foreground singleflight.Group in l1cache dedups against
	// widget.apiref callers too, killing the cross-path thundering herd
	// when a dashboard Row spawns 2+ parallel widget requests that all
	// chase the same compositions-list restaction.
	//
	// Non-cacheable path (extras present) passes ResolvedKey="" which
	// bypasses both singleflight and L1 write.
	sfCtx := req.Context()
	if resolvedKey != "" {
		sfCtx = context.WithoutCancel(req.Context())
	}
	result, resolveErr := l1cache.ResolveAndCache(sfCtx, l1cache.Input{
		Cache:             c,
		Obj:               got.Unstructured.Object,
		ResolvedKey:       resolvedKey,
		AuthnNS:           r.authnNS,
		PerPage:           perPage,
		Page:              page,
		Extras:            extras,
		SnowplowEndpoint:  r.snowplowEndpoint,
		SnowplowK8sClient: r.snowplowK8sClient,
	})
	if resolveErr != nil {
		response.InternalError(wri, resolveErr)
		return
	}
	if result == nil || result.Raw == nil {
		response.InternalError(wri, errEmptyResolveResult)
		return
	}

	// Q-RBAC-DECOUPLE C(d) v4 §2.1 (Fix-2b) — TRUST BOUNDARY:
	// `cacheBody` (formerly `raw`) is the v3 wrapper held in L1; it
	// carries the UNFILTERED ProtectedDict and MUST NEVER be written to
	// the HTTP response. The HTTP response sink is `result.Refiltered`,
	// which holds the per-user-refiltered CR JSON computed inside
	// l1cache.ResolveAndCache. Renamed for clarity so a future reviewer
	// cannot accidentally route the wrong field to wri.Write.
	cacheBody := result.Raw
	_ = cacheBody // referenced only by the cache write inside ResolveAndCache; kept as a named binding for grep/audit visibility.

	if result.Refiltered == nil {
		// Refilter failed inside ResolveAndCache (it logged the cause).
		// FAIL-CLOSED: do NOT serve `cacheBody` — that would re-introduce
		// Q-RBACC-DEFECT-2 (silent RBAC leak on cache MISS).
		log.Error("RESTAction MISS: refilter unavailable; refusing to serve unfiltered wrapper",
			slog.String("key", resolvedKey))
		response.InternalError(wri, errRefilterMissingOnMiss)
		return
	}
	body := result.Refiltered

	// Touch the key so it starts HOT for refresh priority.
	if resolvedKey != "" {
		if rki, ok := cache.ParseResolvedKey(resolvedKey); ok {
			cache.TouchKey(cache.ResolvedKeyBase(rki.Username, rki.GVR, rki.NS, rki.Name))
		}
	}

	pathAttr := "inline"
	if resolvedKey != "" {
		pathAttr = "singleflight"
	}
	log.Info("RESTAction successfully resolved",
		slog.String("key", resolvedKey),
		slog.String("path", pathAttr),
		slog.String("duration", util.ETA(start)))
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, writeSpan := restactionTracer.Start(req.Context(), "http.write",
		trace.WithAttributes(
			attribute.Bool("cache.hit", false),
			attribute.String("path", pathAttr),
			attribute.Int("http.response.body.size", len(body)),
		))
	_, _ = wri.Write(body)
	writeSpan.End()
}

// errEmptyResolveResult indicates the shared l1cache helper returned
// (nil, nil) — should not happen in practice, kept as a typed error so
// the HTTP path returns 500 cleanly instead of panicking on a nil
// dereference downstream.
var errEmptyResolveResult = errEmptyResolve{}

type errEmptyResolve struct{}

func (errEmptyResolve) Error() string {
	return "l1cache.ResolveAndCache returned empty result with no error"
}

// errRefilterMissingOnMiss indicates the dispatcher MISS path observed
// `result.Refiltered == nil` after l1cache.ResolveAndCache returned
// successfully — the refilter call inside ResolveAndCache failed (logged
// at WARN), and FAIL-CLOSED demands HTTP 500 instead of serving the
// unfiltered wrapper. Q-RBAC-DECOUPLE C(d) v4 §2.1 (Fix-2b for
// Q-RBACC-DEFECT-2).
var errRefilterMissingOnMiss = errRefilterMissing{}

type errRefilterMissing struct{}

func (errRefilterMissing) Error() string {
	return "l1cache.ResolveAndCache produced no refiltered bytes (refilter error); refusing to serve unfiltered wrapper"
}

// ResolveRESTActionBackground is the entry point for the L1 refresh
// loop. It forwards to l1cache.ResolveAndCache.
//
// Kept here as a thin shim because internal/handlers/dispatchers/
// l1_refresh.go already imports this package and calls this function;
// moving the call site to l1cache directly would churn l1_refresh
// without any correctness or performance benefit.
//
// snowplowEndpointFn may be nil; that disables userAccessFilter dispatch
// during background refresh (the warm cache will only contain entries
// resolvable without elevated access). Production wiring threads it from
// MakeL1Refresher / WarmL1FromEntryPoints.
//
// Q-RBAC-DECOUPLE C(d) v6 — Path B: snowplowK8sClient is the in-cluster
// dynamic client used for SA dispatch. Either field (endpoint or k8s
// client) is sufficient to enable UAF dispatch — typically both are
// passed in production wiring.
func ResolveRESTActionBackground(ctx context.Context, c cache.Cache, obj map[string]interface{}, resolvedKey, authnNS string, perPage, page int, snowplowEndpointFn func() (*endpoints.Endpoint, error), snowplowK8sClient dynamic.Client) ([]byte, error) {
	result, err := l1cache.ResolveAndCache(ctx, l1cache.Input{
		Cache:             c,
		Obj:               obj,
		ResolvedKey:       resolvedKey,
		AuthnNS:           authnNS,
		PerPage:           perPage,
		Page:              page,
		SnowplowEndpoint:  snowplowEndpointFn,
		SnowplowK8sClient: snowplowK8sClient,
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	return result.Raw, nil
}
