// Package apiref resolves a widget's apiRef: it fetches the referenced
// RESTAction object and resolves it (through the restactions resolver),
// returning the resulting data dictionary for the widget to consume.
package apiref

import (
	"context"
	"fmt"
	"log/slog"

	xcontext "github.com/krateo-platformops/plumbing/context"
	pmaps "github.com/krateo-platformops/plumbing/maps"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	"k8s.io/client-go/rest"
)

type ResolveOptions struct {
	RC      *rest.Config
	ApiRef  templatesv1.ObjectReference
	AuthnNS string
	PerPage int
	Page    int
	Extras  map[string]any
}

// shouldServeRAFullList is the SINGLE gate for the Ship-4a page-independent
// RAFullList serve at the apiRef chokepoint (single-derivation site so the
// decision is stated + tested once). It is true iff ALL hold:
//
//   - the request is PAGINATED (perPage>0 && page>0) — Ship 4a serves only a
//     bounded page window from the shared full list; an unpaginated (0,0)
//     resolve IS the first-sight that populates it and must not re-enter here;
//   - the cache is ON (cache.ResolvedCacheEnabled()) — flag-off (CACHE_ENABLED
//     =false) this is byte-identical to pre-4a (raFullListServe would decline
//     anyway; short-circuiting here keeps the pre-4a path clean);
//   - this resolve is NOT the boot prewarm DISCOVERY WALK.
//
// #42 Option-2 (boot-seed cold-dashboard enabler) — the last conjunct. The
// discovery walk stamps ctx cache.WithFallthroughScope(ScopeBootPrewarmWalk)
// (phase1_walk.go withPhase1SAContext — a plain context value that propagates
// UNCHANGED into this nested apiRef resolve; the resolver packages never
// re-stamp it). Ship-4a's
// first-sight resolves the RA UNPAGINATED (resolveRA(fullCtx,0,0) at
// ra_full_list.go:347) to establish the byte-verify sliceability verdict — which,
// for the 60K-composition compositions-panel RA, is a whole-GVR materialization
// (~22-28 s each, 4× per re-walk = ~411 s, blowing the PHASE1_TIMEOUT budget so
// the per-cohort first-nav seed never starts in budget → the first-nav latch
// never fires → per-cohort dashboard cells cold for the ~7 min backstop window).
// The nav-STRUCTURE harvest needs ONLY the widget's bounded page-1
// resourcesRefs.items[] (child nav endpoints), NOT composition DATA, and has no
// use for a serve-time page-independent slice cache. Excluding it here falls
// straight through to the bounded page-keyed resolveRA(ctx, PerPage, Page) in
// Resolve (perPage=prewarmPageLimit()=5, page=1) — the composition GVR is LISTed
// bounded/informer-served (the #121 1a branch), never a 60K materialization.
//
// SCOPE-PRECISE, NOT a resource/scale special-case (feedback_no_special_cases):
// the exclusion reads the EXISTING generic discovery-walk scope marker, so ANY
// GVR resolved by the discovery walk skips 4a first-sight (no benchapps/50K
// literal). It is deliberately NARROWER than BackgroundResolveFromContext: a
// per-user (foreground) /call never carries this scope AND the REFRESHER
// (resolveOnceProd → WithBackgroundResolve, resolve_populate.go:367) is NOT
// stamped ScopeBootPrewarmWalk, so the refresher's serve-time full-list re-pin is
// UNTOUCHED — only the discovery walk (which has no use for the slice cache) is
// suppressed. The serve-time slice cache is populated by the first real
// foreground /call, exactly as before this change.
// IsPaginatedResolve is the SINGLE pagination predicate that gates the Ship-4a
// full-list machinery: 4a serves a bounded page window from a shared full list,
// so it is meaningful ONLY for a paginated resolve (perPage>0 && page>0). An
// unpaginated resolve (0,0 / -1,-1) consumes the whole list wholesale (e.g. a
// Statistic/Tag widget counting `list:`) and gains nothing from the slice cache.
//
// #130 F4 Option 2 reuses this exact predicate at the seed to SKIP
// seedRAFullListForWidget for an unpaginated-consuming widget — the second full
// ~18s benchapps materialization that only ever pinned a slice cache the
// widget's own unpaginated /call would never engage. Data-derived (reads
// pagination only, never widget-kind — feedback_no_special_cases). Exported so
// the seed and the serve gate share ONE derivation and cannot drift.
func IsPaginatedResolve(perPage, page int) bool {
	return perPage > 0 && page > 0
}

// bumpUAFSinkIfDeclared marks the enclosing resolve as userAccessFilter-narrowed
// when the apiRef'd RESTAction declares a UAF stage. Extracted as a named
// function (rather than inlined at the one call site) so the A-1/R-1 falsifier
// can drive the REAL predicate-to-bump wiring with a real sink on ctx, without
// standing up objects.Get and a live RA fetch just to observe one bump.
//
// nil ra and a ctx with no sink are both no-ops.
func bumpUAFSinkIfDeclared(ctx context.Context, ra *templatesv1.RESTAction) {
	if ra.HasUserAccessFilterStage() {
		cache.BumpUAFTouched(ctx)
	}
}

func shouldServeRAFullList(ctx context.Context, perPage, page int) bool {
	if !IsPaginatedResolve(perPage, page) || !cache.ResolvedCacheEnabled() {
		return false
	}
	if fs := cache.FallthroughScope(ctx); fs != nil && fs.Path == cache.ScopeBootPrewarmWalk {
		return false
	}
	return true
}

func Resolve(ctx context.Context, opts ResolveOptions) (map[string]any, error) {
	if opts.ApiRef.Name == "" || opts.ApiRef.Namespace == "" {
		return map[string]any{}, nil
	}

	res := objects.Get(ctx, opts.ApiRef)
	if res.Err != nil {
		// Task #272 / 0.30.251 — error-type preservation. Pre-fix,
		// the apiref boundary stripped the upstream apiserver status
		// code with `fmt.Errorf("%s", res.Err.Message)`. The downstream
		// dispatcher's `errors.As(err, *apierrors.StatusError)` check
		// (widgets.go:228-234) then failed and ALL apiRef-resolve
		// errors landed in `response.InternalError` → HTTP 500,
		// regardless of the apiserver's actual response code.
		//
		// Architect trace task-262-s8-cj-tablist-trace-2026-06-09.md
		// §3.3 documents the symptom: a cj `restactions:get` 403 from
		// the apiserver became an HTTP 500 on the SPA wire, so the
		// frontend could not distinguish "you lack permission" from
		// "snowplow exploded" and rendered .ant-result-error.
		//
		// Fix: reconstruct an `*apierrors.StatusError` from the code
		// already preserved in `res.Err` (objects.Get's apiserver
		// branch faithfully sets res.Err.Code per apierrors.IsForbidden
		// / IsNotFound — see internal/objects/get.go:209-214), then
		// wrap with `%w` so the dispatcher can recover the code via
		// errors.As. The wrapped chain also preserves the upstream
		// message + adds a `apiref resolve <group>/<resource>/<name>`
		// context prefix for log-side observability.
		statusErr := statusErrorFromResponse(res.Err, opts.ApiRef)
		wrapped := fmt.Errorf("apiref resolve %s/%s/%s: %w",
			res.GVR.Group, res.GVR.Resource, opts.ApiRef.Name, statusErr)
		// Falsifier slog WARN: the runtime artifact tester / observer
		// uses to verify the StatusError chain is preserved. Single
		// emission per apiref error — no per-request fan-out.
		if log := xcontext.Logger(ctx); log != nil {
			log.Warn("apiref.resolve.error_preserved",
				slog.Int("upstream_code", res.Err.Code),
				slog.String("upstream_reason", string(res.Err.Reason)),
				slog.String("gvr_group", res.GVR.Group),
				slog.String("gvr_resource", res.GVR.Resource),
				slog.String("name", opts.ApiRef.Name),
				slog.String("namespace", opts.ApiRef.Namespace),
			)
		}
		return map[string]any{}, wrapped
	}

	ra, err := convertToRESTAction(res.Unstructured.Object)
	if res.Err != nil {
		return map[string]any{}, err
	}

	// 1.12.3 A-1 / R-1 (SECURITY, cross-tenant) — THE PRODUCTION BUMP FOR THE
	// WIDGET CARRIER, and this is the frame where it has to happen.
	//
	// The widgets/widgetContent Put sites gate on cache.UAFTouchedSink, but they
	// cannot detect a userAccessFilter themselves: a widget CR declares none, and
	// the RA that does sits several resolver frames below them. THIS function is
	// the apiRef chokepoint — every widget→RESTAction read funnels through it, and
	// it is the FIRST frame holding the typed RA, so it is the first place the
	// declaration is visible at all. Bumping here makes the sink non-empty for the
	// whole enclosing widget resolve, so the widget's Put declines.
	//
	// WHY THE DECLARATION AND NOT THE REFILTER ITSELF: the refilter's own bump
	// (cache.BumpUAFTouched at the top of applyUserAccessFilterOnPig,
	// internal/resolvers/restactions/api/refilter.go) is owned by another dev and
	// lands separately. The two are COMPLEMENTARY, not duplicates, and both are
	// wanted:
	//   - THIS bump is declaration-based and fires whenever an apiRef'd RA
	//     DECLARES a UAF stage — even if the refilter then narrows nothing (an
	//     empty result set still means the body is requester-dependent), and even
	//     if a future short-circuit skips the refilter for some input.
	//   - THEIR bump is execution-based and fires wherever a refilter actually
	//     runs, including chains this frame never sees (a nested RA→RA hop that
	//     does not pass through apiref).
	// Double-bumping is harmless: the gate reads Count()>0, never an exact count.
	//
	// No-op when no sink is installed on ctx (BumpUAFTouched is nil-safe), so
	// every path that does not cache is byte-unchanged.
	bumpUAFSinkIfDeclared(ctx, &ra)

	// resolveRA is the page-keyed resolve seam: it runs the SAME
	// restactions.Resolve pipeline at the given pagination and returns the RA
	// Status map. A fresh shallow copy of the RA (Status reset) is resolved
	// each call so the unpaginated + page-keyed resolves of Ship 4a's
	// byte-verify do not clobber each other's Status (restactions.Resolve
	// mutates In.Status in place).
	//
	// The rctx parameter lets Ship 4a swap the L1-key context for the
	// UNPAGINATED resolve so the RA's inner-call dep edges attach to the
	// RAFullList key (the cell the refresher re-resolves + re-pins on a
	// dirty-mark). Dep recording is idempotent (sync.Map LoadOrStore), so a
	// page-keyed resolve under the widget's own L1 key and an unpaginated
	// resolve under the RAFullList key coexist safely.
	resolveRA := func(rctx context.Context, perPage, page int) (map[string]any, error) {
		local := ra
		local.Status = nil
		raopts := restactions.ResolveOptions{
			In:      &local,
			SArc:    opts.RC,
			AuthnNS: opts.AuthnNS,
			PerPage: perPage,
			Page:    page,
			Extras:  opts.Extras,
		}
		if _, rerr := restactions.Resolve(rctx, raopts); rerr != nil {
			return nil, rerr
		}
		return rawExtensionToMap(local.Status)
	}

	// #130 F4 — per-seed-pass RA-resolve memo. Installed ONLY on the boot-seed
	// context (cache.WithSeedResolveMemo in withCohortSeedContext); nil (a strict
	// no-op) on the user /call path, the refresher, and the discovery walk, so
	// the request path is provably untouched (C-F4-8). The memo collapses the
	// ~71 statistics/tag widgets that share a small set of heavy RESTActions
	// (compositions-list / dashboard-data — ~18s of gojq over the 60K benchapps
	// array each) to ONE real resolve per distinct (RA, identity, page, extras)
	// within the pass. Keyed by the FULL RBAC-determining identity (username +
	// sorted groups off ctx — the same tuple refilter/cluster_list/ra_full_list
	// read to FILTER the list) so a hit is only ever served to a caller who would
	// compute a byte-identical body; dropping identity would leak cohort A's
	// RBAC-filtered body to cohort B (C-F4-4, proven RED by the divergent-output
	// arm). The memo wraps BOTH the 4a serve and the page-keyed fallthrough: the
	// key includes (perPage, page), so the unpaginated first-sight (0,0), the 4a
	// paginated serve, and any page-keyed fallthrough occupy distinct memo slots
	// and never cross-serve.
	memo := cache.SeedResolveMemoFromContext(ctx)
	var memoKey string
	if memo != nil {
		username, groups := identityForMemo(ctx)
		memoKey = memo.Key(opts.ApiRef.Namespace, opts.ApiRef.Name,
			username, groups, cache.HashExtras(opts.Extras), opts.PerPage, opts.Page)
		if hit, ok := memo.Load(memoKey); ok {
			// Load returns a fresh deep copy; safe to hand straight back.
			return hit, nil
		}
	}

	// storeMemo deep-copies the resolved body (JSON-native round-trip, C-F4-3 —
	// panics AT THIS SEAM on a non-JSON-native value rather than aliasing a bad
	// value into the shared memo) and records it so sibling widgets in the pass
	// hit it. No-op when no memo is installed (nil memo / empty key).
	storeMemo := func(out map[string]any) {
		if memo == nil || memoKey == "" || out == nil {
			return
		}
		memo.Store(memoKey, pmaps.DeepCopyJSON(out))
	}

	// Ship 4a (0.30.198) — page-independent RAFullList serve at the apiRef
	// chokepoint. Engaged ONLY when shouldServeRAFullList (below) is true. On a
	// hit / verified-sliceable shape it serves a cheap Go-slice over the cached
	// full list, shared across pages AND widgets. On a miss / not-cleanly-
	// sliceable shape it transparently falls back to today's page-keyed resolve
	// below — NEVER a wrong result.
	if shouldServeRAFullList(ctx, opts.PerPage, opts.Page) {
		if served, ok, serr := raFullListServe(ctx, res.GVR, opts.ApiRef.Namespace,
			opts.ApiRef.Name, &ra, opts.PerPage, opts.Page, opts.Extras, resolveRA); serr != nil {
			return map[string]any{}, serr
		} else if ok {
			storeMemo(served)
			return served, nil
		}
		// served=false, no error — fall through to the page-keyed resolve.
	}

	out, err := resolveRA(ctx, opts.PerPage, opts.Page)
	if err != nil {
		return map[string]any{}, err
	}
	storeMemo(out)
	return out, nil
}

// identityForMemo reads the RBAC-determining identity (username + groups) off
// ctx — the SAME xcontext.UserInfo the RESTAction resolver reads to RBAC-filter
// the list (refilter.go / cluster_list.go / ra_full_list.go). On the seed path
// withCohortSeedContext installs it via xcontext.WithUserInfo(cohort.Username,
// cohort.Groups). A UserInfo-err (no identity on ctx) yields ("", nil) — a
// distinct, non-colliding identity segment; the memo simply never cross-serves
// an identity-less body to an identity-bearing one.
func identityForMemo(ctx context.Context) (string, []string) {
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return "", nil
	}
	return ui.Username, ui.Groups
}
