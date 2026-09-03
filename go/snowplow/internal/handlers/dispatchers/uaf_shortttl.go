// uaf_shortttl.go — the userAccessFilter (UAF) cache posture: the 1.12.3 A-1
// PUT-DECLINE (current) and the #118 (d) interim short-TTL it supersedes.
//
// ============================ 1.12.3 — A-1 ==============================
// SUPERSEDING POSTURE (read this first): as of 1.12.3, NOTHING WHOSE BODY WAS
// NARROWED BY A userAccessFilter IS CACHED — in any class. Every L1 Put site
// consults uafDeclineReason below and skips the write; the raFullList cell is
// bypassed outright at ra_full_list.go. The request is still served normally —
// 200 with the requester's own correctly-narrowed body; only the cache write is
// skipped.
//
// TWO SIGNALS, because one was not enough (R-1). The first cut gated on the
// DECLARATION — "does the RESTAction being dispatched declare a UAF stage?" —
// and that closed the direct /call on the RA while leaving open the path the
// portal actually renders. Widgets fold the apiRef'd RA's refiltered rows into
// their OWN status.widgetData (widgets/resolve.go), and the declaring CR sits
// several resolver frames below the widget the dispatcher holds, so the
// declaration predicate saw nothing there. Live measurement settled it: 66
// widgets apiRef a UAF RA, and the widgets cell served 298,064 hits against 365
// misses over 5d7h — the cold path was gated and the hot one was not. So the
// gate now also consults an OBSERVED signal, cache.UAFTouchedSink, bumped where
// the refilter actually runs and therefore blind to how far away the declaration
// is. Both are kept: only the declaration exists BEFORE a resolve, which is what
// lets the boot seed skip a UAF RA's whole fan-out.
//
// THE CARRIERS, all now gated: restactions (dispatch, refresher, seed), widgets
// (dispatch, seed), widgetContent (walker populate), raFullList (bypass).
//
// WHY the (d) TTL cap was not enough. (d) assumed the exposure was TIME (a
// user's own view going stale after an out-of-band RBAC change) and capped the
// window. Both red-teams then reproduced a CROSS-USER exposure that no TTL
// bounds: the key folds BindingUID + RBACSubGen (helpers.go
// dispatchCacheLookupKey; Representative* are carried but NOT folded —
// resolved.go), while a UAF stage narrows the body PER REQUESTER PER OBJECT
// (refilter.go). Two users sharing the first-match binding for `get restactions`
// — one CRB granting the group they are both in, divergent per-namespace
// RoleBindings — derive the IDENTICAL key, and the hit path writes
// entry.RawJSON verbatim with no re-narrowing (restactions.go). The second user
// is served the FIRST user's rows immediately, within any TTL. A short TTL
// shortens a cross-tenant leak; it does not close one.
//
// SCOPE OF THE MITIGATION: exactly the RAs whose output is per-requester
// narrowed (RESTAction.HasUserAccessFilterStage — the ONE predicate, on the API
// type, shared by the dispatchers and apiref packages). Every non-UAF RA caches
// byte-identically to 1.12.2. Cost: a UAF /call re-resolves every time.
//
// NOT THE FIX, AND DELIBERATELY SO. 1.13.0 replaces this with a UAF-SCOPE
// DIGEST folded into the cache key (v7) — the requester's effective per-object
// narrowing scope becomes part of the key, so divergent users get divergent
// cells and caching is re-enabled for these RAs. The (d) TTL knob below is kept
// INTACT (it is being chart-plumbed separately) precisely because it becomes
// load-bearing again the moment v7 re-enables these Puts.
//
// ====================== #118 (d) — the TTL cap ==========================
// #118 (d) interim short-TTL for userAccessFilter-bearing resolved cells.
//
// THE DEFECT (docs/118-uaf-rbac-stale-read-design-2026-07-22.md): the resolved-
// cache key folds a single dispatch-authorizing BindingUID, but a
// userAccessFilter stage re-evaluates RBAC PER OBJECT, PER that object's own
// namespace — a dependency the key never sees. An out-of-band RBAC grant/revoke
// bumps RBACGen + rebuilds the snapshot but evicts ZERO resolved cells, so a
// user's own now-stale UAF view (access granted-in-N not yet visible, or
// revoked-in-N still visible) is served until the cell leaves cache. On a hot,
// data-plane-refreshed cell the CreatedAt slides forward every refresh → the
// standard TTL never elapses → effectively-indefinite staleness.
//
// THIS IS THE INTERIM (d), NOT THE FIX. It does NOT fix the key — a within-TTL
// RBAC change is still served stale. It CAPS the exposure window at a short
// TTLOverride on UAF-bearing cells. The durable fix is #118 (c): a per-user RBAC
// sub-generation folded into the cache key.
//
// C-118-6 (THE CRUX): the override must be stamped at BOTH Put sites — the
// customer dispatch Put (restactions.go) AND the refresher re-Put
// (resolve_populate.go, which builds a fresh entry with zero CreatedAt and thus
// slides the absolute TTL forward on every data-plane refresh). Stamping only
// the first Put lets a hot churning UAF cell re-Put without the override and
// OUTLIVE the cap. The customer path detects UAF from the resolved RESTAction CR
// and records it on cacheInputs.HasUAF; the refresher reads the carried
// inputs.HasUAF (it has no CR). Both sites call uafTTLOverrideForEntry, so the
// override derivation is single-source and cannot drift between them.
//
// TOGGLE (project_caching_is_provisional): UAF_RESOLVED_TTL_SECONDS default 0 =
// DISABLED → uafTTLOverrideForEntry returns 0 → TTLOverride stays 0 → every UAF
// cell uses the standard TTL, byte-identical to today. Cleanly removable.
//
// R-d-4 SITE MAP — the complete `ResolvedEntry{` Put enumeration and WHY each
// site is or is not in scope (reasoned, not missed). The in-scope sites are the
// identity-bound restactions cells that carry the per-user REFILTER OUTPUT.
// SINCE 1.12.3 each of these sites does TWO things for a UAF-bearing entry:
// FIRST consult declineUAFPut (skip the Put entirely — the A-1 mitigation), and
// only otherwise stamp uafTTLOverrideForEntry (the (d) cap, which now governs
// only the non-declined populations and is retained for 1.13.0/v7).
//
// IN SCOPE — all three restactions ResolvedEntry Put sites:
//   - restactions.go       — customer dispatch Put.
//   - resolve_populate.go  — refresher re-Put (CreatedAt-slides on every
//                            data-plane refresh; the C-118-6 crux site). It has
//                            no RESTAction CR, so it consults the CARRIED
//                            inputs.HasUAF — defensive symmetry: a UAF cell can
//                            no longer be created, so the refresher should never
//                            see one, but if it ever did it must not re-Put it.
//   - phase1_pip_seed.go seedOneRestaction — boot-seed Put (seeds UAF cells
//                            under a cohort representative identity; added after
//                            the arch gate on 3783e65 caught the "counted 2,
//                            there were 3" miss). Since 1.12.3 the seed
//                            short-circuits BEFORE the resolve for a UAF RA (the
//                            resolve output could never be Put, so paying for it
//                            is pure waste); the tail keeps a defensive decline
//                            so driving the seam directly cannot write one.
//
// THE FOURTH SITE (1.12.3, previously mis-waived): the raFullList cell
// (ra_full_list_store.go PutRAFullList / PutRAFullListPinned, written from
// apiref's raFullListServe). The pre-1.12.3 waiver text claimed UAF refilter
// output "lands in the per-page restactions cell, never here". That was WRONG:
// raFullListServe caches the apiRef'd RA's OWN full resolve output, UAF stage
// included, under a cell keyed on BindingUID ALONE (no RBACSubGen) — strictly
// weaker separation than the restactions cell. It is now bypassed for a
// UAF-bearing RA at raFullListServe, so neither the serve nor the populate side
// is reachable and the store-side waivers are correct again by construction.
//
// OUT OF SCOPE BY DESIGN (the PM's open apistage question, resolved by tracing):
//   - apistage.go:607 / cluster_list.go:417 — CacheEntryClassApistage, keyed by
//     contentKeyInputs(gvr, ns, name): IDENTITY-FREE shared substrate, the raw
//     pre-refilter apiserver envelope. Its staleness is DATA-plane (an informer
//     dirty-mark on the watched GVR invalidates it), NOT RBAC-refilter-output
//     staleness. It carries NO BindingUID and NO per-user refilter output, so an
//     out-of-band RBAC change does not make IT stale — the identity-bound
//     restactions cell (which holds the refilter output and IS capped above) is
//     the one that goes stale. Both already self-stamp the CATALOG_UNSERVABLE
//     data-plane override for their own degradation. Capping the substrate would
//     be wrong-cell and would churn a shared hot cell for zero RBAC-freshness
//     gain. (Traced + agreed with arch/PM; no disagreement to flag.)
//   - partial_result_ttl.go:85 — self-stamps its OWN bounded TTLOverride for a
//     partial-with-errors body; independent bounded-staleness mechanism. Untouched.
//   - RETRACTED — THIS PARAGRAPH WAS THE R-1 BLOCKER. It previously read: "UAF is
//     a restactions-STAGE contract ... the UAF refilter output only ever lands in
//     a restactions-class cell, never a widget-class one", and on that basis put
//     seedOneWidget, widgets.go and widget_content.go OUT of scope. It is FALSE,
//     by the identical mechanism that invalidated the raFullList waiver above.
//
//     What actually happens: widgets.Resolve → resolveApiRef → apiref.Resolve →
//     restactions.Resolve, and the apiRef'd RA's UAF-REFILTERED ROWS ARE FOLDED
//     INTO THE WIDGET'S OWN status.widgetData (widgets/resolve.go:154). That body
//     is Put under the widgets-class per-BINDING key at widgets.go (both the
//     external-TTL and the genuine-Put branches) and by seedOneWidget, and the
//     widgets hit path serves entry.RawJSON verbatim. It is not merely A hole —
//     live measurement makes it THE hole: 66 widgets apiRef a UAF RA, and that
//     cell served 298,064 hits against 365 misses over 5d7h, against the
//     restactions cell's near-zero traffic. The declaration-only gate shipped
//     first closed the cold path and left the hot one open.
//
//     Worse, isRBACSensitiveApiRefWidget (widget_content.go) DELIBERATELY routes
//     apiRef+template widgets INTO that per-cohort cell, in the belief that it is
//     "RBAC-correct by construction". That belief is what A-1 disproves: the cell
//     is per-BINDING, and per-binding is not per-user once a userAccessFilter is
//     in play.
//
//     NOW IN SCOPE, gated on the OBSERVED refilter (cache.UAFTouchedSink) rather
//     than on any declaration, because the declaring CR sits several resolver
//     frames below the object each of these Put sites holds:
//       - widgets.go            — customer dispatch, gate at the HEAD of the Put
//                                 chain (ahead of the partial and external-TTL
//                                 branches, which also write).
//       - phase1_pip_seed.go seedOneWidget — boot seed; no pre-resolve skip is
//                                 possible here (the apiRef chain is nested and
//                                 template-expanded, so it is not statically
//                                 enumerable), so it resolves and then declines.
//       - widget_content.go populateWidgetContentL1 — the identity-FREE shared
//                                 envelope, third sibling of its stage-error and
//                                 external-touched gates.
//     THE LESSON, recorded so the next reader does not repeat it: a leak with N
//     carriers needs an acceptance arm PER CARRIER. The restactions-only arm went
//     green over an open hot carrier. Any comment of the form "UAF output only
//     ever lands in class X" is a stale-waiver smell — check it against
//     widgets/resolve.go before trusting it.

package dispatchers

import (
	"time"

	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
)

// restactionHasUAFStage reports whether any api-step of cr declares a
// userAccessFilter (the per-object refilter contract the resolved key is blind
// to). It is a thin package-local alias for the ONE predicate on the API type,
// RESTAction.HasUserAccessFilterStage — 1.12.3 moved the body there because the
// apiref package needs the SAME predicate for the raFullList bypass and a second
// copy could drift (#64 anti-shadow-drift). Nil cr, nil api-step elements and a
// nil UserAccessFilter are guarded by the method. This is a general per-entry
// predicate ("the entry's RA has a UAF stage"), NOT a per-resource special-case
// (feedback_no_special_cases): it keys on the presence of the UAF contract
// itself, uniform across every RA.
func restactionHasUAFStage(cr *templatesv1.RESTAction) bool {
	return cr.HasUserAccessFilterStage()
}

// UAF decline reasons — the two INDEPENDENT signals that a resolved body is
// narrowed per requester. Either one is sufficient to decline; they are kept
// distinct so a log line and a test can say WHICH fired.
const (
	// uafDeclineDeclared — the entry's own RESTAction declares a userAccessFilter
	// stage (inputs.HasUAF, stamped from RESTAction.HasUserAccessFilterStage).
	// Available PRE-resolve, which is what lets the boot seed skip the fan-out.
	uafDeclineDeclared = "declared"
	// uafDeclineObserved — the resolve under this context was MARKED as
	// userAccessFilter-narrowed (UAFTouchedSink). Necessarily post-resolve, but
	// TRANSITIVE and declaration-blind, so it reaches shapes the declaration
	// cannot see from the Put site's own frame.
	//
	// WHAT IT ACTUALLY REACHES depends on where the bumps are, and that differs
	// between this branch and the assembled tree:
	//   - a widget whose apiRef'd RA declares the UAF several resolver frames
	//     down (the R-1 hot carrier) — COVERED HERE, by apiref.Resolve's
	//     declaration bump;
	//   - a non-UAF RA that NESTS a UAF one — NOT covered here. No bump fires on
	//     that path, because the only bump inspects the RA the apiRef names and
	//     that one declares nothing (asserted by
	//     TestM1_DeclarationLimbIsBlindToNestedUAFChild, apiref package). It
	//     closes with the refilter bump on fix/1.12.3-authz-hardening, whose arm
	//     is TestA4_RefilterBumpsUAFTouchedSink.
	// Read this constant as "something marked the resolve", never as "every
	// narrowed shape is marked".
	uafDeclineObserved = "observed_refilter"
)

// uafDeclineReason is the SINGLE derivation of "must this Put be skipped?",
// shared by every class. It returns "" when the Put may proceed, else the
// reason constant.
//
// TWO SIGNALS, BOTH LOAD-BEARING, NEITHER REDUNDANT (R-1). The declaration is
// the only signal available BEFORE a resolve, so it is what lets the seed skip
// work; the sink is the only signal that survives resolver-frame distance, so it
// is what catches the widgets class the portal actually renders from. Dropping
// either re-opens a carrier: dropping the sink re-opens R-1, dropping the
// declaration turns the seed's cheap skip into a full fan-out it then discards.
//
// Pure — no side effects. The counter bump lives in the thin per-class wrappers
// below so each class counts its own declines while the RULE stays single-source
// (#64 anti-shadow-drift).
func uafDeclineReason(inputs *cache.ResolvedKeyInputs, sink *cache.UAFTouchedSink) string {
	if inputs != nil && inputs.HasUAF {
		return uafDeclineDeclared
	}
	if sink.Count() > 0 { // nil-receiver-safe: no sink installed reads as 0
		return uafDeclineObserved
	}
	return ""
}

// declineUAFPut is the RESTACTIONS-class gate: report whether the L1 Put for
// this resolved entry must be SKIPPED because its body is narrowed PER
// REQUESTER by a dependency the cache key does not fold.
//
// ONE RULE, THREE SITES (#64 anti-shadow-drift). The customer dispatch Put
// (restactions.go), the refresher re-Put (resolve_populate.go) and the boot seed
// (phase1_pip_seed.go) all route their decision through THIS function, exactly
// as they all route their TTL override through uafTTLOverrideForEntry. The
// CR-bearing sites stamp inputs.HasUAF from restactionHasUAFStage; the refresher
// — which has no CR — reads the HasUAF the original Put carried; and all of them
// additionally consult the sink, which is what covers a non-UAF RA that NESTS a
// UAF one. So the sites cannot diverge on what "UAF-bearing" means.
//
// THE COUNTER IS BUMPED HERE, not at the call sites: bumping inside the gate
// makes "the counter ticked" and "a Put was skipped" the same event, so
// snowplow_restactions_uaf_put_declined_total cannot drift from it. The helper
// is therefore NOT side-effect-free — call it exactly once per candidate Put, at
// the decision point.
//
// NO TOGGLE, by design. This is a confirmed cross-tenant correctness defect, so
// there is no env flag to switch the leak back on (no flag-parking); the
// mechanism is instead cleanly REMOVABLE — 1.13.0 deletes these helpers and
// their call sites when the UAF-scope digest lands in the key (v7).
//
// No declaration and no observed refilter → false: byte-identical to 1.12.2 for
// every non-UAF entry, which is every entry in a deployment with no UAF RA.
func declineUAFPut(inputs *cache.ResolvedKeyInputs, sink *cache.UAFTouchedSink) bool {
	if uafDeclineReason(inputs, sink) == "" {
		return false
	}
	cache.BumpRestactionsUAFPutDeclined()
	return true
}

// declineWidgetUAFPut is the WIDGETS/WIDGETCONTENT-class gate — the R-1 carrier.
// Same rule (uafDeclineReason), separate counter.
//
// WHY A SEPARATE COUNTER AND NOT A SHARED ONE: this is the cell the portal
// actually renders from (66 live widgets apiRef a UAF RA; 298,064 hits vs 365
// misses over 5d7h), so its decline rate is the number an operator watches to
// see the real cost of the mitigation. Summing it into the restactions counter
// would bury a 300K-hit class under a 365-miss one.
//
// In practice this class declines on uafDeclineObserved: a widget CR declares no
// userAccessFilter of its own — the declaration lives on the apiRef'd RESTAction
// several resolver frames below — so the sink is the signal that fires. The
// declaration limb is still consulted for uniformity and for any future path
// that stamps HasUAF on a widgets-class ResolvedKeyInputs.
func declineWidgetUAFPut(inputs *cache.ResolvedKeyInputs, sink *cache.UAFTouchedSink) bool {
	if uafDeclineReason(inputs, sink) == "" {
		return false
	}
	cache.BumpWidgetsUAFPutDeclined()
	return true
}

// isWidgetClass reports whether a CacheEntryClass belongs to the widget family,
// so a CLASS-AGNOSTIC caller (the refresher, which refreshes every class through
// one function) can pick the matching decline counter. Mirrors the shape of
// isIdentityFreeClass (resolve_populate.go). "widgets" has no exported constant
// — it is a bare CacheEntryClass literal throughout (see resolved.go's class
// list) — so it is spelled out here once rather than in each caller.
func isWidgetClass(class string) bool {
	return class == "widgets" || class == cache.CacheEntryClassWidgetContent
}

// uafTTLOverrideForEntry returns the short UAF TTLOverride to stamp on a
// resolved entry, or 0 (no override → standard TTL) when either the knob is
// disabled (UAF_RESOLVED_TTL_SECONDS unset/0) OR the entry is not UAF-bearing
// (inputs.HasUAF false / inputs nil). Called at BOTH Put sites (customer +
// refresher) so the cap is derived identically regardless of which path writes
// the cell (C-118-6). effectiveTTLLocked already honours the SHORTER of the
// override and the store TTL, so this only ever TIGHTENS the bound.
func uafTTLOverrideForEntry(inputs *cache.ResolvedKeyInputs) time.Duration {
	if inputs == nil || !inputs.HasUAF {
		return 0
	}
	return cache.UAFResolvedTTL()
}
