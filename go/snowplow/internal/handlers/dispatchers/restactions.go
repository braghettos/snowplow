package dispatchers

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/http/response"
	"github.com/krateo-platformops/snowplow/apis"
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/util"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

func RESTAction() http.Handler {
	// Ship 0.30.167 — Option 2 parallelism regression fix.
	// Resolve the snowplow SA endpoint + *rest.Config ONCE at handler
	// construction (the same cadence the refresher uses at
	// dispatchers.go:56-66 RegisterRefreshHandlers — the load-bearing
	// prior art for this shape). The resolved pair is then captured as
	// struct fields and attached to the per-request ctx in ServeHTTP
	// via a cheap nil-check + field read, eliminating the per-request
	// snowplowSACtx() helper call (which transitively re-acquired the
	// SA singletons' mutexes on every dispatch).
	//
	// Out-of-cluster (unit tests, developer runs): snowplowSACtx
	// returns (nil, nil); both fields stay nil; the ServeHTTP attach
	// block then skips the WithInternalEndpoint / WithInternalRESTConfig
	// calls, preserving AC-307.7 byte-identically.
	saEP, saRC := snowplowSACtx()
	return &restActionHandler{
		authnNS: env.String("AUTHN_NAMESPACE", ""),
		verbose: env.True("DEBUG"),
		saEP:    saEP,
		saRC:    saRC,
	}
}

type restActionHandler struct {
	authnNS string
	verbose bool
	// saEP + saRC are the snowplow ServiceAccount transport pair
	// captured at handler construction (Ship 0.30.167 Option 2). Both
	// may be nil in out-of-cluster runs; ServeHTTP nil-checks before
	// attaching to the request ctx. Mirrors RegisterRefreshHandlers'
	// closure-captured saEP/saRC at dispatchers.go:56-66.
	saEP *endpoints.Endpoint
	saRC *rest.Config
}

var _ http.Handler = (*restActionHandler)(nil)

func (r *restActionHandler) ServeHTTP(wri http.ResponseWriter, req *http.Request) {
	log := xcontext.Logger(req.Context())

	start := time.Now()

	// Ship 0.30.171-debug — per-/call structured timing log. Emits
	// dispatcher.call.complete into the snowplow stdout -> otel-
	// daemonset filelog -> ClickHouse otel_logs pipeline at every
	// ServeHTTP exit (success, RBAC-deny, error). Used to identify
	// the slow /call class in the 8-cycle parallelism diagnostic.
	pcs, pcEmit := beginPerCall(req, "restactions")
	defer pcEmit()

	// Ship 1 — customer-priority signal. Mark this /call as in-flight so
	// the prewarm engine yields its background re-seed work for the
	// duration of the dispatch (feedback_customer_priority_over_refresher).
	// Cheap atomic; deferred decrement covers every return path.
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

	// Revision 2 binding (0.30.4): in cache=on mode every RestAction
	// dispatch is gated by EvaluateRBAC against the CR being dispatched.
	// Cache=off skips this gate — fetchObject already runs per-user
	// against apiserver, which enforces RBAC inline.
	if !cache.Disabled() {
		if !checkDispatchRBACFn(req.Context(), got.GVR, got.Unstructured.GetNamespace()) {
			log.Warn("RESTAction dispatch denied by EvaluateRBAC",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("gvr", got.GVR.String()),
			)
			response.Encode(wri, response.New(http.StatusForbidden,
				fmt.Errorf("forbidden: cannot get %s in namespace %q",
					got.GVR.Resource, got.Unstructured.GetNamespace())))
			return
		}
	}

	// R (composition-resources loopback guard) — HTTP-edge cycle-stop.
	// PLACEMENT (C3): strictly AFTER fetchObject + checkDispatchRBAC above
	// (the stop is NEVER an RBAC bypass — it fires only on an authorized, real
	// CR) and BEFORE the L1 lookup below. Re-seed the nested-resolve guards from
	// the inbound X-Snowplow-{Nested-Depth,Resolve-Ancestors} headers — but ONLY
	// for a TRUSTED self-loopback (isTrustedSelfLoopback; an untrusted/external
	// caller's headers are ignored → guardCtx carries no guards → no stop → a
	// normal full resolve, C2) — then add THIS request's own node and check the
	// HTTP-edge twin of the #79 in-process stop (nested_call.go:114,169). On a
	// self-reference over the HTTP boundary we serve the RAW CR (resolve:false
	// semantics) instead of recursing; the depth-8 backstop trips a bounded
	// error for a non-cyclic pathologically deep chain. guardCtx is throwaway
	// (only the stop decision is read from it); the resolve ctx is re-seeded
	// separately below so its guards flow into the resolver.
	if !cache.Disabled() {
		// Seed ONLY the inbound header ancestors (the parent descent path); do
		// NOT add THIS node before the check — the in-process contract
		// (nested_call.go Step 3.5) membership-checks the node against the
		// INBOUND ancestors BEFORE adding it, so a self-reference is detected
		// because a PRIOR hop of this same node already put it in the emitted
		// header set. Adding it here first would make every call self-match.
		guardCtx := reseedNestedGuardsFromHeaders(req.Context(), req)
		if stop, raw := httpEdgeGuardStop(guardCtx, log,
			got.GVR.Resource, got.Unstructured.GetNamespace(), got.Unstructured.GetName()); stop {
			if raw {
				// Self-reference: return the RAW CR (the same bytes the
				// in-process cycle-stop returns, nested_call.go:174) — the
				// outer resolve then completes with Count()==0 and caches.
				encoded, encErr := encodeResolvedJSON(got.Unstructured.Object)
				if encErr != nil {
					response.InternalError(wri, encErr)
					return
				}
				writeResolvedJSON(wri, encoded)
				return
			}
			// Depth-8 backstop: bounded 508-class error (never empty, never a
			// panic), mirroring the in-process depth guard (nested_call.go:114).
			response.Encode(wri, response.New(http.StatusLoopDetected,
				fmt.Errorf("nested resolve depth limit exceeded (%d) for %s/%s/%s over HTTP self-loopback",
					cache.NestedCallMaxDepth(), got.GVR.Resource,
					got.Unstructured.GetNamespace(), got.Unstructured.GetName())))
			return
		}
	}

	perPage, page := paginationInfo(log, req)

	// Tag 0.30.7: L1 resolved-output cache lookup. Runs strictly
	// AFTER the EvaluateRBAC gate above (Revision 2 binding) so the
	// permission check is never short-circuited by a cache hit. Cache
	// hits short-circuit the resolver + JSON re-encode; misses fall
	// through to the 0.30.6-equivalent resolve-and-encode path.
	//
	// Per feedback_l1_invalidation_delete_only.md:
	//   * DELETE evicts dependent L1 keys (0.30.8 dep tracker).
	//   * UPDATE/PATCH enqueue refresh via the background refresher
	//     (stale-while-revalidate; never evicts).
	//   * TTL remains the outer safety net.
	cacheKey, cacheHandle, cacheInputs := dispatchCacheLookupKey(req.Context(), "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, extras)
	// Ship 0.30.188 — diagnostic slog: emit the dispatcher-side cache
	// key + components symmetrically with widgets.go for the PIP-seed
	// vs dispatcher-get key-divergence investigation.
	emitDispatchCacheKeyDiag(log, "dispatcher_get", req.Context(),
		cacheKey, cacheInputs, "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		perPage, page, extras)
	// #95 SECURITY (A4 serve side) — sibling of FIX-C's populate-side skip,
	// uniform with widgets.go: a re-derived BindingUID of "" collapses the key
	// to the shared empty-identity row → treat as a CACHE MISS, fall through to
	// a direct resolve under THIS request's identity; never serve the shared ""
	// cell to a different ""-deriving identity. See serveFromCacheEligible.
	if cacheHandle != nil && serveFromCacheEligible(cacheInputs) {
		if entry, ok := cacheHandle.Get(cacheKey); ok {
			emitResolvedCacheLookup(log, "restactions", got.GVR.String(), cacheKey, true, entry.SeededAtBoot, len(entry.RawJSON))
			pcs.l1Hit = "hit"
			setRefreshKeyHeader(wri, cacheKey, "restactions")
			writeResolvedJSON(wri, entry.RawJSON)
			log.Info("RESTAction successfully resolved",
				slog.String("name", got.Unstructured.GetName()),
				slog.String("namespace", got.Unstructured.GetNamespace()),
				slog.String("duration", util.ETA(start)),
				slog.String("l1", "hit"),
			)
			return
		}
		emitResolvedCacheLookup(log, "restactions", got.GVR.String(), cacheKey, false, false, 0)
		pcs.l1Hit = "miss"
	}

	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		log.Error("unable to add apis to scheme",
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	var cr v1.RESTAction
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr)
	if err != nil {
		log.Error("unable to convert unstructured to typed rest action",
			slog.String("name", got.Unstructured.GetName()),
			slog.String("namespace", got.Unstructured.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	ctx := xcontext.BuildContext(req.Context())
	// Ship 0.30.167 — Option 2 parallelism regression fix.
	// Read the SA transport pair from struct fields populated once at
	// RESTAction() construction (Ship 0.30.166 attached the pair to ctx
	// PER REQUEST via snowplowSACtx() — under concurrent /call load that
	// serialised every dispatch through the SA singletons' mutexes).
	// The construction-time capture mirrors RegisterRefreshHandlers'
	// closure-captured saEP/saRC at dispatchers.go:56-66.
	//
	// AC-307.7 OUT-OF-CLUSTER INVARIANT: snowplowSACtx returns (nil, nil)
	// when the projected SA volume is absent (every unit test, every
	// out-of-cluster developer run); both fields stay nil; the nil-guard
	// below then SKIPS the attach and the request ctx is byte-identical
	// to pre-0.30.166.
	if r.saEP != nil && r.saRC != nil {
		ctx = cache.WithInternalEndpoint(ctx, r.saEP)
		ctx = cache.WithInternalRESTConfig(ctx, r.saRC)
	}
	// 0.30.94 Edge type 3: attach the L1 key being populated so the
	// resolver can record dep edges for each inner K8s call it makes.
	// Empty cacheKey (L1 disabled, RBAC-skipped) is a no-op inside
	// WithL1KeyContext — the resolver sees an empty key and skips
	// recording.
	if cacheKey != "" {
		ctx = cache.WithL1KeyContext(ctx, cacheKey)
	}
	// #83 Option A — seed THIS top-level RESTAction as an ancestor of the
	// nested-resolve descent BEFORE resolving. The #79 cycle-stop
	// (nested_call.go Step 3.5) only sees nodes registered on descent via
	// ResolveNestedCall; the OUTERMOST node (this request's own CR) was never
	// registered, so a composition RA whose allCompositionResources managed set
	// includes ITSELF recursed one full hop before the cycle detector could
	// fire — the inner self-resolve then ran its own top-level jq filter on an
	// empty `.discovery` stage (cannot iterate over: null), producing a per-item
	// stage error → decline-to-cache → permanent cold re-fan-out (~1500× per
	// /call, never converging). Seeding the node here makes the FIRST
	// self-reentry an immediate cycle-stop (1 hop, raw CR, no inner resolve, no
	// null-iterate, clean parent Put → convergence). The node string uses the
	// SAME shared derivation the cycle-stop membership-checks (nested_call.go),
	// so the seed and the check cannot drift. No-op for a non-self-referential
	// RA (the node is simply never reencountered on descent).
	ctx = cache.WithNestedResolveAncestor(ctx,
		nestedResolveNodeKey(got.GVR.Resource, got.Unstructured.GetNamespace(), got.Unstructured.GetName()))
	// R (composition-resources loopback guard) — re-seed the resolve ctx from
	// the inbound X-Snowplow-{Nested-Depth,Resolve-Ancestors} headers (TRUSTED
	// self-loopback ONLY — reseedNestedGuardsFromHeaders no-ops otherwise), so
	// the DEEPER descent's guards flow into the resolver: the next self-dispatch
	// emits depth+1 and the header ancestor set, and a deeper self-reentry stops
	// at THIS handler's HTTP-edge check (above) on the next hop. Placed AFTER the
	// #83 self-seed so the resolve ctx carries {header ancestors} ∪ {this node};
	// the depth-8 counter continues from the inbound hop's value.
	ctx = reseedNestedGuardsFromHeaders(ctx, req)
	// Ship 0.30.257 (#313) Cache-A — request-path error-aware Put-gate.
	// Install a stage-error sink on the resolve ctx (the SAME seam the
	// background refresher uses at resolve_populate.go:206). The api
	// resolver bumps it whenever it writes dict[errorKey] for a per-item
	// hard error (resolve.go error branches — UNCHANGED by #313). After
	// #313 a per-item iterator failure no longer truncates the result;
	// the partial-with-errors body is SERVED (200) but MUST NOT be
	// PERSISTED — caching a partial pins it for the TTL, so a transient
	// item failure would self-heal far slower. The Put below is gated on
	// sink.Count()==0 — exactly the 0.30.254 posture (never cache an
	// under-served result), reusing the existing sink (prior-art-in-repo,
	// no new mechanism). The request path installed NO sink before
	// 0.30.257, so this is the first request-path consumer; the resolver's
	// bump sites already existed.
	ctx, stageErrSink := cache.WithStageErrorSink(ctx)
	// External-no-cache (proposal 2026-06-22) — install the external-touched
	// sink on the resolve ctx. The api resolver bumps it whenever a stage
	// reaches the live external fetch (httpFetchAllowingNonJSON); the Put-gate
	// below reads Count()>0 and declines to cache an external-touched result
	// (no informer/dep edge can invalidate it). Additive to the stage-error
	// sink — both gate the Put independently. nil-receiver-safe.
	ctx, extTouchedSink := cache.WithExternalTouchedSink(ctx)
	// 1.12.3 A-1 / R-1 — install the UAF-touched sink, third sibling of the two
	// above. Whatever bumps it, wherever that happens and however many resolver
	// frames down, the Put-gate below reads Count()>0 and declines.
	//
	// It is the mechanism INTENDED to cover a NON-UAF RESTAction that NESTS a UAF
	// one — its own spec declares no userAccessFilter, so the declaration limb is
	// blind to it, while its resolved body still carries per-requester-narrowed
	// rows. That case is NOT closed on this branch: the only bump today is
	// declaration-based (apiref.Resolve) and inspects the parent. It closes with
	// the refilter bump on fix/1.12.3-authz-hardening; until then
	// TestM1_NestedUAFChild_NoCellPut_RequiresRefilterBump is RED by design.
	ctx, uafTouchedSink := cache.WithUAFTouchedSink(ctx)
	res, err := restactionsResolveFn(ctx, restactions.ResolveOptions{
		In:      &cr,
		SArc:    r.saRC,
		AuthnNS: r.authnNS,
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	})
	if err != nil {
		log.Error("unable to resolve rest action",
			slog.String("name", cr.GetName()),
			slog.String("namespace", cr.GetNamespace()),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}

	// Encode once, write once, and (if L1 is live) store the encoded
	// bytes for the next lookup. Sharing the same []byte between the
	// http.ResponseWriter write path and the cache entry is safe
	// because the cache treats RawJSON as immutable once put.
	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		log.Error("unable to encode rest action response",
			slog.String("name", cr.Name),
			slog.String("namespace", cr.Namespace),
			slog.Any("err", err))
		response.InternalError(wri, err)
		return
	}
	// Ship 0.30.257 (#313) Cache-A — skip the Put on ANY per-item stage
	// error. The body is already SERVED below (200 + all successful items +
	// the accumulated per-item errors); we just decline to PERSIST a
	// partial-with-errors result. Symmetric with the refresher Put-gate
	// (resolve_populate.go:242) and the 0.30.254 "never cache an under-served
	// result" posture. sink==nil is nil-receiver-safe (Count()==0). A clean
	// resolve (Count()==0) caches exactly as before.
	// 1.12.3 A-1 (SECURITY, cross-tenant) — record whether THIS RESTAction
	// declares a userAccessFilter stage on the carried Inputs, BEFORE the Put
	// chain. Two consumers read it: the decline gate at the head of the chain,
	// and (for a non-declined entry) the short UAF TTLOverride, which the
	// refresher re-Put re-derives from the SAME carried flag (C-118-6). HasUAF is
	// deliberately NOT key-folded — folding a bare boolean would not separate two
	// users' divergent narrowings anyway; the per-requester UAF SCOPE digest is
	// the 1.13.0/v7 key change.
	if cacheInputs != nil {
		cacheInputs.HasUAF = restactionHasUAFStage(&cr)
	}
	// THE UAF DECLINE IS FIRST IN THE CHAIN, and the position is load-bearing.
	// TWO branches below this point also WRITE: the stage-error branch Puts a
	// bounded partial via putPartialWithTTL (when PARTIAL_RESULT_TTL_SECONDS is
	// set), and — in the widgets twin of this chain — the external-TTL branch
	// Puts under an opt-in annotation. Placing the UAF gate only in front of the
	// "genuine Put" branch would leave a UAF body reachable through either of
	// those, under the same shared per-binding key. A per-requester-narrowed body
	// must not be persisted by ANY branch, so the gate sits ahead of all of them.
	//
	// The key this body would be stored under folds only BindingUID + RBACSubGen,
	// so a co-bound user with a DIFFERENT per-object narrowing derives the SAME
	// key and the hit path (above) would hand them these bytes verbatim. Serving
	// is unaffected — the 200 with this requester's own body is written below
	// either way; only the shared-cell write is skipped, so no other identity can
	// ever read it. Reverts in 1.13.0 when the UAF-scope digest (v7) separates
	// them in the key.
	if reason := uafDeclineReason(cacheInputs, uafTouchedSink); reason != "" {
		declineUAFPut(cacheInputs, uafTouchedSink) // bumps the class counter
		// DEBUG, not WARN: this fires on EVERY /call of a UAF RA and
		// LOG_LEVEL=warn is the production floor — a WARN here would flood.
		// snowplow_restactions_uaf_put_declined_total carries the rate.
		log.Debug("RESTAction resolve is userAccessFilter-narrowed; declining to cache (per-requester narrowing is not folded into the key)",
			slog.String("name", cr.Name),
			slog.String("namespace", cr.Namespace),
			slog.String("key_hash", cacheKey),
			slog.String("uaf_reason", reason),
			slog.Int64("uaf_touches", uafTouchedSink.Count()),
			slog.String("effect", "body served (200) narrowed for THIS requester; not persisted — a co-bound user would key onto the same cell and be served these rows (1.12.3 A-1; 1.13.0 folds the UAF scope into the key)"),
		)
	} else if stageErrSink.Count() > 0 {
		// D (bounded partial-cache backstop, default-off) — instead of a bare
		// decline, Put the partial under the SAME per-user cacheKey with a
		// bounded PARTIAL_RESULT_TTL_SECONDS window so a residual un-cacheable RA
		// does not re-storm cold every /call. No-op when PARTIAL_RESULT_TTL_SECONDS
		// is 0 (default) → byte-identical to the pre-D bare decline. With R landed
		// composition-resources resolves clean (Count()==0) so this branch is not
		// even reached for it (C6). Post-serve: the body is written below either
		// way; D only decides whether it is ALSO cached for the bounded window.
		staleCached := putPartialWithTTL(cacheHandle, cacheKey, encoded, cacheInputs,
			got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())
		log.Warn("RESTAction served with per-item stage error(s); declining to cache the partial result",
			slog.String("name", cr.Name),
			slog.String("namespace", cr.Namespace),
			slog.Int64("stage_errors", stageErrSink.Count()),
			slog.Bool("partial_bounded_stale_cached", staleCached),
			slog.String("partial_ttl_s", partialResultTTL().String()),
			slog.String("effect", "partial body served (200); not persisted under the full TTL — transient item failures self-heal on next resolve (D bounded-stale window if enabled)"),
		)
	} else if extTouchedSink.Count() > 0 {
		// External-no-cache (proposal 2026-06-22) — the resolve touched a
		// genuine external endpoint (no informer/dep edge to invalidate it).
		// Serve the body (already encoded above) but DECLINE the Put so every
		// /call re-fetches the external API live (fresh). Declining the Put
		// also declines the self-dep Record below — an external RA never
		// enters the DepTracker. BumpExternalSkippedPut is the process-wide
		// "did the gate fire?" falsifier.
		cache.BumpExternalSkippedPut()
		log.Warn("RESTAction touched an external endpoint; declining to cache (external data has no dep edge to invalidate)",
			slog.String("name", cr.Name),
			slog.String("namespace", cr.Namespace),
			slog.Int64("external_touches", extTouchedSink.Count()),
			slog.String("effect", "body served (200); not persisted — external API re-fetched live on every /call"),
		)
	} else if cacheHandle != nil && cacheKey != "" && serveFromCacheEligible(cacheInputs) {
		// #95 SECURITY (A4 populate side, customer path) — sibling of the
		// widgets.go Put-gate + seed FIX-C: a re-derived BindingUID of "" must
		// NOT POPULATE the shared empty-identity cell (the refresher + SSE
		// subscription readers would otherwise deliver it — a "" BindingUID does
		// NOT make cacheKey==""). The request still served its own content; it
		// just never writes the shared cell. serveFromCacheEligible (helpers.go).
		//
		// Ship 0.30.188 — diagnostic slog: emit the per-user-fallback
		// Put site's cache key + components symmetrically with widgets.go.
		emitDispatchCacheKeyDiag(log, "per_user_fallback_put", req.Context(),
			cacheKey, cacheInputs, "restactions",
			got.GVR.Group, got.GVR.Version, got.GVR.Resource,
			got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
			perPage, page, extras)
		// The UAF gate already ran at the HEAD of this chain — it must precede the
		// stage-error branch's bounded partial Put, not merely this branch — so
		// reaching here means the body is NOT per-requester-narrowed. HasUAF was
		// stamped on cacheInputs there; it is read here only for the TTL override.
		{
			cacheHandle.Put(cacheKey, &cache.ResolvedEntry{
				RawJSON:     encoded,
				Inputs:      cacheInputs,
				TTLOverride: uafTTLOverrideForEntry(cacheInputs),
			})
			// 0.30.8: record the self-dep so a DELETE on this RestAction
			// CR evicts the cached entry, and an UPDATE re-resolves it.
			// Inner-K8s-call deps (edge type 3) are NOT recorded at this
			// tag — that would require a *RecordingDeps context threaded
			// through resolve.go, which is deferred to a future sub-ship.
			// TTL remains the outer safety net for changes the dep
			// tracker cannot see.
			//
			// 0.30.9 Sub-scope B: ensure the informer for got.GVR is
			// registered BEFORE recording the dep. Without this, a
			// previously-unseen RestAction GVR would record a forward
			// edge whose DELETE/UPDATE events the watcher never wires.
			//
			// 1.12.3 A-1: the dep Record + the /refreshes publish are INSIDE the
			// non-declined branch on purpose — a declined entry has no cell, so
			// there is nothing for a dep edge to invalidate and nothing for a
			// subscriber to be told about (the same shape as the external-skip
			// decline above, which also declines its Record).
			ensureWatcherInformerForGVR(got.GVR)
			cache.Deps().Record(cacheKey, got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())

			// #62: GENUINE cold-dispatch Put (this else-if guarantees a real
			// Put + dep-Record — never the stage-error / external-skip declines
			// above). If a /refreshes connection is already armed for this key
			// (it re-armed after a TTL-eviction, and this cold-fill replaces the
			// evicted entry), announce the fill so the viewer's frame goes fresh
			// now instead of waiting for the next churn. No-op when unarmed.
			publishIfSubscribed(cacheKey)
		}
	}

	log.Info("RESTAction successfully resolved",
		slog.String("name", cr.Name),
		slog.String("namespace", cr.Namespace),
		slog.String("duration", util.ETA(start)),
		slog.String("l1", "miss"),
	)

	setRefreshKeyHeader(wri, cacheKey, "restactions")
	writeResolvedJSON(wri, encoded)
}
