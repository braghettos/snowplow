package cache

import (
	"context"
	"os"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Disabled returns true when the CACHE_ENABLED env var is "false" or "0".
func Disabled() bool {
	v := strings.ToLower(os.Getenv("CACHE_ENABLED"))
	return v == "false" || v == "0"
}

// gvrNotifyFunc is the callback signature used to notify the ResourceWatcher
// when a new GVR is registered (so it can start an informer).
type gvrNotifyFunc = func(context.Context, schema.GroupVersionResource)

const (
	DefaultResourceTTL = time.Hour
	ResolvedCacheTTL   = time.Hour
	APIResultCacheTTL  = 60 * time.Second
	ReverseIndexTTL    = 2 * time.Hour
	notFoundTTL        = 30 * time.Second
)

// Cache is the interface satisfied by *MemCache (and test doubles).
// It contains every method that callers outside the cache package need.
//
// Methods intentionally NOT on the interface:
//   - SetGVRNotifier, RegisterGVRTTL (configuration, concrete-only)
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
	// SAddWithTTLN behaves like SAddWithTTL but returns the number of members
	// actually added (0 means the member already existed — i.e. dedup).
	// Used by instrumentation to count cluster-wide dep dedup hits.
	SAddWithTTLN(ctx context.Context, key, member string, ttl time.Duration) (int, error)
	SAddMultiWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error
	SRemMembers(ctx context.Context, key string, members ...string) error
	ReplaceSetWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error
	SMembers(ctx context.Context, key string) ([]string, error)
	// SCard returns the cardinality of the set at key (0 if absent or expired).
	// Used by sampled instrumentation; O(1) on MemCache.
	SCard(ctx context.Context, key string) (int64, error)

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

// compile-time check: *MemCache satisfies Cache.
var _ Cache = (*MemCache)(nil)
