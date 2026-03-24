package cache

import (
	"context"
	"sync"

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
type DependencyTracker struct {
	mu        sync.Mutex
	gvrs      map[string]bool
	resources []ResourceRef
	resSeen   map[string]bool
	l1Deps    map[string]bool // L1 resolved keys accessed via /call during resolution
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

// AddL1Dep records an L1 resolved key that was accessed during resolution
// (e.g., via a /call path to another RESTAction). At serve time, if any of
// these L1 keys no longer exist, the parent L1 key is considered stale.
func (t *DependencyTracker) AddL1Dep(l1Key string) {
	if l1Key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.l1Deps == nil {
		t.l1Deps = make(map[string]bool)
	}
	t.l1Deps[l1Key] = true
}

// L1DepKeys returns the L1 resolved keys accessed during resolution.
func (t *DependencyTracker) L1DepKeys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.l1Deps))
	for k := range t.l1Deps {
		keys = append(keys, k)
	}
	return keys
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
