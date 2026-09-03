// cluster_list.go — Ship D.5 / 0.30.152: cluster-list-when-allowed
// iterator collapse.
//
// When an RA opts in (spec.api[].clusterListWhenAllowed=true) AND the
// requester holds cluster-scope `list` on the target GVR AND the cache
// is enabled AND the Ship B typed-RBAC snapshot is published, the
// per-namespace iterator fan-out is collapsed to a SINGLE cluster-scope
// LIST against /apis/<g>/<v>/<resource>. Everything else (cache key
// derivation, identity-free apistage L1 entry, gateContentEnvelope
// per-user narrowing) flows through the existing F1 content layer.
//
// The decision is taken in `attemptClusterListCollapse` BEFORE the
// resolver's bounded errgroup. On success the helper returns ONE
// httpcall.RequestOptions (path = cluster-scope form) that the existing
// worker loop runs through apistageContentServe — the first dispatch
// populates the cache, every subsequent cluster-list-permitted user
// gets a content hit.
//
// AC-D5.13 — cache-off + Servable gate. The helper short-circuits when
//   cache.Disabled() == true (project_caching_is_provisional invariant)
//   OR when cache.Global().Snapshot() == nil (pre-readiness window: the
//   RBAC snapshot has not been published yet, so EvaluateRBAC degrades
//   to deny — we must NOT execute against a nil snapshot).
//
// AC-D5.14 — defensive multi-element shape check. After the un-gated
//   cluster-scope dispatch returns, the raw envelope is verified to be
//   a list envelope (kind ends in "List"), with a non-empty .items
//   array, and each item carries non-nil apiVersion + kind strings. On
//   any failure: the helper records cache.ReasonClusterListShapeFallback
//   and returns useClusterList=false so the resolver falls through to
//   the per-NS iterator path.
//
// All cache changes preserve the layering contract per
// feedback_restaction_no_widget_logic — the per-user RBAC narrowing
// stays at the existing gateContentEnvelope site; the cluster-list
// entry stores only the un-gated content envelope (identity-free).

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/jqutil"
	"github.com/krateo-platformops/plumbing/ptr"
	templates "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func goMaxProcs() int { return runtime.GOMAXPROCS(0) }

// shapeCheckSlowThreshold is the AC-D5.14 conditional ratification
// budget: the defensive shape check must complete within 10ms. A slow
// fire emits a WARN so PM/architect see budget-busting evidence and
// can re-ratify. The cluster-scope envelope is parsed once at the Put
// site (apistageContentServe does the same parse on the miss path) so
// the shape check is essentially scanning a slice of decoded
// map[string]any — well inside the 10ms budget.
const shapeCheckSlowThreshold = 10 * time.Millisecond

// clusterListCollapseEnabled is the Ship S.1-re sequencing gate. The cluster-list
// iterator collapse is CORRECT but NOT yet refresher-safe: activated unconditionally
// (0.30.205) it ran the full-payload dispatch+unmarshal under the L1 refresher every
// cycle × entry-count → admin warm /call 11-22s, CPU 7.4/8. Held INERT here,
// behaviour-identical to 0.30.204 (the removed per-RA opt-in denied for all 167,111
// prod stages). S.2 flips this true AFTER landing the refresher-decoupling
// (cohort-memo reuse + skip-on-unchanged populate + customer-priority yield).
// Not an env flag (single-flag direction: end state is one CACHE_ENABLED).
//
// Ship 0.30.216 — Path 3 Phase 2'. The cluster-list collapse mechanism
// (held INERT since 0.30.212 and reverted in S.2 / 0.30.213 for wall-clock
// regression) is activated in lockstep with portal-chart 0.30.176, which
// tightens the per-stage filter on compositions-panels + blueprints-panels
// to an SPA-minimal projection (no `.spec.*` fields). Empirical post-jq
// envelope is 10.9 MiB (2.3× under 25 MB cap) per dev Phase 0 probe
// 2026-05-31. This avoids the S.2 failure mode: S.2 flipped this flag
// against the loose `del(managedFields)` per-stage filter → 56.9 MiB
// post-trim envelope → in-process scan cost dominated per-call latency.
// Phase 1' deployed portal-chart 0.30.176 FIRST against this flag still
// OFF (byte-equivalent on SPA-rendered fields), so the YAML-tight state
// is already live; this flag flip activates cluster-LIST collapse on the
// already-tight per-stage filter.
//
// Test-only override stays available via the
// withClusterListCollapseEnabledForTest helper in
// cluster_list_dep_record_test.go (the helper preserves the prior var
// value across nested invocations).
//
// Per project_single_cache_flag_direction: NOT an env flag (end state is
// one CACHE_ENABLED). The package-level var stays.
var clusterListCollapseEnabled = true

// attemptClusterListCollapse decides whether the per-stage iterator
// fan-out can be collapsed to a single cluster-scope LIST and, when so,
// returns the replacement []httpcall.RequestOptions slice — a single
// element whose Path is the cluster-scope form (no /namespaces/<x>/
// segment). The caller (resolve.go) substitutes this for the iterator's
// fan-out tmp slice; the existing bounded errgroup + apistageContentServe
// pipeline then runs over the single element and populates the
// identity-free apistage L1 entry on the miss path. Subsequent
// cluster-list-permitted users hit the cache.
//
// Returns (newTmp, useClusterList, denyGate):
//
//   - useClusterList==false — gate denied (no opt-in, cache disabled,
//     snapshot pre-readiness, RBAC deny, GVR derivation failed, or shape
//     check failed). The caller keeps its existing iterator tmp slice.
//   - useClusterList==true  — newTmp is a one-element slice; the caller
//     replaces tmp.
//
// denyGate is the 0.30.192 instrumentation seam (purely additive, no
// behaviour change): 0 means the gate passed (useClusterList==true);
// 1-7 means the corresponding gate triggered the false return — see the
// PIPStageTiming.ClusterListDenyGate doc on cache/pip_stage_timing.go for
// the value table.
//
// SEQUENCING (Ship S.1-re): the collapse is held INERT behind the
// package-level clusterListCollapseEnabled var (NEVER assigned by
// production). While inert, the FIRST statement returns deny-gate 1
// and NO later gate runs — the helper is byte-identical-behaviour to
// healthy 0.30.204 (where the removed per-RA opt-in denied for every
// prod stage). The gates 2-5 below + all helper machinery stay
// VERBATIM for S.2, just unreachable until S.2 flips the var true
// after landing refresher-decoupling.
//
// When enabled, the helper performs FOUR structural gates in order,
// short-circuiting on the first failure (no wasted work):
//
//  2. Cache-off + snapshot gate (AC-D5.13): !cache.Disabled() AND the
//     Ship B typed-RBAC snapshot is published.
//  3. Iterator present: apiCall.DependsOn.Iterator is non-empty (a
//     no-iterator stage has nothing to collapse).
//  4. GVR derivation: deriveTargetGVRForClusterList parses the first
//     iterator element's resolved Path → (GVR, ns, name); succeeds only
//     when the original path was namespace-scoped.
//  5. RBAC permission: rbac.EvaluateRBAC with Verb="list", Namespace=""
//     against the derived (group, resource) tuple returns permit==true.
//
// On ALL gates passing the helper runs the AC-D5.14 defensive
// dispatch: dispatchViaInformer un-gated → shape check → on success,
// Put under the identity-free apistage key → return the cluster-scope
// call slice. On any defensive failure the helper records the matching
// fall-through counter and returns useClusterList=false.
func attemptClusterListCollapse(
	ctx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	dict map[string]any,
	ep endpoints.Endpoint,
	apistageStore *cache.ResolvedCacheStore,
	apistageEnabled bool,
) ([]httpcall.RequestOptions, bool, int) {
	// Ship S.1-re INERT gate (was the per-RA opt-in, removed with the field).
	// Held off until S.2 lands refresher-decoupling. Returns deny-gate 1
	// (freed by the opt-in removal) so PIP timing self-documents "collapse
	// disabled, S.2-pending".
	if !clusterListCollapseEnabled {
		return nil, false, 1
	}

	// Gate 2: cache-off + Servable. AC-D5.13.
	if cache.Disabled() {
		return nil, false, 2
	}
	rw := cache.Global()
	if rw == nil {
		return nil, false, 2
	}
	if rw.Snapshot() == nil {
		// Pre-readiness window — Ship B's atomic.Pointer[RBACSnapshot]
		// has not been published yet. EvaluateRBAC degrades to deny in
		// this window; the cluster-list collapse must NOT execute
		// against a nil snapshot.
		log.Debug("cluster_list.gate_deny.snapshot_nil",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("reason", "ship-b-snapshot-not-published"),
		)
		return nil, false, 2
	}
	// Gate 3: iterator present. A no-iterator stage has nothing to
	// collapse — the field is a no-op.
	if apiCall.DependsOn == nil || apiCall.DependsOn.Iterator == nil ||
		*apiCall.DependsOn.Iterator == "" {
		return nil, false, 3
	}
	// Apistage L1 is the storage substrate for the identity-free
	// cluster-list entry. When apistageEnabled is false, the storage
	// substrate is missing; we can still cluster-list-dispatch, but
	// the entry would not survive across requests, so the gate denies
	// to keep flag-off behaviour byte-identical to pre-D.5.
	if !apistageEnabled || apistageStore == nil {
		return nil, false, 2
	}

	// Gate 4: derive the target GVR from the first iterator element.
	gvr, derivedOK := deriveTargetGVRForClusterList(ctx, log, apiCall, dict)
	if !derivedOK {
		log.Debug("cluster_list.gate_deny.gvr_derivation_failed",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
		)
		return nil, false, 4
	}

	// Gate 5: RBAC permission. cluster-scope list on the target GVR.
	user, userErr := xcontext.UserInfo(ctx)
	if userErr != nil {
		// No identity on context — degrade to iterator path; the
		// per-user token / SA-dispatch downstream narrows correctly.
		return nil, false, 5
	}
	permitOpts := rbac.EvaluateOptions{
		Username:  user.Username,
		Groups:    user.Groups,
		Verb:      "list",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: "", // cluster-scope check — evaluate.go:213-235
		// Ship L1 (0.30.252): per-item caller discards
		// matchedBindingUID — skip the CRB/RB stable-sort.
		SkipBindingUID: true,
	}
	// Ship 0.30.242 H.c-layered Phase 2 step 2a — per-item caller
	// ignores matchedBindingUID.
	permit, _, evalErr := rbac.EvaluateRBAC(ctx, permitOpts)
	if evalErr != nil || !permit {
		log.Debug("cluster_list.gate_deny.rbac_deny",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("user", user.Username),
			slog.String("gvr", gvr.String()),
			slog.Bool("permit", permit),
			slog.Any("eval_err", evalErr),
		)
		return nil, false, 5
	}

	// All five gates passed. Build the cluster-scope call.
	clusterCall := buildClusterListCall(apiCall, ep, gvr)

	// Path 3.2 / 0.30.218 — CELL-WARM FAST-PATH.
	//
	// Before paying the synchronous defensive prefetch (which decodes the
	// multi-MB envelope ON THE CUSTOMER GOROUTINE at ~10-12 ms/MB —
	// 2,024ms for compositions at 50K production scale per the empirical
	// 0.30.217 marker probe), check whether the apistage cell for this
	// (gvr, "", "") tuple is already warm. If so, return the cluster-scope
	// call and let the worker loop's apistageContentServe consume the
	// cached, decoded envelope (cheap per-user UAF prune — ~ms).
	//
	// The cell-warm check is a sync.Map.Load (ns-scale, zero alloc) — IT
	// IS the customer-priority property that lets cluster_list collapse
	// safely activate at production scale. Without it, every cold customer
	// /call pays the 2-second decode tax (`feedback_cluster_list_decode_irreducibility`).
	//
	// COLD-MISS POLICY: the cell is unpopulated. DO NOT decode
	// synchronously on the customer goroutine. Schedule an async populate
	// via the refresher's HIGH-PRIORITY cluster_list tier, return
	// useClusterList=false (deny-gate 8 — "cell-cold-async-populate-
	// scheduled"), and let the caller fall back to the per-NS iterator
	// path for THIS request. The per-NS path is small per call (1-2 NS
	// for narrow-RBAC users at ~50-200ms aggregate) — well inside the
	// 500ms AC-P3.2.1 decode-attribution gate.
	//
	// The next customer of the same cell hits the populated cell from
	// the refresher's async work — warmth improves monotonically without
	// any customer ever paying raw decode.
	contentKey := cache.ComputeKey(contentKeyInputs(gvr, "", ""))
	if entry, hit := apistageStore.Get(contentKey); hit && entry != nil {
		// CELL WARM. Customer keeps the cluster-scope call; the worker
		// loop's apistageContentServe will Get-hit on this entry and
		// skip the redundant dispatchViaInformer call. NO decode on the
		// customer goroutine.
		cache.RecordApiserverFallthrough(ctx,
			cache.ReasonClusterListDispatch, gvr.String())
		cache.RecordClusterListCellWarm()
		log.Debug("cluster_list.cell.warm",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
			slog.String("user", user.Username),
		)
		return []httpcall.RequestOptions{clusterCall}, true, 0
	}

	// CELL COLD. Schedule async populate via a bounded background
	// goroutine that runs the SAME synchronous populate body the
	// pre-Path-3.2 code path ran on the customer goroutine — only NOT
	// on the customer goroutine. Return useClusterList=false so the
	// caller falls back to per-NS iterator for THIS request.
	//
	// The next customer of the same cell hits warm. PIP boot pre-warm
	// (phase1_clusterlist_prewarm.go) populates every cluster_list
	// cell at boot so this cold-miss path fires ONLY for runtime
	// new-RA / cell-eviction edge cases.
	populateClusterListCellAsync(ctx, log, apiCall, ep, gvr, contentKey, clusterCall, apistageStore)
	cache.RecordClusterListCellColdFallback()
	log.Info("cluster_list.cell.cold_fallback",
		slog.String("subsystem", "cache"),
		slog.String("ra_stage", apiCall.Name),
		slog.String("gvr", gvr.String()),
		slog.String("user", user.Username),
		slog.String("effect", "per-NS fallback for this request; async populate scheduled — next request warm"),
	)
	return nil, false, 8
}

// populateClusterListCellSync runs the synchronous defensive prefetch +
// shape check + materialise + Put for the cluster_list cell identified
// by (gvr, contentKey). This is the pre-Path-3.2 customer-path body,
// extracted so it can run from THREE call sites without duplication:
//
//   1. PIP boot pre-warm (phase1_clusterlist_prewarm.go) — invoked at
//      Step 7.5 BEFORE MarkPhase1Done, populates the cell roster under
//      SA identity in parallel.
//   2. populateClusterListCellAsync — the cold-miss async populate goroutine
//      spawned from attemptClusterListCollapse when a customer /call hits
//      an unpopulated cell. Runs OFF the customer goroutine.
//   3. The refresher handler (registered indirectly via the RestActions
//      refresh path) — invoked when the dep-tracker dirty-marks the
//      cluster_list cell on an informer event.
//
// Returns ok=true when the cell was successfully populated; false on any
// failure (dispatch unservable, shape fallback, materialise error). The
// caller decides what to do on failure (per-NS fallback at customer
// path; log-only at PIP boot path).
//
// ctx MUST be set up so dispatchViaInformer can serve the cluster-scope
// call (cache.WithApistageContentResolve at the customer path; SA
// identity context at the PIP path).
func populateClusterListCellSync(
	ctx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	gvr schema.GroupVersionResource,
	contentKey string,
	clusterCall httpcall.RequestOptions,
	apistageStore *cache.ResolvedCacheStore,
) bool {
	// Belt-and-braces: re-check warmth under the lock-equivalent
	// sync.Map.Load. Two cold-miss goroutines for the same cell may
	// have raced past attemptClusterListCollapse's fast-path check;
	// the second one Puts identical bytes — harmless but wasteful.
	if entry, hit := apistageStore.Get(contentKey); hit && entry != nil {
		return true
	}

	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	dispatchStart := time.Now()
	rawEnvelope, dispatchedOK := dispatchViaInformer(
		cache.WithApistageContentResolve(ctx), clusterCall)
	defensiveDispatchMs := time.Since(dispatchStart).Milliseconds()
	if !dispatchedOK {
		log.Debug("cluster_list.dispatch_unservable",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
		)
		return false
	}

	shapeStart := time.Now()
	shape, shapeOK, shapeReason := validateClusterListShape(gvr, rawEnvelope)
	envelopeOKElapsed := time.Since(shapeStart)
	if envelopeOKElapsed > shapeCheckSlowThreshold {
		log.Warn("cluster_list.shape_check.slow",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.Duration("elapsed", envelopeOKElapsed),
			slog.Duration("threshold", shapeCheckSlowThreshold),
			slog.String("hint", "AC-D5.14 budget exceeded — surface to PM"),
		)
	}
	if !shapeOK {
		cache.RecordApiserverFallthrough(ctx,
			cache.ReasonClusterListShapeFallback, gvr.String())
		log.Warn("cluster_list.shape_fallback",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
			slog.String("shape_reason", shapeReason),
			slog.Int("envelope_bytes", len(rawEnvelope)),
		)
		return false
	}

	materialiseStart := time.Now()
	parsed, decodeErr := decodeClusterListItems(shape)
	materialiseElapsed := time.Since(materialiseStart)
	if decodeErr != "" {
		cache.RecordApiserverFallthrough(ctx,
			cache.ReasonClusterListShapeFallback, gvr.String())
		log.Warn("cluster_list.shape_fallback",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.String("gvr", gvr.String()),
			slog.String("shape_reason", decodeErr),
			slog.Int("envelope_bytes", len(rawEnvelope)),
			slog.Duration("materialise_elapsed", materialiseElapsed),
		)
		return false
	}

	// uaf-scope-waiver: PRE-refilter substrate, structurally incapable of carrying narrowed rows. This cell holds the RAW apiserver LIST envelope keyed by contentKeyInputs(gvr, ns, name) with NO identity fold; the userAccessFilter runs LATER, on the way out, and its output lands in the identity-bound restactions/widgets cells (all gated). Caching the un-narrowed substrate is what makes the per-request refilter cheap — gating it here would be wrong-cell and would defeat the layer for zero isolation gain.
	// scope-waiver:TTLOverride: cluster-list identity-free content substrate — sets its OWN data-plane CATALOG_UNSERVABLE TTLOverride CONDITIONALLY on newEntry below (not a keyed literal element); NOT the UAF-cap class (no BindingUID / no per-user refilter output) — uaf_shortttl.go R-d-4 SITE MAP.
	newEntry := &cache.ResolvedEntry{
		RawJSON:         rawEnvelope,
		Inputs:          ptrTo(contentKeyInputs(gvr, "", "")),
		Items:           parsed.items,
		ItemsAPIVersion: parsed.apiVersion,
		ItemsKind:       parsed.kind,
	}
	// R1 Layer 2 (#36) bounded-staleness backstop — same uniform predicate
	// as the apistage content Put (apistage.go): a catalog entry materialised
	// while its GVR informer is NOT servable gets the short
	// CATALOG_UNSERVABLE_TTL_SECONDS so a degraded cluster_list snapshot
	// self-evicts within the bound. UNIFORM, no path/resource special-case
	// (feedback_no_special_cases); disabled (override stays 0) when unset.
	if ttl := cache.CatalogUnservableTTL(); ttl > 0 && !cache.Global().IsServable(gvr) {
		newEntry.TTLOverride = ttl
	}
	defensiveParseMs := materialiseElapsed.Milliseconds()

	putStart := time.Now()
	apistageStore.Put(contentKey, newEntry)
	cache.Deps().RecordList(contentKey, gvr, "")
	defensivePutMs := time.Since(putStart).Milliseconds()

	// Path 3.2 / 0.30.218 — register the populated cell as a
	// cluster_list tier key so the dep-tracker dirty-mark hook routes
	// future informer-event refreshes to the HIGH-PRIORITY tier.
	// Idempotent.
	cache.RegisterClusterListKey(contentKey)

	pipSink.AccumulateDefensive(
		int64(len(rawEnvelope)),
		defensiveDispatchMs,
		defensiveParseMs,
		defensivePutMs,
	)

	cache.RecordApiserverFallthrough(ctx,
		cache.ReasonClusterListDispatch, gvr.String())
	log.Info("cluster_list.dispatch",
		slog.String("subsystem", "cache"),
		slog.String("ra_stage", apiCall.Name),
		slog.String("gvr", gvr.String()),
		slog.Int("envelope_bytes", len(rawEnvelope)),
		slog.Duration("dispatch_elapsed", time.Since(dispatchStart)),
		slog.Duration("envelope_ok_elapsed", envelopeOKElapsed),
		slog.Duration("materialise_elapsed", materialiseElapsed),
		slog.Int64("defensive_parse_ms", defensiveParseMs),
		slog.Int64("defensive_put_ms", defensivePutMs),
	)
	return true
}

// populateClusterListCellAsync spawns a bounded background goroutine
// that calls populateClusterListCellSync. Used by
// attemptClusterListCollapse's cold-miss path to populate the cell
// off the customer goroutine.
//
// Concurrency bound: each call spawns ONE goroutine bounded by
// clusterListAsyncSemaphore (size = GOMAXPROCS). Excess concurrent
// cold-misses on DIFFERENT cells either block until a slot frees up
// OR drop the populate (TryAcquire pattern — drop is safe because the
// next customer will retry).
//
// We use a non-blocking TrySend semaphore so the customer goroutine is
// NEVER blocked on an inflight populate — the customer keeps the
// per-NS fallback regardless. The customer-priority invariant is
// preserved: at WORST the cold-miss path is amplified by another
// customer hitting the same cell while the populate is still in
// flight; at best the populate completes before the next request
// arrives.
//
// inflightCells tracks which cells already have a populate goroutine
// inflight, so concurrent cold-misses on the SAME cell only spawn ONE
// populate goroutine.
func populateClusterListCellAsync(
	customerCtx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	ep endpoints.Endpoint,
	gvr schema.GroupVersionResource,
	contentKey string,
	clusterCall httpcall.RequestOptions,
	apistageStore *cache.ResolvedCacheStore,
) {
	// Per-cell dedup: only ONE populate goroutine inflight per cell.
	// LoadOrStore returns (loaded=true) when the key was already
	// present — another populate is in flight; this one returns
	// without spawning.
	if _, loaded := clusterListInflightCells.LoadOrStore(contentKey, struct{}{}); loaded {
		return
	}

	// Bounded-concurrency gate (drop-on-full). If GOMAXPROCS populates
	// are already running, drop this one — the next customer to hit
	// the same cell will retry. Customer goroutine NEVER blocks here.
	select {
	case clusterListAsyncSemaphore <- struct{}{}:
	default:
		// Drop. Release the inflight marker so the next caller can
		// re-try (otherwise the cell would be permanently locked
		// inflight=true with no populate running).
		clusterListInflightCells.Delete(contentKey)
		log.Debug("cluster_list.cell.async_populate_dropped",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("reason", "concurrency cap reached — next customer will retry"),
		)
		return
	}

	go func() {
		defer func() {
			<-clusterListAsyncSemaphore
			clusterListInflightCells.Delete(contentKey)
			if r := recover(); r != nil {
				log.Error("cluster_list.cell.async_populate_panic",
					slog.String("subsystem", "cache"),
					slog.String("gvr", gvr.String()),
					slog.Any("panic", r),
				)
			}
		}()

		// Detach from the customer ctx — its cancellation must NOT
		// abort our populate (the customer can return long before we
		// finish; the populate is for the NEXT customer). We carry the
		// internal endpoint + REST config + apistage-content-resolve
		// markers so dispatchViaInformer can serve the cluster-scope
		// call. Use a fresh timeout context anchored to Background.
		populateCtx, cancel := context.WithTimeout(context.Background(),
			clusterListAsyncPopulateTimeout)
		defer cancel()
		// Carry endpoint identity from the customer ctx so the
		// dispatch path can construct the cluster-scope URL. The
		// internal endpoint + REST config carry over via
		// cache.WithInternalEndpoint / WithInternalRESTConfig if set
		// on customerCtx — re-attach them to populateCtx.
		if iep, ok := cache.InternalEndpointFromContext(customerCtx); ok && iep != nil {
			populateCtx = cache.WithInternalEndpoint(populateCtx, iep)
		}
		if irc, ok := cache.InternalRESTConfigFromContext(customerCtx); ok && irc != nil {
			populateCtx = cache.WithInternalRESTConfig(populateCtx, irc)
		}
		// Carry logger.
		populateCtx = xcontext.BuildContext(populateCtx, xcontext.WithLogger(log))
		_ = ep // ep retained in closure for future SA-context wiring if needed

		ok := populateClusterListCellSync(populateCtx, log, apiCall, gvr, contentKey, clusterCall, apistageStore)
		if ok {
			log.Debug("cluster_list.cell.async_populate_completed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
			)
		}
	}()
}

// clusterListAsyncSemaphore bounds the number of concurrent cold-miss
// populate goroutines spawned by populateClusterListCellAsync.
// Sized at GOMAXPROCS — matches the existing PIP errgroup limit. Drops
// excess (customer goroutine NEVER blocks). Initialised in init().
var clusterListAsyncSemaphore chan struct{}

// clusterListInflightCells dedupes concurrent populate goroutines for
// the SAME cell. Keyed on contentKey; value is a struct{} sentinel.
var clusterListInflightCells sync.Map

// clusterListAsyncPopulateTimeout caps a single async populate at 30s.
// Empirically the worst-case cell (compositions @ 174 MiB) costs ~4s
// on 8 cores; 30s gives ~7× safety. After timeout the goroutine exits
// and the cell stays cold; next customer retries via the same cold-miss
// path.
const clusterListAsyncPopulateTimeout = 30 * time.Second

// ResetClusterListAsyncStateForTest clears the inflight-cells dedup map
// + drains the bounded-concurrency semaphore. Test-only — production
// code MUST NOT call this. Used by tests to prevent state leakage
// between cases. Path 3.2 / 0.30.218.
func ResetClusterListAsyncStateForTest() {
	clusterListInflightCells.Range(func(k, _ any) bool {
		clusterListInflightCells.Delete(k)
		return true
	})
	// Drain semaphore non-blockingly.
	for {
		select {
		case <-clusterListAsyncSemaphore:
		default:
			return
		}
	}
}

func init() {
	// Match PIP errgroup parallelism (runtime.GOMAXPROCS(0)). Fallback
	// is 4 if GOMAXPROCS reports <= 0 (defensive — should never happen).
	n := goMaxProcs()
	if n <= 0 {
		n = 4
	}
	clusterListAsyncSemaphore = make(chan struct{}, n)
}

// deriveTargetGVRForClusterList runs ONE jq evaluation of the
// iterator's path template against the FIRST iterator element and
// parses the result via cache.ParseAPIServerPathToDep. Returns
// (gvr, true) ONLY when the parsed path was namespace-scoped — i.e.
// the iterator IS a per-namespace fan-out. A cluster-scope path
// (parsed ns=="") means the RA already operates cluster-wide — no
// collapse is needed; the caller should keep the iterator path
// verbatim. A non-apiserver / malformed path also returns false.
//
// Per design §2.3 Approach (i): the GVR is identical across all
// iteration elements by construction (the iterator fans out over
// (crd × namespace) pairs; same CRD across all of them). One
// evaluation suffices.
func deriveTargetGVRForClusterList(
	ctx context.Context,
	log *slog.Logger,
	apiCall *templates.API,
	dict map[string]any,
) (schema.GroupVersionResource, bool) {
	if apiCall.DependsOn == nil || apiCall.DependsOn.Iterator == nil {
		return schema.GroupVersionResource{}, false
	}
	iter := *apiCall.DependsOn.Iterator
	if iter == "" {
		return schema.GroupVersionResource{}, false
	}

	// Pull the FIRST iterator element via jqutil.ForEach with an
	// early-exit sentinel. Matches the per-element materialisation
	// createRequestOptions performs but stops at element 0.
	var firstElement any
	have := false
	probeErr := jqutil.ForEach(ctx, jqutil.EvalOptions{
		Query:   iter,
		Unquote: true,
		Data:    dict,
	}, func(sa any) error {
		if !have {
			firstElement = sa
			have = true
		}
		return nil
	})
	if probeErr != nil || !have {
		log.Debug("cluster_list.gvr_probe.iterator_empty_or_error",
			slog.String("subsystem", "cache"),
			slog.String("ra_stage", apiCall.Name),
			slog.Any("err", probeErr),
		)
		return schema.GroupVersionResource{}, false
	}

	// Resolve the path template against the first element. Re-use
	// evalJQ — same jq engine, same module loader as
	// createRequestOption.
	resolvedPath := evalJQ(apiCall.Path, firstElement)
	if resolvedPath == "" || strings.Contains(resolvedPath, "${") {
		return schema.GroupVersionResource{}, false
	}

	gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(resolvedPath)
	if !parseOK {
		return schema.GroupVersionResource{}, false
	}
	if ns == "" {
		// Cluster-scope path already — no collapse needed. The RA's
		// iterator does not fan out over namespaces; keep verbatim.
		return schema.GroupVersionResource{}, false
	}
	if name != "" {
		// #74 Class 3 — the iterator's first element resolves to a
		// BY-NAME GET (/…/namespaces/<ns>/<resource>/<name>), NOT a
		// per-namespace LIST. A by-name fan-out must NEVER collapse: the
		// collapse replaces N targeted GET-by-name fetches with ONE
		// cluster-wide LIST, returning the {apiVersion,kind,items} LIST
		// ENVELOPE (an OBJECT) instead of the per-element bare objects the
		// RA filter expects. The RA then runs `map(select(.metadata.name…))`
		// over the OBJECT and indexes its first scalar field (apiVersion
		// "v1") → gojq "expected an object but got: string". It is ALSO a
		// correctness bug (a cluster-wide LIST returns ALL N resources, not
		// the composition's by-name subset). Collapse is valid ONLY for a
		// name=="" per-namespace LIST. Bug introduced at Ship 0.30.216
		// (911b1a8) when collapse flipped on; this restores the by-name
		// fan-out (= the bare array the RA filter consumes).
		//
		// STRUCTURAL guard (feedback_no_special_cases) — keyed on the parsed
		// path SHAPE (a name segment present), never a resource/name literal.
		return schema.GroupVersionResource{}, false
	}
	return gvr, true
}

// buildClusterListCall constructs the single cluster-scoped LIST
// httpcall.RequestOptions for the collapsed dispatch. Headers, payload,
// continue-on-error, error-key are inherited from the per-iteration
// build path (createRequestOption) so the cluster-list call sits in
// the same shape the worker loop expects — no special-case branches in
// the worker.
//
// The Path is /apis/<group>/<version>/<resource> for a named group OR
// /api/<version>/<resource> for the core group (group==""). Verb is
// forced GET (the cluster-list collapse is a read-only mechanism;
// non-GET iterator stages never reach this site — the worker would
// reject them anyway).
func buildClusterListCall(
	apiCall *templates.API,
	ep endpoints.Endpoint,
	gvr schema.GroupVersionResource,
) httpcall.RequestOptions {
	_ = ep // endpoint is wired by the worker loop's per-call assignment;
	// the cluster-list helper just constructs the call shape.

	path := clusterScopePathFor(gvr)
	out := httpcall.RequestOptions{}
	out.Path = path
	out.Verb = ptr.To(http.MethodGet)
	out.ContinueOnError = ptr.Deref(apiCall.ContinueOnError, false)
	out.ErrorKey = ptr.Deref(apiCall.ErrorKey, "error")

	// Headers — inherit the stage's declared headers VERBATIM (the
	// per-iteration evalJQ pass has nothing to substitute here: no
	// .namespace / .plural template references are present in a
	// cluster-scope call). A copy is made so a downstream mutation in
	// the worker loop never aliases back into apiCall.Headers.
	if len(apiCall.Headers) > 0 {
		out.Headers = make([]string, len(apiCall.Headers))
		copy(out.Headers, apiCall.Headers)
	}

	// Payload — clear for a LIST verb. createRequestOption evals it
	// against the iteration element; a cluster-scope LIST never carries
	// a body.

	return out
}

// clusterScopePathFor returns the apiserver REST path for the
// cluster-scoped LIST endpoint of gvr. Core group ("") uses the
// /api/<version> prefix; a named group uses /apis/<group>/<version>.
// No trailing slash. Mirrors apiserverPathFor (apistage.go) for the
// name=="" + namespace=="" form.
func clusterScopePathFor(gvr schema.GroupVersionResource) string {
	var b []byte
	if gvr.Group == "" {
		b = append(b, "/api/"...)
		b = append(b, gvr.Version...)
	} else {
		b = append(b, "/apis/"...)
		b = append(b, gvr.Group...)
		b = append(b, '/')
		b = append(b, gvr.Version...)
	}
	b = append(b, '/')
	b = append(b, gvr.Resource...)
	return string(b)
}

// shapeCheckItemSampleSize bounds the number of items the defensive
// shape check materially walks. Path 3.1 Bug 1 — the previous
// implementation iterated EVERY item in the cluster-LIST envelope (~44K
// at cyberjoker scale) which dominated per-/call latency (1.3-1.5s
// observed in 0.30.216 canonical Chrome MCP). At apiserver wire shape
// uniformity (the apiserver does not emit per-item shape drift inside
// a single LIST response — items decode through one schema), inspecting
// the first k items is sufficient to detect a malformed envelope while
// keeping the check O(1) in items.
const shapeCheckItemSampleSize = 8

// isJSONNull reports whether a json.RawMessage is the literal JSON
// `null` value, ignoring surrounding whitespace. Used by the
// sample-bounded item check: a null item slot must NOT be cached, so
// we reject at shape-check time without paying the per-item decode.
func isJSONNull(raw []byte) bool {
	// Strip ASCII whitespace (matches encoding/json's space-stripping).
	i, j := 0, len(raw)
	for i < j {
		switch raw[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		}
		break
	}
	for j > i {
		switch raw[j-1] {
		case ' ', '\t', '\n', '\r':
			j--
			continue
		}
		break
	}
	return j-i == 4 &&
		raw[i] == 'n' && raw[i+1] == 'u' && raw[i+2] == 'l' && raw[i+3] == 'l'
}

// envelopeShape is the Path 3.1 Bug 1 (architect-mandated correction)
// intermediate form produced by validateClusterListShape: just enough
// shape evidence to (a) confirm the envelope is a well-formed LIST and
// (b) hand off to materialisation WITHOUT having done the per-item
// decode. The rawItems slice carries the deferred per-item bytes so
// decodeClusterListItems can complete materialisation OUT of the
// latency-critical shape-check budget — the architect's exact fix
// instruction: "move decodeClusterListItems OUT of
// validateClusterListShape to the call site".
type envelopeShape struct {
	apiVersion string
	kind       string
	rawItems   []json.RawMessage
}

// validateClusterListShape enforces AC-D5.14's defensive shape check
// on the raw cluster-scope LIST envelope. Returns (shape, true, "")
// on a well-formed envelope; (zero, false, reason) otherwise.
//
// Ship 0.30.217 Path 3.1 Bug 1 + Bug 3 — surgical fixes
// (Bug 1 reflects the architect-mandated correction: materialisation
// is HOISTED out; this function is now O(envelope-fields + first-K-items
// nil-check) and never touches per-item field maps):
//
//   Bug 1 (slow shape check, 1.3-1.5s observed): the function previously
//   iterated EVERY item in the envelope with 4 map ops + per-item
//   stripManagedFields + unstructured.Unstructured allocation. At
//   cyberjoker scale (~44K items, ~10.9 MiB envelope) this dominated
//   per-/call latency. Item materialisation is redundant work in the
//   shape budget — `parseListEnvelope` runs the SAME decode at the Put
//   site (apistage.go:140-170). Fix: the shape check itself is O(1) at
//   the envelope level + sample-bounded at the item level (first k=8
//   items for nil-check only). It now returns the deferred
//   []json.RawMessage so the caller can pay the per-item decode under
//   its OWN separately-named/separately-timed step
//   (`materialise_elapsed`) — see the call site near cluster_list.go:312.
//
//   Bug 3 (per-item TypeMeta false-negative): the previous check
//   asserted `it["apiVersion"]` and `it["kind"]` are non-empty strings.
//   The apiserver does NOT emit per-item apiVersion/kind on a typed
//   LIST endpoint (those live only on the envelope; k8s API convention)
//   — and the dynamic-informer-served path (`marshalAsList` at
//   informer_dispatch.go:209-222) stores items AS DECODED with no
//   per-item TypeMeta injection. Result: EVERY informer-served
//   cluster-LIST tripped this assertion, paying both the dispatch cost
//   AND the shape-check cost for ZERO collapse benefit. Fix: drop the
//   per-item TypeMeta assertion. `parseListEnvelope` already tolerates
//   this — envelope-level TypeMeta (apiVersion/kind) is the source of
//   truth and is synthesized from the GVR if missing.
//
// Definition (post-Path-3.1):
//
//   - kind ends with "List" (envelope-level only)
//   - .items is a non-empty array of objects
//   - sampled items (first k) have non-empty raw bytes
//
// gvr is consulted ONLY when envelope.APIVersion / envelope.Kind are
// empty — synthesized from GVR per parseListEnvelope's contract.
func validateClusterListShape(gvr schema.GroupVersionResource, raw []byte) (envelopeShape, bool, string) {
	// Path 3.1 Bug 1 — envelope-only decode. We need just enough shape
	// to (a) verify the kind ends in "List", and (b) verify .items is a
	// non-empty array. json.RawMessage on Items defers the per-item
	// decode out of the latency-critical shape check; the caller picks
	// up the deferred slice via the returned envelopeShape.rawItems.
	var envelope struct {
		APIVersion string            `json:"apiVersion"`
		Kind       string            `json:"kind"`
		Items      []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return envelopeShape{}, false, "envelope-unmarshal-failed"
	}
	if !strings.HasSuffix(envelope.Kind, "List") {
		return envelopeShape{}, false, "envelope-kind-not-list"
	}
	if len(envelope.Items) == 0 {
		// AC-D5.14 says non-empty items. A genuinely-empty cluster also
		// hits this — but the iterator path's empty-result handling is
		// safer than a cached zero-item entry that could mask later
		// populations until TTL eviction. Fall back to the iterator.
		return envelopeShape{}, false, "envelope-items-empty"
	}

	// Path 3.1 Bug 1 — sample-bounded per-item nil check. The apiserver
	// emits structurally-uniform items inside a single LIST response;
	// a sample of the first k items detects malformed-envelope cases
	// (genuine nil/empty items at the byte level) without the O(N) walk.
	// "null" raw bytes are also rejected — that is the wire form of a
	// json-null item slot, which decodes to a nil map and would fail at
	// materialisation; rejecting at the sample stage keeps the budget
	// honest without inviting a cached zero-map item.
	sampleN := shapeCheckItemSampleSize
	if sampleN > len(envelope.Items) {
		sampleN = len(envelope.Items)
	}
	for i := 0; i < sampleN; i++ {
		raw := envelope.Items[i]
		if len(raw) == 0 || isJSONNull(raw) {
			return envelopeShape{}, false, "envelope-item-nil"
		}
	}

	apiVersion := envelope.APIVersion
	if apiVersion == "" {
		apiVersion = apiVersionForGVR(gvr)
	}
	kind := envelope.Kind
	if kind == "" {
		kind = listKindForResource(gvr.Resource)
	}
	return envelopeShape{
		apiVersion: apiVersion,
		kind:       kind,
		rawItems:   envelope.Items,
	}, true, ""
}

// materialiseClusterListItemsFn is the package-private seam over the
// single envelope-items materialisation pass (decodeClusterListItems'
// real body). Task #328 RED/GREEN falsifier counts invocations of THIS
// pass to prove the Path 3.1 Bug 1 hoist invariant: the materialisation
// runs EXACTLY ONCE on the happy path (at the call site, after the shape
// check) and ZERO times on the envelope-reject path — N items must never
// trigger a per-item materialisation pass INSIDE validateClusterListShape.
//
// Production code path is unchanged: the variable is initialised once to
// materialiseClusterListItems and is only reassigned by test code (which
// swaps in a counting wrapper). There is NO atomic / counter on the
// production hot path — the indirection is a single function-pointer call
// the production binary never reassigns. Mirrors compileCRDSchemaFn
// (internal/resolvers/crds/schema/extract.go:19) and
// discoveryClientForConfigFn (internal/dynamic/cached_client.go:225).
var materialiseClusterListItemsFn = materialiseClusterListItems

// decodeClusterListItems materialises the deferred per-item bytes from a
// validated envelopeShape into the parsedListEnvelope form that
// apistage's content-gate / ResolvedEntry consumes. Per-item decode +
// stripManagedFields run here so the latency-critical
// `validateClusterListShape` envelope check stays O(1) in items —
// Path 3.1 Bug 1 architect-mandated correction.
//
// Output is byte-identical to parseListEnvelope's output for the same
// raw input bytes (same struct tags, same stripManagedFields call, same
// []*unstructured.Unstructured{Object: it} wrap).
//
// Returns (parsedListEnvelope{}, "envelope-item-decode-failed") on any
// item-level decode failure — the caller falls back to the iterator
// path AND records cache.ReasonClusterListShapeFallback exactly as for
// a shape-check failure. Item materialisation is the cell-fresh-populate
// path; subsequent reads of the cell skip this work via the stored
// ResolvedEntry.Items slice.
//
// Task #328 — the body is dispatched through materialiseClusterListItemsFn
// (the count seam) so the hoist invariant is regression-testable. This is
// a behaviour-identical pass-through: production wires the real
// materialiseClusterListItems and pays only one function-pointer hop.
func decodeClusterListItems(shape envelopeShape) (parsedListEnvelope, string) {
	return materialiseClusterListItemsFn(shape)
}

// materialiseClusterListItems is the real (countable) materialisation
// pass. Body is verbatim the pre-#328 decodeClusterListItems body — the
// #328 change is purely the function-var indirection, no logic change.
func materialiseClusterListItems(shape envelopeShape) (parsedListEnvelope, string) {
	items := make([]*unstructured.Unstructured, 0, len(shape.rawItems))
	for _, rawIt := range shape.rawItems {
		var it map[string]any
		if err := json.Unmarshal(rawIt, &it); err != nil {
			return parsedListEnvelope{}, "envelope-item-decode-failed"
		}
		if it == nil {
			return parsedListEnvelope{}, "envelope-item-nil"
		}
		// Ship 2a (0.30.209) — strip managedFields at the item-
		// materialisation site (mirrors parseListEnvelope), so every
		// shared entry.Items map is stripped once at load and the serve
		// path needs no per-serve removeManagedFields walk.
		stripManagedFields(it)
		items = append(items, &unstructured.Unstructured{Object: it})
	}
	return parsedListEnvelope{
		items:      items,
		apiVersion: shape.apiVersion,
		kind:       shape.kind,
	}, ""
}
