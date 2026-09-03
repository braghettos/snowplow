// partial_result_ttl.go — D (bounded partial-cache backstop), default-OFF.
//
// WHAT D IS (and is NOT). D is NOT a reversal of #313 Cache-A's "never PERSIST a
// partial". #313 declines the Put because a partial pinned under the FULL TTL
// (e.g. 3600s) self-heals far too slowly. D introduces a SEPARATE, much shorter
// TTL (PARTIAL_RESULT_TTL_SECONDS, default 0 = OFF) so a partial-with-errors body
// is served warm for a BOUNDED window that stops the per-/call re-storm, then
// EXPIRES and forces a fresh resolve. The distinction from a clean Put is the TTL
// MAGNITUDE + provenance, not the persist/decline decision — exactly the #36/#52
// CATALOG_UNSERVABLE_TTL posture (cluster_list.go:431 / apistage), applied to the
// stage-error branch instead of the not-servable branch.
//
// D is the belt-and-suspenders backstop for anything that STILL declines after R
// converges the composition-resources path. Per feedback_bounding_mechanism_
// discipline the bounded event (a decline) must still happen after the root fix:
// with R landed, composition-resources resolves CLEAN (Count()==0) → normal Put →
// D NEVER fires for it (C6). D fires ONLY for a RESIDUAL genuinely-un-cacheable
// RA — the intended belt-and-suspenders, not a mask of the root defect.
//
// CONSTRAINTS (feedback_bounding_mechanism_discipline + feedback_l1_per_user_
// keyed_never_cohort):
//   - Cost-proportional: one bounded entry per declined key, on the RARE decline
//     branch only (never the hot clean path). A map insert.
//   - Leak-safe: the SAME per-USER cacheKey the clean Put uses (restactions.go /
//     widgets.go) — NEVER a cohort key. A partial resolved under user U is keyed
//     to U; user V re-resolves.
//   - Self-dep ONLY: record the self-dep (a CR DELETE/UPDATE of the RA itself
//     evicts/refreshes the partial) but NOT the inner edge-type-3 deps — a
//     partial's inner set is incomplete/untrustworthy. Bounded TTL is the safety
//     net for changes the self-dep cannot see.
//   - Post-serve: the body is served identically to today; D only changes whether
//     it is ALSO cached-for-a-window. Default 0 ⇒ byte-identical to today.
package dispatchers

import (
	"time"

	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// envPartialResultTTLSeconds is the bounded serve-stale window for a declined
// partial-with-errors result. 0 (the default) disables D entirely — the decline
// stays a bare decline, byte-identical to pre-D. Set to e.g. 30–60s to bound the
// per-/call re-storm of a residual un-cacheable RA. Read via env pass-through
// (chart additionalProperties, same as CLIENT_MAX_RETRIES — no template change).
const envPartialResultTTLSeconds = "PARTIAL_RESULT_TTL_SECONDS"

// partialResultTTL returns the configured bounded serve-stale window, or 0 (D
// off). Read per-call (cheap env lookup, same cadence as the other env gates);
// negative/zero ⇒ 0 ⇒ off.
func partialResultTTL() time.Duration {
	s := env.Int(envPartialResultTTLSeconds, 0)
	if s <= 0 {
		return 0
	}
	return time.Duration(s) * time.Second
}

// putPartialWithTTL is the SHARED D backstop, called from each stage-error
// decline site (restactions.go / widgets.go) INSTEAD of a bare decline, but ONLY
// when partialResultTTL() > 0. It Puts the already-encoded partial body under the
// SAME per-user cacheKey the clean Put would use, stamped with a bounded
// TTLOverride, records ONLY the self-dep, and bumps the "did D fire?" falsifier
// counter. No-op (returns false, no Put) when D is off or the cache handle/key is
// absent — so default-off is byte-identical to today.
//
// Placement contract: the caller invokes this on the RARE decline branch, AFTER
// the body is encoded — it never blocks/alters the 200 response body (the partial
// is served identically to today; D only decides whether it's ALSO cached for a
// bounded window).
func putPartialWithTTL(
	handle cacheHandle,
	cacheKey string,
	encoded []byte,
	inputs *cache.ResolvedKeyInputs,
	gvr schema.GroupVersionResource,
	namespace, name string,
) bool {
	ttl := partialResultTTL()
	if ttl <= 0 || handle == nil || cacheKey == "" {
		return false // D off, or nothing to key against — bare decline (pre-D)
	}
	// 1.12.3 A-1 — DEFENCE IN DEPTH. This helper is the OTHER writer in the
	// restactions and widgets Put chains: it persists a partial-with-errors body
	// under the SAME shared per-binding cacheKey when PARTIAL_RESULT_TTL_SECONDS
	// is set. Both callers now run the UAF gate at the HEAD of their chain,
	// ahead of the stage-error branch that calls this — precisely so a
	// refilter-narrowed body cannot slip through here — so in a correct chain
	// this check never fires. It exists because "the caller checked first" is an
	// ordering invariant, and ordering invariants are what the A-1 first cut got
	// wrong: gating only the genuine-Put branch left this writer open. A future
	// caller that forgets the ordering is stopped here instead of leaking.
	//
	// Declaration limb only — this helper takes no ctx, so it cannot read the
	// UAFTouchedSink. The observed limb is enforced by the callers' chain-head
	// gate, which is where the sink is in scope.
	if declineUAFPut(inputs, nil) {
		return false
	}
	handle.Put(cacheKey, &cache.ResolvedEntry{
		RawJSON:     encoded,
		Inputs:      inputs,
		TTLOverride: ttl, // bounded serve-stale window — self-evicts within ttl
	})
	// Self-dep ONLY (design §4): a DELETE/UPDATE of the RA/widget CR itself
	// evicts/refreshes the partial. Do NOT record inner edge-type-3 deps — a
	// partial's inner set is incomplete; bounded TTL is the net for the rest.
	// Ensure the informer is registered before recording (symmetric with the
	// clean Put's ensureWatcherInformerForGVR at restactions.go).
	ensureWatcherInformerForGVR(gvr)
	cache.Deps().Record(cacheKey, gvr, namespace, name)
	cache.BumpPartialServedStale()
	return true
}
