package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// compile-time check: *MemCache satisfies Cache.
var _ Cache = (*MemCache)(nil)

// ── Entry types ──────────────────────────────────────────────────────────────

// memEntry stores a single key-value pair with an optional expiry.
type memEntry struct {
	data      []byte
	expiresAt int64 // unix nano; 0 = never expires
}

// memSetEntry stores a set of string members with an optional expiry.
type memSetEntry struct {
	members   map[string]bool
	expiresAt int64
	mu        sync.RWMutex
}

// ── MemCache ─────────────────────────────────────────────────────────────────

// MemCache is a pure in-process implementation of the Cache interface using
// sync.Map for storage. Zero external dependencies — no Redis, no disk.
// Suitable for single-replica deployments or integration tests.
type MemCache struct {
	// Key-value store for all cached data (L1 resolved, object cache,
	// API results, user config, strings, negative cache sentinels).
	kv sync.Map // string -> *memEntry

	// Set store for Redis SET-like operations (dep tracking, GVR set,
	// user set, resolved index, list index).
	sets sync.Map // string -> *memSetEntry

	// RBAC decision cache. Key format: "user\x00field" where field is
	// the same format as RBACField (verb:group/resource:namespace).
	rbac sync.Map // string -> *memEntry (data = "true" or "false")

	// GVR TTLs and notifier (same semantics as RedisCache).
	gvrTTLs     sync.Map
	onNewGVR    atomic.Value // stores gvrNotifyFunc
	resourceTTL time.Duration
}

// NewMem creates a new in-process cache with the given default resource TTL.
func NewMem(resourceTTL time.Duration) *MemCache {
	return &MemCache{resourceTTL: resourceTTL}
}

// ── Per-GVR TTL (called via type assertion) ──────────────────────────────────

func (c *MemCache) RegisterGVRTTL(gvr schema.GroupVersionResource, ttl time.Duration) {
	if c == nil || ttl == 0 {
		return
	}
	c.gvrTTLs.Store(GVRToKey(gvr), ttl)
}

func (c *MemCache) TTLForGVR(gvr schema.GroupVersionResource) time.Duration {
	if c == nil {
		return 0
	}
	if v, ok := c.gvrTTLs.Load(GVRToKey(gvr)); ok {
		return v.(time.Duration)
	}
	return c.resourceTTL
}

// SetGVRNotifier registers a callback invoked when a GVR is seen for the
// first time via SAddGVR.
func (c *MemCache) SetGVRNotifier(fn func(context.Context, schema.GroupVersionResource)) {
	if c != nil {
		c.onNewGVR.Store(fn)
	}
}

// ── Background eviction ──────────────────────────────────────────────────────

// StartEviction launches a background goroutine that removes expired entries
// every 30 seconds. The goroutine exits when ctx is cancelled.
func (c *MemCache) StartEviction(ctx context.Context) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now().UnixNano()
				c.kv.Range(func(k, v any) bool {
					if e := v.(*memEntry); e.expiresAt > 0 && e.expiresAt < now {
						c.kv.Delete(k)
					}
					return true
				})
				c.sets.Range(func(k, v any) bool {
					se := v.(*memSetEntry)
					if se.expiresAt > 0 && se.expiresAt < now {
						c.sets.Delete(k)
					}
					return true
				})
				c.rbac.Range(func(k, v any) bool {
					if e := v.(*memEntry); e.expiresAt > 0 && e.expiresAt < now {
						c.rbac.Delete(k)
					}
					return true
				})
			}
		}
	}()
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// expiresAt computes the absolute expiry timestamp from a TTL.
// A zero TTL means no expiry.
func expiresAt(ttl time.Duration) int64 {
	if ttl <= 0 {
		return 0
	}
	return time.Now().Add(ttl).UnixNano()
}

// isExpired returns true if the entry has a non-zero expiry that is in the past.
func isExpired(expiresAt int64) bool {
	return expiresAt > 0 && expiresAt < time.Now().UnixNano()
}

// cloneBytes returns a copy of b so callers cannot mutate cache internals.
func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

// ── Core read/write ──────────────────────────────────────────────────────────

func (c *MemCache) GetRaw(_ context.Context, key string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return nil, false, nil
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.kv.Delete(key)
		return nil, false, nil
	}
	if bytes.Equal(e.data, []byte(notFoundSentinel)) {
		return nil, false, nil
	}
	return cloneBytes(e.data), true, nil
}

func (c *MemCache) Get(ctx context.Context, key string, dest any) (bool, error) {
	if c == nil {
		return false, nil
	}
	raw, ok, err := c.GetRaw(ctx, key)
	if !ok || err != nil {
		return false, err
	}
	return true, json.Unmarshal(raw, dest)
}

func (c *MemCache) GetRawMulti(ctx context.Context, keys []string) map[string][]byte {
	if c == nil || len(keys) == 0 {
		return nil
	}
	result := make(map[string][]byte, len(keys))
	for _, k := range keys {
		raw, ok, _ := c.GetRaw(ctx, k)
		if ok {
			result[k] = raw
		}
	}
	return result
}

func (c *MemCache) Exists(_ context.Context, key string) bool {
	if c == nil {
		return false
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.kv.Delete(key)
		return false
	}
	return true
}

func (c *MemCache) Set(_ context.Context, key string, val any) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kv.Store(key, &memEntry{data: data, expiresAt: expiresAt(c.resourceTTL)})
	return nil
}

func (c *MemCache) SetWithTTL(_ context.Context, key string, val any, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kv.Store(key, &memEntry{data: data, expiresAt: expiresAt(ttl)})
	return nil
}

func (c *MemCache) SetRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(c.resourceTTL)})
	return nil
}

func (c *MemCache) SetForGVR(_ context.Context, gvr schema.GroupVersionResource, key string, val any) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	c.kv.Store(key, &memEntry{data: data, expiresAt: expiresAt(c.TTLForGVR(gvr))})
	return nil
}

func (c *MemCache) SetMultiForGVR(_ context.Context, gvr schema.GroupVersionResource, entries map[string]any) error {
	if c == nil || len(entries) == 0 {
		return nil
	}
	ttl := c.TTLForGVR(gvr)
	exp := expiresAt(ttl)
	for key, val := range entries {
		data, err := json.Marshal(val)
		if err != nil {
			return err
		}
		c.kv.Store(key, &memEntry{data: data, expiresAt: exp})
	}
	return nil
}

func (c *MemCache) SetRawForGVR(_ context.Context, gvr schema.GroupVersionResource, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(c.TTLForGVR(gvr))})
	return nil
}

// SetResolvedRaw stores a fully-resolved widget/restaction output with
// ResolvedCacheTTL. Also tracks the key in a per-user resolved index set
// for O(1) invalidation (matching RedisCache.SetResolvedRaw behavior).
func (c *MemCache) SetResolvedRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(ResolvedCacheTTL)})

	// Maintain per-user resolved index.
	info, hasInfo := ParseResolvedKey(key)
	if hasInfo && info.Username != "" {
		idxKey := UserResolvedIndexKey(info.Username)
		c.saddInternal(idxKey, key, ReverseIndexTTL)
	}
	return nil
}

// SetAPIResultRaw stores an API result with APIResultCacheTTL (60s).
func (c *MemCache) SetAPIResultRaw(_ context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{data: cloneBytes(val), expiresAt: expiresAt(APIResultCacheTTL)})
	return nil
}

func (c *MemCache) Delete(_ context.Context, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	for _, k := range keys {
		c.kv.Delete(k)
	}
	return nil
}

// ── Negative-cache ───────────────────────────────────────────────────────────

func (c *MemCache) GetNotFound(_ context.Context, key string) bool {
	if c == nil {
		return false
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.kv.Delete(key)
		return false
	}
	return bytes.Equal(e.data, []byte(notFoundSentinel))
}

func (c *MemCache) SetNotFound(_ context.Context, key string) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{
		data:      []byte(notFoundSentinel),
		expiresAt: expiresAt(notFoundTTL),
	})
	return nil
}

// ── Atomic ops ───────────────────────────────────────────────────────────────

// AtomicUpdateJSON performs a read-modify-write. Since this is in-process,
// we use a simple load/store approach. There are no current callers that
// race on the same key, so the window is acceptable.
func (c *MemCache) AtomicUpdateJSON(_ context.Context, key string, fn func([]byte) ([]byte, error), ttl time.Duration) error {
	if c == nil {
		return nil
	}
	var existing []byte
	if v, ok := c.kv.Load(key); ok {
		e := v.(*memEntry)
		if !isExpired(e.expiresAt) && !bytes.Equal(e.data, []byte(notFoundSentinel)) {
			existing = cloneBytes(e.data)
		}
	}
	newVal, err := fn(existing)
	if err != nil || newVal == nil {
		return err
	}
	c.kv.Store(key, &memEntry{data: newVal, expiresAt: expiresAt(ttl)})
	return nil
}

// ── Key scanning ─────────────────────────────────────────────────────────────

// ScanKeys returns all non-expired keys matching the given glob pattern.
// Supports * and ? wildcards via path.Match (compatible with Redis SCAN
// glob semantics for the patterns used in this codebase).
func (c *MemCache) ScanKeys(_ context.Context, pattern string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	var keys []string
	c.kv.Range(func(k, v any) bool {
		key := k.(string)
		e := v.(*memEntry)
		if isExpired(e.expiresAt) {
			c.kv.Delete(k)
			return true
		}
		if matched, _ := path.Match(pattern, key); matched {
			keys = append(keys, key)
		}
		return true
	})
	return keys, nil
}

// ── Set operations ───────────────────────────────────────────────────────────

// saddInternal adds a member to a set with TTL. Shared by public methods.
func (c *MemCache) saddInternal(key, member string, ttl time.Duration) {
	exp := expiresAt(ttl)
	for {
		v, loaded := c.sets.LoadOrStore(key, &memSetEntry{
			members:   map[string]bool{member: true},
			expiresAt: exp,
		})
		if !loaded {
			return // freshly created
		}
		se := v.(*memSetEntry)
		se.mu.Lock()
		if isExpired(se.expiresAt) {
			// Expired entry — replace it.
			se.mu.Unlock()
			c.sets.Delete(key)
			continue // retry LoadOrStore
		}
		se.members[member] = true
		se.expiresAt = exp
		se.mu.Unlock()
		return
	}
}

func (c *MemCache) SAddWithTTL(_ context.Context, key, member string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	c.saddInternal(key, member, ttl)
	return nil
}

func (c *MemCache) SAddMultiWithTTL(_ context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	exp := expiresAt(ttl)
	initial := make(map[string]bool, len(members))
	for _, m := range members {
		initial[m] = true
	}
	for {
		v, loaded := c.sets.LoadOrStore(key, &memSetEntry{
			members:   initial,
			expiresAt: exp,
		})
		if !loaded {
			return nil
		}
		se := v.(*memSetEntry)
		se.mu.Lock()
		if isExpired(se.expiresAt) {
			se.mu.Unlock()
			c.sets.Delete(key)
			continue
		}
		for _, m := range members {
			se.members[m] = true
		}
		se.expiresAt = exp
		se.mu.Unlock()
		return nil
	}
}

func (c *MemCache) SRemMembers(_ context.Context, key string, members ...string) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	v, ok := c.sets.Load(key)
	if !ok {
		return nil
	}
	se := v.(*memSetEntry)
	se.mu.Lock()
	defer se.mu.Unlock()
	for _, m := range members {
		delete(se.members, m)
	}
	return nil
}

func (c *MemCache) ReplaceSetWithTTL(_ context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	if len(members) == 0 {
		c.sets.Delete(key)
		return nil
	}
	newMembers := make(map[string]bool, len(members))
	for _, m := range members {
		newMembers[m] = true
	}
	c.sets.Store(key, &memSetEntry{
		members:   newMembers,
		expiresAt: expiresAt(ttl),
	})
	return nil
}

func (c *MemCache) SMembers(_ context.Context, key string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	v, ok := c.sets.Load(key)
	if !ok {
		return nil, nil
	}
	se := v.(*memSetEntry)
	se.mu.RLock()
	defer se.mu.RUnlock()
	if isExpired(se.expiresAt) {
		c.sets.Delete(key)
		return nil, nil
	}
	result := make([]string, 0, len(se.members))
	for m := range se.members {
		result = append(result, m)
	}
	return result, nil
}

// ── List assembly ────────────────────────────────────────────────────────────

// AssembleListFromIndex reads a list-index SET and fetches all referenced GET
// keys, returning the assembled raw JSON as an UnstructuredList. Returns
// (nil, false, nil) if the index does not exist or all items have expired.
func (c *MemCache) AssembleListFromIndex(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	idxKey := ListIndexKey(gvr, namespace)
	members, err := c.SMembers(ctx, idxKey)
	if err != nil || len(members) == 0 {
		return nil, false, err
	}

	// Build GET keys from member names (same logic as RedisCache).
	getKeys := make([]string, len(members))
	for i, member := range members {
		if namespace == "" {
			parts := strings.SplitN(member, "/", 2)
			if len(parts) == 2 {
				getKeys[i] = GetKey(gvr, parts[0], parts[1])
			} else {
				getKeys[i] = GetKey(gvr, "", member)
			}
		} else {
			getKeys[i] = GetKey(gvr, namespace, member)
		}
	}

	results := c.GetRawMulti(ctx, getKeys)
	if len(results) == 0 {
		return nil, false, nil
	}

	// Assemble into an UnstructuredList JSON (same format as RedisCache).
	var buf bytes.Buffer
	buf.WriteString(`{"apiVersion":"`)
	g := gvr.Group
	if g == "" {
		buf.WriteString(gvr.Version)
	} else {
		buf.WriteString(g)
		buf.WriteByte('/')
		buf.WriteString(gvr.Version)
	}
	buf.WriteString(`","kind":"List","metadata":{"resourceVersion":""},"items":[`)

	first := true
	for _, k := range getKeys {
		raw, ok := results[k]
		if !ok || IsNotFoundRaw(raw) {
			continue
		}
		if !first {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		first = false
	}
	buf.WriteString(`]}`)

	return buf.Bytes(), true, nil
}

// ── RBAC ─────────────────────────────────────────────────────────────────────

// rbacKey builds the internal RBAC map key: "user\x00field".
func rbacKey(username, field string) string {
	return username + "\x00" + field
}

func (c *MemCache) IsRBACAllowed(_ context.Context, username, verb string, gr schema.GroupResource, namespace string) (allowed, cached bool) {
	if c == nil {
		return false, false
	}
	field := RBACField(verb, gr, namespace)
	v, ok := c.rbac.Load(rbacKey(username, field))
	if !ok {
		return false, false
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.rbac.Delete(rbacKey(username, field))
		return false, false
	}
	return string(e.data) == "true", true
}

func (c *MemCache) SetRBACResult(_ context.Context, username, verb string, gr schema.GroupResource, namespace string, allowed bool, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	field := RBACField(verb, gr, namespace)
	val := "false"
	if allowed {
		val = "true"
	}
	c.rbac.Store(rbacKey(username, field), &memEntry{
		data:      []byte(val),
		expiresAt: expiresAt(ttl),
	})
	return nil
}

func (c *MemCache) DeleteUserRBAC(_ context.Context, username string) error {
	if c == nil {
		return nil
	}
	prefix := username + "\x00"
	c.rbac.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			c.rbac.Delete(k)
		}
		return true
	})
	return nil
}

// ── GVR tracking ─────────────────────────────────────────────────────────────

func (c *MemCache) SAddGVR(ctx context.Context, gvr schema.GroupVersionResource) error {
	if c == nil {
		return nil
	}
	member := GVRToKey(gvr)
	v, ok := c.sets.Load(WatchedGVRsKey)
	isNew := false
	if !ok {
		// First GVR — create the set.
		c.sets.Store(WatchedGVRsKey, &memSetEntry{
			members: map[string]bool{member: true},
		})
		isNew = true
	} else {
		se := v.(*memSetEntry)
		se.mu.Lock()
		if !se.members[member] {
			se.members[member] = true
			isNew = true
		}
		se.mu.Unlock()
	}
	if isNew {
		if fn, ok := c.onNewGVR.Load().(gvrNotifyFunc); ok && fn != nil {
			fn(ctx, gvr)
		}
	}
	return nil
}

// ── User tracking ────────────────────────────────────────────────────────────

func (c *MemCache) SAddUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	// No TTL for active-users set — matches Redis behavior (no EXPIRE on SADD).
	return c.SAddWithTTL(ctx, ActiveUsersKey, username, 0)
}

func (c *MemCache) SRemUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	return c.SRemMembers(ctx, ActiveUsersKey, username)
}

// ── String helpers ───────────────────────────────────────────────────────────

func (c *MemCache) SetStringWithTTL(_ context.Context, key, value string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	c.kv.Store(key, &memEntry{data: []byte(value), expiresAt: expiresAt(ttl)})
	return nil
}

func (c *MemCache) GetString(_ context.Context, key string) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	v, ok := c.kv.Load(key)
	if !ok {
		return "", false, nil
	}
	e := v.(*memEntry)
	if isExpired(e.expiresAt) {
		c.kv.Delete(key)
		return "", false, nil
	}
	return string(e.data), true, nil
}

// ── Stats ────────────────────────────────────────────────────────────────────

func (c *MemCache) DBSize(_ context.Context) int64 {
	if c == nil {
		return 0
	}
	var n int64
	c.kv.Range(func(_, _ any) bool { n++; return true })
	c.sets.Range(func(_, _ any) bool { n++; return true })
	c.rbac.Range(func(_, _ any) bool { n++; return true })
	return n
}
