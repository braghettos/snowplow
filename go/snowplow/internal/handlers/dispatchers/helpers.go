package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/plumbing/http/response"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	idynamic "github.com/krateo-platformops/snowplow/internal/dynamic"
	"github.com/krateo-platformops/snowplow/internal/handlers/util"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/rbac"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func fetchObject(req *http.Request) (got objects.Result) {
	log := xcontext.Logger(req.Context())

	gvr, err := util.ParseGVR(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("GVR from request query parameters", slog.Any("gvr", gvr))

	nsn, err := util.ParseNamespacedName(req)
	if err != nil {
		got.Err = response.New(http.StatusBadRequest, err)
		return
	}
	log.Debug("Name and Namespace from request query parameters", slog.Any("nsn", nsn))

	return objects.Get(req.Context(), templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name: nsn.Name, Namespace: nsn.Namespace,
		},
		APIVersion: gvr.GroupVersion().String(),
		Resource:   gvr.Resource,
	})
}

func paginationInfo(log *slog.Logger, req *http.Request) (perPage, page int) {
	perPage, page = -1, -1

	if val := req.URL.Query().Get("perPage"); val != "" {
		var err error
		perPage, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert perPage parameter to int",
				slog.Any("err", err))
		}
	}

	if val := req.URL.Query().Get("page"); val != "" {
		var err error
		page, err = strconv.Atoi(val)
		if err != nil {
			log.Error("unable convert page parameter to int",
				slog.Any("err", err))
		}
	}

	return normalizePagination(perPage, page)
}

// normalizePagination is the SINGLE normalization core for the cache-key
// pagination fold, shared by the EMIT path (paginationInfo, from the /call
// query) and the SUBSCRIPTION path (DeriveSubscriptionKey, from ?sub= coords)
// so both stamp byte-identical (perPage, page) into ComputeKey.
//
// (#64 real root cause) A non-paginated /call sends no perPage/page query →
// paginationInfo's -1,-1 default → the emit key folds "-1","-1". The frontend
// subscription sends perPage:0/page:0 (or omits them → json zero) → 0,0. "-1"
// != "0" → key mismatch → published + subscribers>=1 + delivered:0, for EVERY
// class (the fold is class-independent). EXTRACTING the normalization (rather
// than re-implementing it on the subscription side) is the fix: the drift
// between two hand-written copies is exactly what caused the bug.
//
// Rules (mirrors the historical paginationInfo, the page=1 rule INCLUDED):
//   - perPage <= 0  → -1 (non-paginated sentinel)
//   - page    <= 0  → -1 (non-paginated sentinel)
//   - perPage > 0 && page <= 0 → page = 1 (a paginated request with no/zero
//     page means the first page; this rule MUST survive the extraction)
func normalizePagination(perPage, page int) (int, int) {
	if perPage <= 0 {
		perPage = -1
	}
	if page <= 0 {
		page = -1
	}
	if perPage > 0 && page <= 0 {
		page = 1
	}
	return perPage, page
}

// evaluateRBACFn is the RBAC-evaluation seam consumed by checkDispatchRBAC
// (M5 coverage-audit). Defaults to rbac.EvaluateRBAC; production NEVER
// reassigns it (the compiled path is byte-identical to a direct call). Only
// the M5 _test.go shim swaps it, to drive checkDispatchRBAC's fail-closed
// branches (no-UserInfo / eval-error / deny / allow) AND to capture the
// EvaluateOptions so the test can assert the load-bearing (Verb=="get",
// SkipBindingUID==true) contract without wiring a full RBAC snapshot. Mirrors
// the resolveOnceFn / dispatch_seams.go var-seam idiom.
var evaluateRBACFn = rbac.EvaluateRBAC

// checkDispatchRBAC is the cache=on permission gate (Revision 2
// binding, Tag 0.30.4). Returns true iff the user identified by ctx is
// permitted to GET the dispatched CR in namespace.
//
// The check runs against the *dispatch target* (RestAction or Widget
// CR) — the same object the cache=off fetchObject branch hits the
// apiserver for. In cache=on mode fetchObject does not enforce RBAC
// for that GET, so the gate must run explicitly here.
//
// Callers MUST only invoke this in cache=on mode (`!cache.Disabled()`).
// In cache=off mode the gate is implicit in fetchObject's per-user
// apiserver call.
func checkDispatchRBAC(ctx context.Context, gvr schema.GroupVersionResource, namespace string) bool {
	log := xcontext.Logger(ctx)

	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		log.Error("checkDispatchRBAC: unable to extract UserInfo",
			slog.Any("err", err),
		)
		return false
	}

	// Ship 0.30.242 H.c-layered (Phase 2 step 2a) — EvaluateRBAC returns
	// (allowed, matchedBindingUID, err). This per-item caller ignores
	// matchedBindingUID; cache-key callers consume it (helpers.go:202 +
	// :306 via the per-request memo, Phase 2b).
	allowed, _, evalErr := evaluateRBACFn(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     gvr.Group,
		Resource:  gvr.Resource,
		Namespace: namespace,
		// Ship L1 (0.30.252): per-item caller discards
		// matchedBindingUID — skip the CRB/RB stable-sort.
		SkipBindingUID: true,
	})
	if evalErr != nil {
		log.Error("checkDispatchRBAC: EvaluateRBAC error",
			slog.String("user", ui.Username),
			slog.String("gvr", gvr.String()),
			slog.String("namespace", namespace),
			slog.Any("err", evalErr),
		)
		return false
	}
	return allowed
}

// dispatchWidgetContentKey builds the identity-free widget content L1
// key and returns the live cache handle (Ship G, 0.30.16x). Returns
// (key, nil, nil) when the layer is disabled — callers MUST treat
// handle==nil as "skip the content lookup, take the existing per-user
// L1 path".
//
// IDENTITY-FREE BY CONSTRUCTION: Username/Groups are left ZERO; the
// ComputeKey branch skips identity for CacheEntryClassWidgetContent
// (resolved.go). The serve-time gateWidgetEnvelope re-derives every
// embedded status.resourcesRefs.items[].allowed flag under the request
// identity before the body is written to the response — see widgets.go.
// A request that lacks UserInfo would not be able to run the gate, but
// keying itself is identity-independent so we do NOT bail on missing
// identity here; the gate's served=false branch handles the no-identity
// case symmetrically with the existing per-user lookup (which DOES
// nil-check UserInfo at dispatchCacheLookupKey).
func dispatchWidgetContentKey(ctx context.Context, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle, *cache.ResolvedKeyInputs) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil, nil
	}
	if !cache.WidgetContentL1Enabled() {
		return "", nil, nil
	}
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassWidgetContent,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		// Username/Groups intentionally zero — see header.
		PerPage: perPage,
		Page:    page,
		Extras:  extras,
	}
	return cache.ComputeKey(inputs), c, &inputs
}

// dispatchCacheLookupKey builds the L1 resolved-output cache key and
// returns the live cache handle, if the L1 layer is enabled. Returns
// (key, nil, nil) when L1 is disabled — callers MUST treat handle==nil
// as "skip cache lookup, take the 0.30.6 path".
//
// User identity is read from the request context; on error (missing
// or unparseable UserInfo) we treat the request as uncacheable —
// returning a nil handle — so the request still resolves correctly but
// never reads or writes the L1 cache. A keyless request would risk
// cross-user leaks, which is unacceptable.
//
// 0.30.8: the function also returns the canonical ResolvedKeyInputs so
// the caller can stash it on the L1 entry. Refresher reuses Inputs to
// drive a re-resolve on UPDATE/PATCH events.
//
// Ship 0.30.242 H.c-layered Phase 2b — identity is folded as the
// per-layer BindingUID (the first-match binding's UID that authorised
// the layer's GET, returned by rbac.EvaluateRBAC). Replaces the v3
// BindingSetHash uint64 cohort hash. Per-binding sharing (design §1.2):
// two users granted by the SAME binding share the L1 cell.
//
// PATH B (Diego ratified 2026-06-03 R3 deferral): the BindingUID is
// derived via a DIRECT rbac.EvaluateRBAC call per request, not via the
// per-request memo. The memo type ships scaffolding-only in 2b; F3
// falsifier validates this deferral. If F3 reveals per-request memo
// consistency is load-bearing, plumbing is pulled forward as Phase 2d.
//
// FAIL-CLOSED: allowed=false / err yields bindingUID="" (the cell key
// collapses to the empty-identity row — same shape as cache=off's
// transparent fallback, project_cache_off_is_transparent_fallback).
func dispatchCacheLookupKey(ctx context.Context, handlerKind, group, version, resource, namespace, name string, perPage, page int, extras map[string]any) (string, cacheHandle, *cache.ResolvedKeyInputs) {
	c := cache.ResolvedCache()
	if c == nil {
		return "", nil, nil
	}
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		// Defence in depth — without an identity we cannot key
		// safely. Skip the cache for this request.
		return "", nil, nil
	}
	// Path B direct call — derive BindingUID for the layer's GET-permit.
	_, bindingUID, _ := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
		Username:  ui.Username,
		Groups:    ui.Groups,
		Verb:      "get",
		Group:     group,
		Resource:  resource,
		Namespace: namespace,
		Name:      name,
	})
	inputs := cache.ResolvedKeyInputs{
		CacheEntryClass: handlerKind,
		Group:           group,
		Version:         version,
		Resource:        resource,
		Namespace:       namespace,
		Name:            name,
		BindingUID:      bindingUID,
		// #118 (c) — the requesting identity's EFFECTIVE per-subject RBAC
		// sub-generation, folded into ComputeKey (identity-bound classes) so an
		// out-of-band grant/revoke touching THIS user's own bindings rotates the
		// key → cold miss → fresh UAF refilter. RBACSubGenForSubject folds the
		// user + every presented group (+ SA) counter (C-118-2 group-grant
		// crux). ui.Username/ui.Groups are already in hand here (the same tuple
		// EvaluateRBAC read above), so this is a handful of lock-free map reads,
		// no extra walk.
		RBACSubGen: cache.RBACSubGenForSubject(ui.Username, ui.Groups),
		// Representative identity for the refresher's re-resolve.
		// Carried on Inputs but NOT folded into ComputeKey (the cell
		// is keyed by BindingUID, not by the literal name). The first
		// writer's identity is the cell's representative; every member
		// of the equivalence class authorised by this BindingUID
		// produces byte-identical output (per-binding sharing — design
		// §1.2).
		RepresentativeUsername: ui.Username,
		RepresentativeGroups:   ui.Groups,
		PerPage:                perPage,
		Page:                   page,
		Extras:                 extras,
	}
	return cache.ComputeKey(inputs), c, &inputs
}

// serveFromCacheEligible is the #95 SECURITY serve-side guard (A4 read side;
// the sibling of FIX-C's populate-side skip). When dispatchCacheLookupKey
// re-derived a first-match BindingUID of "" (EvaluateRBAC deny/err fail-closed,
// see the FAIL-CLOSED note above), the key collapses to the shared
// empty-identity row — a cell FIX-C no longer seeds but that a broad identity
// (one whose EvaluateRBAC ALSO fail-closed, or a pre-fix residue) could have
// populated. Serving it to a DIFFERENT ""-deriving identity is a cross-identity
// read of a shared cell resolved under someone else's (possibly broad)
// narrowing — the A4 leak shape. So a "" BindingUID makes the serve path treat
// the lookup as a CACHE MISS: fall through to a direct resolve under the
// request's OWN identity (never serve-then-filter, never serve the shared ""
// cell). A non-empty BindingUID is byte-unchanged (true → normal L1 Get).
// handle==nil (cache off / no identity) is already handled by the caller's nil
// check; this adds the ""-BindingUID condition. Uniform across widgets +
// restactions (no per-handler special-case, feedback_no_special_cases).
func serveFromCacheEligible(inputs *cache.ResolvedKeyInputs) bool {
	return inputs != nil && inputs.BindingUID != ""
}

// unionForKey builds the body-dependency fingerprint the widget L1 key must
// fold under inline-extras design P (§1, §4.3): the UNION of both
// author-declared inline maps (apiRef.extras + resourcesRefsTemplateExtras)
// PLUS the per-request extras, with REQUEST WINNING on every collision
// (mirroring the per-surface request-wins precedence in widgets.Resolve).
//
// WHY a flat union is the correct key material: a change to EITHER inline map
// changes the resolved body (apiRef.extras feeds the apiRef fetch → ds; the
// rrt block feeds the resourcesRefsTemplate jq), so the widget content +
// per-cohort keys MUST vary on either. The union is collision-sound because
// the widget key ALSO folds Group/Version/Resource/Namespace/Name (and, on
// the per-cohort cell, BindingUID) — so two DIFFERENT widgets never share a
// cell regardless of extras, and for the SAME widget the inline maps are fixed
// by its CR (cannot vary request-to-request). The union therefore only needs
// to discriminate the per-request variable (request extras) and the
// per-widget-fixed inline contribution — which a flat superset does
// faithfully (design §1 collision-soundness note).
//
// SCOPING: this union keys the WIDGET cell only. The apiRef sub-cell
// (RAFullList) is keyed separately by the apiRef-EFFECTIVE map
// (merge(apiRefInline, request)) threaded through apiref.Resolve's Extras —
// the rrt-inline map MUST NOT enter the apiRef sub-cell (it does not affect
// the apiRef fetch). See widgets.go's resolveApiRef + ra_full_list.go.
//
// All inputs are read off the widget CR via maps.NestedMap (deep copies) or
// are the per-request extras; the result is a fresh map and never aliases any
// of them. nil/empty all-round ⇒ a fresh empty map ⇒ ComputeKey's len>0 guard
// skips the extras fold ⇒ the key is byte-identical to a no-extras request
// (backward-compat, falsifier #8). Mechanism-uniform (feedback_no_special_cases):
// every key folds the same way; no widget-name table, no key allowlist.
func unionForKey(apiRefInline, rrtInline, request map[string]any) map[string]any {
	out := make(map[string]any, len(apiRefInline)+len(rrtInline)+len(request))
	// Inline maps first; request overwrites last so the request value wins on
	// collision — matching the resolved body precedence. (Order between the two
	// inline maps is irrelevant for a key fingerprint: both are CR-fixed, so
	// the body is a deterministic function of the CR regardless.)
	for k, v := range apiRefInline {
		out[k] = v
	}
	for k, v := range rrtInline {
		out[k] = v
	}
	for k, v := range request {
		out[k] = v
	}
	return out
}

// filterDeclaredKeyExtras is the F6 request-extras allowlist
// (docs/f6-chrome-route-key-design-2026-07-12.md §4 Option A-declare). It returns
// the subset of the per-request extras whose keys the widget author DECLARED in
// spec.keyExtras (widgets.GetKeyExtras) — the only request extras allowed to
// PARTITION the cache key. An undeclared / empty spec.keyExtras yields the empty
// map ⇒ NO request extra folds into the key (the chrome-widget fold-nothing
// default: route params {namespace,name} no longer partition the cell, so one
// seeded cell serves every route — closing the #130 F6 first-nav miss).
//
// This is the KEY-SIDE analogue of the A2 identity contract: DeclaredIdentity
// picks which principal keys fold; this picks which request-extras keys fold. It
// runs INSIDE the single shared effectiveKeyExtras (the only site request extras
// meet the key), so the filter cannot drift across the four key consumers.
//
// It touches ONLY key derivation — the caller still hands the RAW, unfiltered
// request extras to widgets.Resolve's jq dict (widgets.go), so a widget that reads
// extras.namespace in its jq WITHOUT declaring it still RESOLVES correctly; it
// merely stops partitioning the cache (the design's deliberate SPURIOUS-over-keying
// removal, with the RED-arm audit proving a widget that genuinely varies on a
// param MUST declare it or serve a wrong shared body).
//
// The result is a fresh map (never aliases requestExtras); nil/empty request or
// nil/empty declaration ⇒ a fresh empty map ⇒ unionForKey's fold degenerates to
// the inline maps only ⇒ ComputeKey's len>0 guard skips the extras fold on an
// all-empty result (backward-compat). Mechanism-uniform (feedback_no_special_cases):
// no widget-name / route / GVR table — every widget folds exactly what it declares.
func filterDeclaredKeyExtras(cr map[string]any, requestExtras map[string]any) map[string]any {
	declared := widgets.GetKeyExtras(cr)
	if len(declared) == 0 || len(requestExtras) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(declared))
	for _, k := range declared {
		if v, ok := requestExtras[k]; ok {
			out[k] = v
		}
	}
	return out
}

// requestExtrasFullyDeclared is the F6 self-quarantine predicate (arch-ruled
// 2026-07-13, docs/f6-chrome-route-key-design-2026-07-12.md §4). It reports
// whether EVERY per-request extra key the client supplied is DECLARED in the
// widget's spec.keyExtras — i.e. filterDeclaredKeyExtras dropped NOTHING.
//
// WHY IT EXISTS (the leak F6 would otherwise open). Dropping undeclared request
// extras from the KEY removes the pre-F6 self-quarantine: two users A,B in the
// SAME BindingUID cohort, an UNDECLARED widget whose OWN widgetDataTemplate reads
// extras.foo. A sends ?extras={"foo":"evil"}; F6 drops foo → A's effective key
// extras == {} == B's → IDENTICAL per-cohort ComputeKey. But A's RAW extras still
// reach widgets.Resolve, so A's rendered body embeds foo=evil. Pre-F6 the foo
// fold partitioned A's cell away from B's; post-F6 A's evil body would be Put into
// the SHARED cohort cell (widgets.go genuine-Put) and B would hit it. The guard
// makes the polluting request DECLINE that Put: A still gets its correct
// per-request 200, but never writes the shared cell, so B misses and resolves
// clean. Clean requests (no undeclared extras) and the declared corpus pay
// nothing — only a request carrying extras the widget did NOT declare is quarantined.
//
// IDENTITY-DIMENSION EXEMPTION — WHY IT IS SAFE (rewritten for A-3, 1.12.3).
//
// Identity keys (username / groups / displayName) are EXEMPT from this
// quarantine. The 1.7.11 fix introduced the exemption for the right practical
// reason and the wrong stated one, so here is the actual justification:
//
//	An identity key can only reach the resolve dict AT ALL if the widget
//	DECLARES it — widgets.Resolve strips every undeclared identity key from
//	opts.Extras before mergeExtras (sanitizeUndeclaredIdentityExtras,
//	internal/resolvers/widgets/resolve.go). And a DECLARED one cannot pollute a
//	shared cell either: declared in spec.identityContext means DeclaredIdentity
//	overwrites it with the JWT's own value server-side, so it is not
//	client-controlled; declared in spec.keyExtras means it PARTITIONS the key, so
//	a spoofed value self-quarantines into a cell only that value can reach.
//
// So the exemption is now true BY CONSTRUCTION rather than by assertion. The
// 1.7.11 comment claimed identity keys are "never a shared-cell body-pollution
// risk" because the cell is BindingUID-keyed; that argued about the KEY
// dimension and was simply false about the RESOLVE-INPUT dimension, which is the
// A-3 defect. Do not restore that reasoning — restore the sanitiser if it ever
// goes missing.
//
// WHAT THE 1.7.11 FIX WAS PROTECTING, and still is. The frontend's
// buildExtrasParam sends identity keys (username, displayName) on the wire
// whenever SNOWPLOW_IDENTITY_INJECTION is off. Without the exemption a declared
// widget carrying them was judged "undeclared" and DECLINED every Put, so its
// content was never cached and every revisit MISSed — the 14/14-same-key,
// 0/14-hit west4 failure. Tightening this predicate to demand an
// identityContext declaration reintroduces exactly that, and unfixably, because
// GetIdentityContext is enum-filtered to {username, groups} so displayName can
// never be declared. TestFARCH_F6_7_RevisitHit_IdentityExtrasExempt is the
// regression guard; do not "fix" it by changing the test.
//
// The quarantine still fires for a genuinely-undeclared, BODY-affecting
// NON-identity request key (the F6-6 `foo` case) — that is the only shared-cell
// pollution this guard exists to stop, and the only one still reachable.
//
// identityDimensionKeys is the closed set the frontend can put on the wire as
// identity (audit §1: buildExtrasParam sends username + displayName when not
// injecting) plus groups (the other A2 identityContext enum value). Not a
// widget-name/route table (feedback_no_special_cases) — a fixed, mechanism-level
// identity vocabulary, the same axis DeclaredIdentity honors. It is duplicated
// (not shared) with the twin in internal/resolvers/widgets/resolve.go: the two
// packages must not import each other, and both are pinned to the same three
// keys by TestA3_IdentityVocabularyIsInSync.
var identityDimensionKeys = map[string]struct{}{
	"username":    {},
	"groups":      {},
	"displayName": {},
}

// Returns TRUE (fully declared → safe to Put) when the request carries no extras,
// or when every supplied key is EITHER declared in spec.keyExtras OR is an
// identity-dimension key. Returns FALSE (decline the Put) when at least one
// supplied key is a genuinely-undeclared, non-identity request key. Reuses
// widgets.GetKeyExtras — no new mechanism, no widget-name/route table.
func requestExtrasFullyDeclared(cr map[string]any, requestExtras map[string]any) bool {
	if len(requestExtras) == 0 {
		return true // nothing supplied → nothing to quarantine
	}
	declared := widgets.GetKeyExtras(cr)
	declaredSet := make(map[string]struct{}, len(declared))
	for _, k := range declared {
		declaredSet[k] = struct{}{}
	}
	for k := range requestExtras {
		if _, ok := declaredSet[k]; ok {
			continue // author-declared keyExtras key — partitions the key, safe
		}
		if _, ok := identityDimensionKeys[k]; ok {
			continue // A2/A6 identity axis — folder handles it, never shared-cell pollution
		}
		return false // a genuinely-undeclared body-affecting request extra → quarantine
	}
	return true
}

// effectiveKeyExtras is the SINGLE shared derivation of the "effective key
// extras" the definitive cache-identity architecture (§2.2,
// docs/definitive-cache-identity-architecture-2026-07-07.md) mandates for ALL
// FOUR key-derivation consumers — dispatch lookup (widgets.go), widgetContent
// lookup (widgets.go), subscription arming (refresh_subscription.go), and the
// boot/keepwarm seed (phase1_pip_seed.go). Extracting it here is the A1
// head-start: a pure behavior-preserving refactor that gives every consumer ONE
// place to fold extras, so the A2 identity-injection lands at a single site and
// cannot drift across the four consumers (the #64 shadow-drift lesson, applied
// preemptively — same principle as the normalizePagination extraction).
//
// TODAY (A1) it returns EXACTLY what the four sites computed inline before:
//
//	unionForKey(GetApiRefExtras(cr), GetResourcesRefsExtras(cr), requestExtras)
//
// ⊎ declaredIdentityForKey(ctx, cr), which is INERT in A1 (returns nil → the
// union is returned unchanged → byte-identical ResolvedKeyInputs at every site).
// A2 wires declaredIdentityForKey to read spec.identityContext + materialise the
// declared subset of xcontext.UserInfo(ctx) with injection-wins precedence; the
// merge slot is present now so A2 is a one-function change with no new call-site
// plumbing. The ctx parameter is accepted now (unused in A1) for the same
// reason — A2 needs it, and threading it now keeps A1↔A2 signature-stable.
//
// cr is the widget CR's unstructured object map (the accessors deep-copy, so the
// result never aliases the shared CR); nil-safe (both accessors return {} on a
// nil/mismatched map → union degenerates to requestExtras → ComputeKey's len>0
// guard skips the extras fold on an all-empty result — the no-extras backward-
// compat path).
func effectiveKeyExtras(ctx context.Context, cr map[string]any, requestExtras map[string]any) map[string]any {
	base := unionForKey(
		widgets.GetApiRefExtras(cr),
		widgets.GetResourcesRefsExtras(cr),
		// F6 (docs/f6-chrome-route-key-design-2026-07-12.md §4 Option A-declare):
		// filter the per-REQUEST extras to ONLY the keys the widget author declared
		// in spec.keyExtras BEFORE the union folds them into the key. Absent/empty
		// declaration ⇒ fold NOTHING (the chrome-widget default — route params like
		// {namespace,name} stop partitioning the cell, so one seeded cell serves all
		// routes). The inline maps above (apiRef.extras + resourcesRefsTemplateExtras)
		// are CR-FIXED, not request extras — they are NOT filtered (they cannot vary
		// request-to-request, and dropping them would change the resolved body's key).
		// KEY-ONLY: undeclared request extras still reach widgets.Resolve's jq dict
		// unchanged (widgets.go passes the RAW extras to the resolver; only this KEY
		// derivation filters). Single-site by construction — the same filtered union
		// feeds all four key consumers (dispatch, widgetContent, subscription, seed),
		// so parity holds (the A1 anti-drift guarantee).
		filterDeclaredKeyExtras(cr, requestExtras),
	)
	// A2 identity injection (§2.2): server-declared identity wins on collision.
	// declaredIdentityForKey → widgets.DeclaredIdentity reads the parent CR's OWN
	// spec.identityContext; nil for an undeclared widget → no-op → byte-identical
	// to pre-A2 for the identity-free corpus.
	for k, v := range declaredIdentityForKey(ctx, cr) {
		base[k] = v
	}
	// A2 inline-variance union (§4.3, F-ARCH-5) — arch ruling option (d): an
	// inline-embedding parent (hasInlineGETRef) embeds children rendered UNDER the
	// requesting user's identity (widgets_inline.go), so its cell must be per-USER
	// even if the PARENT declares no identityContext and even if the embedded
	// child is a template-expanded ref not enumerable pre-resolve. Fold the FULL
	// request identity ({username, groups}) into the KEY as a conservative marker
	// = treat the inline parent as effectively declaring identityContext:[username,
	// groups]. This is a SUPERSET of any child's declared identity (own ∪ children
	// ⊆ full), so the effective-union rule is satisfied by over-approximation with
	// ZERO child-fetch on the hot key path (O(1), C-INLINE-2 triviality) and NO
	// template-surface hole. KEY-ONLY: it does NOT enter the resolve input (the
	// parent's own jq gets only its declared identity via widgets.Resolve) — the
	// child render varies by identity through ResolveNestedCall's per-user ctx.
	// INERT for the ~99% corpus: inline is opt-in + default-off, and a non-inline
	// widget returns nil here → byte-identical to pre-A2.
	for k, v := range inlineParentIdentityForKey(ctx, cr) {
		base[k] = v
	}
	return base
}

// declaredIdentityForKey is the KEY-side injection point for the A2 contract
// (definitive-cache-identity-architecture §2.2). It delegates to the SINGLE
// shared derivation widgets.DeclaredIdentity(ctx, cr) — the SAME function the
// resolve-input path calls (widgets.Resolve) — so a declared widget's key fold
// and its rendered body see byte-identical identity material and cannot desync
// (the #64 anti-drift principle at the identity dimension). Returns nil for an
// undeclared widget or an identity-less ctx → the effectiveKeyExtras merge is a
// no-op → byte-identical to pre-A2 for the ~99% identity-free corpus (the
// prod-inert acceptance). GATE AUTHN-1: the only identity source is
// xcontext.UserInfo (inside DeclaredIdentity) — zero store reads.
func declaredIdentityForKey(ctx context.Context, cr map[string]any) map[string]any {
	return widgets.DeclaredIdentity(ctx, cr)
}

// inlineParentIdentityForKey is the KEY-side inline-variance marker (A2 §4.3,
// F-ARCH-5, arch ruling option (d)). It returns the FULL request identity
// ({username, groups} from xcontext.UserInfo) as key extras iff the widget CR is
// an inline-embedding parent (hasInlineGETRef) — making the parent cell per-USER
// so an embedded child's identity-varying rendered body cannot leak across users
// sharing one binding. Conservative by construction: the full identity is a
// superset of any child's declared identityContext, so it closes the leak for
// BOTH static AND template-expanded inline children WITHOUT fetching any child CR
// at key time (O(1), no hot-path fan-out). Returns nil for a non-inline widget
// (byte-identical to pre-A2) or an identity-less ctx (fail-safe: the request
// already fail-closes to the ""-BindingUID MISS path in dispatchCacheLookupKey).
// KEY-ONLY — never injected into the resolve input.
func inlineParentIdentityForKey(ctx context.Context, cr map[string]any) map[string]any {
	if !hasInlineGETRef(cr) {
		return nil
	}
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return nil
	}
	out := map[string]any{}
	if ui.Username != "" {
		out["username"] = ui.Username
	}
	if len(ui.Groups) > 0 {
		// JSON-native []any (NOT []string) — key-only here, so this doesn't hit
		// the resolve-input DeepCopyJSON panic today, but mirror DeclaredIdentity
		// so ALL identity-extras are JSON-native by construction (shape
		// uniformity, feedback_no_special_cases). Key-parity byte-identical:
		// json.Marshal treats []string and []any identically.
		g := make([]any, len(ui.Groups))
		for i, v := range ui.Groups {
			g[i] = v
		}
		out["groups"] = g
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cacheHandle is the narrow interface the dispatchers depend on. The
// real implementation is *cache.resolvedCache; tests can substitute
// stubs without dragging in the whole singleton. Kept package-private
// so it never leaks beyond dispatchers.
type cacheHandle interface {
	Get(key string) (*cache.ResolvedEntry, bool)
	Put(key string, entry *cache.ResolvedEntry)
}

// emitResolvedCacheLookup writes the per-request falsifier line per
// plan §"Code-path falsifier":
//
//	resolved_cache.lookup hit=true|false key_hash=... resident_bytes=N
//
// We log at INFO so a casual grep on production logs proves whether
// L1 is firing.
//
// Ship OBS-1 (0.30.186): in addition to the log line, bump the
// per-(handlerKind, gvrString) hit/miss counter in
// l1_lookup_metrics.go so /debug/vars exposes the same signal
// without requiring log-line scraping. The bump is one atomic add
// per call — already on the request-path serial budget.
// #130 F3 seed-attribution: seededAtBoot is the hit entry's
// ResolvedEntry.SeededAtBoot (meaningful only on a hit; pass false on a miss).
// It drives the hit_source tag ("seed" | "traffic") in the lookup log and the
// hits_seed_attributable expvar — leak-safe boolean provenance, no per-user data.
func emitResolvedCacheLookup(log *slog.Logger, handlerKind, gvrString, key string, hit, seededAtBoot bool, residentBytes int) {
	if log != nil {
		// hit_source is only meaningful on a hit; on a miss it is "" so a
		// grep for hit_source:"seed" counts exactly the seed-attributable hits.
		hitSource := ""
		if hit {
			if seededAtBoot {
				hitSource = "seed"
			} else {
				hitSource = "traffic"
			}
		}
		log.Info("resolved_cache.lookup",
			slog.String("subsystem", "cache"),
			slog.String("handler", handlerKind),
			slog.String("gvr", gvrString),
			slog.String("key_hash", key),
			slog.Bool("hit", hit),
			slog.String("hit_source", hitSource),
			slog.Int("resident_bytes", residentBytes),
		)
	}
	recordL1Lookup(handlerKind, gvrString, hit, seededAtBoot)
}

// emitDispatchCacheKeyDiag — Ship 0.30.188 — pure-additive diagnostic
// instrumentation for the PIP-seed vs dispatcher-get key-divergence
// investigation (architect's TRACED diff on `BindingSetHash`). Emits a
// structured slog line carrying ALL the components that fold into
// `dispatchCacheLookupKey`'s ComputeKey, so the three emit sites (PIP
// seed Put, dispatcher Get, per-user fallback Put) can be diff'd line-
// by-line for ONE widget/restaction to identify which field(s) drive
// the seed→serve miss.
//
// IDENTITY EXTRACTION: pulls Username + Groups from xcontext.UserInfo
// (the cohort ctx for seed sites, the request ctx for dispatcher
// sites). On missing/unparseable identity (defence in depth — should
// never happen at these sites since they all already nil-checked the
// handle) we emit a sentinel "anonymous" / empty groups so the log
// still differentiates the divergent component.
//
// BindingSetHash is read from `inputs.BindingSetHash` — the SAME value
// that ComputeKey folded into the returned cacheKey — rather than re-
// computing it. This guarantees the logged hash and the cache key are
// derived from the same snapshot read.
//
// NIL-GUARD: a nil `inputs` (cache disabled or identity missing) is
// permitted; the function still emits with binding_uid="" so the
// "why was the lookup skipped" question is greppable. The dispatcher
// callers MUST still nil-check the handle separately.
//
// COST: one slog.Info per /call per site.
//
// Ship 0.30.242 H.c-layered Phase 2b — the diagnostic field renamed
// from `binding_set_hash uint64` to `binding_uid string` (the new
// cell-key identity dimension). Same diagnostic intent (find divergent
// key components between PIP-seed and dispatcher-get sites); new field
// shape carries the per-layer first-match binding identity instead of
// the v3 cohort hash.
func emitDispatchCacheKeyDiag(log *slog.Logger, site string, ctx context.Context,
	cacheKey string, inputs *cache.ResolvedKeyInputs,
	handlerKind, group, version, resource, namespace, name string,
	perPage, page int, extras map[string]any,
) {
	if log == nil {
		return
	}
	var (
		username string
		groups   []string
	)
	if ui, err := xcontext.UserInfo(ctx); err == nil {
		username = ui.Username
		groups = ui.Groups
	}
	var bindingUID string
	if inputs != nil {
		bindingUID = inputs.BindingUID
	} else {
		// Compute directly so the field still differentiates — the
		// cache-disabled / no-identity branch returns nil inputs, but
		// the diagnostic is interested in the per-layer BindingUID
		// value itself. Path B direct call (Phase 2b R3 deferral).
		_, bindingUID, _ = rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username:  username,
			Groups:    groups,
			Verb:      "get",
			Group:     group,
			Resource:  resource,
			Namespace: namespace,
			Name:      name,
		})
	}
	log.Info("dispatch.cache_key.computed",
		slog.String("subsystem", "cache"),
		slog.String("site", site),
		slog.String("key_hash", cacheKey),
		slog.String("binding_uid", bindingUID),
		slog.String("username", username),
		slog.Any("groups", groups),
		slog.String("handler_kind", handlerKind),
		slog.String("gvr", fmt.Sprintf("%s/%s, Resource=%s", group, version, resource)),
		slog.String("namespace", namespace),
		slog.String("name", name),
		slog.Int("per_page", perPage),
		slog.Int("page", page),
		slog.Int("extras_len", len(extras)),
	)

	// Ship 0.30.190 Fix B / 0.30.191 carried-forward — additive
	// single-line stderr lane. Same intent as the pre-ship lane; field
	// renamed from bsh (binding-set-hash uint64) to buid (binding-UID
	// string).
	if env.True("DISPATCH_KEY_DIAG_ENABLED") {
		fmt.Fprintf(os.Stderr,
			"DISPATCH_KEY_DIAG site=%s key=%s buid=%s user=%s groups=%v handler=%s gvr=%s/%s/%s ns=%s name=%s pp=%d p=%d xlen=%d\n",
			site, cacheKey, bindingUID, username, groups,
			handlerKind, group, version, resource, namespace, name, perPage, page, len(extras),
		)
	}
}

// encodeResolvedJSON marshals res with a single canonical encoder shape.
//
// Ship GMC / 0.30.174 — dropped SetIndent("", "  ") for a ~25% wire-
// size reduction at admin scale (the indented LIST envelopes were
// dominated by per-line spaces). The cache-hit + cache-miss paths
// remain byte-equal to each other (the contract here is internal
// consistency); the frontend re-parses JSON regardless of indentation
// so the change is transparent above HTTP.
//
// Centralising the encode here ensures every dispatch site returns
// the same shape; any divergence would break the "cache=on warm
// response equals cache=off response" invariant.
func encodeResolvedJSON(res any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// refreshKeyHeader is the response header carrying the L1 subscription key
// for the live-refresh signal (Ship 1). The frontend reads it to arm a
// /refreshes subscription for this widget (design §6). Additive and invisible
// to the JSON body contract; it MUST be in CORS ExposedHeaders (main.go) so a
// cross-origin fetch/react-query layer can read it.
const refreshKeyHeader = "X-Snowplow-Refresh-Key"

// refreshClassHeader carries the CacheEntryClass the response was keyed
// under. Paired with refreshKeyHeader so the frontend arms the EXACT
// /refreshes subscription class instead of guessing widgets-vs-widgetContent
// (frontend guide §2.5/§8). The value matches SubscriptionCoordinates.Class
// exactly: "widgets" | "widgetContent" | "restactions" | "apistage" |
// "raFullList". Additive; MUST be in CORS ExposedHeaders (main.go).
const refreshClassHeader = "X-Snowplow-Refresh-Class"

// setRefreshKeyHeader stamps the live-refresh subscription key + the class it
// was keyed under, before WriteHeader. No-op on an empty key (L1 disabled /
// RBAC-skipped / cache-off) — the headers are purely additive and must never
// appear empty. class is the CacheEntryClass the dispatcher keyed this
// response by (the SAME class the caller passes to emitResolvedCacheLookup);
// it is stamped only alongside a non-empty key so the two headers are always
// consistent. MUST be called before writeResolvedJSON (which calls
// WriteHeader).
func setRefreshKeyHeader(wri http.ResponseWriter, key, class string) {
	if key == "" {
		return
	}
	wri.Header().Set(refreshKeyHeader, key)
	if class != "" {
		wri.Header().Set(refreshClassHeader, class)
	}
}

// setRefreshKeyHeaderUnlessExternal is the external-widget bounded-TTL cache
// (Option A, 2026-07-10) arming-kill choke point. It stamps the refresh-key
// header EXACTLY like setRefreshKeyHeader, EXCEPT when externalTTL is true, in
// which case it stamps NOTHING — equivalent to passing an empty key.
//
// Why: an external-TTL entry has NO dep edges and never publishes (§6.3 of the
// design), so arming a /refreshes subscription for its key is a wasted
// subscription that can never fire. The subscription is armed browser-side on
// seeing X-Snowplow-Refresh-Key; suppressing the header at the single serve
// choke point (both the HIT branch and the cold Put tail) is the minimal kill
// — it reuses setRefreshKeyHeader's existing key=="" no-op rather than adding a
// class-skip that the subscription path would have to learn to ignore.
//
// externalTTL=false → byte-identical to setRefreshKeyHeader (C4). This is the
// only difference from the plain helper, so a NON-external serve is unchanged.
func setRefreshKeyHeaderUnlessExternal(wri http.ResponseWriter, key, class string, externalTTL bool) {
	if externalTTL {
		return
	}
	setRefreshKeyHeader(wri, key, class)
}

// writeResolvedJSON writes the canonical Content-Type + 200 + payload.
// We deliberately do NOT log here on errors writing to the wire — a
// client disconnect mid-write is normal and not actionable.
//
// MEMORY CEILING (#99, ruled CLOSE-DOCUMENTED 2026-06-12): payload is
// held fully in heap before this single Write. The largest payload today
// is the admin compositions-list at ~12.9 MB (49K compositions;
// transient, 0.05 mix-weight). Bounded and harmless at current traffic.
// Do NOT convert to chunked/http.ResponseController streaming: naive
// streaming on large payloads caused a 22-38x warm-latency regression
// (0.30.176, feedback_no_naive_compression_middleware) and there is no
// streaming-write prior art in this package. If a payload ever exceeds a
// few tens of MB, cache PRE-ENCODED bytes at the value layer rather than
// streaming the write.
func writeResolvedJSON(wri http.ResponseWriter, payload []byte) {
	wri.Header().Set("Content-Type", "application/json")
	wri.WriteHeader(http.StatusOK)
	_, _ = wri.Write(payload)
}

// snowplowSACtx — Ship 0.30.166 / #307 AMEND. Returns the snowplow
// ServiceAccount endpoint + *rest.Config the dispatcher attaches to every
// per-request context so the api-stage K8s GET/LIST dispatch can engage
// dispatchViaInternalRESTConfig (client-go transport that correctly
// installs the cluster CA) instead of falling through to plumbing's
// httpcall.Do (whose tlsConfigFor drops the CA for token-auth endpoints
// — the 0.30.103 / 0.30.165 x509 defect).
//
// IDENTICAL MECHANISM to the prior-art sites that already use it:
//   - Phase 1 walker: phase1_walk.go:231 attaches the same SA pair to the
//     SA-credentialed startup walk context (the 0.30.104 fix surface).
//   - L1 refresher: resolve_populate.go:117-131 attaches the same SA pair
//     to the background re-resolve context (the 0.30.113 Part B fix
//     surface, where the SA is transport-only and per-user identity comes
//     from the cached Inputs).
//
// 0.30.166 extends the SAME attach to the per-request restactions.go +
// widgets.go dispatcher entries — the previously-unpatched surface that
// is the actual cache-off /call request path. See ship-307-tls-x509-cache-
// off-design-amend-2026-05-22.md §2.
//
// GRACEFUL DEGRADATION (AC-307.7): out-of-cluster unit tests have no
// projected SA volume and no KUBERNETES_SERVICE_HOST env — both
// idynamic.ServiceAccountEndpoint() and idynamic.ServiceAccountRESTConfig()
// error. The helper swallows the error and returns (nil, nil). The
// dispatcher caller's nil-guarded attach (`if saEP != nil && saRC != nil`)
// then SKIPS the WithInternalEndpoint / WithInternalRESTConfig calls, so
// the request ctx is byte-identical to pre-0.30.166 and the request flows
// through the unchanged httpcall.Do path. Every unit test in the tree is
// preserved verbatim.
//
// CONCURRENCY: idynamic.ServiceAccountEndpoint memoises a process-wide
// singleton under its own mutex; the helper itself is stateless and safe
// for concurrent callers (every per-request dispatch is a fresh goroutine).
//
// LOG VISIBILITY: by design the helper is silent on the per-request
// happy path — repeated nil-warns would flood the log. The startup-time
// warn already exists at dispatchers.go:58-66 (RegisterRefreshHandlers)
// where the SAME SA pair is fetched once for the refresher; a missing
// SA there is surfaced once and is sufficient for diagnosis.
func snowplowSACtx() (*endpoints.Endpoint, *rest.Config) {
	saEP, err := idynamic.ServiceAccountEndpoint()
	if err != nil {
		return nil, nil
	}
	saRC, err := idynamic.ServiceAccountRESTConfig()
	if err != nil {
		return nil, nil
	}
	return saEP, saRC
}

// rcFromCtx returns the SA *rest.Config that an internal driver
// (Phase 1 walker, L1 refresher, PIP seed, content prewarm) attached
// upstream via cache.WithInternalRESTConfig. Returns nil when no internal
// rc is on ctx (the per-user request path) OR when the attached value is
// of the wrong type.
//
// Ship 0.30.230 fix-at-root: ResolveOptions{RC,SArc} construction sites
// downstream of an internal driver use this helper to thread the SA rc
// at the construction site, so cache.GVRFor / dynamic.NewClient /
// crdschema.ValidateObjectStatus never receive a nil rc. Per-user
// dispatchers (widgets.go, restactions.go) pass r.saRC directly from
// their handler struct field — they do not need this helper.
func rcFromCtx(ctx context.Context) *rest.Config {
	v, ok := cache.InternalRESTConfigFromContext(ctx)
	if !ok {
		return nil
	}
	rc, _ := v.(*rest.Config)
	return rc
}
