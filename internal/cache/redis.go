package cache

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var redisTracer = otel.Tracer("snowplow/redis")

type gvrNotifyFunc = func(context.Context, schema.GroupVersionResource)

const (
	DefaultResourceTTL  = time.Hour
	ResolvedCacheTTL    = time.Hour
	APIResultCacheTTL   = 60 * time.Second // short TTL: event-driven invalidation handles normal case, TTL handles races
	ReverseIndexTTL     = 2 * time.Hour
	notFoundTTL         = 30 * time.Second
)

// ── Transparent zstd compression (with gzip read-back for migration) ────────

const compressMinSize = 256

// Package-level zstd encoder/decoder — these are expensive to create and
// goroutine-safe, so we reuse a single instance of each.
var (
	zstdEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	zstdDecoder, _ = zstd.NewReader(nil)
)

func compressValue(data []byte) []byte {
	if len(data) < compressMinSize {
		return data
	}
	return zstdEncoder.EncodeAll(data, make([]byte, 0, len(data)/2))
}

func decompressValue(data []byte) []byte {
	if len(data) < 2 {
		return data
	}
	// Legacy gzip: magic bytes 0x1F 0x8B
	if data[0] == 0x1f && data[1] == 0x8b {
		return decompressGzip(data)
	}
	// zstd: magic bytes 0x28 0xB5 0x2F 0xFD (check first 4 bytes)
	if len(data) >= 4 && data[0] == 0x28 && data[1] == 0xB5 && data[2] == 0x2F && data[3] == 0xFD {
		out, err := zstdDecoder.DecodeAll(data, nil)
		if err != nil {
			return data
		}
		return out
	}
	// Uncompressed (below compressMinSize threshold)
	return data
}

// decompressGzip handles legacy gzip-compressed values during migration.
func decompressGzip(data []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return data
	}
	return out
}

// resolvedPrefix is the key prefix for L1 resolved cache entries.
// Used to route these keys to disk when a DiskStore is configured.
const resolvedPrefix = "snowplow:resolved:"

// isResolvedKey returns true if the key is an L1 resolved cache entry.
func isResolvedKey(key string) bool {
	return strings.HasPrefix(key, resolvedPrefix)
}

// RedisCache is a Redis-backed cache. All methods are safe on a nil receiver.
type RedisCache struct {
	client      *redis.Client
	diskStore   *DiskStore // nil = use Redis for L1 resolved keys
	ResourceTTL time.Duration
	gvrTTLs     sync.Map
	onNewGVR    atomic.Value // stores gvrNotifyFunc
}

func redisAddr() string {
	if v := os.Getenv("REDIS_ADDRESS"); v != "" {
		return v
	}
	return "localhost:6379"
}

// Disabled returns true when the CACHE_ENABLED env var is explicitly set to
// "false" or "0". When not set the cache is enabled by default.
func Disabled() bool {
	v := strings.ToLower(os.Getenv("CACHE_ENABLED"))
	return v == "false" || v == "0"
}

func New(resourceTTL time.Duration) *RedisCache {
	return &RedisCache{
		client: redis.NewClient(&redis.Options{
			Addr:         redisAddr(),
			DB:           0,
			DialTimeout:  3 * time.Second,
			ReadTimeout:  2 * time.Second,
			WriteTimeout: 2 * time.Second,
			PoolSize:     50,
			MinIdleConns: 5,
			MaxRetries:   2,
		}),
		ResourceTTL: resourceTTL,
	}
}

// SetDiskStore configures a disk-backed store for L1 resolved keys.
// When set, GetRaw/SetResolvedRaw/Delete route snowplow:resolved:* keys
// to disk instead of Redis. Pass nil to disable (default).
func (c *RedisCache) SetDiskStore(ds *DiskStore) {
	if c != nil {
		c.diskStore = ds
	}
}

// ── Per-GVR TTL ───────────────────────────────────────────────────────────────

func (c *RedisCache) RegisterGVRTTL(gvr schema.GroupVersionResource, ttl time.Duration) {
	if c == nil || ttl == 0 {
		return
	}
	c.gvrTTLs.Store(GVRToKey(gvr), ttl)
}

func (c *RedisCache) TTLForGVR(gvr schema.GroupVersionResource) time.Duration {
	if c == nil {
		return 0
	}
	if v, ok := c.gvrTTLs.Load(GVRToKey(gvr)); ok {
		return v.(time.Duration)
	}
	return c.ResourceTTL
}

func (c *RedisCache) SetForGVR(ctx context.Context, gvr schema.GroupVersionResource, key string, val any) error {
	return c.setWithTTL(ctx, key, val, c.TTLForGVR(gvr))
}

// SetMultiForGVR writes multiple key/value pairs in a single pipelined
// round-trip. Each value is JSON-marshaled and zstd-compressed. This is
// dramatically faster than calling SetForGVR in a loop at large scale:
// at 50K items per reconcile, sequential SETs take ~7s; pipelined takes
// ~200ms per batch.
func (c *RedisCache) SetMultiForGVR(ctx context.Context, gvr schema.GroupVersionResource, entries map[string]any) error {
	if c == nil || len(entries) == 0 {
		return nil
	}
	ttl := c.TTLForGVR(gvr)
	pipe := c.client.Pipeline()
	for key, val := range entries {
		data, err := json.Marshal(val)
		if err != nil {
			return err
		}
		pipe.Set(ctx, key, compressValue(data), ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (c *RedisCache) SetRawForGVR(ctx context.Context, gvr schema.GroupVersionResource, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, compressValue(val), c.TTLForGVR(gvr)).Err()
}

// ── GVR notifier ──────────────────────────────────────────────────────────────

func (c *RedisCache) SetGVRNotifier(fn gvrNotifyFunc) {
	if c != nil {
		c.onNewGVR.Store(fn)
	}
}

// ── Pipeline access ──────────────────────────────────────────────────────────

// Pipeline returns a Redis pipeline for batching commands into a single
// round-trip. Returns nil if the cache is disabled. Callers must call
// pipe.Exec(ctx) to flush the pipeline.
func (c *RedisCache) Pipeline(_ context.Context) redis.Pipeliner {
	if c == nil {
		return nil
	}
	return c.client.Pipeline()
}

// ── Core ops ──────────────────────────────────────────────────────────────────

func (c *RedisCache) Ping(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.client.Ping(ctx).Err()
}

func (c *RedisCache) Close() error {
	if c == nil {
		return nil
	}
	return c.client.Close()
}

func (c *RedisCache) Get(ctx context.Context, key string, dest any) (bool, error) {
	if c == nil {
		return false, nil
	}
	val, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	val = decompressValue(val)
	if bytes.Equal(val, []byte(notFoundSentinel)) {
		return false, nil
	}
	return true, json.Unmarshal(val, dest)
}

func (c *RedisCache) GetRaw(ctx context.Context, key string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}

	// Route L1 resolved keys to disk when configured.
	if c.diskStore != nil && isResolvedKey(key) {
		return c.getDiskRaw(ctx, key)
	}

	ctx, span := redisTracer.Start(ctx, "redis.get",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "GET"),
			attribute.String("cache.key", key),
		))
	defer span.End()

	val, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		if span.IsRecording() {
			span.SetAttributes(attribute.Bool("cache.hit", false))
		}
		return nil, false, nil
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, false, err
	}
	val = decompressValue(val)
	if bytes.Equal(val, []byte(notFoundSentinel)) {
		if span.IsRecording() {
			span.SetAttributes(attribute.Bool("cache.hit", false))
		}
		return nil, false, nil
	}
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Bool("cache.hit", true),
			attribute.Int("cache.value_size", len(val)),
		)
	}
	return val, true, nil
}

// getDiskRaw reads an L1 resolved key from the disk store.
// The disk store returns already-compressed bytes, so we decompress here
// (matching the Redis path's behavior).
func (c *RedisCache) getDiskRaw(ctx context.Context, key string) ([]byte, bool, error) {
	_, span := redisTracer.Start(ctx, "disk.get",
		trace.WithAttributes(
			attribute.String("db.system", "disk"),
			attribute.String("db.operation", "GET"),
			attribute.String("cache.key", key),
		))
	defer span.End()

	data, found, err := c.diskStore.Get(key)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, false, err
	}
	if !found {
		if span.IsRecording() {
			span.SetAttributes(attribute.Bool("cache.hit", false))
		}
		return nil, false, nil
	}
	val := decompressValue(data)
	if bytes.Equal(val, []byte(notFoundSentinel)) {
		if span.IsRecording() {
			span.SetAttributes(attribute.Bool("cache.hit", false))
		}
		return nil, false, nil
	}
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Bool("cache.hit", true),
			attribute.Int("cache.value_size", len(val)),
		)
	}
	return val, true, nil
}

// GetRawMulti fetches multiple keys in a single Redis pipeline round-trip.
// Returns a map of key → decompressed value for keys that exist.
// Keys that don't exist or have the not-found sentinel are omitted.
//
// When len(keys) > mgetChunkSize, the request is split into chunks to avoid
// blocking Redis for too long with a single large MGET (B3 scaling roadmap).
func (c *RedisCache) GetRawMulti(ctx context.Context, keys []string) map[string][]byte {
	if c == nil || len(keys) == 0 {
		return nil
	}

	ctx, span := redisTracer.Start(ctx, "redis.mget",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "MGET"),
			attribute.Int("cache.keys.requested", len(keys)),
		))
	defer span.End()

	const mgetChunkSize = 100

	var chunks int
	var totalBytes int
	result := make(map[string][]byte, len(keys))
	for start := 0; start < len(keys); start += mgetChunkSize {
		end := start + mgetChunkSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		chunks++

		chunkCtx, chunkSpan := redisTracer.Start(ctx, "redis.mget.chunk",
			trace.WithAttributes(
				attribute.Int("cache.chunk.size", len(chunk)),
				attribute.Int("cache.chunk.index", chunks-1),
			))
		pipe := c.client.Pipeline()
		cmds := make([]*redis.StringCmd, len(chunk))
		for i, k := range chunk {
			cmds[i] = pipe.Get(chunkCtx, k)
		}
		_, _ = pipe.Exec(chunkCtx)

		var chunkHits, chunkBytes int
		for i, cmd := range cmds {
			val, err := cmd.Bytes()
			if err != nil {
				continue
			}
			val = decompressValue(val)
			if bytes.Equal(val, []byte(notFoundSentinel)) {
				continue
			}
			result[chunk[i]] = val
			chunkHits++
			chunkBytes += len(val)
		}
		totalBytes += chunkBytes
		if chunkSpan.IsRecording() {
			chunkSpan.SetAttributes(
				attribute.Int("cache.chunk.hits", chunkHits),
				attribute.Int("cache.chunk.bytes", chunkBytes),
			)
		}
		chunkSpan.End()
	}
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Int("cache.chunks", chunks),
			attribute.Int("cache.hits", len(result)),
			attribute.Int("cache.bytes", totalBytes),
		)
	}
	return result
}

// Exists returns true when the key is present in the cache (regardless of value).
func (c *RedisCache) Exists(ctx context.Context, key string) bool {
	if c == nil {
		return false
	}
	// Route resolved keys to disk when configured.
	if c.diskStore != nil && isResolvedKey(key) {
		_, found, err := c.diskStore.Get(key)
		return err == nil && found
	}
	n, err := c.client.Exists(ctx, key).Result()
	return err == nil && n > 0
}

// GetNotFound returns true when the key holds the negative-cache sentinel.
func (c *RedisCache) GetNotFound(ctx context.Context, key string) bool {
	if c == nil {
		return false
	}
	val, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		return false
	}
	val = decompressValue(val)
	return bytes.Equal(val, []byte(notFoundSentinel))
}

// SetNotFound caches a 404 sentinel for key with a short TTL.
// NegativeHits tracks sentinel READS (hits), not stores; use ExpiryRefreshes
// for stored sentinels is intentional so metrics stay distinct.
func (c *RedisCache) SetNotFound(ctx context.Context, key string) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, notFoundSentinel, notFoundTTL).Err()
}

func (c *RedisCache) Set(ctx context.Context, key string, val any) error {
	return c.setWithTTL(ctx, key, val, c.ResourceTTL)
}

func (c *RedisCache) SetWithTTL(ctx context.Context, key string, val any, ttl time.Duration) error {
	return c.setWithTTL(ctx, key, val, ttl)
}

func (c *RedisCache) SetRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	ctx, span := redisTracer.Start(ctx, "redis.set",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "SET"),
			attribute.String("cache.key", key),
			attribute.Int("cache.value_size", len(val)),
		))
	defer span.End()

	err := c.client.Set(ctx, key, compressValue(val), c.ResourceTTL).Err()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// SetResolvedRaw stores a fully-resolved widget/restaction output with
// ResolvedCacheTTL. Freshness is guaranteed by the GVR reverse index
// (targeted invalidation from informer events); TTL is only for memory
// management. Also tracks the key in a per-user index SET for O(1) invalidation.
// The SET + SADD + EXPIRE are executed atomically in a MULTI/EXEC transaction
// so a crash between steps cannot leave orphaned keys without index entries (Bug 9).
func (c *RedisCache) SetResolvedRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	compressed := compressValue(val)

	// Route L1 resolved keys to disk when configured.
	// The per-user index SET stays in Redis (it's small and used for O(1) invalidation).
	if c.diskStore != nil && isResolvedKey(key) {
		if err := c.diskStore.Set(key, compressed); err != nil {
			return err
		}
		// Still maintain the per-user index in Redis for invalidation lookups.
		info, hasInfo := ParseResolvedKey(key)
		if hasInfo && info.Username != "" {
			idxKey := UserResolvedIndexKey(info.Username)
			pipe := c.client.TxPipeline()
			pipe.SAdd(ctx, idxKey, key)
			pipe.Expire(ctx, idxKey, ReverseIndexTTL)
			_, err := pipe.Exec(ctx)
			return err
		}
		return nil
	}

	info, hasInfo := ParseResolvedKey(key)
	if hasInfo && info.Username != "" {
		idxKey := UserResolvedIndexKey(info.Username)
		pipe := c.client.TxPipeline()
		pipe.Set(ctx, key, compressed, ResolvedCacheTTL)
		pipe.SAdd(ctx, idxKey, key)
		pipe.Expire(ctx, idxKey, ReverseIndexTTL)
		_, err := pipe.Exec(ctx)
		return err
	}
	return c.client.Set(ctx, key, compressed, ResolvedCacheTTL).Err()
}

// SetAPIResultRaw stores a compressed API result with APIResultCacheTTL.
// Short TTL ensures stale data from race conditions (e.g. CRD status not yet
// populated) auto-expires even if event-driven invalidation misses.
func (c *RedisCache) SetAPIResultRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, compressValue(val), APIResultCacheTTL).Err()
}

func (c *RedisCache) setWithTTL(ctx context.Context, key string, val any, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, key, compressValue(data), ttl).Err()
}

func (c *RedisCache) Delete(ctx context.Context, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}

	// When disk store is configured, split keys: resolved → disk, rest → Redis.
	if c.diskStore != nil {
		var diskKeys, redisKeys []string
		for _, k := range keys {
			if isResolvedKey(k) {
				diskKeys = append(diskKeys, k)
			} else {
				redisKeys = append(redisKeys, k)
			}
		}
		if len(diskKeys) > 0 {
			_ = c.diskStore.Delete(diskKeys...)
		}
		if len(redisKeys) > 0 {
			return c.unlinkOrDel(ctx, redisKeys)
		}
		return nil
	}

	return c.unlinkOrDel(ctx, keys)
}

// unlinkOrDel uses UNLINK for bulk deletes (>1 key) to free memory
// asynchronously in a background thread, avoiding blocking the main
// Redis event loop. For single-key deletes, DEL is used for immediate
// reclaim (important for correctness when re-setting the key right after).
func (c *RedisCache) unlinkOrDel(ctx context.Context, keys []string) error {
	if len(keys) == 1 {
		return c.client.Del(ctx, keys...).Err()
	}
	return c.client.Unlink(ctx, keys...).Err()
}

// ── Atomic read-modify-write ──────────────────────────────────────────────────

// AtomicUpdateJSON performs an optimistic-locking read-modify-write using
// Redis WATCH/MULTI/EXEC. Retries up to 3 times on transaction conflict.
// When the key does not exist, fn is called with nil so the caller can
// choose to create the entry from scratch.
func (c *RedisCache) AtomicUpdateJSON(ctx context.Context, key string, fn func([]byte) ([]byte, error), ttl time.Duration) error {
	if c == nil {
		return nil
	}
	ctx, span := redisTracer.Start(ctx, "redis.atomic_update",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "WATCH/MULTI/EXEC"),
			attribute.String("cache.key", key),
		))
	defer span.End()

	// High-contention workloads (e.g. 10 compositions/ns deployed rapidly) can
	// cause repeated WATCH/EXEC failures on the same LIST key. 20 retries with
	// jitter ensures all patches land even under burst event rates.
	const maxRetries = 20
	for i := 0; i < maxRetries; i++ {
		err := c.client.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Bytes()
			if errors.Is(err, redis.Nil) {
				val = nil
			} else if err != nil {
				return err
			} else {
				val = decompressValue(val)
				if bytes.Equal(val, []byte(notFoundSentinel)) {
					val = nil
				}
			}
			newVal, ferr := fn(val)
			if ferr != nil || newVal == nil {
				return ferr
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				return pipe.Set(ctx, key, compressValue(newVal), ttl).Err()
			})
			return err
		}, key)
		if err == nil {
			if span.IsRecording() {
				span.SetAttributes(attribute.Int("redis.retries", i))
			}
			return nil
		}
		if errors.Is(err, redis.TxFailedErr) {
			// Exponential backoff with full jitter (Bug 6: linear retry
			// without jitter causes synchronized retry waves).
			base := time.Duration(1<<min(i, 6)) * time.Millisecond // 1,2,4,...,64ms
			time.Sleep(base + time.Duration(rand.Int64N(int64(base)+1)))
			continue
		}
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if span.IsRecording() {
		span.SetAttributes(attribute.Int("redis.retries", maxRetries))
	}
	span.RecordError(redis.TxFailedErr)
	span.SetStatus(codes.Error, redis.TxFailedErr.Error())
	return redis.TxFailedErr
}

// ── Set helpers ───────────────────────────────────────────────────────────────

func (c *RedisCache) ScanKeys(ctx context.Context, pattern string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	var keys []string
	iter := c.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

// SAddWithTTL adds member to a Redis set and refreshes the set's TTL.
// Uses MULTI/EXEC transaction to ensure atomicity (Bug 5: if Redis
// crashes between SADD and EXPIRE in a plain pipeline, the set
// persists without TTL, causing unbounded growth).
func (c *RedisCache) SAddWithTTL(ctx context.Context, key, member string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	pipe := c.client.TxPipeline()
	pipe.SAdd(ctx, key, member)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// SAddMultiWithTTL adds multiple members to a Redis set and refreshes the TTL.
// Uses a pipeline for efficiency — all operations in one round-trip.
func (c *RedisCache) SAddMultiWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	args := make([]interface{}, len(members))
	for i, m := range members {
		args[i] = m
	}
	pipe := c.client.TxPipeline()
	pipe.SAdd(ctx, key, args...)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// SRemMembers removes one or more members from a Redis set.
func (c *RedisCache) SRemMembers(ctx context.Context, key string, members ...string) error {
	if c == nil || len(members) == 0 {
		return nil
	}
	args := make([]interface{}, len(members))
	for i, m := range members {
		args[i] = m
	}
	return c.client.SRem(ctx, key, args...).Err()
}

// ReplaceSetWithTTL atomically replaces a Redis SET's members using DEL + SADD + EXPIRE
// in a MULTI/EXEC transaction. This ensures the index is always consistent — no partial
// state visible to concurrent readers.
func (c *RedisCache) ReplaceSetWithTTL(ctx context.Context, key string, members []string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	pipe := c.client.TxPipeline()
	pipe.Del(ctx, key)
	if len(members) > 0 {
		args := make([]interface{}, len(members))
		for i, m := range members {
			args[i] = m
		}
		pipe.SAdd(ctx, key, args...)
		pipe.Expire(ctx, key, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// AssembleListFromIndex reads a list-index SET (SMEMBERS) and fetches all
// referenced GET keys via MGET, returning the assembled raw JSON as an
// UnstructuredList. Falls back to the monolithic list blob if the index
// does not exist (migration period).
//
// The apiVersion and kind parameters are needed to construct the list metadata
// when assembling from per-item keys (the index SET does not store this info).
func (c *RedisCache) AssembleListFromIndex(ctx context.Context, gvr schema.GroupVersionResource, namespace string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	ctx, span := redisTracer.Start(ctx, "cache.assemble_list",
		trace.WithAttributes(
			attribute.String("gvr", gvr.String()),
			attribute.String("namespace", namespace),
		))
	defer span.End()
	idxKey := ListIndexKey(gvr, namespace)
	members, err := c.client.SMembers(ctx, idxKey).Result()
	if err != nil || len(members) == 0 {
		// Index does not exist or is empty — fall back to blob.
		if span.IsRecording() {
			span.SetAttributes(attribute.Bool("cache.index.hit", false))
			if err != nil {
				span.RecordError(err)
			}
		}
		return nil, false, err
	}
	if span.IsRecording() {
		span.SetAttributes(
			attribute.Bool("cache.index.hit", true),
			attribute.Int("cache.members", len(members)),
		)
	}

	// Build GET keys from member names.
	// For namespace-scoped indexes, members are just "name".
	// For cluster-wide indexes (namespace=""), members are "ns/name".
	getKeys := make([]string, len(members))
	for i, member := range members {
		if namespace == "" {
			// Cluster-wide index: member format is "ns/name".
			parts := strings.SplitN(member, "/", 2)
			if len(parts) == 2 {
				getKeys[i] = GetKey(gvr, parts[0], parts[1])
			} else {
				// Cluster-scoped resource (no namespace): member is just "name".
				getKeys[i] = GetKey(gvr, "", member)
			}
		} else {
			getKeys[i] = GetKey(gvr, namespace, member)
		}
	}

	// MGET all item keys in one pipeline.
	results := c.GetRawMulti(ctx, getKeys)
	if len(results) == 0 {
		// All items expired — treat as miss.
		return nil, false, nil
	}

	// Assemble into an UnstructuredList JSON.
	// We build the JSON manually to avoid unmarshal/re-marshal overhead.
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

// IsRBACAllowed performs a cache-only RBAC lookup (no K8s API call).
// Returns (allowed, cached). If the RBAC result is not in the cache,
// cached is false and allowed defaults to false (conservative deny).
//
// Uses Redis HASH: one hash per user, field per RBAC decision.
// Falls back to legacy STRING key format for migration compatibility.
func (c *RedisCache) IsRBACAllowed(ctx context.Context, username, verb string, gr schema.GroupResource, namespace string) (allowed, cached bool) {
	if c == nil {
		return false, false
	}
	hashKey := RBACHashKey(username)
	field := RBACField(verb, gr, namespace)
	val, err := c.client.HGet(ctx, hashKey, field).Result()
	if err == nil {
		return val == "true", true
	}
	return false, false
}

// SetRBACResult stores an RBAC decision in the per-user Redis HASH.
// Sets TTL on the entire hash (all fields for this user expire together).
func (c *RedisCache) SetRBACResult(ctx context.Context, username, verb string, gr schema.GroupResource, namespace string, allowed bool, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	hashKey := RBACHashKey(username)
	field := RBACField(verb, gr, namespace)
	val := "false"
	if allowed {
		val = "true"
	}
	pipe := c.client.TxPipeline()
	pipe.HSet(ctx, hashKey, field, val)
	pipe.Expire(ctx, hashKey, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// DeleteUserRBAC deletes all RBAC decisions for a user with a single DEL.
// O(1) instead of SCAN + bulk DEL.
func (c *RedisCache) DeleteUserRBAC(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	return c.client.Del(ctx, RBACHashKey(username)).Err()
}

func (c *RedisCache) SAddGVR(ctx context.Context, gvr schema.GroupVersionResource) error {
	if c == nil {
		return nil
	}
	added, err := c.client.SAdd(ctx, WatchedGVRsKey, GVRToKey(gvr)).Result()
	if err != nil {
		return err
	}
	if added > 0 {
		if fn, ok := c.onNewGVR.Load().(gvrNotifyFunc); ok && fn != nil {
			fn(ctx, gvr)
		}
	}
	return nil
}

func (c *RedisCache) SMembers(ctx context.Context, key string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	ctx, span := redisTracer.Start(ctx, "redis.smembers",
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", "SMEMBERS"),
			attribute.String("cache.key", key),
		))
	defer span.End()
	members, err := c.client.SMembers(ctx, key).Result()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if span.IsRecording() {
		span.SetAttributes(attribute.Int("cache.members", len(members)))
	}
	return members, nil
}

func (c *RedisCache) SAddUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	return c.client.SAdd(ctx, ActiveUsersKey, username).Err()
}

func (c *RedisCache) SRemUser(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	return c.client.SRem(ctx, ActiveUsersKey, username).Err()
}

// SetStringWithTTL stores a string value with an explicit TTL.
func (c *RedisCache) SetStringWithTTL(ctx context.Context, key, value string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, value, ttl).Err()
}

func (c *RedisCache) GetString(ctx context.Context, key string) (string, bool, error) {
	if c == nil {
		return "", false, nil
	}
	val, err := c.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// DBSize returns the number of keys in the current Redis database.
// Returns 0 on nil receiver or error.
func (c *RedisCache) DBSize(ctx context.Context) int64 {
	if c == nil {
		return 0
	}
	n, err := c.client.DBSize(ctx).Result()
	if err != nil {
		return 0
	}
	return n
}

// DiskFileCount returns the number of .bin files in the disk store.
// Returns 0 if no disk store is configured.
func (c *RedisCache) DiskFileCount() int64 {
	if c == nil || c.diskStore == nil {
		return 0
	}
	return c.diskStore.FileCount()
}

// ── Expiry notifications ──────────────────────────────────────────────────────

func (c *RedisCache) EnableExpiryNotifications(ctx context.Context) error {
	if c == nil {
		return nil
	}
	return c.client.ConfigSet(ctx, "notify-keyspace-events", "Kx").Err()
}

func (c *RedisCache) SubscribeExpired(ctx context.Context) <-chan string {
	ch := make(chan string, 128)
	go func() {
		defer close(ch)
		pubsub := c.client.Subscribe(ctx, "__keyevent@0__:expired")
		defer pubsub.Close()
		msgs := pubsub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-msgs:
				if !ok {
					return
				}
				select {
				case ch <- msg.Payload:
				default:
				}
			}
		}
	}()
	return ch
}
