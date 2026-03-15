package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	DefaultResourceTTL = time.Hour
	ResolvedCacheTTL   = time.Hour
	HTTPCacheTTL       = time.Hour
	notFoundTTL        = 30 * time.Second
)

// RedisCache is a Redis-backed cache. All methods are safe on a nil receiver.
type RedisCache struct {
	client      *redis.Client
	ResourceTTL time.Duration
	gvrTTLs     sync.Map
	onNewGVR    func(context.Context, schema.GroupVersionResource)
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
			PoolSize:     20,
			MinIdleConns: 2,
			MaxRetries:   2,
		}),
		ResourceTTL: resourceTTL,
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

func (c *RedisCache) SetRawForGVR(ctx context.Context, gvr schema.GroupVersionResource, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, val, c.TTLForGVR(gvr)).Err()
}

// ── GVR notifier ──────────────────────────────────────────────────────────────

func (c *RedisCache) SetGVRNotifier(fn func(context.Context, schema.GroupVersionResource)) {
	if c != nil {
		c.onNewGVR = fn
	}
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
	if bytes.Equal(val, []byte(notFoundSentinel)) {
		return false, nil
	}
	return true, json.Unmarshal(val, dest)
}

func (c *RedisCache) GetRaw(ctx context.Context, key string) ([]byte, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	val, err := c.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if bytes.Equal(val, []byte(notFoundSentinel)) {
		return nil, false, nil
	}
	return val, true, nil
}

// Exists returns true when the key is present in Redis (regardless of value).
func (c *RedisCache) Exists(ctx context.Context, key string) bool {
	if c == nil {
		return false
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
	return err == nil && bytes.Equal(val, []byte(notFoundSentinel))
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

func (c *RedisCache) SetPersist(ctx context.Context, key string, val any) error {
	return c.setWithTTL(ctx, key, val, 0)
}

func (c *RedisCache) SetRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, val, c.ResourceTTL).Err()
}

// SetResolvedRaw stores a fully-resolved widget/restaction output with
// ResolvedCacheTTL. Freshness is guaranteed by the GVR reverse index
// (targeted invalidation from informer events); TTL is only for memory
// management.
func (c *RedisCache) SetResolvedRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, val, ResolvedCacheTTL).Err()
}

// SetHTTPRaw stores a raw HTTP response. Freshness is guaranteed by the GVR
// reverse index (targeted invalidation from informer events); TTL is only for
// memory management.
func (c *RedisCache) SetHTTPRaw(ctx context.Context, key string, val []byte) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, val, HTTPCacheTTL).Err()
}

func (c *RedisCache) setWithTTL(ctx context.Context, key string, val any, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	data, err := json.Marshal(val)
	if err != nil {
		return err
	}
	return c.client.Set(ctx, key, data, ttl).Err()
}

func (c *RedisCache) Delete(ctx context.Context, keys ...string) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	return c.client.Del(ctx, keys...).Err()
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
	const maxRetries = 3
	for i := 0; i < maxRetries; i++ {
		err := c.client.Watch(ctx, func(tx *redis.Tx) error {
			val, err := tx.Get(ctx, key).Bytes()
			if errors.Is(err, redis.Nil) {
				val = nil
			} else if err != nil {
				return err
			} else if bytes.Equal(val, []byte(notFoundSentinel)) {
				val = nil
			}
			newVal, ferr := fn(val)
			if ferr != nil || newVal == nil {
				return ferr
			}
			_, err = tx.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				return pipe.Set(ctx, key, newVal, ttl).Err()
			})
			return err
		}, key)
		if err == nil {
			return nil
		}
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return err
	}
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

// DeletePattern deletes all keys matching the given glob pattern using SCAN+DEL.
// Safe on a nil receiver (no-op).
func (c *RedisCache) DeletePattern(ctx context.Context, pattern string) error {
	keys, err := c.ScanKeys(ctx, pattern)
	if err != nil || len(keys) == 0 {
		return err
	}
	return c.Delete(ctx, keys...)
}

// SAddWithTTL adds member to a Redis set and refreshes the set's TTL.
// Used for reverse-index sets (l1gvr, l2gvr) that should expire when the
// associated cache entries expire.
func (c *RedisCache) SAddWithTTL(ctx context.Context, key, member string, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	pipe := c.client.Pipeline()
	pipe.SAdd(ctx, key, member)
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *RedisCache) SAddGVR(ctx context.Context, gvr schema.GroupVersionResource) error {
	if c == nil {
		return nil
	}
	added, err := c.client.SAdd(ctx, WatchedGVRsKey, GVRToKey(gvr)).Result()
	if err != nil {
		return err
	}
	if added > 0 && c.onNewGVR != nil {
		c.onNewGVR(ctx, gvr)
	}
	return nil
}

func (c *RedisCache) SMembers(ctx context.Context, key string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	return c.client.SMembers(ctx, key).Result()
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

func (c *RedisCache) SetString(ctx context.Context, key, value string) error {
	if c == nil {
		return nil
	}
	return c.client.Set(ctx, key, value, 0).Err()
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
