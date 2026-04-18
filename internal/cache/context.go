package cache

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type contextKey struct{}

// WithCache returns a context carrying c.
func WithCache(ctx context.Context, c *RedisCache) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// FromContext extracts the RedisCache from ctx, returning nil if none was set.
func FromContext(ctx context.Context) *RedisCache {
	c, _ := ctx.Value(contextKey{}).(*RedisCache)
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
	pairs   map[string]bool // gvrKey + "\x00" + ns → true (namespace-scoped match)
	gvrKeys map[string]bool // gvrKey → true (cluster-wide match)
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
