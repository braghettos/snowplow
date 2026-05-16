// phase1.go — 0.30.102 Tag B: startup informer-warmup state + the
// hardcoded meta-query seed budget + the all-informer sync-wait.
//
// Tag B premise (resting on Tag A 0.30.100): the resolver pivot
// (RESOLVER_USE_INFORMER=true) can only serve a GVR whose informer is
// registered AND synced. 0.30.99's bench failed because the navigated
// informers were registered lazily-late and never synced inside the
// navigation window — the pivot served nothing cold.
//
// Tag B closes that cold window with a startup PHASE 1: at boot, before
// traffic, the TWO navigation roots (the `routesloaders` and `navmenus`
// widget CRs) are LISTed cluster-wide and every CR is RECURSIVELY
// resolved with the snowplow SERVICE-ACCOUNT identity through the
// standard widget/RESTAction resolver (0.30.105: the walk recurses
// Root -> Route -> Page -> Row/Column -> DataGrid/Table leaf via each
// resolved widget's status.resourcesRefs.items[]). As the resolver walks
// inner-call paths it auto-registers an informer for every GVR it
// touches via the flag-independent `lazyRegisterInnerCallPaths` hook —
// including the heavy `composition.krateo.io` informer behind the
// Compositions Page DataGrid. After the walk, Phase 1 BLOCKS until every
// registered informer reaches HasSynced, then signals Phase1Done. The
// /readyz probe gates pod readiness on Phase1Done so traffic only
// arrives once the navigated informers are warm.
//
// CRITICAL — feedback_no_special_cases.md: Phase 1 does NOT consult any
// configured GVR / RESTAction list. The ONLY hardcoded budget is the 8
// meta-query seeds below — bare anchors needed to bootstrap discovery,
// not per-resource policy. Every BUSINESS GVR (widgets, panels,
// compositions) is discovered by recursively resolving the two
// navigation roots.
//
// BEHAVIOR-NEUTRAL — PrewarmEnabled() gates the whole feature behind
// PREWARM_ENABLED (default OFF), mirroring PREWARM_REGISTER_ENABLED.
// When OFF: Phase 1 never runs and Phase1Done is pre-set true at startup
// so /readyz is an immediate-200 no-op. The flag is NOT in the chart
// configmap — absent => OFF.

package cache

import (
	"context"
	"os"
	"sync/atomic"
	"time"

	"log/slog"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientcache "k8s.io/client-go/tools/cache"
)

// envPrewarmEnabled is the opt-in gate for the Tag B startup warmup
// (Phase 1 + CRD-watch). Default OFF — absent / "" / anything but
// "true" => the feature is dormant and behavior-neutral.
const envPrewarmEnabled = "PREWARM_ENABLED"

// PrewarmEnabled reports whether the Tag B startup warmup is opted in.
// Read once at startup by main.go; cheap enough to read per call.
func PrewarmEnabled() bool {
	return os.Getenv(envPrewarmEnabled) == "true"
}

// phase1Done is the process-wide atomic that flips true exactly once,
// when the Phase 1 SA-credentialed resolution walk has finished AND
// every registered informer (including the CRD-watch-spawned composition
// informers that exist at boot) has reached HasSynced.
//
// When PrewarmEnabled()==false the startup sequence calls
// MarkPhase1Done immediately (nothing to wait for) so /readyz is a
// no-op. When true, MarkPhase1Done is called only at the END of
// Phase1Warmup. /readyz returns 200 iff phase1Done.Load()==true.
var phase1Done atomic.Bool

// MarkPhase1Done flips the process-wide Phase1Done signal to true. Safe
// to call multiple times — atomic store is idempotent. Called once by
// the startup sequence (immediately when PREWARM_ENABLED is OFF, or at
// the tail of Phase1Warmup when ON).
func MarkPhase1Done() {
	phase1Done.Store(true)
}

// IsPhase1Done reports whether the Tag B startup warmup has completed.
// The /readyz probe handler returns 200 iff this is true. Liveness
// (/health) does NOT consult this — a not-yet-warm pod is alive.
func IsPhase1Done() bool {
	return phase1Done.Load()
}

// ResetPhase1DoneForTest clears the Phase1Done signal. TEST-ONLY — the
// production lifecycle is set-once. Exported so the readyz handler test
// in another package can drive the gate deterministically.
func ResetPhase1DoneForTest() {
	phase1Done.Store(false)
}

// customResourceDefinitionGVR is the GVR of the apiextensions
// CustomResourceDefinition resource — the navigation root the CRD-watch
// (Part 2) registers an informer against to discover composition GVRs
// as their CRDs appear.
//
// Per feedback_no_special_cases.md: NOT a per-resource policy. It is one
// of the 7 bare meta-query anchors — the CRD-watch needs SOMETHING to
// LIST/WATCH to learn about composition CRDs, and that something is the
// CRD type itself.
var customResourceDefinitionGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// routesLoadersGVR is the GVR of the `routesloaders` widget CR.
//
// 0.30.107 — this is NO LONGER a root-SELECTION driver. The navigation
// roots Phase 1 walks are read from the frontend ConfigMap at runtime
// (config.json .api.INIT / .api.ROUTES_LOADER — see
// dispatchers/phase1_roots.go); the resource name `routesloaders` is
// never a Go literal in that selection path. This GVR remains ONLY as a
// meta-query INFORMER-ANCHOR seed: the watcher pre-registers an informer
// for this resource type so that a `/call` to a routesloaders CR can be
// served from cache rather than the apiserver. It is the informer-warming
// anchor, not "where navigation starts".
//
// Per feedback_no_special_cases.md: a bare informer-anchor seed for a
// well-known navigation resource type, not a per-resource carve-out and
// not a root-selection special-case.
var routesLoadersGVR = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "routesloaders",
}

// navMenusGVR is the GVR of the `navmenus` widget CR.
//
// 0.30.107 — like routesLoadersGVR, this is NO LONGER a root-SELECTION
// driver: the navigation roots come from the frontend ConfigMap's
// config.json (.api.INIT). This GVR remains ONLY as a meta-query
// INFORMER-ANCHOR seed so a `/call` to a navmenus CR can be served from
// the informer cache. The resource name `navmenus` is never a Go literal
// in the root-selection path.
//
// Per feedback_no_special_cases.md: a bare informer-anchor seed, not a
// per-resource carve-out.
var navMenusGVR = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "navmenus",
}

// RoutesLoadersGVR exposes the routesloaders meta-query informer-anchor
// seed. Read-only accessor. 0.30.107: no longer consumed by the Phase 1
// root-selection path (roots come from the frontend ConfigMap) — retained
// for the seed-set and its falsifier test.
func RoutesLoadersGVR() schema.GroupVersionResource {
	return routesLoadersGVR
}

// NavMenusGVR exposes the navmenus meta-query informer-anchor seed.
// Read-only accessor. 0.30.107: no longer consumed by the Phase 1
// root-selection path.
func NavMenusGVR() schema.GroupVersionResource {
	return navMenusGVR
}

// CustomResourceDefinitionGVR exposes the CRD meta-query anchor.
func CustomResourceDefinitionGVR() schema.GroupVersionResource {
	return customResourceDefinitionGVR
}

// MetaQuerySeeds returns the COMPLETE hardcoded seed budget for Tag B —
// EXACTLY these 8 GVRs, nothing else (feedback_no_special_cases.md is a
// hard requirement here). Every entry is a meta-query INFORMER-ANCHOR
// seed: the watcher pre-registers an informer for the resource type so a
// `/call` to one of these can be served from cache. None of them is a
// root-SELECTION driver — the navigation roots come from the frontend
// ConfigMap (config.json .api.INIT / .api.ROUTES_LOADER; see
// dispatchers/phase1_roots.go).
//
//  1. routesloaders            — informer-anchor for the routesloaders
//     widget type. 0.30.107: no longer a root-selection literal.
//  2. navmenus                 — informer-anchor for the navmenus widget
//     type. 0.30.107: no longer a root-selection literal.
//  3. restactions              — the restActionGVR anchor (already cited
//     by inventory.go; the resolver's apiRef edges target it).
//  4. customresourcedefinitions — the CRD-watch root (Part 2).
//  5-8. the 4 RBACResourceTypes — roles / rolebindings / clusterroles /
//     clusterrolebindings (already bootstrap-registered in
//     NewResourceWatcher; included here so the seed set is the single
//     auditable source of truth).
//
// Every BUSINESS GVR — widgets, panels, compositions — is ABSENT from
// this set by construction. Those are discovered by RESOLVING the
// ConfigMap-derived navigation roots, never named in code. A test
// asserts this slice has exactly 8 entries and that none of them is a
// composition/widget/panel business GVR.
func MetaQuerySeeds() []schema.GroupVersionResource {
	seeds := []schema.GroupVersionResource{
		routesLoadersGVR,
		navMenusGVR,
		restActionGVR,
		customResourceDefinitionGVR,
	}
	seeds = append(seeds, RBACResourceTypes...)
	return seeds
}

// RegisterMetaQuerySeeds registers an informer for each of the 4
// non-RBAC meta-query seeds (routesloaders, navmenus, restactions,
// customresourcedefinitions) plus re-confirms the 4 RBAC GVRs (already
// registered by NewResourceWatcher — EnsureResourceType observes
// added=false for those) — 8 seeds total. Idempotent + singleflighted
// under rw.mu.
//
// This is the ONLY code that hands a hardcoded GVR to EnsureResourceType
// at startup. The Phase 1 walk registers everything else by resolution.
//
// Returns the count newly registered. Nil-receiver / passthrough are
// no-ops.
func (rw *ResourceWatcher) RegisterMetaQuerySeeds() int {
	if rw == nil || rw.mode == modePassthrough {
		return 0
	}
	registered := 0
	for _, gvr := range MetaQuerySeeds() {
		added, _ := rw.EnsureResourceType(gvr)
		if added {
			registered++
		}
	}
	slog.Info("cache.phase1.meta_query_seeds_registered",
		slog.String("subsystem", "cache"),
		slog.Int("seed_count", len(MetaQuerySeeds())),
		slog.Int("newly_registered", registered),
		slog.String("note", "bare meta-query anchors only — every business GVR is discovered by resolution"),
	)
	return registered
}

// WaitAllInformersSynced blocks until EVERY registered informer reaches
// HasSynced AND no new informer was registered DURING the wait, or ctx
// is cancelled. This is the Phase 1 sync barrier: after the SA-credentialed
// resolution walk has fanned out (registering an informer per touched
// GVR via lazyRegisterInnerCallPaths) AND the CRD-watch has spawned its
// composition informers, this call guarantees the navigated set is warm
// before Phase1Done flips.
//
// RE-SNAPSHOT LOOP — the load-bearing concurrency property. A single
// snapshot+wait has a race: a CRD-add (the CRD-watch's per-GVR
// EnsureResourceType) that lands AFTER the snapshot is taken but while
// WaitForCacheSync is blocked would NOT be in the sync set — Phase1Done
// could then flip while that composition informer is still cold, the
// exact premature-Ready failure /readyz exists to prevent. So this loop
// re-snapshots after every WaitForCacheSync pass and only returns when a
// full pass completed with the registered-informer count UNCHANGED
// across it (no registration occurred during the wait). client-go's
// HasSynced is monotonic — once true it stays true — so a stable count
// across a pass means every informer observed at the start of the pass
// is synced AND nothing new appeared, hence every informer is synced.
//
// It does NOT layer its own timeout — the caller (Phase1Warmup) owns the
// deadline via ctx so the PHASE1_TIMEOUT_SECONDS budget is the single
// source of truth and also bounds a pathological never-stabilizing loop
// (a cluster that keeps adding CRDs forever).
//
// INVARIANT the count-equality test depends on: the registered-informer
// set is append-only — informers are never de-registered (there is no
// delete from rw.informers; the CRD-watch deliberately omits DeleteFunc).
// So an unchanged COUNT across a pass implies an unchanged SET. If a
// future change adds a de-registration path, this proxy breaks and the
// loop must compare the GVR set, not the count.
//
// Returns nil on success, ctx.Err()/DeadlineExceeded on cancellation. In
// modePassthrough there are no informers — returns nil immediately.
func (rw *ResourceWatcher) WaitAllInformersSynced(ctx context.Context) error {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}

	start := time.Now()
	for pass := 1; ; pass++ {
		if ctx.Err() != nil {
			slog.Warn("cache.phase1.sync_wait_incomplete",
				slog.String("subsystem", "cache"),
				slog.Int("pass", pass),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
				slog.Any("err", ctx.Err()),
			)
			return ctx.Err()
		}

		// Snapshot the informer set + count under the lock.
		rw.mu.RLock()
		countBefore := len(rw.informers)
		syncs := make([]clientcache.InformerSynced, 0, countBefore)
		for _, gi := range rw.informers {
			syncs = append(syncs, gi.Informer().HasSynced)
		}
		rw.mu.RUnlock()

		if len(syncs) == 0 {
			// Nothing registered — vacuously synced.
			return nil
		}

		// Wait for this snapshot's informers to sync (outside the lock,
		// so concurrent registrations are not blocked).
		if !clientcache.WaitForCacheSync(ctx.Done(), syncs...) {
			slog.Warn("cache.phase1.sync_wait_incomplete",
				slog.String("subsystem", "cache"),
				slog.Int("pass", pass),
				slog.Int("informer_count", countBefore),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
				slog.Any("err", ctx.Err()),
			)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return context.DeadlineExceeded
		}

		// Re-snapshot: if the count is unchanged, NO informer was
		// registered during the WaitForCacheSync pass — the barrier is
		// genuinely complete. If it grew, a CRD-add (or a late resolver
		// touch) landed mid-wait; loop and re-wait so the new informer
		// is included.
		countAfter := rw.RegisteredCount()
		if countAfter == countBefore {
			slog.Info("cache.phase1.sync_wait_complete",
				slog.String("subsystem", "cache"),
				slog.Int("passes", pass),
				slog.Int("informer_count", countAfter),
				slog.Int64("waited_ms", time.Since(start).Milliseconds()),
			)
			return nil
		}
		slog.Info("cache.phase1.sync_wait_repass",
			slog.String("subsystem", "cache"),
			slog.Int("pass", pass),
			slog.Int("count_before", countBefore),
			slog.Int("count_after", countAfter),
			slog.String("reason", "informer registered mid-wait — re-snapshotting so the new informer is in the barrier"),
		)
	}
}

// RegisteredGVRs returns a snapshot of every GVR with a registered
// informer. The no-hardcode falsifier asserts over this full set that
// the orphan GVR is absent — a stronger check than a single-GVR probe.
func (rw *ResourceWatcher) RegisteredGVRs() []schema.GroupVersionResource {
	if rw == nil || rw.mode == modePassthrough {
		return nil
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	out := make([]schema.GroupVersionResource, 0, len(rw.informers))
	for gvr := range rw.informers {
		out = append(out, gvr)
	}
	return out
}

// IsRegistered reports whether an informer exists for gvr. Convenience
// over RegisteredGVRs for single-GVR assertions (falsifier tests).
func (rw *ResourceWatcher) IsRegistered(gvr schema.GroupVersionResource) bool {
	if rw == nil || rw.mode == modePassthrough {
		return false
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	_, ok := rw.informers[gvr]
	return ok
}

// RegisteredCount returns the number of registered informers without
// allocating a slice. The Phase 1 walk driver polls this to detect when
// the CRD-watch + resolution fan-out has settled.
func (rw *ResourceWatcher) RegisteredCount() int {
	if rw == nil || rw.mode == modePassthrough {
		return 0
	}
	rw.mu.RLock()
	defer rw.mu.RUnlock()
	return len(rw.informers)
}

// ctxKeyInternalEndpointType is the typed empty-struct context key for
// WithInternalEndpoint / InternalEndpointFromContext.
type ctxKeyInternalEndpointType struct{}

var ctxKeyInternalEndpoint = ctxKeyInternalEndpointType{}

// WithInternalEndpoint attaches an internal-dispatch apiserver endpoint
// to ctx. The RESTAction resolver's endpoint-resolution step consults
// this when a non-UAF api[] stage has NO EndpointRef AND the request is
// driven by an internal/startup path that has no per-user `-clientconfig`
// Secret to read.
//
// This is a GENERAL mechanism, not a per-resource carve-out
// (feedback_no_special_cases.md): any internal driver — Phase 1's
// SA-credentialed resolution walk today, a future background refresher
// tomorrow — can tell the standard resolver which endpoint to dispatch
// against instead of the per-user clientconfig lookup. The resolver
// stays one code path; only the endpoint SOURCE is parameterised.
//
// ep is carried as `any` so the cache package does not couple to the
// plumbing endpoints type; the resolver type-asserts to its endpoint
// type. nil ep returns the parent context unchanged.
func WithInternalEndpoint(ctx context.Context, ep any) context.Context {
	if ctx == nil || ep == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyInternalEndpoint, ep)
}

// InternalEndpointFromContext returns the internal-dispatch endpoint
// attached by WithInternalEndpoint, or (nil, false) when none was set
// (the ordinary per-user request path — the resolver then takes its
// standard per-user clientconfig lookup).
func InternalEndpointFromContext(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(ctxKeyInternalEndpoint)
	if v == nil {
		return nil, false
	}
	return v, true
}

// ctxKeyInternalRESTConfigType is the typed empty-struct context key for
// WithInternalRESTConfig / InternalRESTConfigFromContext.
type ctxKeyInternalRESTConfigType struct{}

var ctxKeyInternalRESTConfig = ctxKeyInternalRESTConfigType{}

// WithInternalRESTConfig attaches a ready-built apiserver *rest.Config to
// ctx. The snowplow object/resourceRef client-construction sites consult
// this when an internal/startup driver (Phase 1's SA-credentialed
// resolution walk) is in flight, and use it directly instead of rebuilding
// a client from the context endpoint via kubeconfig.NewClientConfig.
//
// 0.30.103 bug fix — WHY a *rest.Config and not just the endpoint:
// kubeconfig.NewClientConfig(ctx, ep) marshals the endpoint into a
// kubeconfig document and hands it to client-go's clientcmd loader. That
// path is CERT-AUTH-ONLY and base64-aware:
//   - it has NO token field — a token-bearing endpoint loses its only
//     credential (the SA client would then be unauthenticated);
//   - clientcmd base64-DECODES certificate-authority-data, so it requires
//     the CA to be base64-encoded. The per-user `<user>-clientconfig`
//     Secret stores credentials base64-encoded (the authn signup flow
//     base64-encodes them), so the per-user path works. But the snowplow
//     service account's in-cluster credentials — the projected
//     /var/run/secrets/.../token (a raw JWT) and ca.crt (raw PEM) — are
//     NOT base64-encoded. Feeding the raw-PEM SA CA through that path
//     fails with "illegal base64 data at input byte 0".
//
// So the SA cannot be expressed as a kubeconfig-loadable endpoint at all.
// The SA *rest.Config must be built directly from the raw in-cluster
// credentials (rest.InClusterConfig), then carried here so the resolver's
// client-construction sites use it verbatim — bypassing the
// base64/cert-only kubeconfig path entirely.
//
// This is a GENERAL mechanism, not a per-resource carve-out
// (feedback_no_special_cases.md): any internal driver can hand the
// resolver a pre-built *rest.Config; only the client SOURCE is
// parameterised, the resolver stays one code path. Ordinary per-user
// requests never set it and fall through to the unchanged
// kubeconfig.NewClientConfig path.
//
// rc is carried as `any` so the cache package does not couple to the
// k8s.io/client-go/rest type; the consuming site type-asserts to
// *rest.Config. nil rc returns the parent context unchanged.
func WithInternalRESTConfig(ctx context.Context, rc any) context.Context {
	if ctx == nil || rc == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyInternalRESTConfig, rc)
}

// InternalRESTConfigFromContext returns the internal-dispatch *rest.Config
// attached by WithInternalRESTConfig, or (nil, false) when none was set
// (the ordinary per-user request path — the caller then builds a client
// from the context endpoint via kubeconfig.NewClientConfig).
func InternalRESTConfigFromContext(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	v := ctx.Value(ctxKeyInternalRESTConfig)
	if v == nil {
		return nil, false
	}
	return v, true
}

// IsInternalDispatch reports whether ctx is driven by an internal/startup
// driver — today, Phase 1's SA-credentialed resolution walk. It is true
// iff WithInternalRESTConfig attached an internal-dispatch *rest.Config
// (the SINGLE marker the walk sets on every resolution context it builds;
// see resolveNavigationRoot in dispatchers/phase1_walk.go).
//
// WHY a dedicated predicate: the resolver pivot's per-user RBAC narrowing
// (filterListByRBAC / filterGetByRBAC) is keyed on the REQUEST USER
// identity against the Krateo Role/RoleBinding CRs. Phase 1 resolves
// under the snowplow service account, which carries NO Krateo RBAC CRs,
// so that narrowing default-denies and silently empties every
// informer-served LIST inside an apiRef RESTAction — the navmenu's
// `navmenuitems` LIST returns zero items and the navigation descent dies
// before it reaches the Compositions DataGrid. Phase 1 is identity-
// independent informer DISCOVERY, not per-user rendering: the pivot's
// RBAC filter must NOT narrow an internal-dispatch read. The per-user
// `allowed` render gate is correctly applied later, at real request time.
//
// SECURITY — this can NEVER widen a real user's view: an ordinary
// per-user request never calls WithInternalRESTConfig, so
// IsInternalDispatch is false on every request path. Only Phase 1's
// startup walk sets the marker, and Phase 1 DISCARDS its resolution
// output (it populates no L1, persists no status) — the unfiltered
// bytes never reach a user. The bypass is uniform over every GVR
// (feedback_no_special_cases.md): one context-state predicate, no
// per-resource carve-out.
func IsInternalDispatch(ctx context.Context) bool {
	_, ok := InternalRESTConfigFromContext(ctx)
	return ok
}
