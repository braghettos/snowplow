package cache

import (
	"context"
	"sync"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type trackerContextKey struct{}

// ResourceRef identifies a specific K8s resource (or a LIST of resources)
// accessed during resolution. Name is empty for LIST operations.
type ResourceRef struct {
	GVRKey string
	NS     string
	Name   string
}

// DependencyTracker records the GVRs and specific resources accessed during a
// single resolution pass. It is safe for concurrent use (inner HTTP fan-out
// calls run in parallel).
//
// Q-RBAC-DECOUPLE C(d) v4 §2.3 (Fix-W) — adds an atomic UAFTouching flag.
// Set to true by the widget apiref resolver when it observes that the
// resolved RESTAction CR has at least one api[] entry with UserAccessFilter
// configured. Read by the widget dispatcher to gate the L1 write: when
// true, the widget body would inline a per-user-filtered apiref view that
// must NOT be shared across users in the same binding-identity group, so
// the L1 write is SKIPPED. atomic.Bool gives us the cheapest read on the
// hot widget HIT path with zero contention against the AddGVR /
// AddResource mutex critical section.
type DependencyTracker struct {
	mu          sync.Mutex
	gvrs        map[string]bool
	resources   []ResourceRef
	resSeen     map[string]bool
	uafTouching atomic.Bool
}

func NewDependencyTracker() *DependencyTracker {
	return &DependencyTracker{
		gvrs:    make(map[string]bool),
		resSeen: make(map[string]bool),
	}
}

func (t *DependencyTracker) AddGVR(gvr schema.GroupVersionResource) {
	if gvr.Resource == "" {
		return
	}
	t.mu.Lock()
	t.gvrs[GVRToKey(gvr)] = true
	t.mu.Unlock()
}

// AddResource records a dependency on a specific K8s resource (GET) or a LIST
// of resources in a namespace. For GET, pass all three. For LIST, pass name="".
func (t *DependencyTracker) AddResource(gvr schema.GroupVersionResource, ns, name string) {
	if gvr.Resource == "" {
		return
	}
	gvrKey := GVRToKey(gvr)
	dedupKey := gvrKey + ":" + ns + ":" + name
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resSeen[dedupKey] {
		return
	}
	t.resSeen[dedupKey] = true
	t.resources = append(t.resources, ResourceRef{GVRKey: gvrKey, NS: ns, Name: name})
}

func (t *DependencyTracker) GVRKeys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.gvrs))
	for k := range t.gvrs {
		keys = append(keys, k)
	}
	return keys
}

// ResourceRefs returns all specific resource dependencies recorded during resolution.
func (t *DependencyTracker) ResourceRefs() []ResourceRef {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]ResourceRef, len(t.resources))
	copy(out, t.resources)
	return out
}

// MarkUAFTouching records that the resolution pass touched (transitively
// or directly) at least one RESTAction whose api[] declares a
// UserAccessFilter. The widget dispatcher reads this via UAFTouching()
// to decide whether to skip the L1 write — see Q-RBAC-DECOUPLE C(d) v4
// §2.3 (Fix-W for the widget L1 transitive leak).
//
// Idempotent: repeated calls are no-ops. Safe for concurrent invocation
// from multiple apiref resolution goroutines (atomic.Bool.Store).
func (t *DependencyTracker) MarkUAFTouching() {
	t.uafTouching.Store(true)
}

// UAFTouching returns true iff MarkUAFTouching() was invoked at least
// once during this resolution pass.
func (t *DependencyTracker) UAFTouching() bool {
	return t.uafTouching.Load()
}

func WithDependencyTracker(ctx context.Context, t *DependencyTracker) context.Context {
	return context.WithValue(ctx, trackerContextKey{}, t)
}

func TrackerFromContext(ctx context.Context) *DependencyTracker {
	if t, ok := ctx.Value(trackerContextKey{}).(*DependencyTracker); ok {
		return t
	}
	return nil
}
