package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Cache is the interface satisfied by *RedisCache and (later) by test doubles.
// It contains every method that callers outside the cache package need.
//
// Methods intentionally NOT on the interface:
//   - Pipeline (returns redis.Pipeliner — Redis-specific; use PipelineFrom)
//   - Ping / Close (lifecycle, only used by main.go on the concrete type)
//   - SetDiskStore, SetGVRNotifier, RegisterGVRTTL (configuration, concrete-only)
//   - DiskFileCount (implementation detail; callers use type assertion)
type Cache interface {
	// ── Core read/write ──────────────────────────────────────────────────
	Get(ctx context.Context, key string, dest any) (bool, error)
	GetRaw(ctx context.Context, key string) ([]byte, bool, error)
	GetRawMulti(ctx context.Context, keys []string) map[string][]byte
	Exists(ctx context.Context, key string) bool

	Set(ctx context.Context, key string, val any) error
	SetWithTTL(ctx context.Context, key string, val any, ttl time.Duration) error
	SetRaw(ctx context.Context, key string, val []byte) error
	SetResolvedRaw(ctx context.Context, key string, val []byte) error
	SetAPIResultRaw(ctx context.Context, key string, val []byte) error
	SetForGVR(ctx context.Context, gvr schema.GroupVersionResource, key string, val any) error
	SetMultiForGVR(ctx context.Context, gvr schema.GroupVersionResource, entries map[string]any) error
	SetRawForGVR(ctx context.Context, gvr schema.GroupVersionResource, key string, val []byte) error

	Delete(ctx context.Context, keys ...string) error

	// ── Negative-cache ───────────────────────────────────────────────────
	GetNotFound(ctx context.Context, key string) bool
	SetNotFound(ctx context.Context, key string) error

	// ── Atomic ops ───────────────────────────────────────────────────────
	AtomicUpdateJSON(ctx context.Context, key string, fn func([]byte) ([]byte, error), ttl time.Duration) error

	// ── Set (Redis SET data structure) ───────────────────────────────────
	ScanKeys(ctx context.Context, pattern string) ([]string, error)
	SAddWithTTL(ctx context.Context, key, member string, ttl time.Duration) error
	SAddMultiWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error
	SRemMembers(ctx context.Context, key string, members ...string) error
	ReplaceSetWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error
	SMembers(ctx context.Context, key string) ([]string, error)

	// ── List assembly ────────────────────────────────────────────────────
	AssembleListFromIndex(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, bool, error)

	// ── RBAC ─────────────────────────────────────────────────────────────
	IsRBACAllowed(ctx context.Context, username, verb string, gr schema.GroupResource, namespace string) (allowed, cached bool)
	SetRBACResult(ctx context.Context, username, verb string, gr schema.GroupResource, namespace string, allowed bool, ttl time.Duration) error
	DeleteUserRBAC(ctx context.Context, username string) error

	// ── GVR tracking ─────────────────────────────────────────────────────
	SAddGVR(ctx context.Context, gvr schema.GroupVersionResource) error
	TTLForGVR(gvr schema.GroupVersionResource) time.Duration

	// ── User tracking ────────────────────────────────────────────────────
	SAddUser(ctx context.Context, username string) error
	SRemUser(ctx context.Context, username string) error

	// ── String helpers ───────────────────────────────────────────────────
	SetStringWithTTL(ctx context.Context, key, value string, ttl time.Duration) error
	GetString(ctx context.Context, key string) (string, bool, error)

	// ── Stats ────────────────────────────────────────────────────────────
	DBSize(ctx context.Context) int64
}

// compile-time check: *RedisCache satisfies Cache.
var _ Cache = (*RedisCache)(nil)

// PipelineFrom returns the Redis pipeliner if the cache is a *RedisCache, nil otherwise.
// Callers that need pipeline access (RegisterL1Dependencies, widget dep registration)
// already guard against a nil return.
func PipelineFrom(ctx context.Context, c Cache) redis.Pipeliner {
	if rc, ok := c.(*RedisCache); ok {
		return rc.Pipeline(ctx)
	}
	return nil
}
