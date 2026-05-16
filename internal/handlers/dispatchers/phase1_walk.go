// phase1_walk.go — 0.30.102 Tag B Part 1: the startup SA-credentialed
// resolution walk that warms the navigation-reached informers.
//
// This file lives in the dispatchers package because the widget /
// RESTAction resolution machinery (widgets.Resolve, the api resolver's
// lazyRegisterInnerCallPaths hook) is reachable from here without an
// import cycle. The cache package owns the Phase1Done state, the
// meta-query seed budget, and the CRD-watch (Part 2); main.go owns the
// startup wiring.
//
// THE WALK (Part 1):
//   1. LIST every `routesloaders` widget CR cluster-wide (SA dyn client).
//   2. Resolve each through the STANDARD widget resolver under the
//      snowplow service-account identity. The resolution recursively
//      reaches every downstream RESTAction + widget the navigation
//      surface touches.
//   3. As the RESTAction resolver processes inner-call paths, the
//      flag-independent lazyRegisterInnerCallPaths hook auto-registers
//      an informer for every GVR the inner-call path touches AND feeds
//      the CRD-watch's auto-discover group set. Informer registration is
//      a free side-effect — no separate GVR-collection step.
//   4. The resolution OUTPUT is DISCARDED — Phase 1 is discovery-only.
//      It does NOT populate L1 (that is Phase 2, deferred). The resolver
//      mutates the in-memory CR copy but never persists status back to
//      the apiserver.
//
// CRITICAL — feedback_no_special_cases.md: Phase 1 seeds ONLY from the
// resolved routesloaders roots. There is NO configured widget-GVR list
// and NO configured RESTAction list. RESTActions + downstream GVRs are
// discovered purely by resolving the routesloaders roots — an orphan
// RESTAction wired to no routesloaders page is never reached and never
// registers an informer.
//
// BEHAVIOR-NEUTRAL — the whole walk runs only when cache.PrewarmEnabled()
// (PREWARM_ENABLED=true). main.go does not call Phase1Warmup otherwise.

package dispatchers

import (
	"context"
	"log/slog"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	idynamic "github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/resolvers/widgets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// routesLoadersLister abstracts the cluster-wide LIST of routesloaders
// CRs so the no-hardcode falsifier test can substitute an in-memory
// inventory without a cluster.
type routesLoadersLister func(ctx context.Context) ([]*unstructured.Unstructured, error)

// rootResolver abstracts resolving a single routesloaders CR. Production
// passes resolveRoutesLoaderRoot (the standard widget resolver);
// the falsifier test substitutes a stub that drives the same
// informer-registration side effects deterministically.
type rootResolver func(ctx context.Context, root *unstructured.Unstructured) error

// Phase1Warmup runs the Tag B Part 1 SA-credentialed resolution walk,
// then blocks on the Phase 1 sync barrier and signals cache.Phase1Done.
//
// Sequence:
//   - register the 7 meta-query seeds (routesloaders / restactions /
//     customresourcedefinitions + the 4 RBAC GVRs already present);
//   - start the CRD-watch (Part 2) so composition informers spawn as
//     their CRDs are observed for navigation-discovered groups;
//   - LIST every routesloaders CR and resolve each under SA identity —
//     the resolution auto-registers an informer per touched GVR;
//   - let the registered set settle (the CRD-watch may still be adding
//     composition informers after the walk's last resolve);
//   - WaitAllInformersSynced — block until every registered informer
//     (including the CRD-watch-spawned ones present at boot) is synced;
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
		return listRoutesLoaders(lctx, dynCli)
	}
	resolver := func(rctx context.Context, root *unstructured.Unstructured) error {
		return resolveRoutesLoaderRoot(rctx, root, *saEP, rc, authnNS)
	}

	return phase1WarmupWith(ctx, rw, lister, resolver)
}

// phase1WarmupWith is the testable core: it takes the watcher, the
// routesloaders lister, and the per-root resolver as injected
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
func phase1WarmupWith(ctx context.Context, rw *cache.ResourceWatcher, lister routesLoadersLister, resolve rootResolver) error {
	log := slog.Default()
	start := time.Now()

	// Step 1 — register the hardcoded meta-query seeds. This is the ONLY
	// place a hardcoded GVR is handed to the watcher at startup.
	rw.RegisterMetaQuerySeeds()

	// Step 2 — start the CRD-watch. Composition informers spawn as the
	// CRD informer replays existing CRDs (boot) and on CRD-add, but only
	// for groups Phase 1's walk has fed into the auto-discover set.
	rw.StartCRDWatch(ctx)

	// Step 3 — LIST the routesloaders roots.
	roots, listErr := lister(ctx)
	if listErr != nil {
		log.Warn("phase1.warmup.routesloaders_list_failed",
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
		slog.Int("routesloaders_count", len(roots)),
	)

	// Step 4 — resolve each root under SA identity. The resolution's
	// inner-call walk auto-registers an informer per touched GVR
	// (lazyRegisterInnerCallPaths) and feeds the CRD-watch auto-discover
	// set. Output discarded. Resolution errors are collected, not fatal:
	// one broken root must not block warming the rest.
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

	// Step 5 — let the registered set settle. The CRD-watch may still be
	// adding composition informers after the last resolve returned (its
	// informer's initial LIST + the per-GVR EnsureResourceType run
	// asynchronously). Poll RegisteredGVRs until it stops growing for one
	// settle window, bounded by ctx.
	settleRegisteredSet(ctx, rw)

	// Step 6 — the Phase 1 sync barrier. Block until every registered
	// informer (meta-query seeds + resolution-discovered + CRD-watch-
	// spawned) reaches HasSynced, bounded by ctx.
	syncErr := rw.WaitAllInformersSynced(ctx)

	// Step 7 — signal Phase1Done. /readyz flips to 200.
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

// rootKey renders a routesloaders CR's namespace/name for logging.
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

// routesLoadersListPageLimit bounds each page of the routesloaders LIST
// so the apiserver does not stream an unbounded response — mirrors the
// listPageLimit policy the informer factory uses.
const routesLoadersListPageLimit int64 = 500

// listRoutesLoaders LISTs every routesloaders CR cluster-wide via the SA
// dynamic client, paging with a bounded limit.
func listRoutesLoaders(ctx context.Context, dynCli k8sdynamic.Interface) ([]*unstructured.Unstructured, error) {
	gvr := cache.RoutesLoadersGVR()
	var (
		out           []*unstructured.Unstructured
		continueToken string
	)
	for {
		list, err := dynCli.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
			Limit:    routesLoadersListPageLimit,
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

// resolveRoutesLoaderRoot resolves one routesloaders CR through the
// STANDARD widget resolver under the snowplow SA identity. The resolved
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
//     through kubeconfig.NewClientConfig — which CANNOT carry the SA's
//     raw-PEM CA (it base64-decodes the CA field) nor the SA's bearer
//     token (the kubeconfig user block is cert-auth-only). Without saRC
//     the root fetch fails with "illegal base64 data at input byte 0"
//     and Phase 1 warms nothing (the 0.30.102 bug).
func resolveRoutesLoaderRoot(ctx context.Context, root *unstructured.Unstructured, saEP endpoints.Endpoint, saRC *rest.Config, authnNS string) error {
	rctx := xcontext.BuildContext(ctx,
		xcontext.WithUserConfig(saEP),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: snowplowSAUsername}),
		xcontext.WithLogger(slog.Default()),
	)
	rctx = cache.WithInternalEndpoint(rctx, &saEP)
	rctx = cache.WithInternalRESTConfig(rctx, saRC)

	// Resolve the routesloaders CR. The widget resolver recursively
	// reaches every downstream RESTAction + child widget; each inner
	// RESTAction call auto-registers its GVR's informer. We deliberately
	// do NOT page-bound (PerPage/Page = -1): Phase 1 wants the FULL
	// navigation fan-out registered, not just the first visible page.
	_, err := widgets.Resolve(rctx, widgets.ResolveOptions{
		In:      root,
		AuthnNS: authnNS,
		PerPage: -1,
		Page:    -1,
	})
	// The resolver returns the (mutated-in-memory) CR plus a possible
	// error. The output is discarded — Phase 1 never writes status back
	// and never populates L1. A resolution error is returned so the
	// caller can log it; informer registration for whatever the walk
	// DID reach has already happened as a side effect.
	return err
}
