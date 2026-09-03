// phase1_pip_seed.go — Ship PIP (0.30.173): the per-identity prewarm
// seed of restactions + widgets top-level L1.
//
// FOLDED 2026-07-03 (docs/prewarm-engine-implicit-on-cache-2026-07-03.md §4):
// the LEGACY orchestration in this file — runPIPSeed (the GOMAXPROCS errgroup
// cohort fan-out), seedCohort, seedCohortFn, enumerateAggregatePrewarmTargets
// (+Fn) — is DELETED. The prewarm engine is now implicit-on-cache and its
// engine seed (prewarm_engine_boot.go seedScopeYielding) is the ONLY seed path;
// the errgroup back-out lever (PREWARM_ENGINE_ENABLED=false) that kept the
// legacy path alive is retired. This file is now the SHARED SEED-PRIMITIVE
// LIBRARY: seedOneRestaction / seedOneWidget / seedRAFullListForWidget /
// withCohortSeedContext / cohortLogLabel + the navWidgetHarvester type — all
// called by the engine boot seed. The seedTarget type + PrewarmPIPEnabled gate
// also live here.
//
// THE NORTH-STAR DEFECT THIS SHIPS AGAINST (unchanged rationale). After
// phase1Done=true at 0.30.172, admin's first compositions-list /call was
// l1_hit:"miss" (~13.8 s) — the per-USER resolved-output L1 (top-level
// restactions / widgets cache classes) was cold for every cohort. The seed
// primitives here fill that gap: BEFORE phase1Done flips, the engine seed warms
// the top-level L1 once per (per-binding target, restaction) AND once per
// (target, widget) reached by the Phase-1 walker, so the first /call returns
// l1_hit:"hit" with zero resolve.
//
// PER-BINDING TARGETS. The seed target set is derived from the BindingsByGVR
// reverse index via cache.EnumeratePrewarmTargetsForGVR (per navigated GVR),
// built by the engine boot re-walk — see prewarm_engine_boot.go.
//
// MEMORY BOUND (fold 2026-07-03). Each seed unit funnels through the ADAPTIVE
// seed-unit gate (enterSeedUnit, seed_bound.go) — serialize against live
// GOMEMLIMIT headroom, per-unit calibration, AssertSeedUnitFootprint diagnostic.
// This is the ONLY thing bounding seedRAFullListForWidget's unpaginated
// full-list (the #23 dominant allocation); the adaptive nested-resolve bound
// does NOT cover it (§3.1).
//
// CONCURRENCY (architect's design §3). The cohort loop runs under a
// bounded errgroup with limit = runtime.GOMAXPROCS(0) — matches the F2
// content-warm's bounded fan-out shape. Each cohort's seed is
// SEQUENTIAL inside the goroutine: it iterates the harvested
// (restaction, widget) sets one at a time. The bound on transient RSS
// per cohort is N_restactions×envelope_bytes + N_widgets×envelope_bytes
// — same OOM profile as the F2 content pass per cohort.
//
// PER-COHORT TIMEOUT (restored 0.30.191 SCOPE CORRECTION). Set via
// context.WithTimeout inside the per-cohort closure. A stuck cohort
// thus cannot wedge Phase 1 past Step 7.6's global budget; the timeout
// firing returns ctx.Err() up the errgroup which propagates as the
// cohort's seed-failure path. The 0.30.190 proportional-timeout model
// (computeCohortTimeout) was REVERTED at 0.30.191: it was an INFERENCE
// from a file header comment ("1.5s/widget × 132 widgets = 198s"), not
// a measurement. Per feedback_data_driven_workflow +
// feedback_empirical_root_cause_trace_before_fix we are NOT raising the
// ceiling until 0.30.191 instrumentation tells us which abort cause
// actually fires for the 0.30.189 sentinel cohort. The 120s fixed
// ceiling is the 0.30.179 value.
//
// FEEDBACK_CHECK_K8S_CLIENTGO_PRIOR_ART: client-go has no equivalent
// for per-RBAC-cohort prewarm. RBAC subject enum is a custom snowplow
// concern (no upstream evaluator exists in client-go; rbac/v1
// authorizer evaluates one request at a time). PIP's cohort
// enumeration is therefore a custom mechanism.
//
// FEEDBACK_NO_SPECIAL_CASES: no hardcoded admin / hardcoded user / no
// resource-name literals. The cohort list is derived purely from the
// published RBAC snapshot; the restaction set is the same harvester
// the F2 content pass drains; the widget set is the new
// navWidgetHarvester populated by the existing walk. Every identifier
// flows from the cluster state, not from Go literals.
//
// FEEDBACK_RESTACTION_NO_WIDGET_LOGIC + FEEDBACK_L1_PER_USER_KEYED_
// NEVER_COHORT: PIP keys every L1 entry per-user via the dispatcher's
// canonical dispatchCacheLookupKey (helpers.go) under a ctx whose
// xcontext.UserInfo carries the cohort's Username + Groups. No cohort
// cross-leak path exists — the cache layer SEES per-user keys; PIP just
// pre-populates one entry per cohort.

package dispatchers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/endpoints"
	"github.com/krateo-platformops/plumbing/jqutil"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/snowplow/apis"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/handlers/util"
	"github.com/krateo-platformops/snowplow/internal/objects"
	"github.com/krateo-platformops/snowplow/internal/resolvers/restactions"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets"
	"github.com/krateo-platformops/snowplow/internal/resolvers/widgets/apiref"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

const (
	// Ship 2 / 0.30.196 — the PER-COHORT CAP IS DELETED. pipCohortCapDefault
	// (50), envPrewarmPIPCohortCap ("PREWARM_PIP_COHORT_CAP"), and
	// pipCohortCap() are GONE. The cap was the not-Ready-forever landmine:
	// at cohort #51 runPIPSeed returned a fatal `cohort_cap_exceeded` error
	// and phase1WarmupWith fail-closed (/readyz 503 FOREVER). With readiness
	// now decoupled from the per-cohort seed (the seed runs as a background
	// best-effort warm AFTER MarkPhase1Done — see phase1_walk.go Step 7.6),
	// there is NO storage rationale to fail closed on cohort count, and an
	// O(users)-cohort topology (per-user User-kind bindings) must not wedge
	// the pod. The cap is the forbidden unbounded-cohort landmine and is
	// removed entirely.

	// pipCohortTimeout is the per-cohort hard ceiling. A stuck cohort
	// cannot wedge Phase 1 past Step 7.6's global budget.
	//
	// Ship A.3 / 0.30.179 — raised 20s -> 120s. Binding-set enumeration
	// produces more classes than the prior canonical-cohort dedupe, and
	// each class's restactions seed walks per-namespace LIST calls (a
	// compositions-list RESTAction emits one K8s call per namespace via
	// the namespace iterator). A 50-namespace cluster needs ~30s per
	// cohort to seed cleanly; 120s adds ~4x headroom.
	//
	// Ship 0.30.191 SCOPE CORRECTION restored this fixed value from the
	// 0.30.190 proportional-timeout model (computeCohortTimeout). The
	// 0.30.190 raise was an INFERENCE from a file header comment, not a
	// measurement of the actual 0.30.189 sentinel-cohort abort cause —
	// 0.30.191 ships the instrumentation that will tell us empirically
	// which abort cause fires before any further timeout change.
	pipCohortTimeout = 120 * time.Second

	// pipGlobalTimeout is the absolute Step 7.6 budget. Designed to fit
	// the architect's pod-start→phase1Done projection (baseline + seed
	// ceiling). Ship A.3 / 0.30.179 — raised 40s -> 8 minutes per the
	// PM gate's "baseline + 8 min seed ceiling" target. The per-cohort
	// timeout × cohort cap caps the total at 50 × 120 s = 6000 s but the
	// parallelism + harvest dedup keep the empirical wall-clock well
	// inside 8 min.
	pipGlobalTimeout = 8 * time.Minute
)

// PrewarmPIPEnabled reports whether the Ship PIP per-identity prewarm seed
// runs. FOLDED 2026-07-03 (docs/prewarm-engine-implicit-on-cache-2026-07-03.md):
// the standalone PREWARM_PIP_ENABLED env read is RETIRED (registered in
// cache.retiredFlags). It is now IMPLICIT-ON-CACHE — the PIP seed runs whenever
// prewarm runs, mirroring #57. (PIP still shares the content-prewarm harvester;
// both gates now derive from cache.PrewarmEnabled().)
func PrewarmPIPEnabled() bool {
	return cache.PrewarmEnabled() // implicit-on-cache (#57); was env "PREWARM_PIP_ENABLED"=="true"
}

// navWidgetEntry is one navigation widget CR captured during the
// Phase-1 walk together with the GVR + pagination tuple it resolved
// under. The seed loop re-resolves the SAME CR per cohort under per-
// cohort identity and Puts the per-user widgets L1 entry.
//
// Ship 0.30.187 D2: the RESOLUTION tuple (PerPage, Page — passed to
// widgets.Resolve) is DECOUPLED from the dispatcher-lookup KEY tuple
// (KeyPerPage, KeyPage — passed to dispatchCacheLookupKey). Pre-0.30.187
// both used the walker's prewarmPageLimit() default for no-slice
// widgets, but the dispatcher's serve-time paginationInfo defaults to
// (-1, -1) when the request URL carries no ?page/?perPage params —
// seed→serve cells thus missed on every no-slice widget. The seed-key
// tuple is now derived via deriveSeedKeyTuple from the /call Path the
// walker reached the widget through; the resolution tuple stays
// bounded by prewarmPageLimit() as the 0.30.127 storm guard.
type navWidgetEntry struct {
	W       *unstructured.Unstructured
	GVR     schema.GroupVersionResource
	PerPage int // resolution tuple — passed to widgets.Resolve
	Page    int // resolution tuple — passed to widgets.Resolve

	// Ship 0.30.187 D2 — dispatcher-lookup KEY tuple. Set to (-1, -1)
	// for widgets reached via a /call Path with no slice declared (the
	// dispatcher's paginationInfo default), or to the declared (page,
	// perPage) when the Path carries them. See deriveSeedKeyTuple.
	KeyPerPage int
	KeyPage    int

	// #42 FIX-E — FIRST-NAV WALK ORDER. A monotonic sequence number
	// assigned at harvest time. The Phase-1 walk visits nav roots in
	// config.json order (default-route/dashboard subtree first —
	// phase1_roots.go) and descends each depth-first, so harvest order IS
	// the frontend's first-nav priority order. The seed uses this as the
	// WITHIN-RANK widget order (replacing the A2 count-sort: count≠cost —
	// A2 seeded a cheap-but-late widget before the dashboard's own
	// widgets). 100% walk-derived — zero static name lists, no depth/route
	// literal (the ORDERING is the signal, not any hardcoded name).
	NavOrder int

	// RootIndex is the zero-based index of the config-root subtree this widget
	// was FIRST harvested under (BeginRoot increments it as the walk moves to
	// the next root). Roots iterate in config.json order (phase1_roots.go).
	//
	// #130 F3b-r2 (2026-07-12): RootIndex is now a DEAD-STAMP — no seed-ordering
	// or readyz-latch branch keys on `RootIndex==0` any longer. The old FIX-F
	// latch (fire when the RootIndex==0 dashboard segment seeded) and the F3
	// Lever-1 rank tier (RootIndex==0 cohorts sort first) were BOTH deleted:
	// keying on `==0` as a partition hardcoded "the first config root is the
	// dashboard / is first-nav," a frontend-page concept Diego rejected. The
	// seed order is now a NavOrder total order and the latch keys on the
	// widget-vs-RA CLASS boundary (design §5). The field is RETAINED because
	// (a) the redrive-walk stamp-correctness regression test
	// (TestNavWidgetHarvester_RedriveWalk_StampsRootIndexZeroForRootZero, #99b
	// Fix 2) still asserts BeginWalk/BeginRoot stamp it right per pass, and
	// (b) it is emitted as a diagnostic-only `root_index` field in the
	// widget_targets telemetry. Walk-derived, no literal.
	RootIndex int
}

// navWidgetHarvester accumulates the deduplicated navigation widget
// set the Phase-1 walker reaches under SA identity (Step 7.6a). The
// phase1Walker writes into it as it resolves each widget; the Step
// 7.6 seed pass drains it once per cohort. Dedupe key is the per-
// (gvr, ns, name, perPage, page) tuple — admin and cyberjoker hitting
// the same widget under the same pagination land on the same harvested
// entry, and the seed produces one per-user Put per cohort.
//
// Concurrency: the walk is single-threaded per root and roots resolve
// sequentially (phase1WarmupWith), but the mutex makes the harvester
// safe regardless of how the walk is scheduled — same shape as
// contentPrewarmHarvester.
type navWidgetHarvester struct {
	mu      sync.Mutex
	entries map[string]navWidgetEntry
	// #42 FIX-E — monotonic harvest-order counter. Incremented under mu on
	// each FIRST harvest of a distinct widget so NavOrder captures the walk's
	// first-nav priority sequence (roots in config.json order, depth-first).
	navSeq int
	// #42 FIX-F seam — zero-based current config-root index. BeginRoot()
	// increments it as the walk advances to the next root (roots resolve
	// sequentially — phase1WarmupWith). curRoot starts at -1 so the first
	// BeginRoot() sets it to 0 (the default-route/dashboard subtree). Stamped
	// into RootIndex on first harvest; no consumer here (FIX-F builds the latch).
	curRoot int
}

// newNavWidgetHarvester returns an empty harvester.
func newNavWidgetHarvester() *navWidgetHarvester {
	// curRoot starts at -1 so the first BeginRoot() sets RootIndex 0.
	return &navWidgetHarvester{entries: map[string]navWidgetEntry{}, curRoot: -1}
}

// BeginWalk resets the current config-root index to -1 (#99b Fix 2) at the top
// of EACH walk pass so the following per-root BeginRoot() calls stamp RootIndex
// 0..N-1 in config.json order every pass — the initial boot walk AND every
// engine/config-vars re-walk. Without the reset, a re-walk that calls BeginRoot
// per root would resume from the boot walk's final curRoot (N-1) and stamp
// re-walk-first-harvested widgets N..2N-1 (the naive-BeginRoot-only defect the
// doc §4 Fix 2 warns against). First-write-wins dedupe (harvestNavWidget)
// preserves the boot-walk stamp for any widget already harvested, so a widget
// FIRST harvested during a re-walk gets the correct root index for its pass.
// Nil-safe. Inert on this deployment's single-root config (curRoot pinned at 0
// after the first BeginRoot); required the day a second config root
// (ROUTES_LOADER) is declared and the effective harvest arrives via a re-walk.
func (h *navWidgetHarvester) BeginWalk() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.curRoot = -1
	h.mu.Unlock()
}

// BeginRoot advances the current config-root index (#42 FIX-F seam, STAMP-ONLY).
// The phase-1 walk calls this before descending each root; roots resolve
// sequentially so the increment is deterministic — RootIndex 0 is the first
// (default-route/dashboard) subtree. Nil-safe (flag-off Phase 1 passes no
// harvester). No consumer here; FIX-F's readyz latch reads RootIndex.
func (h *navWidgetHarvester) BeginRoot() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.curRoot++
	h.mu.Unlock()
}

// harvestNavWidget records a navigation widget CR plus the GVR +
// pagination tuples it was reached under. Nil-safe: a nil harvester /
// nil widget is a no-op (flag-off Phase 1 passes no harvester).
//
// Ship 0.30.187 D2: TWO pagination tuples are now passed.
//   - resolvePerPage/resolvePage: what the walker passes to
//     widgets.Resolve (bounded by prewarmPageLimit() for no-slice
//     widgets — the 0.30.127 storm guard).
//   - keyPerPage/keyPage: what the per-cohort seed loop passes to
//     dispatchCacheLookupKey — derived from the /call Path the walker
//     reached the widget through so the cell matches the dispatcher's
//     serve-time lookup. See deriveSeedKeyTuple.
//
// Dedupe is over (gvr, ns, name, keyPerPage, keyPage) — the dispatcher-
// key tuple — because that is the cell the seed populates. Two
// different roots reaching the same widget via the same key tuple yield
// one Put (idempotent — the resolver output is per-cohort identical for
// a given key tuple).
func (h *navWidgetHarvester) harvestNavWidget(w *unstructured.Unstructured, gvr schema.GroupVersionResource,
	resolvePerPage, resolvePage, keyPerPage, keyPage int) {
	if h == nil || w == nil {
		return
	}
	key := navWidgetHarvestKey(gvr, w.GetNamespace(), w.GetName(), keyPerPage, keyPage)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, seen := h.entries[key]; seen {
		// First-write-wins. The dedupe is intentional: the walk's
		// visited-set in phase1Walker.walk already prevents re-traversal,
		// so a second harvest for the same key only happens across roots
		// (idempotent — same CR + same key tuple yields identical Put).
		return
	}
	// Deep-copy the CR so a downstream resolver mutation does not race
	// with the original walker's `in` (the resolver mutates the
	// in-memory object during resolve — widgets.Resolve sets
	// status.widgetData etc.). The seed loop runs its own Resolve per
	// cohort against the CR; concurrent cohort resolves MUST NOT share
	// a single *unstructured. The DeepCopy is bounded by the widget CR
	// size (small) and runs once per distinct widget.
	// #42 FIX-E — stamp the first-nav sequence number at first harvest. The
	// walk reaches widgets in nav order (roots in config.json order,
	// depth-first descent), so navSeq IS the frontend first-nav priority.
	seq := h.navSeq
	h.navSeq++
	// #42 FIX-F seam — stamp the config-root index (STAMP-ONLY, no consumer
	// here). curRoot is -1 only if a harvest somehow precedes the first
	// BeginRoot(); clamp to 0 (the first-nav segment) defensively.
	rootIdx := h.curRoot
	if rootIdx < 0 {
		rootIdx = 0
	}
	h.entries[key] = navWidgetEntry{
		W:          w.DeepCopy(),
		GVR:        gvr,
		PerPage:    resolvePerPage,
		Page:       resolvePage,
		KeyPerPage: keyPerPage,
		KeyPage:    keyPage,
		NavOrder:   seq,
		RootIndex:  rootIdx,
	}
}

// deriveSeedKeyTuple computes the dispatcher-lookup key tuple
// (perPage, page) the per-cohort seed Put MUST use for a widget the
// walker reached via the given /call Path. Ship 0.30.187 D2.
//
// CONTRACT: the returned tuple MUST equal what the dispatcher's
// paginationInfo (helpers.go:50-76) returns at serve time for a request
// with that Path's query parameters.
//
//   - Empty path (root navigation widget — fetched directly via
//     objects.Get, no /call Path) → the frontend's first request
//     URL carries no slice params → paginationInfo returns (-1, -1) →
//     seed-key tuple = (-1, -1).
//   - Path with no page/perPage params → ParseCallPathPagination
//     returns ok=false → paginationInfo returns (-1, -1) →
//     seed-key tuple = (-1, -1).
//   - Path with explicit ?page=N&perPage=M → ParseCallPathPagination
//     returns the declared values → paginationInfo at serve time
//     returns the same → seed-key tuple = (perPage=M, page=N).
//
// The returned order is (perPage, page) — matches the seedOneWidget
// argument order to dispatchCacheLookupKey.
func deriveSeedKeyTuple(callPath string) (perPage, page int) {
	if callPath == "" {
		return -1, -1
	}
	p, pp, ok := util.ParseCallPathPagination(callPath)
	if !ok {
		return -1, -1
	}
	return pp, p
}

// navWidgetHarvestKey is the canonical dedup key for harvested
// navigation widgets. The tuple matches the dispatcher's serve-time
// key shape for widgets (dispatchCacheLookupKey at helpers.go:174 takes
// the same fields).
func navWidgetHarvestKey(gvr schema.GroupVersionResource, ns, name string, perPage, page int) string {
	return gvr.String() + "|" + ns + "|" + name + "|" + fmt.Sprintf("%d|%d", perPage, page)
}

// snapshot returns a stable list of harvested widget entries.
func (h *navWidgetHarvester) snapshot() []navWidgetEntry {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]navWidgetEntry, 0, len(h.entries))
	for _, e := range h.entries {
		out = append(out, e)
	}
	return out
}

// seedTarget is the dispatcher-package equivalent of cache.PrewarmTarget,
// shaped for the seed loop's per-target dispatch. Carries a representative
// SubjectIdentity (Username + Groups, k8s-shape) so the cohort ctx setup
// flow (withCohortSeedContext) can lift it into xcontext.WithUserInfo
// byte-equivalently to the pre-ship cache.Cohort path.
//
// Ship 0.30.242 H.c-layered Phase 2b replacement for the deleted
// cache.Cohort type. Identical surface (Username + Groups); production
// callers in this file consume only those two fields. The BindingUID
// dimension is carried but not used by the seed loop directly — it
// flows through the cell key at populate time via dispatchCacheLookupKey
// (helpers.go) per Path B.
type seedTarget struct {
	BindingUID string
	Username   string
	Groups     []string
	// CollapsedBindings — #42 FIX-D: the dedup's collapsed-binding count for
	// this representative identity (≥1), carried from cache.PrewarmTarget. Used
	// ONLY to rank identities for the identity-rank-major seed order; NOT part
	// of the seed dispatch or the cell key.
	CollapsedBindings int
}

// withCohortSeedContext builds the per-cohort seed context. Mirrors
// withContentPrewarmSAContext (phase1_content_prewarm.go) for the SA
// transport seam (WithUserConfig / WithInternalEndpoint /
// WithInternalRESTConfig) but installs the COHORT's identity via
// xcontext.WithUserInfo instead of the SA's canonical username. Same
// inner-call iterator-serial marker (WithPrewarmIterSerial) so the
// seed pass shares the F2 content-warm's OOM profile.
//
// NOT marked WithApistagePrewarm — the apistage content L1 was already
// populated in Step 7.5; here we are populating the TOP-LEVEL
// per-user L1 (restactions + widgets dispatcher classes).
func withCohortSeedContext(ctx context.Context, cohort seedTarget,
	saEP endpoints.Endpoint, saRC *rest.Config) context.Context {

	opts := []xcontext.WithContextFunc{
		xcontext.WithUserConfig(saEP),
		xcontext.WithLogger(slog.Default()),
		xcontext.WithUserInfo(jwtutil.UserInfo{
			Username: cohort.Username,
			Groups:   cohort.Groups,
		}),
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)
	// #130 F3b Fix 2 — attach the shared watcher so the cohort seed's inner LIST
	// calls (dispatchViaInternalRESTConfig, resolve.go) serve a servable GVR's
	// LIST from the synced informer indexer instead of a live ~20s/60K paged
	// apiserver LIST. BYTE-IDENTICAL to the discovery walk's withPhase1SAContext
	// (phase1_walk.go:1029) and the F2 content-prewarm attach: without it the
	// internal-dispatch informer-serve branch (internal_dispatch.go, `ServeWatcher
	// FromContext(ctx); haveRW`) is never entered, so every whale-widget cohort
	// seed pays the live LIST — the ~20s/target cost that blew the seed budget on
	// 1.7.7. This is what makes the whale LISTs cheap so the NavOrder pass fits
	// budget. PREWARM-SCOPED (only this SA cohort context carries it; per-user
	// /call never does). nil-safe (cache.Global() may be nil under
	// CACHE_ENABLED=false → WithServeWatcher returns ctx unchanged → live LIST as
	// today).
	rctx = cache.WithServeWatcher(rctx, cache.Global())
	rctx = cache.WithPrewarmIterSerial(rctx)
	// #42 Option-2 — OVERRIDE the inherited discovery-walk scope with the SEED
	// scope. rePrewarmBootScoped passes seedScopeYielding the SAME walk-scoped
	// rctx (withPhase1SAContext stamped ScopeBootPrewarmWalk), and BuildContext
	// above layers values without stripping it — so without this the seed cohort
	// ctx would carry ScopeBootPrewarmWalk and apiref.shouldServeRAFullList would
	// WRONGLY exclude the seed's seedRAFullListForWidget 4a serve (the resolve
	// that EXISTS to pin the per-cohort full-list cell — the whole point of the
	// seed). WithFallthroughScope is a plain WithValue, so this last stamp on the
	// chain wins over the inherited walk scope. The seed scope is NOT
	// ScopeBootPrewarmWalk, so the gate lets its paginated 4a serve through; the
	// discovery walk (which never enters withCohortSeedContext) keeps the walk
	// scope and stays excluded. Inert under cache-off (WithFallthroughScope
	// returns ctx unchanged when Disabled(); the seed never runs cache-off).
	rctx = cache.WithFallthroughScope(rctx, cache.ScopeBootPrewarmSeed)
	return rctx
}

// seedOneRestaction resolves ONE RESTAction under the cohort ctx and
// Puts the resolved JSON into the per-user restactions L1 under the
// dispatcher's canonical key. STRUCTURALLY MATCHES the per-request
// dispatch at restactions.go:117-230 (architect's
// feedback_claim_vs_code_identity_at_diff_review):
//
//   - dispatchCacheLookupKey("restactions", group, version, resource,
//     ns, name, -1, -1, nil) — identical args (PerPage:-1, Page:-1,
//     extras:nil match the dispatcher's first /call by a cohort that
//     supplies no per-call pagination/extras; HG-PIP.3 byte-identity
//     gate verifies SHA-256 between seed Put and serve hit).
//   - cache.WithL1KeyContext(ctx, key) before Resolve so the inner-call
//     dep tracker records edges against the L1 key.
//   - restactions.Resolve same entrypoint at restactions.go:183-189.
//   - encodeResolvedJSON + cacheHandle.Put + ensureWatcherInformerForGVR
//   - cache.Deps().Record — same Put shape as restactions.go:212-230.
// seedSkipDecision is the SHARED per-mode seed-skip predicate for both seed
// primitives (single derivation site — F4-C2a / keepwarm c2 §4.2). It consumes
// the EXACT `key` the Put will use (passed by the caller, itself the single
// dispatchCacheLookupKey derivation) so skip-key correctness cannot drift from
// Put-key correctness. Returns true iff the target should be skipped (resolve +
// Put elided, target counted processed); the caller returns nil in that case.
//
// The mode selects BOTH the skip predicate and the counter:
//
//   - seedModeBoot: bare liveness (F.4 boot fresh-skip). A live cell under the
//     production key is already warm → skip; bumps pipSeedFreshSkipTotal. This
//     makes a deadline-cut boot chunk's continuation re-pay only the cold
//     remainder.
//   - seedModeKeepwarm: age-skip (c2 §4.2). A live cell YOUNGER than
//     keepwarmAgeSkipThreshold() (= TTL−sweepInterval = TTL/4) is skipped
//     (bumps keepwarmAgeSkipTotal); a live-but-OLD cell (age ≥ threshold) is NOT
//     skipped → the caller re-resolves + re-Puts it with a fresh CreatedAt. A
//     bare-liveness predicate here would make the sweep a no-op (it would skip
//     the very cells it exists to refresh — the F4-C3 keepwarm boundary); the
//     age term is what makes the sweep re-Put quiet-but-still-live cells before
//     they lazy-expire while dedup'ing churny/fresh cells (recovering F.4's
//     cost-proportional chunk resume for keepwarm with zero cycle-tracking
//     state).
//   - seedModeGVRDiscovered: NEVER skip. gvr-discovered must RE-RESOLVE
//     already-warm cells so the dep edge against the newly-registered GVR is
//     recorded (the S4 fix; F4-C3 boundary).
//
// The store's Get is itself the freshness/liveness oracle — it returns
// (entry, true) iff the entry exists AND is non-expired per the exact
// effectiveTTL logic the serve path uses (resolved.go Get, strict `>` expiry).
// For the age-skip, the entry's CreatedAt (Put-time-stamped, resolved.go Put)
// is read directly off the returned live entry — no cache-side accessor needed.
func seedSkipDecision(ctx context.Context, mode seedScopeMode, handle cacheHandle, key, class, target, cohortLabel string) bool {
	switch mode {
	case seedModeBoot:
		// #132 F4b Lever A — BEFORE the liveness Get: if this EXACT key was
		// already resolved-and-declined-external THIS boot scope, skip re-
		// resolving it. The cell is intentionally cold (the #102 decline was
		// correct — an external-touch cell has no dep edge), so a resume pass's
		// handle.Get would MISS (never Put) and re-resolve the whale from scratch,
		// paying its full external round-trip for zero forward progress (§3 loop).
		// The set is keyed by the full dispatchCacheLookupKey (encodes cohort
		// identity) and is nil off the boot seed path, so this is a strict no-op
		// on /call, keepwarm, gvr-discovered, and cache-off (nil-receiver Marked =
		// false). C-F4B-1/-2/-3.
		if cache.SeedDeclinedExternalSetFromContext(ctx).Marked(key) {
			pipSeedFreshSkipTotal.Add(1)
			// #105 seeded-set: a declined-external key is intentionally cold (no Put)
			// but was RESOLVED-and-reached this boot — for the boot-convergence signal
			// it is NOT re-attemptable progress, and it is NOT a failing target either.
			// Do NOT Mark it into the seededSet (it never warmed a servable cell), and
			// it is not in the failedSet (it declined, not errored). It is simply inert
			// for the progress signal — leaving it out of both sets keeps the delta
			// honest (declined externals neither grow the seeded set nor block
			// convergence).
			slog.Default().Debug("phase1.seed.skip.declined_external",
				slog.String("subsystem", "cache"),
				slog.String("class", class),
				slog.String("target", target),
				slog.String("cohort", cohortLabel),
				slog.String("effect", "boot-scope Lever A skip: this (widget,cohort) key was resolved-and-"+
					"declined external earlier THIS boot; re-resolving it every resume pass makes zero forward "+
					"progress (Put stays declined) — skipping breaks the §3 external-whale loop"),
			)
			return true
		}
		entry, live := handle.Get(key)
		if !live || entry == nil {
			return false
		}
		pipSeedFreshSkipTotal.Add(1)
		// #105 seeded-set: a fresh-skip means "this (target,cohort) cell IS warm this
		// boot" (a live cell under the production key). It belongs in the seeded SET
		// even though it did no Put this pass — the union "Put ∪ fresh-skip" = "warm
		// targets reached this boot" is the set whose GROWTH is the forward-progress
		// signal (design §5.0/§As-built). Marking here is what makes a stable
		// re-walk (every healthy cohort fresh-skipped) read as "no growth" without
		// false-negativing on the TTL re-Put oscillation a count would suffer.
		cache.BootSeededSetFromContext(ctx).Mark(key)
		slog.Default().Debug("phase1.seed.fresh_skip",
			slog.String("subsystem", "cache"),
			slog.String("class", class),
			slog.String("target", target),
			slog.String("cohort", cohortLabel),
			slog.String("effect", "boot-scope fresh-skip: live L1 cell under the production key; "+
				"resolve+Put skipped, target counted as processed (F.4 cost-proportional resume)"),
		)
		return true
	case seedModeKeepwarm:
		entry, live := handle.Get(key)
		if !live || entry == nil {
			return false
		}
		// Age-skip: skip iff the live cell is YOUNGER than the threshold. A cell
		// at or beyond the threshold is re-resolved (return false). Strict `<`
		// mirrors the store's strict `>` expiry so a cell AT the threshold is
		// re-resolved, never left to race the expiry edge.
		if time.Since(entry.CreatedAt) < keepwarmAgeSkipThreshold() {
			keepwarmAgeSkipTotal.Add(1)
			slog.Default().Debug("phase1.seed.keepwarm_age_skip",
				slog.String("subsystem", "cache"),
				slog.String("class", class),
				slog.String("target", target),
				slog.String("cohort", cohortLabel),
				slog.Int64("age_ms", time.Since(entry.CreatedAt).Milliseconds()),
				slog.Int64("threshold_ms", keepwarmAgeSkipThreshold().Milliseconds()),
				slog.String("effect", "keepwarm age-skip: live cell younger than TTL/4; resolve+Put "+
					"skipped (cell survives to next sweep at age<TTL; refresher/customer-Put dedup)"),
			)
			return true
		}
		return false
	default: // seedModeGVRDiscovered — never skip (F4-C3 boundary).
		return false
	}
}

// hasTemplatedEndpointRef reports whether any api-step of cr carries a
// TEMPLATED endpointRef.name — a jq template (${...}-shaped, jqutil.MaybeQuery)
// that the resolver evaluates against REQUEST extras to select the dispatch
// endpoint (#113 hub-spoke). Such a RESTAction cannot be seeded (the boot seed
// has no request extras → the endpoint resolve misses → a truncated body would
// be Put under the no-extras key without either GTTL-1 sink bumping, the §4
// poisoning nuance). Each api-step and its *Reference EndpointRef is nil-guarded
// (API is a *API, EndpointRef a *templates.Reference; either may be nil).
func hasTemplatedEndpointRef(cr *templatesv1.RESTAction) bool {
	if cr == nil {
		return false
	}
	for _, step := range cr.Spec.API {
		if step == nil || step.EndpointRef == nil {
			continue
		}
		if _, isTemplate := jqutil.MaybeQuery(step.EndpointRef.Name); isTemplate {
			return true
		}
	}
	return false
}

// hasTemplatedEndpointRefFn is the #113 seed-skip PREDICATE seam. Production
// wires it to the real hasTemplatedEndpointRef; the ONLY reassigner is the
// _test.go falsifier, which transiently NEUTERS it to always-false to prove the
// RED (an impl that fails to skip a templated-endpointRef RA reaches the Put and
// poisons the no-extras cell with a truncated body — the #113 §4 defect). A
// package var (not a parameter) so seedOneRestaction's signature is untouched;
// production never reassigns it. Idiom-match: resolveOnceFn.
var hasTemplatedEndpointRefFn = hasTemplatedEndpointRef

// seedObjectsGetFn is the RESTAction-fetch seam for seedOneRestaction.
// Production wires it to the real objects.Get; the ONLY reassigner is the
// _test.go falsifier, which injects a fetched RESTAction unstructured (a
// templated- vs static-endpointRef shape) to drive the #113 seed-skip
// hermetically — without a cluster. A package var (not a parameter) so
// seedOneRestaction's signature is untouched; production never reassigns it.
// Idiom-match: resolveOnceFn / paginationFetchPageFn.
var seedObjectsGetFn = objects.Get

// seedRestactionResolveAndPutFn is the RESOLVE+encode+GTTL-gate+Put TAIL of
// seedOneRestaction — everything AFTER the #113 templated-endpointRef skip.
// Extracting it as a seam lets a test observe whether the seed reached the Put
// (static RA) or short-circuited before it (templated RA → NO Put) without a
// live resolver/encoder. Production wires it to the real tail
// (seedRestactionResolveAndPutProd); only the _test.go falsifier reassigns it.
var seedRestactionResolveAndPutFn = seedRestactionResolveAndPutProd

func seedOneRestaction(ctx context.Context, cohortLabel string, ref templatesv1.ObjectReference, authnNS string, mode seedScopeMode) error {
	got := seedObjectsGetFn(ctx, ref)
	if got.Err != nil {
		// #158 (design §1.3): preserve the typed status error so the call
		// site's classifySeedErr sees 403 (RBAC deny) vs 5xx (operational)
		// instead of an opaque string. statusErrFromResponse lifts the
		// plumbing *response.Status back into an *apierrors.StatusError
		// carrying Code+Reason. Wrapped with %w so the RESTAction identity
		// is visible in logs while errors.As/Is still thread through to the
		// embedded StatusError.
		return fmt.Errorf("fetch RESTAction %s/%s: %w", ref.Namespace, ref.Name, statusErrFromResponse(got.Err))
	}
	if got.Unstructured == nil {
		return fmt.Errorf("fetch RESTAction %s/%s: nil object", ref.Namespace, ref.Name)
	}

	// Compute the per-user dispatcher key — IDENTICAL shape to
	// restactions.go:117. ctx already carries the cohort's UserInfo, so
	// dispatchCacheLookupKey reads it and hashes Username + Groups into
	// the key.
	key, handle, inputs := dispatchCacheLookupKey(ctx, "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		-1, -1, nil)
	// Ship 0.30.188 — diagnostic slog: emit the seed-side cache key +
	// its components so it can be diff'd against the dispatcher_get and
	// per_user_fallback_put log lines at widgets.go / restactions.go.
	emitDispatchCacheKeyDiag(slog.Default(), "seed", ctx,
		key, inputs, "restactions",
		got.GVR.Group, got.GVR.Version, got.GVR.Resource,
		got.Unstructured.GetNamespace(), got.Unstructured.GetName(),
		-1, -1, nil)
	if handle == nil || key == "" {
		// L1 disabled OR no identity on ctx — defensive skip. PIP's
		// cohort ctx ALWAYS installs WithUserInfo, so an empty key here
		// is a configuration bug (PREWARM_PIP_ENABLED on while
		// CACHE_ENABLED off); log + skip.
		return nil
	}
	// #42 FIX-C (A4 security finding, populate side): the cohort's identity
	// re-derived a first-match BindingUID of "" — EvaluateRBAC denied or
	// errored at lookup and fail-closed (helpers.go). Every "" identity folds
	// the SAME shared empty-identity cell, resolved under a cohort (e.g.
	// system:kube-controller-manager) whose serve-time narrowing can be broad;
	// any real request that also derives "" would HIT it. Do NOT Put that cell
	// from the seed — skip with one log line (non-empty identities still
	// seeded). The serve-side treatment of a re-derived "" as a MISS is the
	// separate task #95 gate.
	if inputs != nil && inputs.BindingUID == "" {
		slog.Default().Info("phase1.seed.skip.empty_binding",
			slog.String("subsystem", "cache"),
			slog.String("class", "restactions"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.String("cohort", cohortLabel),
			slog.String("effect", "cohort re-derived first-match BindingUID=\"\" (RBAC deny/err fail-closed); "+
				"skipping the shared empty-identity cell Put (A4 populate-side guard)"),
		)
		return nil
	}

	// Per-mode seed skip (F.4 boot fresh-skip + keepwarm c2 age-skip, single
	// derivation site). seedSkipDecision consults `key` — the SAME key derived
	// above that the Put below uses — via the store's OWN TTL-expiry Get, so
	// skip-key correctness cannot drift from Put-key correctness (F4-C2a,
	// feedback_consultation_mutation_is_not_key_correctness). Placed BEFORE
	// enterSeedUnit so a skipped target never consumes the #46 memory admission.
	// The mode selects the predicate: boot = bare liveness (F.4), keepwarm =
	// age-skip (c2 §4.2), gvr-discovered = never skip (F4-C3 boundary).
	if seedSkipDecision(ctx, mode, handle, key, "restactions", ref.Namespace+"/"+ref.Name, cohortLabel) {
		return nil
	}

	// #46 / fold 2026-07-03: bound this seed unit's footprint via the ADAPTIVE
	// seed-unit gate (enterSeedUnit — serialize against live GOMEMLIMIT
	// headroom + per-unit calibration + AssertSeedUnitFootprint), AFTER the
	// identity short-circuit so the customer /call path is untouched.
	// seedOneRestaction is the engine seed's shared primitive. Transparent when
	// GOMEMLIMIT is unset (unlimited headroom).
	seedRelease, seedErr := enterSeedUnit(ctx, "restaction/"+ref.Namespace+"/"+ref.Name)
	if seedErr != nil {
		return seedErr // ctx cancelled while blocked on the bound
	}
	defer seedRelease()

	scheme := k8sruntime.NewScheme()
	if err := apis.AddToScheme(scheme); err != nil {
		return fmt.Errorf("add apis to scheme: %w", err)
	}
	var cr templatesv1.RESTAction
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(
		got.Unstructured.Object, &cr); err != nil {
		return fmt.Errorf("unstructured -> RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// #113 seed-skip — a RESTAction with a TEMPLATED api-step endpointRef.name
	// (a jq template that selects the dispatch endpoint from REQUEST extras, e.g.
	// hub-spoke) cannot be meaningfully seeded: the boot seed carries NO request
	// extras, so evalJQ(ref.Name, dict) yields an empty/error name → the endpoint
	// resolve MISSES → the stage TRUNCATES. Crucially that miss bumps NEITHER the
	// stageErrSink NOR the extTouchedSink (the external bump is downstream of a
	// SUCCESSFUL endpoint resolve), so declineSeedPutOnError would NOT decline —
	// the seed would Put a TRUNCATED/degraded body under the no-extras key, which
	// then serves cold on the first real /call until TTL (the §4 poisoning
	// nuance). Skip is provably NON-LOSSY: a templated-endpointRef RA dials a
	// spoke apiserver (external) → the spoke read-back is external-touch-declined
	// (§3.2) → never cached anyway, so the seed was never going to warm a servable
	// cell for it. Greppable skip line mirrors the Class-5 refHasUnresolvedTemplateToken
	// idiom. Every api-step EndpointRef is nil-guarded (*Reference, may be nil).
	if hasTemplatedEndpointRefFn(&cr) {
		slog.Default().Info("phase1.seed.skip.templated_endpointref",
			slog.String("subsystem", "cache"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.String("cohort", cohortLabel),
			slog.String("effect", "RESTAction has a templated api-step endpointRef.name (request-extras-driven "+
				"endpoint selection, e.g. hub-spoke); the boot seed has no request extras to resolve it → skipping "+
				"to avoid Putting a truncated body under the no-extras key (#113 §4). Spoke reads are external → "+
				"never cached; skip is non-lossy."),
		)
		return nil
	}

	// 1.12.3 A-1 (SECURITY, cross-tenant) — the SEED leg of the shared three-site
	// UAF Put-decline gate, placed BEFORE the resolve. The boot seed warms a
	// restactions cell under a cohort REPRESENTATIVE identity; for a UAF-bearing
	// RA that cell holds the representative's OWN per-object narrowing, and every
	// other cohort member who derives the same BindingUID + RBACSubGen would be
	// served it verbatim on their first /call — the leak at its widest (a whole
	// cohort reading one member's rows, warm from boot).
	//
	// SKIP-BEFORE-RESOLVE, not resolve-and-decline: the Put is the seed's ONLY
	// product (the tail's dep Record + informer wiring exist to service that
	// cell), so resolving first would buy nothing and cost a full fan-out per
	// cohort per UAF RA at boot. Non-lossy by the same argument as the #113
	// templated-endpointRef skip above: the seed was never going to leave a
	// servable cell behind. HasUAF is stamped here (not only in the tail) so the
	// ONE shared helper decides, and so the flag is already correct if a future
	// path carries these inputs onward. The tail keeps its own defensive decline
	// for a caller that drives the seam directly.
	if inputs != nil {
		inputs.HasUAF = restactionHasUAFStage(&cr)
	}
	// nil sink: this gate runs BEFORE the resolve, so no refilter can have been
	// observed yet — the DECLARATION is the only signal available here, and that
	// is exactly the point (it is what lets the seed skip the fan-out instead of
	// paying for a resolve whose output it would then discard). The post-resolve
	// tail below consults the sink as well.
	if declineUAFPut(inputs, nil) {
		// INFO, not WARN: this is a per-boot, per-(cohort × UAF RA) event — a
		// handful of lines, not hot-path noise — and it is the greppable
		// counterpart of the other phase1.seed.skip.* lines around it, which the
		// boot-seed post-mortems read. The hot-path decline sites log at DEBUG.
		slog.Default().Info("phase1.seed.skip.user_access_filter",
			slog.String("subsystem", "cache"),
			slog.String("class", "restactions"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.String("cohort", cohortLabel),
			slog.String("effect", "RESTAction declares a userAccessFilter: its resolved body is narrowed per requester, "+
				"but the seed cell is keyed per BINDING — a co-bound cohort member would be served the representative's "+
				"rows. Skipping resolve+Put entirely (1.12.3 A-1); these RAs resolve per request until 1.13.0 folds the "+
				"UAF scope into the key."),
		)
		return nil
	}

	// Ship 0.30.192 — pure-additive per-stage timing sink for cost
	// attribution. The 0.30.179 cluster-list-deny / per-NS iterator
	// fallback at iter_serial=1 is the architect's TRACED hypothesis
	// for the 46s/restaction wall-clock on the four 0.30.189-sentinel
	// cohorts — but the "5K namespaces × ~10ms" projection was
	// invalidated by the cluster reality (62 ns actual). This sink lets
	// the resolver record per-stage ElapsedMs + ClusterListUsed +
	// ClusterListDenyGate + IteratorCalls + IteratorElapsedMs so the
	// failing cohort's 46s can be attributed to a real code path.
	//
	// SINK ISOLATION (feedback_shared_vs_copy_is_a_concurrency_change):
	// one sink per seedOneRestaction invocation; never shared across
	// cohorts. The sink's sync.Mutex is defensive — the resolver writes
	// only from the parent goroutine (between stages) — but a future
	// path that records from an errgroup worker stays race-safe.
	stageTimingSink := cache.NewPIPStageTimingSink()
	restactionStart := time.Now()
	defer func() {
		snapshot := stageTimingSink.Snapshot()
		slog.Default().Info("phase1.seed.restaction.timing",
			slog.String("subsystem", "cache"),
			slog.String("cohort", cohortLabel),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.Int64("elapsed_ms_total", time.Since(restactionStart).Milliseconds()),
			slog.Int("stages_total", len(snapshot)),
			slog.Any("stages", snapshot),
		)
	}()

	// Install the L1 key on ctx BEFORE Resolve so the inner-call dep
	// tracker records edges against this entry — matches
	// restactions.go:180-182.
	resCtx := cache.WithL1KeyContext(ctx, key)
	resCtx = cache.WithPIPStageTimingSink(resCtx, stageTimingSink)
	// #102 GTTL-1 (arch Option A, uniform): install the error-aware Put-gate
	// sinks so the seed honors the SAME backstop the refresher does
	// (resolve_populate.go:207-285). A swallowed/continueOnError'd STAGE error
	// (resolve returns nil-overall with degraded bytes) or a genuine EXTERNAL
	// endpoint touch (no dep edge to invalidate it) must NOT be Put — on the
	// keepwarm sweep that would blind-overwrite a good warm entry with degraded
	// bytes (the GTTL-1 defect); on boot it would serve under-populated content
	// on first paint. The gate keys on error PRESENCE, never on result
	// emptiness (a legitimately-empty result produces no stage error → sink==0
	// → still Put). Uniform across boot + sweep (feedback_no_special_cases).
	resCtx, stageErrSink := cache.WithStageErrorSink(resCtx)
	resCtx, extTouchedSink := cache.WithExternalTouchedSink(resCtx)
	// 1.12.3 A-1 / R-1 — install the UAF-touched sink, third sibling of the two
	// above, so the tail's Put-gate sees a refilter that ran ANYWHERE under this
	// resolve. The pre-resolve declaration skip above already caught an RA that
	// declares a UAF stage itself; this catches the one it cannot see — a seeded
	// RA that NESTS a UAF RA. Threaded into the tail through resCtx (the seam
	// signature is unchanged; the tail reads it back off the context).
	resCtx, _ = cache.WithUAFTouchedSink(resCtx)

	// Resolve + encode + GTTL-gate + Put TAIL (via the seedRestactionResolveAndPutFn
	// seam — prod default is seedRestactionResolveAndPutProd). The #113 templated-
	// endpointRef skip above short-circuits BEFORE this tail, so a templated RA
	// never reaches the Put.
	return seedRestactionResolveAndPutFn(ctx, resCtx, &cr, ref, authnNS, key, handle, inputs, got, stageErrSink, extTouchedSink)
}

// seedRestactionResolveAndPutProd is the production resolve+encode+GTTL-gate+Put
// tail of seedOneRestaction, extracted verbatim behind the
// seedRestactionResolveAndPutFn seam. Behaviour is byte-identical to the inlined
// tail; the only change is that it is now reachable/observable through a seam.
func seedRestactionResolveAndPutProd(
	ctx, resCtx context.Context,
	cr *templatesv1.RESTAction,
	ref templatesv1.ObjectReference,
	authnNS, key string,
	handle cacheHandle,
	inputs *cache.ResolvedKeyInputs,
	got objects.Result,
	stageErrSink *cache.StageErrorSink,
	extTouchedSink *cache.ExternalTouchedSink,
) error {
	res, err := restactions.Resolve(resCtx, restactions.ResolveOptions{
		In: cr,
		// Ship 0.30.230 fix-at-root: SArc is the SA *rest.Config carried
		// on ctx by withCohortSeedContext upstream. Threading it here
		// ensures the inner endpointReferenceMapper has a non-nil rc for
		// the `<user>-clientconfig` Secret fetch.
		SArc:    rcFromCtx(resCtx),
		AuthnNS: authnNS,
		PerPage: -1,
		Page:    -1,
	})
	if err != nil {
		return fmt.Errorf("resolve RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		return fmt.Errorf("encode RESTAction %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	// #102 GTTL-1: decline the Put (return nil, best-effort — NOT an error, so
	// the engine seed loop's PROCESSED-not-SUCCEEDED latch decrement is
	// unaffected and readyz never hangs) when the resolve observed a stage
	// error or touched an external endpoint. Keeps the prior good entry (if
	// any); TTL is the outer net.
	if declineSeedPutOnError(ctx, "restactions", ref.Namespace+"/"+ref.Name, key, stageErrSink, extTouchedSink) {
		return nil
	}

	// #118 (d) interim — THE THIRD PUT SITE (arch gate on 3783e65). The boot
	// seed Puts a UAF restactions cell under a cohort REPRESENTATIVE identity
	// (proactive_ra_seed enumerates ALL RBAC-reachable RESTActions incl. UAF
	// composition-list RAs), but `inputs` came from dispatchCacheLookupKey which
	// NEVER sets HasUAF (only the customer path does). Without this stamp a
	// boot-seeded UAF cell is governed by the LONG standard TTL AND stays
	// un-capped across refresher re-Puts (HasUAF not carried) until a customer
	// /call overwrites it — exactly the seeded/warm-without-a-fresh-dispatch
	// population (d) exists to cap. Set HasUAF from the typed CR (`cr`, converted
	// above) and stamp the short override via the SAME single-source helper the
	// customer + refresher Puts use, so all THREE Put sites cap identically
	// (C-118-6 extended to the seed). uafTTLOverrideForEntry returns 0 (no
	// override) when the knob is unset or the RA has no UAF stage → byte-identical
	// to today for a non-UAF seed cell and when disabled.
	if inputs != nil {
		inputs.HasUAF = restactionHasUAFStage(cr)
	}
	// 1.12.3 A-1 / R-1 — the POST-RESOLVE decline, and NOT merely defensive.
	// Two distinct cases reach it:
	//   - the DECLARATION case, which seedOneRestaction already short-circuited
	//     before the resolve, so in production it arrives here only when a caller
	//     drives this REASSIGNABLE SEAM (seedRestactionResolveAndPutFn) directly;
	//   - the OBSERVED case, which the pre-resolve skip structurally cannot
	//     catch: a seeded RA that declares no userAccessFilter of its own but
	//     NESTS one. That is only knowable after resolving, and this is the only
	//     gate standing between it and a per-binding cell holding one cohort
	//     member's narrowing.
	// The sink is read back off resCtx (installed by seedOneRestaction), so the
	// seam signature is unchanged and a direct caller that supplies its own resCtx
	// is gated by whatever it installed.
	if reason := uafDeclineReason(inputs, cache.UAFTouchedSinkFromContext(resCtx)); reason != "" {
		declineUAFPut(inputs, cache.UAFTouchedSinkFromContext(resCtx))
		slog.Default().Info("phase1.seed.skip.user_access_filter",
			slog.String("subsystem", "cache"),
			slog.String("class", "restactions"),
			slog.String("restaction", ref.Namespace+"/"+ref.Name),
			slog.String("site", "resolve_and_put_tail"),
			slog.String("uaf_reason", reason),
			slog.String("effect", "declining the seed Put: this resolve is userAccessFilter-narrowed (1.12.3 A-1). A declared UAF RA normally skips before the resolve; an RA that only NESTS a UAF RA can be caught nowhere but here."),
		)
		return nil
	}
	// Put under the per-user key — exactly the shape restactions.go
	// :212-216 puts under at serve time.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON:      encoded,
		Inputs:       inputs,
		SeededAtBoot: true, // #130 F3 seed-attribution: this cell was warmed by the boot seed
		TTLOverride:  uafTTLOverrideForEntry(inputs),
	})
	// counters-hygiene 2026-07-04: this success Put is a seed UNIT resolved +
	// written to per-user L1 — the real meaning of
	// snowplow_phase1_bindingset_seed_resolves_total. Its only historical
	// incrementer (runPIPSeed) was deleted in the prewarm-family fold, leaving
	// the counter dead-at-0 (which made a 287-real-seed boot look like "seed
	// didn't run"). Wired here + at seedOneWidget's success Put so the counter
	// again means "seed units resolved+Put".
	pipBindingSetSeedResolvesTotal.Add(1)
	// #105 seeded-set: a genuine Put is the primary "warm target reached this
	// boot" event — record it (same site as the resolves counter, §As-built).
	// Keyed on the full per-cohort `key`, so a genuinely-new cohort reaching this
	// target GROWS the seeded SET (forward progress); a TTL re-Put of an
	// already-member key is a no-op Mark (no growth) so it correctly reads as "no
	// net progress." nil-set-safe off the boot path.
	cache.BootSeededSetFromContext(ctx).Mark(key)

	// Record the self-dep + ensure the informer for the RESTAction GVR
	// is wired (AC-PIP.5 — without this the refresher never wakes for
	// the seeded entry; falsifier #5 triggers). Matches
	// restactions.go:229-230.
	ensureWatcherInformerForGVR(got.GVR)
	cache.Deps().Record(key, got.GVR, got.Unstructured.GetNamespace(), got.Unstructured.GetName())
	return nil
}

// seedOneWidget resolves ONE navigation widget under the cohort ctx
// and Puts the resolved JSON into the per-user widgets L1 under the
// dispatcher's canonical key. STRUCTURALLY MATCHES widgets.go:148-231:
//
//   - dispatchCacheLookupKey("widgets", group, version, resource, ns,
//     name, KeyPerPage, KeyPage, nil) with the DISPATCHER-LOOKUP key
//     tuple (Ship 0.30.187 D2 decoupling) so cohort A's first /call with
//     no URL slice params hits the SAME cell as the seed Put. Pre-D2
//     this used the RESOLUTION tuple (prewarmPageLimit()) which never
//     matched the dispatcher's paginationInfo default of (-1, -1) and
//     caused the 0.30.186 14/17 first-nav-hit defect.
//   - cache.WithL1KeyContext(ctx, key) before Resolve so the inner-call
//     dep tracker records edges.
//   - widgets.Resolve at widgets.go:187-193 (same entrypoint). The
//     RESOLUTION tuple (e.PerPage, e.Page) stays bounded by
//     prewarmPageLimit() — the 0.30.127 storm guard. For no-slice
//     navigation widgets the resolved output is structurally invariant
//     under pagination (no row fan-out at the top widget level — row
//     data flows from declared-slice child resourcesRefs which carry
//     their own URL-matching pagination).
//   - encodeResolvedJSON + cacheHandle.Put + recordWidgetDeps —
//     matches widgets.go:215-231 (recordWidgetDeps calls
//     ensureWatcherInformerForGVR for the widget GVR + apiRef GVR +
//     each resourcesRefs GVR, satisfying AC-PIP.5 for widgets).
func seedOneWidget(ctx context.Context, e navWidgetEntry, authnNS string, mode seedScopeMode) error {
	if e.W == nil {
		return nil
	}

	// inline-extras design P §5 / MUST-FIX #1 — PIP-seed key parity at the
	// per-cohort widget cell. The dispatcher keys this cell on the UNION of
	// both inline maps + request (§1); the seed has NO request extras, so the
	// union degenerates to unionForKey(apiRefInline, rrtInline, nil) read off
	// the widget CR (e.W). Without this fold the seed would key on extras=nil
	// while the dispatcher keys on the non-empty union → every widget
	// declaring inline extras MISSES its prewarmed cell and resolves cold on
	// first paint (the #317 seed/serve key-mismatch class). The seed BODY
	// parity is AUTOMATIC: widgets.Resolve reads the inline maps off in.Object
	// (the seeded CR) itself (resolveApiRef §4.1 + the §4.2 fold), so no
	// ResolveOptions.Extras field is needed — only this KEY arg is computed
	// outside Resolve and must be hand-threaded. Absent both blocks ⇒ {} + {}
	// ⇒ a fresh empty union ⇒ byte-identical to the pre-inline-extras nil arg
	// (HG-PIP.3 backward-compat). Falsifier #6 asserts the resulting HIT.
	// A1: route through the shared effectiveKeyExtras (definitive-architecture
	// §2.2) — identical to the pre-A1 unionForKey(apiRefInline, rrtInline, nil)
	// today (injection slot inert), so the seed key stays byte-identical to the
	// dispatcher key it must match. A2 wires declaredIdentityForKey; the seed ctx
	// carries the cohort identity (WithUserInfo) so a declared widget's seed key
	// will fold the same identity a live request folds.
	seedKeyExtras := effectiveKeyExtras(ctx, e.W.Object, nil)

	// Ship 0.30.187 D2: the dispatcher-lookup key uses the KEY tuple
	// (KeyPerPage, KeyPage) — derived from the /call Path the walker
	// reached this widget through so the cell matches the dispatcher's
	// serve-time paginationInfo. The resolution tuple (e.PerPage,
	// e.Page) is still used for widgets.Resolve below (the 0.30.127
	// storm guard).
	key, handle, inputs := dispatchCacheLookupKey(ctx, "widgets",
		e.GVR.Group, e.GVR.Version, e.GVR.Resource,
		e.W.GetNamespace(), e.W.GetName(),
		e.KeyPerPage, e.KeyPage, seedKeyExtras)
	// Ship 0.30.188 — diagnostic slog: emit the widget seed Put cache
	// key + components so it can be diff'd against the dispatcher_get
	// and per_user_fallback_put log lines at widgets.go.
	emitDispatchCacheKeyDiag(slog.Default(), "seed", ctx,
		key, inputs, "widgets",
		e.GVR.Group, e.GVR.Version, e.GVR.Resource,
		e.W.GetNamespace(), e.W.GetName(),
		e.KeyPerPage, e.KeyPage, seedKeyExtras)
	if handle == nil || key == "" {
		// L1 disabled or no identity — same defensive skip as
		// seedOneRestaction.
		return nil
	}
	// #42 FIX-C (A4 security finding, populate side) — mirror of
	// seedOneRestaction: skip the shared empty-identity cell Put when the
	// cohort re-derived a first-match BindingUID of "" (EvaluateRBAC deny/err
	// fail-closed). See seedOneRestaction for the full rationale; serve-side
	// treatment is task #95.
	if inputs != nil && inputs.BindingUID == "" {
		// The cohort identity is on ctx (WithUserInfo) and is emitted by the
		// emitDispatchCacheKeyDiag "seed" line just above; this skip line keys
		// on the widget + class (the diag line pairs the identity to it).
		slog.Default().Info("phase1.seed.skip.empty_binding",
			slog.String("subsystem", "cache"),
			slog.String("class", "widgets"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.String("effect", "cohort re-derived first-match BindingUID=\"\" (RBAC deny/err fail-closed); "+
				"skipping the shared empty-identity cell Put (A4 populate-side guard)"),
		)
		return nil
	}

	// Per-mode seed skip (F.4 boot fresh-skip + keepwarm c2 age-skip) — mirror of
	// seedOneRestaction, via the SHARED seedSkipDecision (single derivation site).
	// boot = bare liveness (F.4); keepwarm = age-skip (c2 §4.2); gvr-discovered =
	// never skip (F4-C3). Freshness/age both read the store's OWN TTL-expiry Get
	// consuming the SAME `key` derived above. BEFORE enterSeedUnit so a skip never
	// consumes the #46 admission. The unpaginated seedRAFullListForWidget
	// accelerator is skipped along with the widget resolve — correct, because a
	// live widget cell implies its first paginated /call already found (or will
	// lazily rebuild) the pinned full-list.
	if seedSkipDecision(ctx, mode, handle, key, "widgets", e.W.GetNamespace()+"/"+e.W.GetName(), "") {
		return nil
	}

	// #46: bound this seed unit's footprint (semaphore admission + per-unit
	// HeapInuse assert), AFTER the identity short-circuit so the customer
	// /call path is untouched. fold 2026-07-03: enterSeedUnit is now the
	// ADAPTIVE gate (serialize against live GOMEMLIMIT headroom). seedOneWidget
	// is the engine seed's shared primitive; this bracket covers both
	// widgets.Resolve AND seedRAFullListForWidget (the unpaginated full-list —
	// the seed's dominant allocation). Transparent when GOMEMLIMIT is unset.
	seedRelease, seedErr := enterSeedUnit(ctx, "widget/"+e.W.GetNamespace()+"/"+e.W.GetName())
	if seedErr != nil {
		return seedErr // ctx cancelled while blocked on the bound
	}
	defer seedRelease()

	// DeepCopy the widget CR — widgets.Resolve mutates its In object
	// (sets status.widgetData etc.). The harvester already DeepCopied
	// once, but the SAME copy is fed to N cohort goroutines; we MUST
	// give each cohort its own *unstructured to avoid the
	// shared-vs-copy-is-a-concurrency-change defect
	// (feedback_shared_vs_copy_is_a_concurrency_change.md).
	in := e.W.DeepCopy()

	// Ship 0.30.193 Checkpoint 1 — install per-widget PIP timing sink.
	// Mirrors the restaction shape at lines 802-813: sink lives for the
	// duration of THIS widget's resolve; the deferred log emits a
	// phase1.seed.widget.timing line with widget identity + total
	// wall-clock + stages (the widget's apiref phase re-enters
	// restactions.Resolve which itself appends per-stage entries to the
	// SAME sink, so per-restaction stage breakdowns flow through here).
	//
	// SINK ISOLATION (feedback_shared_vs_copy_is_a_concurrency_change):
	// one sink per seedOneWidget invocation; never shared across
	// widgets or cohorts.
	stageTimingSink := cache.NewPIPStageTimingSink()
	widgetStart := time.Now()
	defer func() {
		snapshot := stageTimingSink.Snapshot()
		slog.Default().Info("phase1.seed.widget.timing",
			slog.String("subsystem", "cache"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.String("gvr", e.GVR.String()),
			slog.Int64("elapsed_ms_total", time.Since(widgetStart).Milliseconds()),
			slog.Int("stages_total", len(snapshot)),
			slog.Any("stages", snapshot),
		)
	}()

	resCtx := cache.WithL1KeyContext(ctx, key)
	resCtx = cache.WithPIPStageTimingSink(resCtx, stageTimingSink)
	// #102 GTTL-1 (arch Option A, uniform) — see seedOneRestaction: the
	// error-aware Put-gate sinks make the seed honor the refresher's backstop
	// so the keepwarm sweep never blind-re-Puts degraded bytes over a warm cell.
	resCtx, stageErrSink := cache.WithStageErrorSink(resCtx)
	resCtx, extTouchedSink := cache.WithExternalTouchedSink(resCtx)
	// 1.12.3 A-1 / R-1 — install the UAF-touched sink around the WIDGET resolve.
	// This is the seed leg of the hot carrier: widgets.Resolve → resolveApiRef →
	// apiref.Resolve → restactions.Resolve folds the apiRef'd RA's refiltered
	// rows into status.widgetData, and the boot seed writes that under a
	// per-BINDING widgets key held by a cohort REPRESENTATIVE — so every co-bound
	// cohort member would read the representative's narrowing on their first
	// paint.
	//
	// WHY THERE IS NO PRE-RESOLVE SKIP HERE, unlike seedOneRestaction: the widget
	// CR declares no userAccessFilter of its own, and the apiRef chain is not
	// statically enumerable (nested and template-expanded), so "does this widget
	// reach a UAF RA?" is not answerable before resolving. The seed therefore
	// resolves and then declines. That costs the resolve; it is the price of the
	// carrier being transitive.
	resCtx, uafTouchedSink := cache.WithUAFTouchedSink(resCtx)

	res, err := widgets.Resolve(resCtx, widgets.ResolveOptions{
		In: in,
		// Ship 0.30.230 fix-at-root: RC is the SA *rest.Config carried
		// on ctx by withCohortSeedContext upstream. Threading it here
		// fixes the nil-rc crash at crdschema.ValidateObjectStatus →
		// cache.GVRFor → discoverPluralInfo (the four-revert root cause).
		RC:      rcFromCtx(resCtx),
		AuthnNS: authnNS,
		PerPage: e.PerPage,
		Page:    e.Page,
	})
	if err != nil {
		return fmt.Errorf("resolve widget %s/%s: %w", e.W.GetNamespace(), e.W.GetName(), err)
	}

	encoded, err := encodeResolvedJSON(res)
	if err != nil {
		return fmt.Errorf("encode widget %s/%s: %w", e.W.GetNamespace(), e.W.GetName(), err)
	}

	// #102 GTTL-1: decline the Put (best-effort, return nil) on a stage error
	// or external touch — keeps any prior good entry; TTL is the outer net.
	if declineSeedPutOnError(ctx, "widgets", e.W.GetNamespace()+"/"+e.W.GetName(), key, stageErrSink, extTouchedSink) {
		return nil
	}

	// 1.12.3 A-1 / R-1 (SECURITY, cross-tenant) — decline the widget seed Put when
	// the resolve ran a userAccessFilter refilter. Placed with the other two
	// decline gates, before the Put and therefore before the dep Record, the
	// seeded-set Mark and the 4a full-list pin below — none of which should fire
	// for a cell that is not being written.
	if declineWidgetUAFPut(inputs, uafTouchedSink) {
		slog.Default().Info("phase1.seed.skip.user_access_filter",
			slog.String("subsystem", "cache"),
			slog.String("class", "widgets"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.Int64("uaf_touches", uafTouchedSink.Count()),
			slog.String("uaf_reason", uafDeclineObserved),
			slog.String("effect", "widget resolve ran a userAccessFilter refilter via its apiRef RESTAction: the body is "+
				"narrowed for the cohort REPRESENTATIVE, but the widgets cell is keyed per BINDING, so a co-bound member "+
				"would be served the representative's rows. Skipping the Put, the dep Record and the 4a full-list pin "+
				"(1.12.3 A-1/R-1); these widgets resolve per request until 1.13.0 folds the UAF scope into the key."),
		)
		return nil
	}

	// scope-waiver:TTLOverride: seedOneWidget — widgets-class boot seed. 1.12.3 A-1/R-1 CORRECTED WAIVER: the pre-1.12.3 text claimed "UAF is a restactions-STAGE contract, so a widget's apiRef-resolved UAF RA warms the restactions cell, never this widgets cell". That was WRONG and it was the R-1 blocker — widgets/resolve.go folds the apiRef'd RA's UAF-refiltered output into status.widgetData, which IS this cell. A refilter-touched widget can no longer REACH this Put (the UAFTouchedSink gate immediately above declines it), so every cell written here is refilter-free and needs no UAF cap.
	handle.Put(key, &cache.ResolvedEntry{
		RawJSON:      encoded,
		Inputs:       inputs,
		SeededAtBoot: true, // #130 F3 seed-attribution: this cell was warmed by the boot seed
	})
	// counters-hygiene 2026-07-04 — see seedOneRestaction: this success Put is
	// a seed UNIT resolved+written; wired so snowplow_phase1_bindingset_seed_resolves_total
	// again means "seed units resolved+Put" (was dead-at-0 post-fold).
	pipBindingSetSeedResolvesTotal.Add(1)
	// #105 seeded-set — mirror of seedOneRestaction: record this genuine widget
	// Put into the boot-convergence seeded SET (keyed on the full per-cohort key).
	cache.BootSeededSetFromContext(ctx).Mark(key)

	// Record widget deps — self + apiRef + render-eligible
	// resourcesRefs. Matches widgets.go:230. recordWidgetDeps ensures
	// the informer for every recorded GVR is wired (AC-PIP.5 / falsifier
	// #5).
	recordWidgetDeps(slog.Default(), key, e.GVR, res)

	// Ship 4a (0.30.198) — prewarm + PIN the page-independent RAFullList
	// cell for this (widget→RESTAction × cohort). The cell survives LRU
	// thrash (resident region) so the cohort's FIRST paginated /call hits a
	// warm full-list and is served as a cheap Go-slice — the zero-cold-nav
	// requirement (feedback_zero_cold_navigations_hard_requirement). Best-
	// effort: a prewarm error is log-only and never fails the widget seed
	// (the cohort's per-user widget cell above already seeded; RAFullList is
	// an accelerator). NON-FATAL by design.
	// #42 FIX-G: seedRAFullListForWidget consults the caller's OWN per-key
	// sliceability verdict via the RA-CR-coordinate derivation (shared with
	// raFullListServe — G2-A), reading the cohort identity from resCtx. No
	// bindingUID is threaded (the eg1 defect was threading the WIDGET-coordinate
	// bindingUID, which key-diverged from the RA-CR verdict → G inert).
	//
	// #130 F4 Option 2 — SKIP the 4a full-list pin when THIS widget resolves
	// UNPAGINATED (apiref.IsPaginatedResolve(e.PerPage, e.Page) false). The 4a
	// pin exists to warm a sliceable full-list for a subsequent PAGINATED /call
	// (shouldServeRAFullList engages only when perPage>0 && page>0). An
	// unpaginated-consuming widget (a Statistic/Tag widget counting `list:`
	// wholesale) resolves at (0,0)/(−1,−1), so its own /call never engages the
	// slice serve — the pin's second full ~18s benchapps materialization is pure
	// waste. Reuses the EXACT serve-gate pagination predicate (one derivation
	// site, no drift) and is DATA-DERIVED (reads the resolve tuple, never
	// widget-kind — feedback_no_special_cases / C-F4-7). A paginated widget is
	// byte-unchanged (still pins). Emits a greppable skip line for the falsifier.
	if !apiref.IsPaginatedResolve(e.PerPage, e.Page) {
		slog.Default().Info("phase1.seed.rafulllist.skip_unpaginated",
			slog.String("subsystem", "cache"),
			slog.String("widget", e.W.GetNamespace()+"/"+e.W.GetName()),
			slog.Int("perpage", e.PerPage),
			slog.Int("page", e.Page),
			slog.String("effect", "F4 Option 2 — widget consumes the list unpaginated; the 4a full-list "+
				"pin would never be slice-served on its own /call, so the redundant unpaginated re-resolve is skipped"),
		)
		return nil
	}
	seedRAFullListForWidget(resCtx, in, authnNS, e.W.GetNamespace(), e.W.GetName())
	return nil
}

// seedRAFullListForWidget prewarms + pins the page-independent RAFullList
// cell for a widget's underlying RESTAction, under the cohort ctx — Ship 4a
// (0.30.198). It resolves the widget's apiRef at a PAGINATED tuple
// (prewarmPageLimit, page 1) so apiref.Resolve engages raFullListServe,
// which: resolves the RA UNPAGINATED, byte-verifies sliceability for the
// apiRef shape, Puts the full cell (pinned when the cost predicate fires —
// envelope bytes ≥ threshold), and records the verdict. Reusing the serve
// path means ZERO duplicated slice/verify/pin logic and guarantees the
// prewarmed cell is byte-identical to what the first /call would build.
//
// Best-effort: the function swallows errors (log-only). A widget with no
// apiRef (e.g. a static-data widget) is a no-op. CACHE off → apiref.Resolve's
// raFullListServe nil-checks the cache and returns served=false → the resolve
// is a harmless extra read (only reached when ResolvedCacheEnabled, see the
// guard below). The whole block is gated under cache.ResolvedCacheEnabled()
// so a cache-off process never runs the extra resolve.
func seedRAFullListForWidget(ctx context.Context, w *unstructured.Unstructured, authnNS, ns, name string) {
	if !cache.ResolvedCacheEnabled() {
		return
	}
	apiRef, err := widgets.GetApiRef(w.Object)
	if err != nil || apiRef.Name == "" || apiRef.Namespace == "" {
		// No apiRef (static widget) or unparseable — nothing to prewarm.
		return
	}

	// inline-extras design P §5 / MUST-FIX #1 — PIP-seed key parity at the
	// RAFullList sub-cell. The dispatcher's apiRef path keys this sub-cell on
	// the apiRef-EFFECTIVE map (merge(apiRefInline, request)) via
	// apiref.Resolve → RAFullListKeyInputs (ra_full_list.go). The seed has no
	// request extras, so the effective map degenerates to the apiRef-inline
	// map read off the widget CR. Threading it into apiref.Resolve's Extras
	// below makes the seed's RAFullList key fold the SAME apiRef-inline map the
	// dispatcher's first /call will → seed-key == serve-key on this sub-cell.
	// The resourcesRefsTemplateExtras map is correctly NOT folded here — it
	// does not affect the apiRef fetch (§1). Absent ⇒ {} ⇒ byte-identical to
	// the pre-inline-extras nil Extras (extrasMinusSlice({}) → nil → no fold).
	// Computed BEFORE the pre-check so FIX-G's per-key raKey folds the SAME
	// extras the RAFullList cell keys on (raKey parity).
	apiRefInline := widgets.GetApiRefExtras(w.Object)

	// #42 FIX-B (shape-level) + FIX-G (per-key) — SKIP the whole prewarm resolve
	// when this apiRef→RESTAction is already known NOT sliceable, so the
	// discarded resolve #3 (design §A1; ~4.7s/identity on estate-graph, ~140s/
	// rank across the un-skipped heavy list widgets) never runs. On a
	// not-sliceable RA, apiref.Resolve's raFullListServe returns served=false and
	// Resolve falls through to a page-keyed resolve whose result THIS function
	// discards — pure waste. One objects.Get to derive the RA is cheap vs that
	// resolve. TWO checks, both drift-free (derived by the apiref helpers that
	// share raFullListServe's exact key+shape derivation):
	//   FIX-B: SHAPE-level negative — structurally non-sliceable under ANY
	//     identity (permanent-only, FIX-A shape set). Covers identities #2..N
	//     once the FIRST identity's first-sight records the permanent verdict.
	//   FIX-G: THIS identity's OWN PER-KEY verdict (permanent OR NOT) — recorded
	//     by the widgets.Resolve first-sight moments ago in THIS seedOneWidget
	//     call. Catches the NON-permanent list-shaped negatives FIX-B's
	//     permanent-only shape set does not, per-identity. G2-A: the per-key
	//     raKey is derived by SeedFullListPerKeyKnownNonSliceable off the RA-CR
	//     COORDINATES (via the shared seedFullListRAKey, reading identity from
	//     ctx) — the SAME derivation raFullListServe records under, so no
	//     producer/consumer key divergence (the eg1 defect was keying off the
	//     WIDGET's coordinate bindingUID). ctx carries the cohort identity.
	// The FIRST identity (both unknown) still runs the full first-sight that
	// RECORDS the verdict → nothing under-seeded; only later work skips.
	if got := objects.Get(ctx, apiRef); got.Err == nil && got.Unstructured != nil {
		var ra templatesv1.RESTAction
		if cerr := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(
			got.Unstructured.Object, &ra); cerr == nil {
			shapeNeg := apiref.SeedFullListShapeKnownNonSliceable(got.GVR, apiRef.Namespace, apiRef.Name, &ra)
			perKeyNeg := apiref.SeedFullListPerKeyKnownNonSliceable(ctx, got.GVR, apiRef.Namespace, apiRef.Name, &ra, apiRefInline)
			if shapeNeg || perKeyNeg {
				reason := "shape-negative (FIX-A/B, structurally non-sliceable under any identity)"
				if !shapeNeg {
					reason = "per-key negative (FIX-G, this identity's own first-sight verdict — non-permanent list shape)"
				}
				slog.Default().Info("phase1.seed.rafulllist.skip_nonsliceable",
					slog.String("subsystem", "cache"),
					slog.String("widget", ns+"/"+name),
					slog.String("apiref", apiRef.Namespace+"/"+apiRef.Name),
					slog.Bool("shape_negative", shapeNeg),
					slog.Bool("per_key_negative", perKeyNeg),
					slog.String("effect", "known non-sliceable ("+reason+"); "+
						"skipping the discarded fallback resolve (design §A1 resolve #3 elimination)"),
				)
				return
			}
		}
	}
	// Resolve at a paginated tuple so raFullListServe engages (it requires
	// perPage>0 && page>0). page 1 + prewarmPageLimit is sufficient: the
	// byte-verify + pin are per-(RA × shape), NOT per-page, so this single
	// prewarm populates+verifies+pins the cell for EVERY subsequent (page,
	// perPage) /call by this cohort. The result is discarded (we only want
	// the cell-populating side-effect).
	pp := prewarmPageLimit()
	if pp <= 0 {
		pp = 1
	}
	if _, rerr := apiref.Resolve(ctx, apiref.ResolveOptions{
		ApiRef: apiRef,
		// Ship 0.30.230 fix-at-root: thread the SA rc explicitly so the
		// downstream restactions.ResolveOptions SArc field chain inside
		// apiref.Resolve carries a non-nil rc. The ctx upstream
		// (seedOneWidget's resCtx) already carries it via
		// withCohortSeedContext / WithInternalRESTConfig — this makes
		// the option-struct propagation explicit and matches the rest
		// of the construction-site fixes.
		RC:      rcFromCtx(ctx),
		AuthnNS: authnNS,
		PerPage: pp,
		Page:    1,
		// inline-extras P §5 — fold the apiRef-inline map so the RAFullList
		// key matches the dispatcher's apiRef-effective key (seed has no
		// request extras). NOT the union — rrt-inline must not key this cell.
		Extras: apiRefInline,
	}); rerr != nil {
		slog.Default().Warn("phase1.seed.rafulllist.skipped",
			slog.String("subsystem", "cache"),
			slog.String("widget", ns+"/"+name),
			slog.String("apiref", apiRef.Namespace+"/"+apiRef.Name),
			slog.Any("err", rerr),
			slog.String("effect", "RAFullList prewarm skipped for this (widget,cohort); first /call cold-resolves + pins lazily"),
		)
	}
}

// declineSeedPutOnError is the #102 GTTL-1 error-aware Put-gate for the shared
// seed primitives — the seed-side mirror of the refresher's gate
// (resolve_populate.go:251-285). It returns true (decline the Put) when the
// seed re-resolve observed a swallowed/continueOnError'd STAGE error or touched
// a genuine EXTERNAL endpoint, in which case the caller MUST skip handle.Put
// and return nil (best-effort — NOT an error, so the engine seed loop's
// PROCESSED-not-SUCCEEDED latch decrement is unaffected and readyz never hangs).
//
// WHY (arch Option A, uniform across boot + keepwarm sweep): a stage-errored
// resolve produces degraded/under-populated bytes; a Put would (on the keepwarm
// sweep) blind-overwrite a good warm entry — the GTTL-1 defect — or (on boot)
// serve under-served content on first paint. An external touch has no dep edge
// to invalidate it. The prior good entry (if any) is kept; TTL is the outer net
// (§1.6). The gate keys on error PRESENCE, never on result emptiness — a
// legitimately-empty result produces no stage error (sink Count()==0) → NOT
// declined → its empty bytes ARE stored (mirrors resolve_populate.go:248-250).
//
// The identity for the log is read from ctx (withCohortSeedContext installs
// WithUserInfo) so both primitives share one helper without threading a label.
func declineSeedPutOnError(ctx context.Context, class, target, key string,
	stageErrSink *cache.StageErrorSink, extTouchedSink *cache.ExternalTouchedSink) bool {

	if stageErrSink.Count() > 0 {
		pipSeedSkippedStageErrorTotal.Add(1)
		// Back-compat parity with the refresher's process-wide counter so
		// existing "how many degraded re-Puts were declined" dashboards see the
		// seed declines too (the seed-scoped counter above disambiguates source).
		cache.BumpRefresherSkippedStageError()
		stage, sampleErr := stageErrSink.Sample()
		slog.Default().Info("phase1.seed.skip.stage_error",
			slog.String("subsystem", "cache"),
			slog.String("class", class),
			slog.String("target", target),
			slog.String("identity", seedIdentityLabelFromCtx(ctx)),
			slog.Int64("stage_errors", stageErrSink.Count()),
			slog.String("stage_err_stage", stage),
			slog.String("stage_err_sample", sampleErr),
			slog.String("effect", "seed re-resolve observed a swallowed stage error; declining the Put "+
				"(keeps any prior good entry; TTL is the outer net) — GTTL-1 backstop, uniform with the refresher"),
		)
		return true
	}
	if extTouchedSink.Count() > 0 {
		pipSeedSkippedStageErrorTotal.Add(1)
		cache.BumpExternalSkippedPut()
		// #132 F4b Lever A — mark this EXACT key resolved-and-declined-external for
		// the boot scope, so a resume pass's seedSkipDecision skips re-resolving it
		// (breaks the §3 loop). ONLY the external branch marks (NOT stage_error): a
		// stage error is transient/operational and SHOULD be retried on resume,
		// whereas an external touch is the structural "will decline every time"
		// case with no dep edge to ever warm it. Set is nil off the boot seed ctx
		// (keepwarm/gvr-discovered/user-/call never install one), so Mark is a
		// nil-receiver no-op there — this can never make a keepwarm sweep or a
		// /call skip a resolve (C-F4B-3). Keyed by the full dispatchCacheLookupKey
		// the Put/Get pair uses (C-F4B-2). If #129 later gives external widgets a
		// warmable TTL cell, this decline stops firing and the marker self-retires.
		cache.SeedDeclinedExternalSetFromContext(ctx).Mark(key)
		slog.Default().Info("phase1.seed.skip.external_touch",
			slog.String("subsystem", "cache"),
			slog.String("class", class),
			slog.String("target", target),
			slog.String("identity", seedIdentityLabelFromCtx(ctx)),
			slog.Int64("external_touches", extTouchedSink.Count()),
			slog.String("effect", "seed re-resolve touched an external endpoint (no dep edge to invalidate); "+
				"declining the Put — GTTL-1 backstop, uniform with the refresher"),
		)
		return true
	}
	return false
}

// seedIdentityLabelFromCtx renders the seeding identity from the cohort ctx
// (withCohortSeedContext installs WithUserInfo). Username else first group else
// "anonymous" — mirrors cohortLogLabel's domain for log parity.
func seedIdentityLabelFromCtx(ctx context.Context) string {
	ui, err := xcontext.UserInfo(ctx)
	if err != nil {
		return "anonymous"
	}
	if ui.Username != "" {
		return ui.Username
	}
	if len(ui.Groups) > 0 {
		return "group:" + ui.Groups[0]
	}
	return "anonymous"
}

// cohortLogLabel renders a cohort into a stable log/metric label. The
// label is used in structured log fields AND as the expvar map key for
// the per-cohort counters; it MUST be stable across pod restarts (which
// EnumerateRBACCohorts's sort ordering guarantees).
//
// User-kind cohort: the canonical Username (e.g. "system:admin",
// "alice@example.com"). Group-kind cohort: "group:" + the group name.
// A cohort with neither (defensive — should never happen post-enum)
// falls back to "anonymous".
func cohortLogLabel(c seedTarget) string {
	if c.Username != "" {
		return c.Username
	}
	if len(c.Groups) > 0 {
		return "group:" + c.Groups[0]
	}
	return "anonymous"
}
