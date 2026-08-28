// resolve_populate.go — Ship C (0.30.112): the single resolve-and-store
// path for the L1 resolved-output cache.
//
// resolveAndPopulateL1 re-resolves an L1 entry from its own
// ResolvedKeyInputs and writes the fresh bytes back under the canonical
// key. It is the body the runtime refresher's RefreshFunc invokes on a
// dirty-mark (Ship C) and the body Ship F's prewarm will reuse — one
// resolve path, no duplication.
//
// IDENTITY (PM directive, AC-C7): the re-resolve runs under the entry's
// OWN Inputs identity — Username + Groups from the ResolvedKeyInputs.
// A refresh of user U's entry resolves as U, so RBAC narrowing and the
// resolved content stay user-correct. The re-resolve context also carries
// WithL1KeyContext(key) so the resolver re-records dep edges (the inner
// object set may have changed since the original resolve).
//
// SA TRANSPORT (Ship 0.30.113 Part B): a background refresh has no live
// per-user bearer token — the original request's Endpoint is long gone.
// The widget resolver (widgets.Resolve) reads xcontext.UserConfig(ctx)
// directly and fails "user *Endpoint not found in context" if only the
// identity (WithUserInfo) is supplied. With the informer pivot ON
// (#57: implicit-on-cache, i.e. the cache subsystem on) every K8s read
// is informer-served and
// RBAC-narrowed IN-PROCESS from WithUserInfo — never from the user's
// token — so the user's Endpoint is needed ONLY as a transport. We
// therefore supply the snowplow ServiceAccount endpoint + *rest.Config
// as that transport (WithUserConfig + WithInternalEndpoint +
// WithInternalRESTConfig) while keeping WithUserInfo{Username,Groups}
// for per-user correctness. No per-user token is stored. This is the
// EXACT pattern Phase 1 uses (withPhase1SAContext, phase1_walk.go) — the
// load-bearing 0.30.103 SA-CA seam. When saEP/saRC are nil (no SA creds
// — unit test / outside-cluster) the context is identity-only as before.
//
// Per feedback_l1_invalidation_delete_only.md: this path only ever
// Put()s — it never evicts. A refresh that lands after the entry was
// evicted must not resurrect it (see the post-resolve liveness re-check).

package dispatchers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/snowplow/apis"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	restactionsapi "github.com/krateo-platformops/snowplow/internal/resolvers/restactions/api"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// resolveOnceFn is the resolve-and-encode seam. It re-fetches the CR
// named by inputs (under ctx's identity) and returns the encoded
// resolver output. Production wires it to resolveOnceProd; tests stub
// it to exercise resolveAndPopulateL1's queue/identity/Put plumbing
// without a live cluster.
//
// A package var rather than a parameter so the refresher's RefreshFunc
// signature (cache.RefreshFunc) is untouched. Swapped only by the
// _test.go shim; production never reassigns it.
var resolveOnceFn = resolveOnceProd

// resolveAndPopulateL1 is the single resolve-and-store path. It:
//
//  1. computes the canonical L1 key from inputs (must equal the key the
//     entry is filed under — ComputeKey is deterministic);
//  2. builds the re-resolve context: the entry's own Username+Groups
//     identity, the SA transport (saEP/saRC) so the resolver's
//     UserConfig/object-fetch sites have an apiserver client-config,
//     plus WithL1KeyContext(key) so dep edges are re-recorded;
//  3. re-resolves + encodes via the resolveOnce seam;
//  4. re-checks the entry is still live (a DELETE-evict may have raced
//     the refresh) and, if so, Put()s the fresh bytes.
//
// saEP/saRC are the process-singleton snowplow ServiceAccount endpoint +
// *rest.Config — supplied as TRANSPORT only (see the file header). When
// nil (no SA creds) the context is identity-only and a widget re-resolve
// that reads xcontext.UserConfig fails; the refresher's bounded retry
// then drops the key to TTL.
//
// Returns an error on resolve failure so the refresher can retry with
// backoff; returns nil (no-op) for an Inputs the path cannot drive.
func resolveAndPopulateL1(ctx context.Context, inputs cache.ResolvedKeyInputs, saEP *endpoints.Endpoint, saRC *rest.Config) error {
	log := xcontext.Logger(ctx)

	c := cache.ResolvedCache()
	if c == nil {
		// L1 disabled — nothing to populate. Not an error.
		return nil
	}

	key := cache.ComputeKey(inputs)

	// Ship 4a (0.30.198) — capture the prior entry's pin status so a
	// RAFullList refresh RE-PINS rather than demoting a resident cell to
	// transient on a dirty-mark (the prewarm-protection contract:
	// feedback_zero_cold_navigations_hard_requirement). For every other
	// class prePinned stays false and the re-Put is unchanged.
	prePinned := false
	if inputs.CacheEntryClass == cache.CacheEntryClassRAFullList {
		if prior, ok := c.Get(key); ok && prior != nil {
			prePinned = prior.Pinned
		}
	}

	// AC-C7: re-resolve under the entry's OWN identity. Ship A.3 /
	// 0.30.179 — the entry is per-COHORT (keyed by BindingSetHash); the
	// refresher re-resolves under the cohort's REPRESENTATIVE tuple
	// (the first writer's Username + Groups recorded on Inputs). Cohort
	// members produce byte-identical resolved output, so the
	// representative is equivalent to any other cohort member at resolve
	// time. A binding mutation that shifts the cohort topology shifts
	// BindingSetHash, MISSes on the next /call, and the seed reseeds
	// under a fresh representative — no stale-identity risk.
	//
	// Ship 1.3 (lever 2) — the IDENTITY-FREE classes (widgetContent /
	// apistage) carry NO representative identity: their key skips the
	// identity fold (ComputeKey, resolved.go:611-612) and their populate
	// sites never set RepresentativeUsername/Groups, so the tuple above
	// would be ("", nil). Re-resolving under that EMPTY identity drives
	// CohortNSACL("") -> no bindings -> permitAll=false -> the apiRef RA's
	// per-namespace stage drops every item -> status.resourcesRefs.items
	// is re-stored EMPTY: the refresher RE-POISONS the cell it is meant to
	// correct (the >3,100-cycle defect, project_prewarm_page_offset_bug).
	//
	// The identity-free cell holds an SA-MAXIMAL SHELL by construction —
	// the F2 walker Put()s it under withPhase1SAContext (phase1_walk.go),
	// and the serve-time gate (gateWidgetEnvelope, widget_content.go)
	// re-derives status.resourcesRefs.items[].allowed PER REQUESTER, so
	// the body that leaves the pod is per-user-narrowed regardless of the
	// shell's identity. The REFRESH must therefore re-resolve under the
	// SAME SA canonical identity the boot walk used, NOT the empty tuple,
	// so it CORRECTS the shell (full resourcesRefs.items) instead of
	// re-poisoning it. Ship 1.1 made the SA's CohortNSACL `*/*`
	// permitAll=true (CohortNSACL ServiceAccount-kind landing), so the
	// canonical `system:serviceaccount:<ns>:<name>` identity yields the
	// full list. The username is derived from the SA token's JWT `sub`
	// claim via phase1SAUsername (no Go literal — feedback_no_special_cases)
	// — the EXACT seam withPhase1SAContext uses. When the SA token is
	// absent (unit test / outside-cluster) we fall back to the
	// representative tuple, the unchanged degraded posture.
	refreshUser := inputs.RepresentativeUsername
	refreshGroups := inputs.RepresentativeGroups
	if isIdentityFreeClass(inputs.CacheEntryClass) {
		if saEP != nil {
			if saUser, ok := phase1SAUsername(saEP.Token); ok {
				refreshUser = saUser
				// SA identity is username-only — the SA's grant lands via
				// its ServiceAccount-kind binding, matched by username in
				// EvaluateRBAC + CohortNSACL. Mirrors withPhase1SAContext,
				// which installs WithUserInfo{Username: saUser} with no
				// Groups.
				refreshGroups = nil
			}
		}
	}
	opts := []xcontext.WithContextFunc{
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: refreshUser,
			Groups:   refreshGroups,
		}),
	}
	// Ship 0.30.113 Part B — SA transport. A background refresh has no
	// per-user token; the widget resolver reads xcontext.UserConfig
	// directly. Supply the SA endpoint as the transport so that read
	// succeeds. Under the informer pivot every K8s read is RBAC-narrowed
	// in-process from WithUserInfo above, never from this endpoint's
	// token — so this is transport-only, no per-user-token storage.
	// Mirrors withPhase1SAContext (phase1_walk.go).
	if saEP != nil {
		opts = append(opts, xcontext.WithUserConfig(*saEP))
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	// WithInternalEndpoint / WithInternalRESTConfig make cache.ClientConfigFor
	// (internal_client.go) return the pre-built SA *rest.Config verbatim
	// for the objects.Get apiserver fall-through and resourcesrefs.Resolve
	// — the load-bearing 0.30.103 SA-CA seam (the SA's raw-PEM CA cannot
	// survive the base64/cert-only kubeconfig path).
	if saEP != nil {
		rctx = cache.WithInternalEndpoint(rctx, saEP)
	}
	if saRC != nil {
		rctx = cache.WithInternalRESTConfig(rctx, saRC)
	}
	// WithL1KeyContext threads the L1 key so the resolver's inner-call
	// recording site re-records dep edges for this refresh.
	rctx = cache.WithL1KeyContext(rctx, key)

	// Ship 0.30.120 layer (b) — error-aware Put-gate. Install a
	// stage-error sink on the re-resolve context. The api resolver bumps
	// it whenever it writes dict[call.ErrorKey] (a swallowed,
	// continueOnError'd stage failure — e.g. the 401 from an exportJwt
	// loopback that has no per-user JWT in a background refresh). After
	// the re-resolve we read stageErrSink.Load(): a non-zero value means
	// the fresh bytes were produced under a stage error and MUST NOT
	// overwrite the prior good entry. The sink is request-path-inert:
	// only the refresher installs it, so a cold dispatch is unaffected.
	rctx, stageErrSink := cache.WithStageErrorSink(rctx)
	// External-no-cache (proposal 2026-06-22) — defense-in-depth. The
	// refresher only ever re-Puts already-cached keys, and external RAs never
	// get cached in the first place (the request-path Put-gates decline them),
	// so the refresher should never re-resolve an external RA. But install +
	// gate the external-touched sink here too, so that IF an external RA ever
	// reached this path it would still decline the re-Put rather than persist
	// stale external data. Additive to the stage-error sink.
	rctx, extTouchedSink := cache.WithExternalTouchedSink(rctx)

	encoded, err := resolveOnceFn(rctx, inputs)
	if err != nil {
		return fmt.Errorf("resolveAndPopulateL1 %s/%s: %w",
			inputs.CacheEntryClass, inputs.Name, err)
	}
	if encoded == nil {
		// The seam declined to resolve (e.g. unknown handler kind) —
		// skip-to-TTL, not an error.
		return nil
	}

	// A refresh that lands AFTER the entry was DELETE-evicted must not
	// resurrect it. Re-Get under the key: if it is gone, drop the fresh
	// bytes on the floor (the eviction is authoritative).
	if _, alive := c.Get(key); !alive {
		log.Debug("resolveAndPopulateL1: entry evicted during refresh; not resurrecting",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
		)
		return nil
	}

	// Ship 0.30.120 layer (b) — error-aware Put-gate. If the re-resolve
	// observed ANY stage error (a swallowed, continueOnError'd inner-call
	// failure — e.g. the 401 from an exportJwt loopback that the SA-
	// transport refresher cannot satisfy), the fresh bytes are an
	// under-served result: they MUST NOT overwrite the user's prior good
	// entry. Decline the Put and return nil — NOT an error. A deterministic
	// stage failure must not drive AddRateLimited / burn the retry budget;
	// the prior good entry stays and the outer TTL is the safety net.
	//
	// The gate keys on STAGE-ERROR PRESENCE, never on result emptiness — a
	// user who legitimately has 0 compositions produces no stage error
	// (sink == 0) and their empty result IS stored.
	if stageErrSink.Count() > 0 {
		// #301 observability: surface the FIRST failing stage's name + err
		// inline so a decline can be attributed without a cross-grep
		// against the resolver's error lines (which cost two traces).
		sampleStage, sampleErr := stageErrSink.Sample()
		log.Warn("resolveAndPopulateL1: stage error during refresh; declining to overwrite good entry",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
			slog.String("handler", inputs.CacheEntryClass),
			slog.String("user", refreshUser),
			slog.Int64("stage_errors", stageErrSink.Count()),
			slog.String("stage_err_stage", sampleStage),
			slog.String("stage_err_sample", sampleErr),
			slog.String("effect", "prior good entry kept; TTL is the outer net"),
		)
		cache.BumpRefresherSkippedStageError()
		return nil
	}

	// External-no-cache (proposal 2026-06-22) — defense-in-depth re-Put gate.
	// If the re-resolve touched a genuine external endpoint, the fresh bytes
	// have no dep edge to invalidate them; decline the re-Put (keep the prior
	// entry; TTL is the outer net) exactly as the stage-error gate does.
	if extTouchedSink.Count() > 0 {
		cache.BumpExternalSkippedPut()
		// Not a fault: a designed defense-in-depth decline (external data has no
		// dep edge to invalidate it, so keep the prior entry; TTL is the outer
		// net). Fires on every refresh of an external-touching entry — DEBUG.
		// The BumpExternalSkippedPut counter carries the rate for operators.
		log.Debug("resolveAndPopulateL1: re-resolve touched an external endpoint; declining to overwrite entry",
			slog.String("subsystem", "cache"),
			slog.String("key_hash", key),
			slog.String("handler", inputs.CacheEntryClass),
			slog.String("user", refreshUser),
			slog.Int64("external_touches", extTouchedSink.Count()),
			slog.String("effect", "prior entry kept; external data has no dep edge — TTL is the outer net"),
		)
		return nil
	}

	entry := &cache.ResolvedEntry{
		RawJSON: encoded,
		Inputs:  &inputs,
		// Ship 4a (0.30.198) — preserve the resident pin on a RAFullList
		// refresh so a dirty-mark re-resolve never demotes a prewarmed
		// expensive cell to the transient LRU. Put honours the pin subject
		// to the resident budget (else demotes — the safe degrade).
		Pinned: prePinned,
		// #118 (d) interim — C-118-6 CRUX: re-stamp the short UAF TTLOverride on
		// the REFRESHER re-Put too. The refresher builds a FRESH entry with zero
		// CreatedAt (Put stamps a new time.Now()), so a hot, data-plane-refreshed
		// UAF cell would slide its CreatedAt forward every refresh and OUTLIVE the
		// cap if the override were stamped only on the first customer Put. inputs
		// is the STORED ResolvedKeyInputs (carrying HasUAF from the original
		// dispatch), so uafTTLOverrideForEntry re-derives the same override here
		// without needing the RESTAction CR. Returns 0 (no override) when the knob
		// is unset or the cell is non-UAF → byte-identical to today.
		TTLOverride: uafTTLOverrideForEntry(&inputs),
	}
	// Ship #97 (0.30.214) — restore the R3 fast-path on refresher Puts
	// of apistage-class LIST entries. Pre-fix the refresher Put wrote
	// RawJSON only (Items: nil); the apistage read path at apistage.go:487
	// then evaluated `len(entry.Items) > 0 == false` on every content-Get-hit,
	// falling through to gateListEnvelope → parseListEnvelope on the
	// customer request goroutine (45% cum CPU at 0.30.212 production scale
	// per ship-97-prefix-falsifier-2026-05-31). Populating Items here
	// pushes the parse cost ONCE onto the refresher goroutine (per cycle)
	// and lets every subsequent Get-hit short-circuit through
	// gateListItemsWithMemo.
	//
	// GET-by-name (Name != "") and malformed-at-Put envelopes return ok=false
	// and the entry keeps Items=nil — byte-identical fallback to today.
	if inputs.CacheEntryClass == cache.CacheEntryClassApistage {
		if items, apiVer, kind, ok := restactionsapi.ParseListEnvelopeForRefresh(inputs, encoded); ok {
			entry.Items = items
			entry.ItemsAPIVersion = apiVer
			entry.ItemsKind = kind
		}
	}
	c.Put(key, entry)
	// Ship 1 (live-refresh-coherence, option A) — emit the live-refresh
	// signal STRICTLY post-commit, on the refresher path only. This line is
	// reached ONLY after the Put returned and ONLY on a genuine L1 change:
	// the four no-Put success-returns above (cache-off :96-99, declined
	// :214-218, evicted-during-refresh :223-229, stage-error decline
	// :243-260) all return before here, so the signal never fires when L1
	// did not actually change (design §1.1 — coherent by construction).
	// resolveAndPopulateL1 is invoked ONLY from the refresher closure
	// (dispatchers.go:86), never from cold dispatch or the prewarm walker, so
	// this announces dep-change-driven re-resolves only. PublishRefresh is
	// nil-safe and a no-op when the SSE layer is disabled or cache is off.
	cache.PublishRefresh(key)
	log.Debug("resolveAndPopulateL1: re-resolved + stored",
		slog.String("subsystem", "cache"),
		slog.String("key_hash", key),
		slog.String("handler", inputs.CacheEntryClass),
		slog.String("user", refreshUser),
		slog.Bool("pinned", prePinned),
	)
	return nil
}

// isIdentityFreeClass reports whether the entry class is one of the two
// SHARED, identity-free cache classes whose ComputeKey skips the identity
// fold (resolved.go:611-612): the widget-content shell and the api-stage
// content cell. Both hold an SA-maximal SHELL that the serve-time gate
// (gateWidgetEnvelope) narrows per-requester; their refresh therefore
// re-resolves under the SA canonical identity (lever 2) rather than the
// empty representative tuple. Kept as a single predicate so the two
// call-class checks stay symmetric and the rule is stated once.
func isIdentityFreeClass(class string) bool {
	return class == cache.CacheEntryClassWidgetContent ||
		class == cache.CacheEntryClassApistage
}

// resolveOnceProd is the production resolve-and-encode implementation.
// It re-fetches the dispatch CR named by inputs (objects.Get, under
// ctx's identity) and dispatches the matching resolver, returning the
// encoded output byte-identical to the request-path encode
// (encodeResolvedJSON — same encoder settings as a cold dispatch).
func resolveOnceProd(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	authnNS := env.String("AUTHN_NAMESPACE", "")

	// C5 (aggregate OOM bound): the refresher is BACKGROUND — its nested-resolve
	// trees must YIELD the aggregate admission gate to a customer /call (which
	// has a browser deadline; the refresher does not). Mark the ctx so
	// enterNestedResolveUnit de-prioritises this tree behind waiting customers
	// (it still COUNTS toward the aggregate once admitted — the OOM floor is
	// preserved). Covers BOTH the content-refresh and RA/widget-refresh branches
	// below (this is the single refresher resolve entry).
	ctx = cache.WithBackgroundResolve(ctx)

	// Ship F1 (0.30.119): an api-stage entry is a CONTENT-keyed K8s call
	// (gvr, namespace, name-or-empty) — NOT a RESTAction. Its refresh is
	// a single un-gated K8s re-dispatch + re-Put under the same content
	// key; there is no whole-RESTAction re-run, no objects.Get of a CR,
	// no self-hit (a content entry is re-dispatched, never self-Got).
	// resolveContentEntryForRefresh returns the fresh raw envelope, which
	// resolveAndPopulateL1 Puts under the content key.
	if inputs.CacheEntryClass == cache.CacheEntryClassApistage {
		return resolveContentEntryForRefresh(ctx, inputs)
	}

	// restactions / widgets entries identify a CR — re-fetch it, then
	// dispatch the matching resolver.
	ref := templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      inputs.Name,
			Namespace: inputs.Namespace,
		},
		APIVersion: schemaGroupVersion(inputs.Group, inputs.Version),
		Resource:   inputs.Resource,
	}
	got := objects.Get(ctx, ref)
	if got.Err != nil {
		return nil, fmt.Errorf("re-fetch %s/%s: %s",
			inputs.Resource, inputs.Name, got.Err.Message)
	}
	if got.Unstructured == nil {
		return nil, fmt.Errorf("re-fetch %s/%s: nil object", inputs.Resource, inputs.Name)
	}

	switch inputs.CacheEntryClass {
	case "restactions":
		return resolveRestActionForRefresh(ctx, got, inputs, authnNS)
	case cache.CacheEntryClassRAFullList:
		// Ship 4a (0.30.198) — refresh the page-independent RA full-list
		// cell. The cell holds the RA's STATUS MAP (the resolved result,
		// e.g. {compositionspanels:[...]}) resolved UNPAGINATED — NOT the
		// whole RESTAction CR. resolveRAFullListForRefresh re-resolves the
		// RA at PerPage=0/Page=0 (no `.slice` injected → full sorted set) and
		// returns json.Marshal(ra.Status-map) so the bytes are byte-identical
		// to the apiref serve path's PutRAFullList (which marshals the same
		// map). Using resolveRestActionForRefresh here would store the whole
		// CR — a SHAPE MISMATCH the Go-slice serve could not consume.
		return resolveRAFullListForRefresh(ctx, got, inputs, authnNS)
	case "widgets":
		return resolveWidgetForRefresh(ctx, got, inputs, authnNS)
	case cache.CacheEntryClassWidgetContent:
		// Ship G (0.30.16x) — refresh path for the identity-free widget
		// content layer. The entry's Inputs identify the SAME widget CR
		// the per-user "widgets" entries identify, just under a
		// different key shape (identity-free vs per-user-keyed). The
		// resolver call is identical — widgets.Resolve on the re-fetched
		// CR — so we delegate to the same helper. The fresh bytes are
		// Put back under the identity-free key (ComputeKey reads
		// inputs.CacheEntryClass and skips identity for this class).
		//
		// Stage-error sink semantics + post-resolve liveness check are
		// inherited from resolveAndPopulateL1 (the caller). The Put
		// declines symmetrically when stage errors fire, preserving
		// the AC-G.5 contract.
		return resolveWidgetForRefresh(ctx, got, inputs, authnNS)
	default:
		// Unknown handler kind — skip-to-TTL.
		return nil, nil
	}
}

// resolveContentEntryForRefresh re-dispatches the single K8s call an
// api-stage CONTENT entry caches (Ship F1, 0.30.119). The entry's Inputs
// are a content key (gvr, namespace, name-or-empty); the refresh is one
// un-gated K8s re-dispatch — no whole-RESTAction re-run, no self-hit.
// The fresh raw envelope is returned for resolveAndPopulateL1 to Put
// back under the content key. (nil, nil) — the call is not currently
// pivot-servable — is a skip-to-TTL, not an error.
func resolveContentEntryForRefresh(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	return restactionsapi.RefreshContentEntry(ctx, inputs)
}

// schemaGroupVersion renders the apiVersion string objects.Get expects.
// Core group ("") renders as just the version.
func schemaGroupVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

// resolveRestActionForRefresh converts the re-fetched CR to a typed
// RESTAction and dispatches restactions.Resolve, returning encoded
// output. Mirrors the restActionHandler.ServeHTTP resolve+encode path.
func resolveRestActionForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("unstructured -> RESTAction: %w", err)
	}

	// Ship 0.30.123 (#155): the Ship 0.30.120 layer-(a) exportJwt
	// skip-to-TTL scan was REMOVED here. Layer (a) existed only because a
	// background refresh had no way to complete an exportJwt /call-loopback
	// stage (no per-user JWT) — so it declined to refresh those RESTActions
	// at all. 0.30.123's in-process nested /call resolves a /call-loopback
	// stage WITHOUT an Authorization header (identity carried on ctx), so
	// the refresher CAN now correctly refresh an exportJwt RESTAction with
	// real, non-empty content. The skip-to-TTL net is obsolete.
	//
	// Layer (b) — the error-aware Put-gate in resolveAndPopulateL1 — STAYS
	// as the general backstop: any stage that still errors (a genuine RBAC
	// denial, an apiserver fault) bumps the stage-error sink and the
	// Put-gate declines to overwrite the good entry.

	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In: &cr,
		// Ship 0.30.230 fix-at-root: SArc threaded from ctx. The
		// refresher's resolveAndPopulateL1 attaches the SA rc via
		// cache.WithInternalRESTConfig (resolve_populate.go:191) before
		// invoking the resolveOnceFn that lands here.
		SArc:    rcFromCtx(ctx),
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve RESTAction: %w", err)
	}
	return encodeResolvedJSON(res)
}

// resolveRAFullListForRefresh re-resolves a RAFullList cell — Ship 4a
// (0.30.198). The cell holds the RA's resolved STATUS MAP (the full sorted
// result) resolved UNPAGINATED. It converts the re-fetched CR to a typed
// RESTAction, resolves it at PerPage=0/Page=0 (no `.slice` → the RA's output
// jq returns the full sorted set), and returns json.Marshal of the Status
// map — byte-identical to the apiref serve path's PutRAFullList (which
// marshals the same map via cache.PutRAFullList). Returns (nil, nil) when the
// resolve produced no Status (skip-to-TTL).
func resolveRAFullListForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	scheme := runtime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(got.Unstructured.Object, &cr); err != nil {
		return nil, fmt.Errorf("unstructured -> RESTAction (raFullList): %w", err)
	}
	// UNPAGINATED — Inputs already carry PerPage=0/Page=0 (RAFullListKeyInputs),
	// but pass 0/0 explicitly so the contract is local + obvious.
	res, err := restactions.Resolve(ctx, restactions.ResolveOptions{
		In: &cr,
		// Ship 0.30.230 fix-at-root: SArc from ctx — refresher attaches
		// SA rc upstream (resolve_populate.go:191).
		SArc:    rcFromCtx(ctx),
		AuthnNS: authnNS,
		PerPage: 0,
		Page:    0,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve RAFullList: %w", err)
	}
	if res.Status == nil || res.Status.Raw == nil {
		// No status produced — skip-to-TTL (not an error).
		return nil, nil
	}
	// The cell stores the Status MAP (decoded then re-marshaled canonically),
	// matching cache.PutRAFullList(json.Marshal(full)). Round-trip through a
	// map so the bytes are canonical (sorted keys) and byte-identical to the
	// serve-path Put regardless of the jq emitter's key order.
	var full map[string]any
	if err := json.Unmarshal(res.Status.Raw, &full); err != nil {
		return nil, fmt.Errorf("RAFullList status not a JSON object: %w", err)
	}
	// EMPTY-FULL GUARD twin (0.30.208) — symmetric to the apiref serve-path
	// guard. If a refresh re-resolves to an EMPTY full (single array key,
	// length 0), it is INDISTINGUISHABLE from a not-yet-synced /
	// continueOnError-degraded resolve, so refuse to overwrite the existing
	// (possibly good, non-empty) cell with empty. Treat it as skip-to-TTL
	// (return nil,nil — the same no-status sentinel) so the cell is left
	// untouched and re-resolved on the next cycle once the informer is
	// synced. Mechanism-uniform: keyed off "the full is empty", NO
	// resource/name/GVR literal.
	if cache.FullListIsEmpty(full) {
		return nil, nil
	}
	return json.Marshal(full)
}

// resolveWidgetForRefresh dispatches widgets.Resolve on the re-fetched
// CR, returning encoded output. Mirrors the widgetsHandler.ServeHTTP
// resolve+encode path.
func resolveWidgetForRefresh(ctx context.Context, got objects.Result, inputs cache.ResolvedKeyInputs, authnNS string) ([]byte, error) {
	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In: got.Unstructured,
		// Ship 0.30.230 fix-at-root: RC threaded from ctx. The
		// refresher's resolveAndPopulateL1 (resolve_populate.go:191)
		// attaches the SA rc via cache.WithInternalRESTConfig before
		// invoking the resolveOnceFn that lands here. Without RC set,
		// downstream crdschema.ValidateObjectStatus → cache.GVRFor →
		// discoverPluralInfo 500s with "plurals discovery: nil
		// *rest.Config" (the four-revert root cause).
		RC:      rcFromCtx(ctx),
		AuthnNS: authnNS,
		PerPage: inputs.PerPage,
		Page:    inputs.Page,
		Extras:  inputs.Extras,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve widget: %w", err)
	}
	return encodeResolvedJSON(res)
}
