package dispatchers

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/plumbing/maps"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/util"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
)

func Widgets() http.Handler {
	// Ship 0.30.167 — Option 2 parallelism regression fix. Same shape
	// as RESTAction() above and RegisterRefreshHandlers at
	// dispatchers.go:56-66 (the load-bearing prior art): resolve the
	// SA transport pair ONCE at handler construction, capture into
	// struct fields, attach in ServeHTTP via a cheap nil-check + field
	// read. Eliminates the per-request snowplowSACtx() helper call
	// that serialised dispatches through the SA singletons' mutexes.
	saEP, saRC := snowplowSACtx()
	return &widgetsHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
		saEP:    saEP,
		saRC:    saRC,
	}
}

type widgetsHandler struct {
	authnNS string
	verbose bool
	// saEP + saRC: see restActionHandler.saEP / saRC. Same shape;
	// captured at handler construction.
	saEP *endpoints.Endpoint
	saRC *rest.Config
}

var _ http.Handler = (*widgetsHandler)(nil)

func (r *widgetsHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	start := time.Now()

	// Ship 0.30.171-debug — per-/call structured timing log; see
	// restactions.go for the rationale. Emits dispatcher.call.complete
	// into the existing stdout->otel-daemonset->ClickHouse pipeline.
	pcs, pcEmit := beginPerCall(req, "widgets")
	defer pcEmit()

	// Ship 1 — customer-priority signal (see restactions.go). Marks this
	// /call as in-flight so the prewarm engine yields its background
	// re-seed work for the dispatch's duration.
	defer markCustomerInFlight()()

	extras, err := util.ParseExtras(req)
	if err != nil {
		response.BadRequest(wri, err)
		return
	}

	got := fetchObjectFn(req)
	if got.Err != nil {
		response.Encode(wri, got.Err)
		return
	}
	pcs.gvr = got.GVR.String()

	log := xcontext.Logger(req.Context()).
		With(
			slog.Group("widget",
				slog.String("name", widgets.GetName(got.Unstructured.Object)),
				slog.String("namespace", widgets.GetNamespace(got.Unstructured.Object)),
				slog.String("apiVersion", widgets.GetAPIVersion(got.Unstructured.Object)),
				slog.String("kind", widgets.GetKind(got.Unstructured.Object)),
			),
		)

	// Revision 2 binding (0.30.4): cache=on mode gates every widget
	// dispatch by EvaluateRBAC. Cache=off skips the gate — fetchObject
	// already ran per-user against apiserver.
	if !cache.Disabled() {
		if !checkDispatchRBACFn(req.Context(), got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("widget dispatch denied by EvaluateRBAC",
				slog.String("gvr", got.GVR.String()),
			)
			response.Encode(wri, response.New(http.StatusForbidden,
				fmt.Errorf("forbidden: cannot get %s in namespace %q",
					got.GVR.Resource, got.Unstructured.GetNamespace())))
			return
		}
	}

	// R (composition-resources loopback guard) — HTTP-edge cycle-stop (twin of
	// the restactions.go block; see there for the full rationale). PLACEMENT
	// (C3): strictly AFTER fetchObject + checkDispatchRBAC above and BEFORE any
	// L1 lookup below. TRUSTED self-loopback only (isTrustedSelfLoopback); an
	// untrusted caller's headers are ignored → no stop → normal resolve (C2). The
	// widget arm exists because a top-level widget whose nested resolve fans back
	// to ITSELF over HTTP hits the same self-recursion as a RESTAction (the seam
	// has a widget resolve arm, nested_call.go).
	if !cache.Disabled() {
		// Seed ONLY the inbound header ancestors (see restactions.go for the full
		// rationale — do NOT add THIS node before the check; a self-reference is
		// detected because a prior hop already put it in the emitted header set).
		guardCtx := reseedNestedGuardsFromHeaders(req.Context(), req)
		if stop, raw := httpEdgeGuardStop(guardCtx, log,
			got.GVR.Resource, got.Unstructured.GetNamespace(), got.Unstructured.GetName()); stop {
			if raw {
				encoded, encErr := encodeResolvedJSON(got.Unstructured.Object)
				if encErr != nil {
					response.InternalError(wri, encErr)
					return
				}
				writeResolvedJSON(wri, encoded)
				return
			}
			response.Encode(wri, response.New(http.StatusLoopDetected,
				fmt.Errorf("nested resolve depth limit exceeded (%d) for %s/%s/%s over HTTP self-loopback",
					cache.NestedCallMaxDepth(), got.GVR.Resource,
					got.Unstructured.GetNamespace(), got.Unstructured.GetName())))
			return
		}
	}

	perPage, page := paginationInfo(log, req)

	// inline-extras design P §1/§4.3 — the cache-key union. The widget CR is
	// already in hand (got.Unstructured, fetched above), so read BOTH
	// author-declared inline maps off it and fold their UNION with the
	// per-request extras (request wins). keyExtras is the body-dependency
	// fingerprint passed IN PLACE OF the bare request extras to every widget
	// key site below (content + per-cohort) and the two diagnostic emits, so
	// the widget keys vary across either inline map — without which two widgets
	// sharing the (gvr/ns/name/pagination/request-extras) tuple but differing
	// in apiRef.extras OR resourcesRefsTemplateExtras would collide on one
	// identity-free cell and serve each other's body (the make-or-break crux).
	// The apiRef sub-cell (RAFullList) is keyed separately by the apiRef-
	// effective map threaded through apiref.Resolve (resolveApiRef §4.1) — the
	// rrt-inline map deliberately does NOT enter that sub-cell. Absent both
	// blocks ⇒ both accessors return {} ⇒ keyExtras == request extras ⇒
	// byte-identical keys (backward-compat). The accessors return deep copies,
	// so keyExtras never aliases the shared CR.
	keyExtras := effectiveKeyExtras(req.Context(), got.Unstructured.Object, extras)

	// Ship G (0.30.16x) — identity-free widget content L1 lookup runs
	// FIRST. Same gating semantics as the per-user lookup below
	// (strictly after EvaluateRBAC at :62-72). The content key is
	// (gvr, ns, name, perPage, page, extras) — Username/Groups
	// OMITTED — so admin and cyberjoker hit the SAME cell. The
	// cached body carries SA-evaluated `allowed=true` flags
	// (the F2 walker resolved under the snowplow SA); gateWidgetEnvelope
	// OVERWRITES every status.resourcesRefs.items[].allowed per-request
	// via rbac.UserCan under THIS request's identity, so the body that
	// leaves the pod is per-user-narrowed. The body in the cache is
	// the SHELL — never served verbatim.
	//
	// On MISS we fall through to the existing per-user widget L1 lookup
	// below — the expected path when F2 has not warmed this
	// (gvr, ns, name, perPage, page) tuple.
	//
	// Ship (task #69) — RBAC-sensitivity routing guard. The identity-free
	// widgetContent layer's serve-gate (gateWidgetEnvelope) only narrows
	// status.resourcesRefs.items[].allowed per requester; it NEVER narrows
	// status.widgetData. An apiRef-driven render-template widget
	// (piechart/table over an aggregating apiRef RA) renders from
	// status.widgetData, so serving it from the shared identity-free cell
	// would leak the SA-maximal aggregate cross-user. isRBACSensitiveApiRefWidget
	// runs PRE-RESOLVE on the fetched widget CR (got.Unstructured carries
	// spec.apiRef / spec.widgetDataTemplate / spec.resourcesRefsTemplate —
	// no extra apiserver round-trip). When it classifies the widget, the
	// ENTIRE identity-free lookup block is skipped and control falls
	// straight through to the per-cohort `widgets` L1 lookup below
	// (dispatchCacheLookupKey), which is RBAC-narrowed at resolve under
	// each cohort's own identity → no shared cell, no leak.
	if !isRBACSensitiveApiRefWidget(got.Unstructured.Object) {
		contentKey, contentHandle, _ := dispatchWidgetContentKey(req.Context(),
			got.GVR.Group, got.GVR.Version, got.GVR.Resource,
			got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
			perPage, page, keyExtras)
		if contentHandle != nil {
			if entry, ok := contentHandle.Get(contentKey); ok {
				if gated, served := gateWidgetEnvelope(req.Context(), entry.RawJSON); served {
					cache.RecordApiserverFallthrough(req.Context(),
						cache.ReasonWidgetContentHit, got.GVR.String())
					emitResolvedCacheLookup(log, "widgetContent", got.GVR.String(),
						contentKey, true, entry.SeededAtBoot, len(gated))
					pcs.l1Hit = "content-hit"
					setRefreshKeyHeader(wri, contentKey, "widgetContent")
					writeResolvedJSON(wri, gated)
					log.Info("Widget successfully resolved",
						slog.String("duration", util.ETA(start)),
						slog.String("l1", "content-hit"),
					)
					return
				}
				// served==false — fail-closed (no identity / malformed
				// stored envelope). Fall through to the existing per-user
				// L1 lookup, which symmetrically nil-checks UserInfo at
				// dispatchCacheLookupKey.
			}
			// Content-layer MISS — fall through to the per-user L1 lookup
			// below. Record the diagnostic counter.
			cache.RecordApiserverFallthrough(req.Context(),
				cache.ReasonWidgetContentMissPerUserFallback, got.GVR.String())
		}
	}

	// Tag 0.30.7: L1 resolved-output cache lookup. Same gating
	// semantics as restactions.go — strictly after EvaluateRBAC.
	// 0.30.8: cacheInputs is returned so we can stash it on the L1
	// entry for the refresher to drive a re-resolve on UPDATE.
	cacheKey, cacheHandle, cacheInputs := dispatchCacheLookupKey(req.Context(), "widgets",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, keyExtras)
	// Ship 0.30.188 — diagnostic slog: emit the dispatcher-side cache
	// key + its components so it can be diff'd against the PIP seed Put
	// log line at phase1_pip_seed.go and the per-user-fallback Put line
	// below. Pure-additive instrumentation per architect's TRACED diff.
	emitDispatchCacheKeyDiag(log, "dispatcher_get", req.Context(),
		cacheKey, cacheInputs, "widgets",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, keyExtras)
	// #95 SECURITY (A4 serve side): a re-derived BindingUID of "" collapses the
	// key to the shared empty-identity row — treat it as a CACHE MISS and fall
	// through to a direct resolve under THIS request's identity; NEVER serve the
	// shared "" cell to a different ""-deriving identity (cross-identity read of
	// a cell resolved under someone else's narrowing). Sibling of FIX-C's
	// populate-side skip; uniform predicate (serveFromCacheEligible, helpers.go).
	if cacheHandle != nil && serveFromCacheEligible(cacheInputs) {
		if entry, ok := cacheHandle.Get(cacheKey); ok {
			emitResolvedCacheLookup(log, "widgets", got.GVR.String(), cacheKey, true, entry.SeededAtBoot, len(entry.RawJSON))
			pcs.l1Hit = "hit"
			// External-widget bounded-TTL cache (Option A, 2026-07-10) — C2
			// arming kill on the HIT branch. The entry may have been Put under
			// the external-TTL path (no dep edges, never publishes); a HIT does
			// NO resolve, so there is no context ExternalTouchedSink to read —
			// the PERSISTED entry.ExternalTTL marker is the only available
			// signal. Suppress the refresh-key header for such an entry so the
			// browser never arms a /refreshes subscription for a key that can
			// never fire. entry.ExternalTTL is false for every normal entry →
			// byte-identical header behavior when the feature is off (C4).
			setRefreshKeyHeaderUnlessExternal(wri, cacheKey, "widgets", entry.ExternalTTL)
			writeResolvedJSON(wri, entry.RawJSON)
			log.Info("Widget successfully resolved",
				slog.String("duration", util.ETA(start)),
				slog.String("l1", "hit"),
			)
			return
		}
		emitResolvedCacheLookup(log, "widgets", got.GVR.String(), cacheKey, false, false, 0)
		pcs.l1Hit = "miss"
	}

	ctx := xcontext.BuildContext(req.Context())
	// Ship 0.30.167 — Option 2 parallelism regression fix. Symmetric
	// with restactions.go: read the SA transport pair from struct
	// fields populated once at Widgets() construction. AC-307.7 byte-
	// identical out-of-cluster: snowplowSACtx returned (nil, nil) so
	// both fields are nil and the attach below SKIPS.
	if r.saEP != nil && r.saRC != nil {
		ctx = cache.WithInternalEndpoint(ctx, r.saEP)
		ctx = cache.WithInternalRESTConfig(ctx, r.saRC)
	}
	// 0.30.94 Edge type 3: attach the L1 key being populated so the
	// underlying restactions resolver (called transitively via apiRef)
	// records dep edges against each inner K8s call. Widget L1 key
	// flows through into the inner resolver — Edge type 3 correctly
	// records against the widget L1 key because the widget cache entry
	// depends on every K8s object its underlying RestActions touch.
	if cacheKey != "" {
		ctx = cache.WithL1KeyContext(ctx, cacheKey)
	}
	// #83 Option A — seed THIS top-level widget as an ancestor of the
	// nested-resolve descent BEFORE resolving (symmetric with restactions.go).
	// The seam has a widget resolve arm (nested_call.go), so a top-level widget
	// whose nested resolve fans back to ITSELF would hit the same #79 off-by-one
	// as a self-referential RESTAction — the outermost node was never registered
	// so the cycle-stop fired one hop too late. Seeding it here makes the first
	// self-reentry an immediate cycle-stop. Same shared node derivation the
	// cycle-stop membership-checks (nested_call.go), so seed and check cannot
	// drift. No-op for a non-self-referential widget.
	ctx = cache.WithNestedResolveAncestor(ctx,
		nestedResolveNodeKey(got.GVR.Resource, got.Unstructured.GetNamespace(), got.Unstructured.GetName()))
	// R (composition-resources loopback guard) — re-seed the resolve ctx from the
	// inbound guard headers (TRUSTED self-loopback ONLY; no-op otherwise), twin of
	// restactions.go. Placed AFTER the #83 self-seed so the resolve ctx carries
	// {header ancestors} ∪ {this node}; the depth continues from the inbound hop.
	ctx = reseedNestedGuardsFromHeaders(ctx, req)

	// Ship 0.30.257 (#313) Cache-A — request-path error-aware Put-gate
	// (see restactions.go for the full rationale). A widget's apiRef
	// RESTAction is resolved transitively under THIS ctx; the api resolver
	// bumps the sink on any per-item iterator error. After #313 such a
	// partial-with-errors widget envelope is SERVED but MUST NOT be cached
	// in the per-cohort `widgets` L1 — gated on sink.Count()==0 below.
	ctx, stageErrSink := cache.WithStageErrorSink(ctx)
	// External-no-cache (proposal 2026-06-22) — install the external-touched
	// sink. A widget's apiRef RESTAction (and any nested resolve) runs under
	// THIS ctx; the api resolver bumps the sink when it reaches the live
	// external fetch. The Put-gate below declines to cache an external-touched
	// widget envelope. The sink reaches the external-fetch site via the
	// widget→apiref→RA ctx inheritance (context.WithValue is preserved down
	// the resolve chain). Additive to the stage-error sink.
	ctx, extTouchedSink := cache.WithExternalTouchedSink(ctx)

	res, err := widgetsResolveFn(ctx, widgets.ResolveOptions{
		In:      got.Unstructured,
		RC:      r.saRC,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve widget", slog.Any("err", err))
		var statusErr *apierrors.StatusError
		if errors.As(err, &statusErr) {
			code := int(statusErr.Status().Code)
			msg := fmt.Errorf("%s", statusErr.Status().Message)
			response.Encode(wri, response.New(code, msg))
			return
		}
		response.InternalError(wri, err)
		return
	}

	traceId := xcontext.TraceId(ctx, false)
	if traceId != "" {
		err := maps.SetNestedField(res.Object, traceId, "status", "traceId")
		if err != nil {
			log.Warn("unable to set traceId in status", slog.Any("err", err))
		}
	}

	// #72 inline-rendered-children PRODUCER (Phase 1, default-off): embed any
	// inline+allowed+GET resourcesRefs child's resolved envelope into
	// items[i].rendered, server-side under THIS per-user ctx (carrying
	// WithUserInfo + the SA transport + WithL1KeyContext(cacheKey)). No-op when
	// no item carries inline → byte-identical to today. Runs BEFORE the single
	// encodeResolvedJSON; each inline child is decoded into a map (deep-copy-safe,
	// A1 path i — NOT json.RawMessage, which panics the unstructured deep-copy)
	// and re-encoded once with the parent. RBAC is enforced per-child by ResolveNestedCall;
	// the inline widget is routed to the per-user `widgets` L1 (not the shared
	// content cell) by isRBACSensitiveApiRefWidget's hasInlineGETRef OR-clause.
	if n := embedInlineChildren(ctx, log, res, perPage, page, extras); n > 0 {
		log.Info("inline-rendered-children embedded",
			slog.Int("count", n),
			slog.String("widget", got.Unstructured.GetNamespace()+"/"+got.Unstructured.GetName()),
		)
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		log.Error("unable to encode widget response", slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}
	// External-widget bounded-TTL cache (Option A, 2026-07-10) — declared
	// BEFORE the if/else-if Put-gate chain so the cold-Put tail (below) can
	// read it on ALL paths. Set true ONLY by the external-TTL else-if; every
	// other branch leaves it false → byte-identical refresh-key header
	// behavior on the tail for them (C4).
	servedExternalTTL := false

	// Ship 0.30.257 (#313) Cache-A — skip the Put on ANY per-item stage
	// error (symmetric with restactions.go + the refresher gate). The
	// partial-with-errors widget body is SERVED below; it is just not
	// PERSISTED, so a transient apiRef-item failure self-heals on the next
	// resolve instead of pinning a partial cell for the TTL.
	if stageErrSink.Count() > 0 {
		// D (bounded partial-cache backstop, default-off) — twin of restactions.go;
		// Put the partial under the SAME per-user widgets cacheKey with a bounded
		// PARTIAL_RESULT_TTL_SECONDS window. No-op when the env is 0 (default). With
		// R landed the widget path resolves clean so this branch is not reached for
		// a self-referential widget (C6).
		staleCached := putPartialWithTTL(cacheHandle, cacheKey, encoded, cacheInputs,
			got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())
		log.Warn("Widget served with per-item stage error(s); declining to cache the partial result",
			slog.Int64("stage_errors", stageErrSink.Count()),
			slog.Bool("partial_bounded_stale_cached", staleCached),
			slog.String("partial_ttl_s", partialResultTTL().String()),
			slog.String("effect", "partial body served (200); not persisted under the full TTL — transient item failures self-heal on next resolve (D bounded-stale window if enabled)"),
		)
	} else if extTTL := externalCacheTTLFromAnnotations(got.Unstructured); extTouchedSink.Count() > 0 && extTTL > 0 &&
		cacheHandle != nil && cacheKey != "" && serveFromCacheEligible(cacheInputs) {
		// External-widget bounded-TTL cache (Option A, 2026-07-10) — the
		// widget's resolve touched a genuine external endpoint AND the widget
		// CR opted in via the `krateo.io/external-cache-ttl-seconds` annotation
		// (extTTL>0, capped). INVERT the decline below: Put the external result
		// under the UNCHANGED per-binding `widgets` cacheKey with a short
		// TTLOverride so it serves from L1 for at most the TTL, then TTL-evicts
		// and re-fetches live. Correctness basis = time-bounded staleness, NOT
		// dep-edge invalidation (design §2) — so we deliberately SKIP the
		// dep-Record and the publish (an external entry has no dep edges and
		// must never enter the /refreshes path, design §6.3).
		//
		// serveFromCacheEligible gates the same #95 "" BindingUID collapse as
		// the genuine-Put branch: a ""-derived key never populates the shared
		// empty-identity cell. The per-binding key is UNCHANGED — per-user
		// isolation rides on the existing BindingUID fold (ComputeKey), NEVER an
		// identity-free cell (design §4.1).
		//
		// ExternalTTL:true persists the C2 marker so the HIT-serve branch and
		// this branch's cold tail both suppress the refresh-key header (the
		// arming kill, §6). No BumpExternalSkippedPut here — the Put was ALLOWED.
		emitDispatchCacheKeyDiag(log, "external_ttl_put", req.Context(),
			cacheKey, cacheInputs, "widgets",
			got.GVR.Group, got.GVR.Version, got.GVR.Resource,
			got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
			perPage, page, keyExtras)
		cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
			RawJSON:     encoded,
			Inputs:      cacheInputs,
			TTLOverride: extTTL,
			ExternalTTL: true,
		})
		servedExternalTTL = true
		log.Info("Widget touched an external endpoint; caching with bounded external TTL (opt-in annotation)",
			slog.Int64("external_touches", extTouchedSink.Count()),
			slog.String("external_ttl", extTTL.String()),
			slog.String("effect", "body served (200) + persisted under the per-binding widgets key with TTLOverride; served from L1 for <=TTL then re-fetched live; NO dep-Record, NO publish, NO /refreshes arm"),
		)
	} else if extTouchedSink.Count() > 0 {
		// External-no-cache (proposal 2026-06-22) — the widget's resolve
		// touched a genuine external endpoint (e.g. an external apiRef RA) and
		// did NOT opt in (default OFF). Serve the envelope but DECLINE the Put +
		// the dep Record (no informer edge can invalidate external data).
		// Re-fetched live on every /call. Byte-identical to pre-Option-A.
		cache.BumpExternalSkippedPut()
		log.Warn("Widget touched an external endpoint; declining to cache (external data has no dep edge to invalidate)",
			slog.Int64("external_touches", extTouchedSink.Count()),
			slog.String("effect", "body served (200); not persisted — external API re-fetched live on every /call"),
		)
	} else if !requestExtrasFullyDeclared(got.Unstructured.Object, extras) {
		// F6 SELF-QUARANTINE (arch-ruled 2026-07-13, docs/f6-chrome-route-key-
		// design-2026-07-12.md §4). This request carries request-extras the widget
		// did NOT declare in spec.keyExtras. Post-F6 those undeclared extras DROP
		// from the per-cohort key, so this request's key == a co-cohort clean
		// request's key — but this request's RAW extras reached widgets.Resolve, so
		// the rendered body is extras-influenced. Writing it into the shared cohort
		// cell would serve THIS request's extras-shaped body to a co-cohort user who
		// sent none (the leak F6 would otherwise open, since dropping the extras from
		// the key removed the pre-F6 self-quarantine). SYMMETRIC with the
		// extTouchedSink decline above: serve THIS request's correct body (already
		// written below the if-chain), bump the counter, decline the Put + dep-Record
		// + publishIfSubscribed. The declared corpus + chrome widgets never reach here
		// (their supplied extras are all declared, or they send none). Placed BEFORE
		// the genuine-Put branch so a polluting request cannot fall into it.
		bumpWidgetSkippedUndeclaredExtrasPut()
		log.Warn("Widget request carried extras not declared in spec.keyExtras; declining to cache (would pollute the shared per-cohort cell post-F6)",
			slog.String("effect", "body served (200) under this request's own extras; not persisted — a co-cohort request without these extras must not be served this extras-shaped body"),
		)
	} else if cacheHandle != nil && cacheKey != "" && serveFromCacheEligible(cacheInputs) {
		// #95 SECURITY (A4 populate side, customer path) — the single-mechanism
		// closure. A re-derived BindingUID of "" collapses to the shared
		// empty-identity cell; do NOT POPULATE it from a customer request. The
		// request still resolved + served its own content above; it just never
		// writes the shared cell. This is load-bearing (not "dead weight"): a ""
		// BindingUID does NOT make cacheKey=="", so the refresher (refresher.go
		// Get→re-resolve→re-Put) and SSE subscription arming
		// (refresh_subscription.go) would READ/deliver a populated ""-cell — the
		// serve-guard alone leaves those readers exposed. Gating the Put closes
		// the population source once, uniformly (sibling of seed FIX-C's skip +
		// the serve-side #95 guard). serveFromCacheEligible is the SAME predicate
		// (helpers.go).
		//
		// Ship 0.30.188 — diagnostic slog: emit the per-user-fallback Put
		// site's cache key + components so they can be diff'd against the
		// dispatcher_get (above) and seed (phase1_pip_seed.go) emissions
		// for the SAME widget. If a PIP seed Put existed under a different
		// key, this Put establishes the cohort's "real" served cell; the
		// divergent field(s) identify the seed/dispatcher key mismatch.
		emitDispatchCacheKeyDiag(log, "per_user_fallback_put", req.Context(),
			cacheKey, cacheInputs, "widgets",
			got.GVR.Group, got.GVR.Version, got.GVR.Resource,
			got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
			perPage, page, keyExtras)
		// scope-waiver:TTLOverride: widgets-class cell — UAF refilter output only ever lands in a restactions-class cell (a widget's apiRef resolves the UAF RA via the seedOneRestaction/dispatch path, which IS capped); a widgets Put is never the UAF-stale cell (uaf_shortttl.go R-d-4 SITE MAP).
		cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
			RawJSON: encoded,
			Inputs:  cacheInputs,
		})
		// 0.30.8: record dep edges. Widget self-dep, apiRef→RestAction
		// dep, and render-eligible resourcesRefs deps (action-only
		// refs filtered out per Revision 14). Edge type 3 (inner K8s
		// calls inside the RestAction) is OUT OF SCOPE at this tag.
		recordWidgetDeps(log, cacheKey, got.GVR, res)

		// #62: GENUINE cold-dispatch Put (this else-if guarantees a real
		// Put + dep-Record — never the stage-error / external-skip declines
		// above). If a /refreshes connection is already armed for this key
		// (it re-armed after a TTL-eviction, and this cold-fill replaces the
		// evicted entry), announce the fill so the viewer's frame goes fresh
		// now instead of waiting for the next churn. No-op when unarmed.
		// SCOPE: widgets only — NOT widgetContent (shared key w/o BindingUID
		// fold → cross-deliver risk) per refresh_publish.go.
		publishIfSubscribed(cacheKey)
	}

	log.Info("Widget successfully resolved",
		slog.String("duration", util.ETA(start)),
		slog.String("l1", "miss"),
	)

	// External-widget bounded-TTL cache (Option A, 2026-07-10) — C2 arming
	// kill on the cold tail. This line serves ALL non-HIT paths (genuine Put,
	// stage-error decline, external-skip decline, AND the new external-TTL
	// Put). Suppress the refresh-key header ONLY for the external-TTL Put
	// (servedExternalTTL==true); every other branch left it false → byte-
	// identical header behavior for them (C4).
	setRefreshKeyHeaderUnlessExternal(wri, cacheKey, "widgets", servedExternalTTL)
	writeResolvedJSON(wri, encoded)
}
