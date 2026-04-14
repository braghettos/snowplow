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
// This is the interface that replaces L3 Redis reads — the informer
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
