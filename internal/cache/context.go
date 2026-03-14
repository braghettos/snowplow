package cache

import "context"

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
