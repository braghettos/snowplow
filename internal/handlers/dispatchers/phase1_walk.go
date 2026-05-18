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
// THE WALK (Part 1, recursive as of 0.30.105; ConfigMap-derived roots as
// of 0.30.107):
//   1. READ the navigation roots from the frontend ConfigMap (NOT a
//      hardcoded GVR LIST). The frontend ConfigMap's `config.json`
//      declares the two `/call` entry points the frontend itself
//      dispatches on login — `.api.INIT` and `.api.ROUTES_LOADER`. Phase
//      1 parses each `/call?resource=...&apiVersion=...&name=...&
//      namespace=...` URL into an ObjectReference and fetches those EXACT
//      two widget CRs as the navigation roots. The resource names
//      (`navmenus`, `routesloaders`) appear NOWHERE as Go literals — they
//      arrive at runtime from config.json. If the frontend changes its
//      INIT widget, Phase 1 follows automatically. See phase1_roots.go.
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
//      WHY the `allowed` flag is NOT a recursion gate: Phase 1 walks the
//      FULL GET-navigation structure for informer DISCOVERY — informer
//      registration is identity-independent (the composition informer the
//      Compositions Page needs is the same object set no matter which user
//      can see it). The per-user `allowed` RENDER gate (which widgets to
//      show a logged-in user) belongs at real request time, not startup
//      warmup. Note: Phase 1 resolves under the snowplow service account's
//      CANONICAL username (system:serviceaccount:<ns>:<name>, derived from
//      the SA token's JWT `sub` claim — 0.30.108), and that SA holds a
//      native ClusterRoleBinding granting `*/*` get/list/watch, so
//      EvaluateRBAC correctly AUTHORIZES it; `allowed` is therefore true
//      for the navigation widgets anyway. It is still not used as the
//      recursion gate because discovery must not depend on render
//      authorization at all. See walkShouldRecurse for the full rationale.
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
// discovered purely by recursively resolving the navigation roots — an
// orphan RESTAction wired to no navigation page is never reached and
// never registers an informer. The two roots themselves are NOT Go
// literals either: they are read from the frontend ConfigMap's
// config.json (.api.INIT / .api.ROUTES_LOADER) — the navigation contract.
// The ConfigMap pointer is config too (FRONTEND_CONFIG_CONFIGMAP env var
// + AUTHN_NAMESPACE). See phase1_roots.go.
//
// BEHAVIOR-NEUTRAL — the whole walk runs only when cache.PrewarmEnabled()
// (PREWARM_ENABLED=true). main.go does not call Phase1Warmup otherwise.

package dispatchers

import (
	"context"
	"log/slog"
	"strings"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/maps"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/handlers/util"
	"github.com/krateoplatformops/snowplow/internal/objects"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sdynamic "k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

// phase1SAUsername resolves the CANONICAL Kubernetes ServiceAccount
// username for the pod Phase 1 runs as — the form
// `system:serviceaccount:<namespace>:<name>`.
//
// WHY canonical (0.30.108 — the bug 0.30.105–107 misdiagnosed): the
// resolution-context identity flows into snowplow's RBAC evaluator
// (rbac.EvaluateRBAC). Its subject matcher (anySubjectMatches →
// parseServiceAccountUsername) can only match a ServiceAccount-kind RBAC
// subject when the username carries the `system:serviceaccount:` prefix.
// The snowplow SA genuinely holds a native ClusterRoleBinding granting
// `*/*` get/list/watch — but that binding's subject is ServiceAccount-kind
// (name + namespace). A bare label like "snowplow-serviceaccount" has no
// prefix, so parseServiceAccountUsername returns isSA=false, the
// ServiceAccount-kind subject can never fire, and EvaluateRBAC DENIES a
// fully-authorized SA. The fix is to pass the canonical form so the
// evaluator can connect Phase 1's identity to the SA's real binding.
//
// DERIVATION (no hardcoded ns/name literals — feedback_no_special_cases.md):
// the in-cluster projected SA token is a JWT whose `sub` claim is EXACTLY
// `system:serviceaccount:<ns>:<name>`. Phase 1 already loads that token
// (idynamic.ServiceAccountEndpoint puts the raw JWT in saEP.Token), so we
// decode `sub` from it via jwtutil.ExtractUserInfo — the canonical
// username arrives verbatim from the runtime identity, never a Go literal.
// The pod's namespace and SA name are NOT named in code; they are whatever
// the apiserver minted into the token snowplow runs with.
//
// Returns ("", false) when the token is absent or its `sub` is not a
// canonical ServiceAccount username; the caller logs and proceeds (Phase 1
// is best-effort warmup — see resolveNavigationRoot).
func phase1SAUsername(saToken string) (string, bool) {
	if saToken == "" {
		return "", false
	}
	ui, err := jwtutil.ExtractUserInfo(saToken)
	if err != nil {
		return "", false
	}
	if _, _, isSA := splitCanonicalSAUsername(ui.Username); !isSA {
		return "", false
	}
	return ui.Username, true
}

// splitCanonicalSAUsername reports whether u is a canonical
// `system:serviceaccount:<ns>:<name>` username and, if so, returns its ns
// and name. It mirrors rbac.parseServiceAccountUsername so phase1SAUsername
// can VERIFY the JWT-decoded subject is the canonical form the RBAC
// evaluator will actually match — keeping the two in lockstep without an
// import cycle.
func splitCanonicalSAUsername(u string) (string, string, bool) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(u, prefix) {
		return "", "", false
	}
	rest := u[len(prefix):]
	i := strings.Index(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

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
//   - READ the navigation roots from the frontend ConfigMap (config.json
//     .api.INIT / .api.ROUTES_LOADER) and recursively resolve each
//     navigation tree under SA identity — the resolution auto-registers
//     an informer per touched GVR;
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

	// The navigation-config namespace: snowplow's control-plane namespace
	// — the same authn-namespace it already runs in / authenticates
	// against. It is NOT a Go constant: it flows from the AUTHN_NAMESPACE
	// chart value via main.go's --authn-namespace flag.
	cfgNamespace := authnNS

	// listNavigationRootsFromConfigMap fetches the frontend ConfigMap and
	// the two named root CRs via objects.Get, which honours the
	// internal-dispatch context. So the lister runs under the same
	// SA-credentialed context the per-root resolver uses — built here
	// once via withPhase1SAContext.
	lister := func(lctx context.Context) ([]*unstructured.Unstructured, error) {
		return listNavigationRootsFromConfigMap(
			withPhase1SAContext(lctx, *saEP, rc), dynCli, cfgNamespace)
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

	// Step 3 — READ the navigation roots from the frontend ConfigMap
	// (config.json .api.INIT / .api.ROUTES_LOADER → the two named root
	// widget CRs). No hardcoded GVR LIST.
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
// withPhase1SAContext builds the SA-credentialed context Phase 1
// resolution runs under. It is the SINGLE place the SA identity +
// internal-dispatch markers are installed, shared by the navigation-root
// lister (the ConfigMap read + root-CR fetch) and resolveNavigationRoot
// (the per-root recursive walk) so both run under exactly one identity.
//
// The context it installs:
//   - xcontext.WithUserConfig(saEP)   — the endpoint shape the resolver
//     expects on the context.
//   - xcontext.WithUserInfo({canonical SA username}) — the identity. The
//     username is the CANONICAL `system:serviceaccount:<ns>:<name>` form
//     (0.30.108) so rbac.EvaluateRBAC's ServiceAccount-kind subject
//     matcher can connect Phase 1's identity to the snowplow SA's real
//     ClusterRoleBinding (`*/*` get/list/watch). Derived from the SA
//     token's JWT `sub` claim — see phase1SAUsername.
//   - cache.WithInternalEndpoint(&saEP) — the RESTAction resolver's
//     non-UAF inner-call endpoint resolution returns the SA endpoint
//     instead of looking up a (non-existent) `<sa>-clientconfig` Secret.
//   - cache.WithInternalRESTConfig(saRC) — the SA *rest.Config built by
//     main.go from rest.InClusterConfig; the resolver's object-fetch
//     sites (objects.Get, resourcesrefs.Resolve) use it verbatim instead
//     of rebuilding a client from saEP through kubeconfig.NewClientConfig
//     (the LOAD-BEARING 0.30.103 fix — the SA's raw-PEM CA cannot survive
//     the base64/cert-only kubeconfig path).
func withPhase1SAContext(ctx context.Context, saEP endpoints.Endpoint, saRC *rest.Config) context.Context {
	opts := []xcontext.WithContextFunc{
		xcontext.WithUserConfig(saEP),
		xcontext.WithLogger(slog.Default()),
	}
	// The CANONICAL SA username: derived from the JWT `sub` claim of the
	// projected SA token saEP already carries. Without it (token absent /
	// malformed) Phase 1 still runs — it is best-effort warmup — but the
	// RBAC evaluator then has no SA subject to match; that degraded case
	// is logged at the call site (resolveNavigationRoot / the lister).
	if u, ok := phase1SAUsername(saEP.Token); ok {
		opts = append(opts, xcontext.WithUserInfo(jwtutil.UserInfo{Username: u}))
	}
	rctx := xcontext.BuildContext(ctx, opts...)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)
	// 0.30.111 Part 2 — mark this as a Phase-1 (startup-warmup)
	// resolution. This is the SOLE production setter of the marker (a
	// grep-able invariant): the RESTAction resolver's createRequestOptions
	// caps `dependsOn.iterator` fan-out at phase1IteratorCap only when
	// cache.IsPhase1Resolution(ctx) is true. Every real `/call` leaves
	// the marker absent.
	rctx = cache.WithPhase1Resolution(rctx)
	return rctx
}

func resolveNavigationRoot(ctx context.Context, root *unstructured.Unstructured, saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {
	rctx := withPhase1SAContext(ctx, saEP, saRC)

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
//
// STRUCTURAL ASSUMPTION (architect follow-up note): this bound's safety
// rests on no single parent widget having more than
// phase1PerGVRSampleLimit distinct same-GVR DataGrid/Table siblings. The
// gvrSamples counter is keyed on GVR alone, so siblings that share a GVR
// share one budget. Today the navigation tree never crosses that count,
// so every distinct-purpose widget is sampled. If a future navigation
// tree adds same-GVR sibling widgets beyond this count, an
// earlier-iterated sibling could exhaust the budget and a later
// distinct-purpose sibling (e.g. the Compositions DataGrid) could be
// skipped before its informer chain is discovered — at which point this
// limit MUST rise (or the counter must key on a finer identity than the
// bare GVR). Until then, 4 is a deliberate, navigation-shape-validated
// constant, not an arbitrary one.
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
		// gate here (Phase 1 is informer DISCOVERY, which is identity-
		// independent; the per-user `allowed` render gate belongs at real
		// request time).
		if !walkShouldRecurse(child) {
			continue
		}

		ref, ok := util.ParseCallPathToObjectRef(child.Path)
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
// WHY `allowed` is DELIBERATELY NOT a recursion gate:
//
//	The `allowed` flag a resolved resourcesRefs item carries is set by
//	resourcesrefs.resolveOne via rbac.UserCan -> EvaluateRBAC — snowplow's
//	in-process evaluator of NATIVE Kubernetes RBAC (Role / RoleBinding /
//	ClusterRole / ClusterRoleBinding) keyed on the resolution-context
//	identity. It is the same answer the apiserver's RBAC would give, just
//	served from the informer cache.
//
//	Phase 1 resolves under the snowplow service account's CANONICAL
//	username (system:serviceaccount:<ns>:<name>, derived from the SA
//	token's JWT `sub` claim — 0.30.108 — see phase1SAUsername). That SA
//	holds a native ClusterRoleBinding granting `*/*` get/list/watch, and
//	with the canonical username EvaluateRBAC's ServiceAccount-kind subject
//	matcher connects the identity to that binding — so EvaluateRBAC
//	correctly AUTHORIZES every navigation read and `allowed` is true for
//	the navigation widgets.
//
//	`allowed` is STILL not used as the recursion gate: Phase 1 is informer
//	DISCOVERY, and discovery is identity-independent — the composition
//	informer the Compositions Page needs is the same object set no matter
//	which user can see it. The walk must register the informer for the
//	full GET-navigation STRUCTURE regardless of any per-user render
//	verdict. The frontend WidgetRenderer applies
//	items.filter(({allowed})=>allowed) because a denied widget must not
//	RENDER for a logged-in user — that per-user render gate is correctly
//	applied later, at real request time, not during startup warmup.
//	Gating discovery on a render verdict would couple two concerns that
//	must stay independent. Dropping `allowed` here does NOT weaken the
//	read-only guarantee — verb == "GET" is the sole safety invariant and
//	it is independent of RBAC.
//
//	(HISTORICAL: 0.30.105 misdiagnosed this as "the SA walk is
//	RBAC-denied because it carries no Krateo RBAC CRs" — there are no
//	"Krateo RBAC CRs"; EvaluateRBAC evaluates native Kubernetes RBAC. The
//	actual 0.30.105–107 defect was a MALFORMED SA username
//	("snowplow-serviceaccount", no system:serviceaccount: prefix) that
//	parseServiceAccountUsername could not resolve, so the SA's real
//	ClusterRoleBinding never matched and a fully-authorized SA was
//	silently denied. 0.30.108 fixes the username; see phase1SAUsername.)
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

// parseCallPathToObjectRef was LIFTED to internal/handlers/util/callpath.go
// at Ship 0.30.123 (#155) — util.ParseCallPathToObjectRef — so the
// resolver package (which cannot import dispatchers) can share the same
// /call decoder. Call sites here now use util.ParseCallPathToObjectRef.

// navWidgetEndpointKey renders an ObjectReference into the stable dedupe
// key the visited-set is keyed on.
func navWidgetEndpointKey(ref templatesv1.ObjectReference) string {
	return ref.APIVersion + "|" + ref.Resource + "|" + ref.Namespace + "|" + ref.Name
}
