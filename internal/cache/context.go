package cache

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// bindingToUser maps binding identity hashes to a representative username.
// Populated during HTTP requests (CachedUserConfig middleware) and prewarm.
// Used by L1 refresh to look up user credentials for a binding identity key.
var bindingToUser sync.Map // map[string]string

// RegisterBindingUser records that the given username has the given binding
// identity. During L1 refresh, the binding identity from a parsed key can be
// mapped back to a real username for credential lookup.
func RegisterBindingUser(bindingIdentity, username string) {
	if bindingIdentity == "" || username == "" {
		return
	}
	bindingToUser.Store(bindingIdentity, username)
}

// UsernameForBinding returns the representative username for a binding
// identity. Returns ("", false) if no mapping exists.
func UsernameForBinding(bindingIdentity string) (string, bool) {
	v, ok := bindingToUser.Load(bindingIdentity)
	if !ok {
		return "", false
	}
	return v.(string), true
}

type contextKey struct{}

// WithCache returns a context carrying c.
func WithCache(ctx context.Context, c Cache) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext extracts the Cache from ctx, returning nil if none was set.
func FromContext(ctx context.Context) Cache {
	c, _ := ctx.Value(contextKey{}).(Cache)
	return c
}

// InformerReader provides read access to the informer's in-memory store.
// This is the interface that replaced L3 Redis reads -- the informer
// already holds all K8s objects via WATCH, so reading from it is
// zero-I/O and zero-copy (returns pointers to live objects).
type InformerReader interface {
	// GetObject returns a single object from the informer store.
	// Returns (nil, false) if the GVR has no registered informer or the
	// object does not exist.
	GetObject(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, bool)

	// ListObjects returns all objects for a GVR, optionally scoped to a
	// namespace (ns="" means cluster-wide). Returns (nil, false) if the
	// GVR has no registered informer.
	ListObjects(gvr schema.GroupVersionResource, ns string) ([]*unstructured.Unstructured, bool)
}

type informerReaderKey struct{}

// WithInformerReader returns a context carrying the InformerReader.
func WithInformerReader(ctx context.Context, ir InformerReader) context.Context {
	return context.WithValue(ctx, informerReaderKey{}, ir)
}

// InformerReaderFromContext extracts the InformerReader from ctx.
func InformerReaderFromContext(ctx context.Context) InformerReader {
	ir, _ := ctx.Value(informerReaderKey{}).(InformerReader)
	return ir
}

// DirtyEntry identifies a single GVR+namespace that changed and needs
// API result cache bypass during the next L1 refresh.
type DirtyEntry struct {
	GVRKey string // e.g. "compositions.core.krateo.io/v1alpha1"
	NS     string // namespace, or "" for cluster-wide
}

// DirtySet holds the set of GVR+namespace pairs that should bypass
// the API result cache. Built once per dirty L1 refresh cycle.
type DirtySet struct {
	bypassAll bool            // bypass API result cache for ALL pairs (background refresh)
	pairs     map[string]bool // gvrKey + "\x00" + ns → true (namespace-scoped match)
	gvrKeys   map[string]bool // gvrKey → true (cluster-wide match)
}

// NewBypassAllDirtySet returns a DirtySet that bypasses the API result
// cache for ALL pairs. Used during background L1 refresh where the
// informer is always fresh and faster than Redis GET + json.Unmarshal.
func NewBypassAllDirtySet() *DirtySet {
	return &DirtySet{bypassAll: true}
}

// NewDirtySet builds an immutable DirtySet from the given entries.
func NewDirtySet(entries []DirtyEntry) *DirtySet {
	ds := &DirtySet{
		pairs:   make(map[string]bool, len(entries)),
		gvrKeys: make(map[string]bool, len(entries)),
	}
	for _, e := range entries {
		ds.gvrKeys[e.GVRKey] = true
		if e.NS != "" {
			ds.pairs[e.GVRKey+"\x00"+e.NS] = true
		}
	}
	return ds
}

// ShouldBypassAPIResult returns true if the API result cache should be
// skipped for the given gvrKey and namespace. A nil receiver returns false.
func (ds *DirtySet) ShouldBypassAPIResult(gvrKey, pathNS string) bool {
	if ds == nil {
		return false
	}
	if ds.bypassAll {
		return true
	}
	if pathNS == "" {
		// Cluster-wide list: bypass if any entry touches this GVR.
		return ds.gvrKeys[gvrKey]
	}
	// Namespace-scoped: bypass only if the exact pair was marked dirty.
	return ds.pairs[gvrKey+"\x00"+pathNS]
}

type dirtySetKey struct{}

// WithDirtySet returns a context carrying the DirtySet for targeted
// API result cache bypass during L1 refresh.
func WithDirtySet(ctx context.Context, ds *DirtySet) context.Context {
	return context.WithValue(ctx, dirtySetKey{}, ds)
}

// DirtySetFromContext extracts the DirtySet from ctx, returning nil
// if none was set (which means no bypass).
func DirtySetFromContext(ctx context.Context) *DirtySet {
	ds, _ := ctx.Value(dirtySetKey{}).(*DirtySet)
	return ds
}

type rbacWatcherKey struct{}

// WithRBACWatcher returns a context carrying the RBACWatcher for local
// RBAC evaluation. When present, UserCan uses in-memory rule matching
// instead of SelfSubjectAccessReview API calls.
func WithRBACWatcher(ctx context.Context, rw *RBACWatcher) context.Context {
	return context.WithValue(ctx, rbacWatcherKey{}, rw)
}

// RBACWatcherFromContext extracts the RBACWatcher from ctx.
// Returns nil if not set (callers fall back to SSAR).
func RBACWatcherFromContext(ctx context.Context) *RBACWatcher {
	rw, _ := ctx.Value(rbacWatcherKey{}).(*RBACWatcher)
	return rw
}

// RBACEvaluator is the minimal contract callers (e.g. applyUserAccessFilter
// in resolvers/restactions/api) need from the RBACWatcher: a single in-memory
// access-check method. *RBACWatcher implements it. Existing as a 1-method
// interface keeps the call sites trivially mockable in unit tests without
// requiring a full informer factory at test time (Q-RBACC-IMPL-1).
type RBACEvaluator interface {
	EvaluateRBAC(username string, groups []string, verb string, gr schema.GroupResource, namespace string) bool
}

type rbacEvaluatorKey struct{}

// WithRBACEvaluator attaches a test-friendly RBACEvaluator to ctx. Callers
// that prefer interface-based mocking over the full RBACWatcher (with its
// informer factory + synthetic RBAC objects) install one here. Production
// code should keep using WithRBACWatcher; the helper consumers fall back
// to the watcher when no evaluator override is present.
//
// IMPORTANT — TEST ISOLATION ONLY:
// Tests must NOT install both WithRBACWatcher and WithRBACEvaluator in the
// same context. The mock-wins-when-both-installed precedence in
// applyUserAccessFilter (see resolvers/restactions/api/user_access_filter.go)
// exists strictly for unit-test isolation; in a production-shaped context
// it would silently bypass the real informer-backed evaluator, producing
// inconsistent RBAC decisions between code paths that read the watcher
// directly (e.g. RBACWatcher.EvaluateRBAC) and those routing through
// applyUserAccessFilter. The envtest harness (see
// internal/handlers/restactions_envtest_test.go) deliberately wires only
// WithRBACWatcher to verify the real path.
func WithRBACEvaluator(ctx context.Context, ev RBACEvaluator) context.Context {
	if ev == nil {
		return ctx
	}
	return context.WithValue(ctx, rbacEvaluatorKey{}, ev)
}

// RBACEvaluatorFromContext returns the test-mock RBACEvaluator if any was
// installed via WithRBACEvaluator. Returns nil otherwise. Callers fall back
// to RBACWatcherFromContext.
func RBACEvaluatorFromContext(ctx context.Context) RBACEvaluator {
	ev, _ := ctx.Value(rbacEvaluatorKey{}).(RBACEvaluator)
	return ev
}

// CallResolver resolves a nested /call RESTAction inline (in-process)
// without making an HTTP round-trip. During background refresh, the
// L1 refresh function injects one via WithCallResolver so that nested
// /call paths (e.g. compositions-list → compositions-get-ns-and-crd)
// are resolved from the informer instead of calling back to snowplow
// over HTTP, which would timeout under high load.
//
// Parameters: ctx, obj (the target RESTAction CR as unstructured map),
// resolvedKey (L1 key to write), authnNS.
// Returns the serialized resolved output (same as HTTP /call response body).
type CallResolver func(ctx context.Context, obj map[string]any, resolvedKey, authnNS string) ([]byte, error)

type callResolverKey struct{}

// WithCallResolver returns a context carrying an inline /call resolver.
func WithCallResolver(ctx context.Context, fn CallResolver) context.Context {
	return context.WithValue(ctx, callResolverKey{}, fn)
}

// CallResolverFromContext extracts the CallResolver from ctx.
// Returns nil if not set (callers fall back to HTTP).
func CallResolverFromContext(ctx context.Context) CallResolver {
	fn, _ := ctx.Value(callResolverKey{}).(CallResolver)
	return fn
}

// SnowplowEndpointFn is the callback shape used by api[] entries that
// declare userAccessFilter to obtain the snowplow ServiceAccount endpoint
// at dispatch time. The callback re-reads the projected SA token on each
// invocation (~10µs tmpfs read) so token rotation is handled transparently.
//
// Stored in context so all paths (HTTP /call, widget apiref, L1 refresh,
// prewarm) inherit one provider without per-dispatcher threading. The
// resolver sites accept the provider on ResolveOptions for explicit unit
// testing; nil ResolveOptions.SnowplowEndpoint falls back to this context
// value (Q-RBAC-DECOUPLE C(d)).
type SnowplowEndpointFn func() (any, error)

type snowplowEndpointKey struct{}

// WithSnowplowEndpoint returns a context carrying the snowplow-SA endpoint
// provider for elevated userAccessFilter dispatch. The value type is
// intentionally `any` to avoid a cache→plumbing/endpoints import cycle;
// the api package re-asserts it back to its own callback shape.
func WithSnowplowEndpoint(ctx context.Context, fn func() (any, error)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, snowplowEndpointKey{}, fn)
}

// SnowplowEndpointFromContext extracts the snowplow-SA endpoint provider
// from ctx. Returns nil when not set.
func SnowplowEndpointFromContext(ctx context.Context) func() (any, error) {
	fn, _ := ctx.Value(snowplowEndpointKey{}).(func() (any, error))
	return fn
}

// snowplowK8sKey carries the snowplow-SA dynamic K8s client used by the
// SA-elevated dispatch path (Q-RBAC-DECOUPLE C(d) v6 — Path B). The value
// type is intentionally `any` to avoid a cache→internal/dynamic import
// cycle; the api package re-asserts it back to dynamic.Client.
//
// This is a sibling to snowplowEndpointKey: the same set of context
// installation sites (main.go withCache, prewarm, l1_refresh) install
// both, so every dispatch path that previously had access to the SA
// endpoint now also has access to a client-go-backed dynamic client. The
// SA branch of api.Resolve uses this client INSTEAD of httpcall.Do +
// SnowplowEndpoint, structurally bypassing plumbing's tlsConfigFor bug
// that silently drops CertificateAuthorityData on token-auth endpoints
// (the v5 D1 defect that v6 closes).
type snowplowK8sKey struct{}

// WithSnowplowK8s returns a context carrying the snowplow-SA dynamic K8s
// client. The value type is `any` to avoid an import cycle; the api
// package type-asserts it back to dynamic.Client.
//
// nil-safe: if c is nil, returns ctx unchanged so callers don't have to
// guard the install site.
func WithSnowplowK8s(ctx context.Context, c any) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, snowplowK8sKey{}, c)
}

// SnowplowK8sFromContext extracts the snowplow-SA dynamic K8s client from
// ctx as `any`. Returns nil when not set. Callers re-assert to the
// concrete dynamic.Client type.
func SnowplowK8sFromContext(ctx context.Context) any {
	return ctx.Value(snowplowK8sKey{})
}

type restActionNameKey struct{}

// WithRESTActionName returns a context carrying the RESTAction name being
// resolved. Used for audit/observability so per-call helpers (e.g.
// applyUserAccessFilter) can attribute their work to the originating
// RESTAction CR. Mirrors the WithRBACWatcher / WithUserInfo pattern.
//
// Per Q-RBACC-IMPL-2 (architect recommendation 2026-05-04).
func WithRESTActionName(ctx context.Context, name string) context.Context {
	if name == "" {
		return ctx
	}
	return context.WithValue(ctx, restActionNameKey{}, name)
}

// RESTActionNameFromContext extracts the RESTAction name from ctx, returning
// "" if no name was set (e.g. /jq endpoint, unit tests).
func RESTActionNameFromContext(ctx context.Context) string {
	name, _ := ctx.Value(restActionNameKey{}).(string)
	return name
}

type bindingIdentityKey struct{}

// WithBindingIdentity returns a context carrying the user's binding identity
// (hash of their RBAC bindings). Used as the L1 cache key component instead
// of the username, enabling shared cache entries across users with identical
// RBAC permissions.
func WithBindingIdentity(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, bindingIdentityKey{}, id)
}

// BindingIdentityFromContext extracts the binding identity from ctx.
// Returns "" if not set (fallback to username-based keys).
func BindingIdentityFromContext(ctx context.Context) string {
	id, _ := ctx.Value(bindingIdentityKey{}).(string)
	return id
}

// systemIdentityKey is the context key that flags a context as carrying a
// synthesized system identity (today: the snowplow ServiceAccount used by
// the L1 refresh path). Q-RBAC-DECOUPLE C(d) v4 §2.2 (Fix-3a for
// Q-RBACC-DEFECT-3).
type systemIdentityKey struct{}

// WithSystemIdentity marks ctx as carrying a synthesized system identity
// (e.g., the snowplow ServiceAccount built by MakeL1Refresher). The api
// dispatch fork at internal/resolvers/restactions/api/resolve.go uses
// this to route ALL api[] entries through the snowplow elevated endpoint
// regardless of UserAccessFilter — because the per-user clientconfig
// fallback at internal/resolvers/restactions/endpoints/endpoints.go
// cannot resolve a Secret named after the synthesized SA identity (which
// would attempt `systemserviceaccountkrateo-systemsnowplow-clientconfig`
// — a non-existent Secret — and ERROR out).
//
// SECURITY (NON-NEGOTIABLE per spec §11 rule 2): this flag is RESERVED
// for the L1 refresh path (and any future system-only resolver path).
// It MUST NEVER be set on contexts that carry a real user identity.
// Doing so would route real-user traffic through the snowplow SA token
// — equivalent to bypassing per-user clientconfig and silently elevating
// every request to cluster-admin equivalence. The §6.8 negative test
// (restactions_systemidentity_negative_test.go) asserts the invariant.
func WithSystemIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemIdentityKey{}, true)
}

// IsSystemIdentity returns true iff WithSystemIdentity was set on ctx.
// Single read site today: the dispatch-fork predicate in
// internal/resolvers/restactions/api/resolve.go.
func IsSystemIdentity(ctx context.Context) bool {
	v, _ := ctx.Value(systemIdentityKey{}).(bool)
	return v
}

// testRestConfigKey is a TEST-ONLY context key used by the v4 envtest
// fixtures (Q-RBAC-DECOUPLE C(d) v4 §6.4 - §6.6) to inject a stub
// rest.Config so the api.Resolve `if opts.RC == nil` early guard does
// not try (and fail) to call rest.InClusterConfig() outside an
// in-cluster environment.
//
// PRODUCTION CALLERS: zero. This key is set ONLY by *_test.go files
// in internal/handlers/dispatchers/. Production wiring relies on the
// dispatcher's l1cache.Input.SArc being nil and the resolver falling
// back to rest.InClusterConfig(), exactly as it does today. Reading
// the key as an additive fallback inside l1cache.ResolveAndCache keeps
// the change strictly additive — the production execution path is
// bit-for-bit identical with the v3 baseline.
//
// Why this is acceptable scaffolding under the v4 spec §11 (no out-of-
// scope refactor): the §6.4 envtest is load-bearing — the architect's
// §6.10 lesson is that dispatcher-level invariants MUST be exercised
// via the real handler. Without an SArc injection the real handler
// cannot run outside a kubelet-mounted SA pod, defeating §6.4's intent.
type testRestConfigKey struct{}

// WithTestRestConfig is a TEST-ONLY helper. The value type is `any`
// to avoid a cache→client-go import cycle; callers pass *rest.Config
// and l1cache asserts the type back at the read site.
func WithTestRestConfig(ctx context.Context, rc any) context.Context {
	if rc == nil {
		return ctx
	}
	return context.WithValue(ctx, testRestConfigKey{}, rc)
}

// TestRestConfigFromContext returns the test-injected rest.Config (as
// `any`) or nil. Read by l1cache.ResolveAndCache as an additive
// fallback when in.SArc is nil — does NOT change production behavior
// because production never calls WithTestRestConfig.
func TestRestConfigFromContext(ctx context.Context) any {
	return ctx.Value(testRestConfigKey{})
}
