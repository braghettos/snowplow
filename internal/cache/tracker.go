package cache

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

type trackerContextKey struct{}

// DependencyTracker records the GVRs accessed during a single resolution pass.
// It is safe for concurrent use (inner HTTP fan-out calls run in parallel).
type DependencyTracker struct {
	mu   sync.Mutex
	gvrs map[string]bool
}

func NewDependencyTracker() *DependencyTracker {
	return &DependencyTracker{
		gvrs: make(map[string]bool),
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

func (t *DependencyTracker) GVRKeys() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.gvrs))
	for k := range t.gvrs {
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
