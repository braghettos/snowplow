// phase1_walk.go — 0.30.102 Tag B Part 1 (recursive walker added in
// 0.30.105): the startup SA-credentialed resolution walk that warms the
// navigation-reached informers.
//
// This file lives in the dispatchers package because the widget /
// RESTAction resolution machinery (widgets.Resolve, the api resolver's
// lazyRegisterInnerCallPaths hook) is reachable from here without an
// import cycle. The cache package owns the Phase1Done state, the
// meta-query seed budget, and the CRD-watch (Part 2); main.go owns the
// startup wiring.
//
// THE WALK (Part 1, recursive as of 0.30.105):
//   1. LIST every navigation-root widget CR cluster-wide (SA dyn client).
//      There are TWO roots — `routesloaders` and `navmenus` — both
//      entry-point widget CRs in the widgets.templates.krateo.io group.
//      They are the frontend's two `/call` login entry points
//      (ROUTES_LOADER + INIT).
//   2. Recursively resolve the navigation widget tree under the snowplow
//      service-account identity through the STANDARD widget resolver.
//      Each resolved widget returns `status.resourcesRefs.items[]`; every
//      item whose `verb == "GET"` is itself a `/call?...` widget endpoint
//      — the walker fetches that child widget CR and recurses into it.
//      Recursion proceeds Root -> Route -> Page -> Row/Column ->
//      DataGrid/Table leaf. A visited-set keyed on the child widget
//      endpoint dedupes shared subtrees and prevents cycles.
//
//      WHY verb == "GET" ONLY (load-bearing — correctness AND safety):
//      a non-GET resourcesRefs item is a mutation/action endpoint
//      (POST/PUT/PATCH/DELETE) bound to a widget's `actions`, never part
//      of the navigation/render tree. Following one would (a) walk an
//      edge that is not navigation and (b) — the SA walk runs with
//      privileged service-account credentials — issue a DESTRUCTIVE
//      apiserver mutation. The walk MUST stay strictly read-only.
//
//      WHY the `allowed` flag is NOT a recursion gate: it is snowplow's
//      typed-RBAC evaluator (EvaluateRBAC) keyed on the REQUEST USER
//      identity against the Krateo Role/RoleBinding CRs — NOT the
//      apiserver's RBAC. The Phase 1 SA-walk context carries no Krateo
//      RBAC CRs, so EvaluateRBAC default-denies and every child resolves
//      allowed=false. Gating on it prunes the whole tree at the first
//      Route and the composition informer never registers. Phase 1 is
//      identity-independent informer DISCOVERY, not per-user rendering;
//      the `allowed` render gate belongs at real request time. See
//      walkShouldRecurse for the full rationale.
//   3. As the RESTAction resolver processes inner-call paths (fired when
//      the recursion reaches an apiRef-bearing leaf such as the
//      Compositions Page DataGrid), the flag-independent
//      lazyRegisterInnerCallPaths hook auto-registers an informer for
//      every GVR the inner-call path touches AND feeds the CRD-watch's
//      auto-discover group set (e.g. composition.krateo.io). Informer
//      registration is a free side-effect — no separate GVR-collection
//      step.
//   4. The resolution OUTPUT is DISCARDED — Phase 1 is discovery-only.
//      It does NOT populate L1 (that is Phase 2, deferred). The resolver
//      mutates the in-memory CR copy but never persists status back to
//      the apiserver.
//
// CRITICAL — feedback_no_special_cases.md: Phase 1 seeds ONLY from the
// two resolved navigation roots. There is NO configured widget-GVR list
// and NO configured RESTAction list. RESTActions + downstream GVRs are
// discovered purely by recursively resolving the routesloaders / navmenus
// roots — an orphan RESTAction wired to no navigation page is never
// reached and never registers an informer. The ONLY named GVRs are the
// two entry-point roots; they ARE the frontend config contract.
//
// BEHAVIOR-NEUTRAL — the whole walk runs only when cache.PrewarmEnabled()
// (PREWARM_ENABLED=true). main.go does not call Phase1Warmup otherwise.

package dispatchers

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"github.com/krateoplatformops/plumbing/maps"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// snowplowSAUsername is the identity Phase 1 resolves under. It is the
// snowplow service account — the same identity dynamic.ServiceAccountEndpoint
// authenticates as. The resolver's RESTAction inner-call path keys its
// per-user clientconfig lookup on the username; Phase 1 overrides that
// lookup via cache.WithInternalEndpoint so no `<sa>-clientconfig` Secret
// is required.
//
// Per feedback_no_special_cases.md: this is an IDENTITY label, not a
// per-resource policy — it names WHO Phase 1 resolves as, exactly as a
// real request names its user.
const snowplowSAUsername = "snowplow-serviceaccount"

// rootsLister abstracts the cluster-wide LIST of the navigation-root CRs
// so the no-hardcode falsifier test can substitute an in-memory inventory
// without a cluster. Production lists BOTH routesloaders and navmenus.
type rootsLister func(ctx context.Context) ([]*unstructured.Unstructured, error)

// rootResolver abstracts resolving a single navigation-root CR (and, in
// production, recursively walking its widget subtree). Production passes
// resolveNavigationRoot (the standard widget resolver + recursive
// walker); the falsifier tests substitute a stub that drives the same
// informer-registration side effects deterministically.
type rootResolver func(ctx context.Context, root *unstructured.Unstructured) error

// Phase1Warmup runs the Tag B Part 1 SA-credentialed recursive resolution
// walk, then blocks on the Phase 1 sync barrier and signals
// cache.Phase1Done.
//
// Sequence:
//   - register the 8 meta-query seeds (routesloaders / navmenus /
//     restactions / customresourcedefinitions + the 4 RBAC GVRs);
//   - start the CRD-watch (Part 2) so composition informers spawn as
//     their CRDs are observed for navigation-discovered groups;
//   - LIST every routesloaders AND navmenus CR and recursively resolve
//     each navigation tree under SA identity — the resolution
//     auto-registers an informer per touched GVR;
//   - let the registered set settle (the CRD-watch may still be adding
//     composition informers after the walk's last resolve);
//   - WaitAllInformersSynced — block until every registered informer
//     (including the CRD-watch-spawned composition informer) is synced;
//   - cache.MarkPhase1Done — flips the /readyz gate to 200.
//
// ctx bounds the whole walk + sync barrier (main.go gives it the
// startupProbe budget). On ctx cancellation Phase1Warmup still calls
// MarkPhase1Done in its caller — a pod that cannot warm in the budget
// is better Ready-and-degraded than CrashLoop; main.go owns that
// decision. Phase1Warmup itself returns the first error encountered.
//
// MUST be called only when cache.PrewarmEnabled() — main.go enforces
// this. Calling it with the cache disabled / passthrough is a no-op.
func Phase1Warmup(ctx context.Context, rc *rest.Config, authnNS string) error {
	log := slog.Default()
	rw := cache.Global()
	if rw == nil {
		log.Info("phase1.warmup.skipped",
			slog.String("subsystem", "cache"),
			slog.String("reason", "cache disabled — no informer factory to warm"),
		)
		return nil
	}

	saEP, saErr := idynamic.ServiceAccountEndpoint()
	if saErr != nil {
		log.Warn("phase1.warmup.no_sa_endpoint",
			slog.String("subsystem", "cache"),
			slog.Any("err", saErr),
			slog.String("effect", "Phase 1 cannot resolve under SA identity; lazy register-on-navigation still covers every GVR on first request"),
		)
		return saErr
	}

	dynCli, dynErr := k8sdynamic.NewForConfig(rc)
	if dynErr != nil {
		log.Warn("phase1.warmup.no_dyn_client",
			slog.String("subsystem", "cache"),
			slog.Any("err", dynErr),
		)
		return dynErr
	}

	lister := func(lctx context.Context) ([]*unstructured.Unstructured, error) {
		return listNavigationRoots(lctx, dynCli)
	}
	resolver := func(rctx context.Context, root *unstructured.Unstructured) error {
		return resolveNavigationRoot(rctx, root, *saEP, rc, authnNS)
	}

	return phase1WarmupWith(ctx, rw, lister, resolver)
}

// phase1WarmupWith is the testable core: it takes the watcher, the
// navigation-roots lister, and the per-root resolver as injected
// dependencies. Production wires the real cluster-backed versions;
// the no-hardcode + premature-Ready falsifier tests wire in-memory
// stubs.
//
// It NEVER calls cache.MarkPhase1Done on the error path before the sync
// barrier — Phase1Done must reflect a truly-warm informer set
// (premature-Ready falsifier). MarkPhase1Done is called exactly once,
// after WaitAllInformersSynced returns, regardless of walk errors:
// informer registration already happened, so the registered set IS what
// /readyz should gate on. A walk error (one root failed to resolve) is
// returned for logging but does not by itself withhold readiness — the
// other roots' informers are warm.
func phase1WarmupWith(ctx context.Context, rw *cache.ResourceWatcher, lister rootsLister, resolve rootResolver) error {
	log := slog.Default()
	start := time.Now()

	// Step 1 — register the hardcoded meta-query seeds. This is the ONLY
	// place a hardcoded GVR is handed to the watcher at startup.
	rw.RegisterMetaQuerySeeds()

	// Step 2 — start the CRD-watch. Composition informers spawn as the
	// CRD informer replays existing CRDs (boot) and on CRD-add, but only
	// for groups Phase 1's walk has fed into the auto-discover set.
	rw.StartCRDWatch(ctx)

	// Step 3 — LIST the navigation roots (routesloaders + navmenus).
	roots, listErr := lister(ctx)
	if listErr != nil {
		log.Warn("phase1.warmup.roots_list_failed",
			slog.String("subsystem", "cache"),
			slog.Any("err", listErr),
			slog.String("effect", "no roots to walk; lazy register-on-navigation still covers GVRs on first request"),
		)
		// No roots — still run the sync barrier over whatever the
		// meta-query seeds + CRD-watch registered, then signal done.
		_ = rw.WaitAllInformersSynced(ctx)
		cache.MarkPhase1Done()
		return listErr
	}

	log.Info("phase1.warmup.roots_discovered",
		slog.String("subsystem", "cache"),
		slog.Int("roots_count", len(roots)),
	)

	// Step 4 — recursively resolve each navigation root under SA identity.
	// The resolution's inner-call walk auto-registers an informer per
	// touched GVR (lazyRegisterInnerCallPaths) and feeds the CRD-watch
	// auto-discover set. Output discarded. Resolution errors are
	// collected, not fatal: one broken root must not block warming the
	// rest.
	var walkErr error
	resolved := 0
	for _, root := range roots {
		if ctx.Err() != nil {
			walkErr = ctx.Err()
			break
		}
		if err := resolve(ctx, root); err != nil {
			log.Warn("phase1.warmup.root_resolve_failed",
				slog.String("subsystem", "cache"),
				slog.String("root", rootKey(root)),
				slog.Any("err", err),
			)
			if walkErr == nil {
				walkErr = err
			}
			continue
		}
		resolved++
	}

	// Step 5 — reconcile the CRD-watch against the now-complete
	// auto-discover set. The walk discovers composition groups (e.g.
	// composition.krateo.io) AFTER StartCRDWatch's CRD informer has
	// already replayed every existing CRD — so the composition CRD was
	// seen with matchesAutoDiscoverGroup==false and dropped. Now that the
	// walk has finished, the auto-discover set is complete; a single CRD
	// store re-scan registers every composition informer whose CRD was
	// replayed too early. Idempotent for CRDs already registered live.
	reconciled := rw.ReconcileAutoDiscoverCRDs()
	if reconciled > 0 {
		log.Info("phase1.warmup.crd_reconcile",
			slog.String("subsystem", "cache"),
			slog.Int("newly_registered", reconciled),
		)
	}

	// Step 6 — let the registered set settle. The CRD-watch may still be
	// adding composition informers after the reconcile (the per-GVR
	// EnsureResourceType + the informer's initial LIST run
	// asynchronously). Poll RegisteredGVRs until it stops growing for one
	// settle window, bounded by ctx.
	settleRegisteredSet(ctx, rw)

	// Step 7 — the Phase 1 sync barrier. Block until every registered
	// informer (meta-query seeds + resolution-discovered + CRD-watch-
	// spawned) reaches HasSynced, bounded by ctx.
	syncErr := rw.WaitAllInformersSynced(ctx)

	// Step 8 — signal Phase1Done. /readyz flips to 200.
	cache.MarkPhase1Done()

	log.Info("phase1.warmup.completed",
		slog.String("subsystem", "cache"),
		slog.Int("roots_total", len(roots)),
		slog.Int("roots_resolved", resolved),
		slog.Int("informers_registered", rw.RegisteredCount()),
		slog.Int64("elapsed_ms", time.Since(start).Milliseconds()),
		slog.Bool("sync_ok", syncErr == nil),
	)

	if walkErr != nil {
		return walkErr
	}
	return syncErr
}

// phase1SettleWindow is how long RegisteredGVRs must stay constant
// before the walk's registered set is considered settled.
const phase1SettleWindow = 750 * time.Millisecond

// phase1SettlePoll is the poll cadence for the settle check.
const phase1SettlePoll = 150 * time.Millisecond

// settleRegisteredSet polls the watcher's registered-GVR count until it
// holds steady for phase1SettleWindow, or ctx is cancelled. This lets
// the CRD-watch's asynchronous per-GVR registrations land before the
// sync barrier snapshots the informer set.
func settleRegisteredSet(ctx context.Context, rw *cache.ResourceWatcher) {
	last := rw.RegisteredCount()
	stableSince := time.Now()
	t := time.NewTicker(phase1SettlePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := rw.RegisteredCount()
			if now != last {
				last = now
				stableSince = time.Now()
				continue
			}
			if time.Since(stableSince) >= phase1SettleWindow {
				return
			}
		}
	}
}

// rootKey renders a navigation-root CR's namespace/name for logging.
func rootKey(root *unstructured.Unstructured) string {
	if root == nil {
		return "<nil>"
	}
	ns := root.GetNamespace()
	if ns == "" {
		return root.GetName()
	}
	return ns + "/" + root.GetName()
}

// navigationRootListPageLimit bounds each page of a navigation-root LIST
// so the apiserver does not stream an unbounded response — mirrors the
// listPageLimit policy the informer factory uses.
const navigationRootListPageLimit int64 = 500

// listNavigationRoots LISTs every navigation-root CR cluster-wide via the
// SA dynamic client: BOTH the routesloaders GVR and the navmenus GVR
// (0.30.105). A LIST error on one root GVR does not suppress the other —
// the per-GVR error is returned only if BOTH fail; a partial result is
// still walkable.
func listNavigationRoots(ctx context.Context, dynCli k8sdynamic.Interface) ([]*unstructured.Unstructured, error) {
	gvrs := []schema.GroupVersionResource{
		cache.RoutesLoadersGVR(),
		cache.NavMenusGVR(),
	}
	var (
		out     []*unstructured.Unstructured
		lastErr error
		okAny   bool
	)
	for _, gvr := range gvrs {
		items, err := listAllOfGVR(ctx, dynCli, gvr)
		if err != nil {
			slog.Warn("phase1.warmup.root_gvr_list_failed",
				slog.String("subsystem", "cache"),
				slog.String("gvr", gvr.String()),
				slog.Any("err", err),
			)
			lastErr = err
			continue
		}
		okAny = true
		out = append(out, items...)
	}
	if !okAny && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// listAllOfGVR LISTs every CR of one GVR cluster-wide via the SA dynamic
// client, paging with a bounded limit.
func listAllOfGVR(ctx context.Context, dynCli k8sdynamic.Interface, gvr schema.GroupVersionResource) ([]*unstructured.Unstructured, error) {
	var (
		out           []*unstructured.Unstructured
		continueToken string
	)
	for {
		list, err := dynCli.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			Limit:    navigationRootListPageLimit,
			Continue: continueToken,
		})
		if err != nil {
			return nil, err
		}
		for i := range list.Items {
			item := list.Items[i]
			out = append(out, &item)
		}
		continueToken = list.GetContinue()
		if continueToken == "" {
			break
		}
	}
	return out, nil
}

// phase1MaxWalkDepth bounds the recursive widget-tree descent. The portal
// navigation tree is shallow (Root -> Route -> Page -> Row/Column ->
// DataGrid/Table is ~5 levels); this cap is a defensive guard against a
// pathological CR graph that the visited-set somehow fails to dedupe. It
// is NOT a per-resource policy — it is a uniform recursion-safety bound.
const phase1MaxWalkDepth = 32

// resolveNavigationRoot resolves one navigation-root CR through the
// STANDARD widget resolver under the snowplow SA identity, then
// RECURSIVELY walks the resolved widget tree (0.30.105). The resolved
// output is discarded — Phase 1 is discovery-only. The resolution's side
// effects (informer auto-registration via lazyRegisterInnerCallPaths,
// CRD-watch group feed) are the whole point.
//
// The SA credentials are injected so the standard resolver runs unchanged
// under SA identity:
//   - xcontext.WithUserConfig(saEP): the endpoint shape the resolver
//     expects on the context (it carries the SA apiserver URL).
//   - xcontext.WithUserInfo({snowplow SA}): the resolver requires an
//     identity on the context.
//   - cache.WithInternalEndpoint(&saEP): the RESTAction resolver's
//     non-UAF inner-call endpoint resolution returns the SA endpoint
//     instead of looking up a (non-existent) `<sa>-clientconfig` Secret.
//   - cache.WithInternalRESTConfig(saRC): the LOAD-BEARING 0.30.103 fix.
//     saRC is the SA *rest.Config built by main.go from
//     rest.InClusterConfig — it carries the SA bearer token and CA with
//     the correct in-cluster semantics. The resolver's object-fetch sites
//     (objects.Get, resourcesrefs.Resolve) use it verbatim via
//     cache.ClientConfigFor instead of rebuilding a client from saEP
//     through kubeconfig.NewClientConfig.
func resolveNavigationRoot(ctx context.Context, root *unstructured.Unstructured, saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(saEP),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: snowplowSAUsername}),
		xcontext.WithLogger(slog.Default()),
	)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)

	w := &phase1Walker{
		authnNS:    authnNS,
		visited:    map[string]struct{}{},
		gvrSamples: map[string]int{},
	}
	return w.walk(rctx, root, 0)
}

// phase1PerGVRSampleLimit caps how many widget CRs of the SAME GVR the
// walk resolves+recurses into across the whole walk. It is the key
// 0.30.105 data-fan-out bound.
//
// WHY (the on-cluster finding): a Compositions Page DataGrid yields one
// resourcesRefs child PER COMPOSITION ROW — at production scale that is
// ~49K children, every one a `githubscaffoldingwithcompositionpage`
// widget of the SAME GVR. Recursing into all of them turns the startup
// walk into a 49K-deep per-composition resolution storm that blows the
// PHASE1_TIMEOUT_SECONDS budget (the first on-cluster 0.30.105 bench
// CrashLooped the pod). But for INFORMER DISCOVERY every row-widget of a
// given GVR is identical — resolving one fires the exact same
// lazyRegisterInnerCallPaths apiRef chain as resolving the 49000th. So
// the walk samples a SMALL bounded number per distinct child GVR: enough
// to traverse genuine navigation structure (each distinct widget GVR is
// still visited) while skipping the per-row data fan-out.
//
// The limit is >1 so a parent that legitimately has a couple of
// different-purpose children sharing a GVR is not under-covered; it is
// small because additional same-GVR widgets discover no new informer.
const phase1PerGVRSampleLimit = 4

// phase1Walker carries the per-root recursive-walk state. A fresh walker
// is created per navigation root so the dedupe state never crosses roots
// — but because the two roots can share Page subtrees, dedupe WITHIN a
// root is what matters for cycle-safety; cross-root re-resolves are
// harmless (idempotent informer registration) and rare.
type phase1Walker struct {
	authnNS string
	// visited dedupes by the child widget endpoint (resource+apiVersion+
	// namespace+name) so a shared subtree is resolved once and a cyclic
	// reference cannot loop forever.
	visited map[string]struct{}
	// gvrSamples counts how many widget CRs of each GVR (resource+
	// apiVersion) the walk has already resolved. Once a GVR hits
	// phase1PerGVRSampleLimit, further siblings of that GVR are skipped —
	// the data-fan-out bound (see phase1PerGVRSampleLimit).
	gvrSamples map[string]int
}

// walk resolves widget `in` through the standard widget resolver under
// the SA-credentialed ctx, then recurses into every resolved
// `status.resourcesRefs.items[]` child whose verb == "GET" (and which is
// allowed). Resolution side effects (informer registration) are the
// point; the resolved output is read only to discover children, never
// persisted.
//
// Errors are NON-FATAL and not propagated upward past the immediate
// resolve: a single broken child widget must not abort warming the rest
// of the navigation tree. The function returns an error ONLY for the
// top-level root resolve failure, so the caller (phase1WarmupWith) can
// log a root as failed.
func (w *phase1Walker) walk(ctx context.Context, in *unstructured.Unstructured, depth int) error {
	log := slog.Default()
	if in == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if depth > phase1MaxWalkDepth {
		log.Warn("phase1.walk.max_depth",
			slog.String("subsystem", "cache"),
			slog.Int("depth", depth),
			slog.String("widget", rootKey(in)),
			slog.String("effect", "recursion capped — deeper navigation widgets covered by lazy register-on-navigation"),
		)
		return nil
	}

	// Resolve this widget. The resolver recursively reaches this widget's
	// apiRef RESTAction (firing lazyRegisterInnerCallPaths on any
	// apiRef-bearing leaf such as the Compositions Page DataGrid) and
	// returns status.resourcesRefs.items[] — the child widget endpoints.
	// PerPage/Page = -1: Phase 1 wants the FULL navigation fan-out, not
	// just the first visible page.
	res, err := widgets.Resolve(ctx, widgets.ResolveOptions{
		In:      in,
		AuthnNS: w.authnNS,
		PerPage: -1,
		Page:    -1,
	})
	if err != nil {
		// A resolution error at depth>0 is non-fatal — log and stop
		// descending this branch. At depth 0 the caller treats a non-nil
		// return as a failed root.
		if depth == 0 {
			return err
		}
		log.Warn("phase1.walk.child_resolve_failed",
			slog.String("subsystem", "cache"),
			slog.Int("depth", depth),
			slog.String("widget", rootKey(in)),
			slog.Any("err", err),
		)
		return nil
	}
	if res == nil {
		return nil
	}

	// Read status.resourcesRefs.items[] — the child widget endpoints.
	children := extractResourcesRefsItems(res.Object)
	for _, child := range children {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// SAFETY + CORRECTNESS gate: recurse ONLY into verb=="GET" refs
		// that carry a path. walkShouldRecurse is the single auditable
		// predicate — see its doc for why verb=="GET" is the load-bearing
		// read-only invariant and why `allowed` is deliberately NOT a
		// gate here (the SA walk is RBAC-denied on every nav widget).
		if !walkShouldRecurse(child) {
			continue
		}

		ref, ok := parseCallPathToObjectRef(child.Path)
		if !ok {
			// A child path that is not a /call?... widget endpoint
			// (external link, malformed) — nothing to recurse into.
			continue
		}
		key := navWidgetEndpointKey(ref)
		if _, seen := w.visited[key]; seen {
			continue
		}

		// DATA-FAN-OUT BOUND: skip this child once its GVR has already
		// been sampled phase1PerGVRSampleLimit times. A Compositions Page
		// DataGrid yields one child per composition row — ~49K children
		// of the same GVR — and resolving every one would blow the
		// PHASE1_TIMEOUT budget. Every same-GVR row-widget fires the
		// identical lazyRegisterInnerCallPaths apiRef chain, so a small
		// sample saturates informer discovery for that GVR. Distinct
		// navigation-structure GVRs are each still visited up to the
		// limit. See phase1PerGVRSampleLimit.
		gvrKey := ref.APIVersion + "|" + ref.Resource
		if w.gvrSamples[gvrKey] >= phase1PerGVRSampleLimit {
			continue
		}

		w.visited[key] = struct{}{}
		w.gvrSamples[gvrKey]++

		// Fetch the child widget CR under the SA-credentialed ctx. The
		// resolver mutates the object in place, so a fresh fetch per
		// child is required.
		got := objects.Get(ctx, ref)
		if got.Err != nil {
			log.Warn("phase1.walk.child_fetch_failed",
				slog.String("subsystem", "cache"),
				slog.Int("depth", depth),
				slog.String("child", key),
				slog.Any("err", got.Err),
			)
			continue
		}
		if got.Unstructured == nil {
			continue
		}
		// Recurse into the child widget subtree.
		_ = w.walk(ctx, got.Unstructured, depth+1)
	}
	return nil
}

// navChildRef is the subset of a resolved status.resourcesRefs item the
// walker needs: the navigation edge to a child widget. It mirrors the
// templatesv1.ResourceRefResult fields (id/path/verb/allowed) — the same
// shape the frontend ResourceRef carries.
type navChildRef struct {
	ID      string
	Path    string
	Verb    string
	Allowed bool
}

// walkShouldRecurse is the single, auditable predicate the recursive
// walk applies before descending into a resourcesRefs child.
//
// THE LOAD-BEARING GATE — verb == "GET" (case-insensitive):
//
//	A non-GET resourcesRefs item is a mutation/action endpoint
//	(POST/PUT/PATCH/DELETE) bound to a widget's `actions`, never part of
//	the navigation/render tree. Recursing into it is wrong navigation —
//	AND, because the Phase 1 walk runs with the snowplow service
//	account's PRIVILEGED credentials, following such a ref would issue a
//	DESTRUCTIVE apiserver mutation. verb == "GET" alone fully guarantees
//	the walk stays strictly read-only: a GET is non-destructive
//	regardless of any RBAC verdict.
//
// WHY `allowed` is DELIBERATELY NOT a recursion gate (0.30.105
// on-cluster finding):
//
//	The `allowed` flag a resolved resourcesRefs item carries is set by
//	resourcesrefs.resolveOne via rbac.UserCan -> EvaluateRBAC — snowplow's
//	OWN typed-RBAC evaluator, keyed on the REQUEST USER IDENTITY
//	(username + groups extracted from the context) against the
//	informer-cached Krateo Role/RoleBinding CRs. It is NOT the apiserver's
//	RBAC and NOT the walk identity's apiserver permissions.
//
//	The Phase 1 walk's context identity is the snowplow service account.
//	That identity HAS broad apiserver get/list/watch (so objects.Get
//	below succeeds) — but it carries no Krateo Role/RoleBinding CRs, so
//	EvaluateRBAC evaluates an effectively-empty subject and default-denies:
//	every navigation child resolves allowed=false. Gating the recursion on
//	allowed==true therefore prunes the ENTIRE tree at the first Route
//	level: the walk never reaches the Compositions Page DataGrid and the
//	composition.krateo.io informer is never registered (exactly the
//	0.30.104 failure this release exists to fix; the first 0.30.105
//	on-cluster bench reproduced it when this gate was applied).
//
//	The frontend WidgetRenderer applies items.filter(({allowed})=>allowed)
//	because a denied widget must not RENDER for that logged-in user. But
//	Phase 1 is informer DISCOVERY, not rendering — informer registration
//	is identity-independent: the composition informer the Compositions
//	Page needs is the same object set no matter which user can see it.
//	The walk must discover the full GET-navigation STRUCTURE; the
//	per-user `allowed` render gate is correctly applied later, at real
//	request time, not during startup warmup. Dropping `allowed` here
//	does NOT weaken the read-only guarantee — verb == "GET" is the sole
//	safety invariant and it is independent of RBAC.
//
// Also requires a non-empty path — nothing to fetch/recurse into
// otherwise.
func walkShouldRecurse(child navChildRef) bool {
	return strings.EqualFold(child.Verb, "GET") && child.Path != ""
}

// extractResourcesRefsItems reads status.resourcesRefs.items[] from a
// resolved widget object and returns the navigation child refs. The
// resolver stores items as a []any of map[string]any (the marshalled
// ResourceRefResult slice); this reads them defensively without coupling
// to the resolver's internal marshalling.
func extractResourcesRefsItems(obj map[string]any) []navChildRef {
	items, ok, err := maps.NestedSlice(obj, "status", "resourcesRefs", "items")
	if !ok || err != nil {
		return nil
	}
	out := make([]navChildRef, 0, len(items))
	for _, raw := range items {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		ref := navChildRef{}
		if v, ok := m["id"].(string); ok {
			ref.ID = v
		}
		if v, ok := m["path"].(string); ok {
			ref.Path = v
		}
		if v, ok := m["verb"].(string); ok {
			ref.Verb = v
		}
		if v, ok := m["allowed"].(bool); ok {
			ref.Allowed = v
		}
		out = append(out, ref)
	}
	return out
}

// parseCallPathToObjectRef parses a `/call?resource=...&apiVersion=...&
// name=...&namespace=...` widget endpoint into the ObjectReference the
// objects.Get fetch needs. Returns ok=false for any path that is not a
// /call widget endpoint (external link, missing resource/apiVersion).
//
// This mirrors the frontend recursion contract: every navigation child's
// `path` is itself a /call?... widget endpoint. It is NOT a hardcoded
// resource/path special-case — it is the generic /call query-param
// decoder, the same params util.ParseGVR / util.ParseNamespacedName read
// off a real HTTP request.
func parseCallPathToObjectRef(path string) (templatesv1.ObjectReference, bool) {
	u, err := url.Parse(path)
	if err != nil {
		return templatesv1.ObjectReference{}, false
	}
	// Only a /call endpoint carries a widget CR. The trimmed path must
	// end in "/call" (it may be host-qualified or root-relative).
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed != "" && !strings.HasSuffix(trimmed, "/call") {
		return templatesv1.ObjectReference{}, false
	}
	q := u.Query()
	resource := q.Get("resource")
	apiVersion := q.Get("apiVersion")
	if resource == "" || apiVersion == "" {
		return templatesv1.ObjectReference{}, false
	}
	return templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      q.Get("name"),
			Namespace: q.Get("namespace"),
		},
		Resource:   resource,
		APIVersion: apiVersion,
	}, true
}

// navWidgetEndpointKey renders an ObjectReference into the stable dedupe
// key the visited-set is keyed on.
func navWidgetEndpointKey(ref templatesv1.ObjectReference) string {
	return ref.APIVersion + "|" + ref.Resource + "|" + ref.Namespace + "|" + ref.Name
}
