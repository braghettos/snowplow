// scope.go — Issue #156 (/call cluster-scoped read+write): authoritative
// GVR→scope resolution for the raw /call passthrough handler.
//
// WHY — handlers.buildURIPath (internal/handlers/call.go) needs to know
// whether a GVR is namespaced or cluster-scoped to decide whether to emit
// the `namespaces/<ns>` URI segment. The authoritative signal is the
// discovery RESTMapping's Scope (mirrors resourceInterfaceFor at
// client.go:137-158, which already has `.Scope` in hand), NOT the
// caller-supplied namespace (a proxy, not the real signal).
//
// This file exposes that scope signal through the SAME process-wide SA
// discovery singleton the ValidateObjectStatus path already uses
// (SharedSADiscoveryClient / cached_client.go), so:
//   - the discovery download is paid ONCE per process (already warmed at
//     boot by Phase 1);
//   - a newly-installed CRD's scope stays fresh via the existing
//     InvalidateSADiscovery CRD-lifecycle wiring (cached_client.go:277);
//   - handlers.Call() needs NO rc threading through main.go — it calls the
//     package accessor lazily.
//
// RBAC-SAFE — the SA mapper reads cluster SHAPE ONLY (which resources are
// namespaced). It is NEVER used to authorize or perform the write; the
// /call write still runs entirely under the caller's own per-user
// endpoint (call.go ep = xcontext.UserConfig). This is shape metadata
// below the per-user layer (cached_client.go:44-52).

package dynamic

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// scopeRESTMapper is the narrow slice of meta.RESTMapper that scope
// resolution needs. Both the real *DeferredDiscoveryRESTMapper and a test
// fake satisfy it, so ScopeForGVR is unit-testable without a real
// discovery client.
type scopeRESTMapper interface {
	KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error)
	RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error)
}

// ScopeForGVR resolves the cluster scope of a GVR through the given
// RESTMapper. It mirrors the two-step KindFor→RESTMapping that
// resourceInterfaceFor performs (client.go:139-146) — a bare GVR carries
// no Scope, so we must resolve GVR→GVK→RESTMapping to reach
// RESTMapping.Scope.
//
// Returns namespaced=true iff the resource is namespace-scoped
// (RESTScopeNameNamespace). Any resolution error (unknown GVR, mapper not
// yet synced, ambiguous resource) is returned to the caller so the /call
// path can FAIL-CLOSED (4xx) rather than guess a scope — see the
// namespace-absent branch in handlers.validateRequest. Single-sited so the
// two-step is fake-injectable and cannot drift from client.go.
func ScopeForGVR(mapper scopeRESTMapper, gvr schema.GroupVersionResource) (namespaced bool, err error) {
	if mapper == nil {
		return false, fmt.Errorf("dynamic.ScopeForGVR: nil RESTMapper")
	}

	gvk, err := mapper.KindFor(gvr)
	if err != nil {
		return false, fmt.Errorf("dynamic.ScopeForGVR: KindFor(%s): %w", gvr.String(), err)
	}

	m, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return false, fmt.Errorf("dynamic.ScopeForGVR: RESTMapping(%s): %w", gvk.String(), err)
	}

	return m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

// SharedSAScopeForGVR resolves a GVR's cluster scope through the
// process-wide SA discovery singleton (SharedSADiscoveryClient), so
// callers with no rest.Config in hand (handlers.Call() is arg-less,
// mounted at ~6 sites in main.go) can consult cluster shape lazily with
// ZERO main.go plumbing.
//
// It self-resolves the SA rest.Config (ServiceAccountRESTConfig, memoized)
// and the SA singleton's typed mapper, then delegates to ScopeForGVR.
//
// Every error (nil/erroring rc, singleton not built, KindFor/RESTMapping
// miss) is returned so the /call caller FAILS-CLOSED. This accessor is
// consulted ONLY when the /call request omits a namespace (a 400 today);
// the namespaced path never reaches here, so a discovery-lag / cold-mapper
// window can never regress a currently-working namespaced request.
//
// Goroutine-safe (SharedSADiscoveryClient + the mapper are).
func SharedSAScopeForGVR(gvr schema.GroupVersionResource) (namespaced bool, err error) {
	rc, err := ServiceAccountRESTConfig()
	if err != nil {
		return false, fmt.Errorf("dynamic.SharedSAScopeForGVR: SA rest.Config: %w", err)
	}

	mapper, err := sharedSAMapper(rc)
	if err != nil {
		return false, fmt.Errorf("dynamic.SharedSAScopeForGVR: SA mapper: %w", err)
	}

	return ScopeForGVR(mapper, gvr)
}

// ScopeResolverForConfig builds a scope-resolver func backed by a FRESH
// DeferredDiscoveryRESTMapper constructed from rc. Unlike
// SharedSAScopeForGVR (which self-resolves the in-cluster SA rest.Config
// and reuses the process singleton), this takes an explicit rc — the
// integration/kind arm needs it because the test process runs OUTSIDE the
// cluster and has no projected SA volume, so the SA singleton path is
// unavailable. Production uses SharedSAScopeForGVR; this is the
// arbitrary-rc constructor. Returns an error only if the discovery client
// cannot be built; per-GVR resolution errors surface from the returned func.
func ScopeResolverForConfig(rc *rest.Config) (func(schema.GroupVersionResource) (bool, error), error) {
	if rc == nil {
		return nil, fmt.Errorf("dynamic.ScopeResolverForConfig: nil *rest.Config")
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("dynamic.ScopeResolverForConfig: NewDiscoveryClientForConfig: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(
		cacheddiscovery.NewMemCacheClient(discoveryClient),
	)
	return func(gvr schema.GroupVersionResource) (bool, error) {
		return ScopeForGVR(mapper, gvr)
	}, nil
}

// sharedSAMapper returns the typed RESTMapper held by the SA discovery
// singleton, building it (once) if needed. It reuses SharedSADiscoveryClient's
// build/cache/invalidation machinery: the first call builds the singleton
// (and pays the discovery download), later calls return the SAME warm
// mapper, and InvalidateSADiscovery keeps a newly-installed CRD's scope
// fresh. Returns an error (never a nil mapper) when the singleton is not
// available, so SharedSAScopeForGVR fails-closed.
func sharedSAMapper(rc *rest.Config) (scopeRESTMapper, error) {
	// Ensure the singleton is built (pays the discovery download once; a
	// no-op on the warm path). We discard the Client — we only need the
	// co-located typed mapper, which the Client interface does not expose.
	if _, err := SharedSADiscoveryClient(rc); err != nil {
		return nil, err
	}

	saDiscoveryMu.RLock()
	st := saDiscoveryInstance
	saDiscoveryMu.RUnlock()
	if st == nil || st.mapper == nil {
		return nil, fmt.Errorf("SA discovery singleton has no mapper")
	}
	return st.mapper, nil
}
