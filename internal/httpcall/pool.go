// Package httpcall provides a pooled wrapper around plumbing/http/request.Do
// that reuses *http.Client / *http.Transport across calls keyed by endpoint
// identity. This eliminates the persistConn leak (~2 goroutines per call)
// observed with plumbing v0.9.3, which builds a fresh *http.Client and
// *http.Transport on every Do() call.
//
// Reversibility: setting SNOWPLOW_HTTP_POOL=off (case-insensitive) at process
// start falls back to plumbing's per-call client (the pre-fix behavior).
//
// Pool semantics:
//   - Keyed by SHA-256 of endpoint identity (URL, proxy, TLS material, auth
//     credentials hashed). Credentials never appear in the key in plaintext
//     and the key itself is treated as opaque — never logged.
//   - Hard cap of poolHardCap distinct entries. When the cap is hit, new
//     endpoints bypass the pool and fall back to plumbing's per-call client.
//     There is no LRU and no eviction. If you observe pool-bypass in
//     production, that means you have endpoint-explosion in templates and
//     that is a different bug to investigate; the pool is not the place to
//     fix it.
//   - AWS-authenticated endpoints are NEVER pooled (see Do for rationale):
//     plumbing builds awsAuthRoundTripper with the request signature baked
//     in at client-construction time, so a cached client would re-sign every
//     subsequent request with the headers of the first one. AWS goes through
//     plumbing's per-call path. This is the 0.01% path.
package httpcall

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Per-host idle pool. The plumbing default is 100 / 0 (default 2 per
	// host) which leaks across freshly-allocated transports. We cap idle
	// at 32/host with a 90s TTL — enough for keep-alive reuse, bounded
	// for goroutine count.
	poolMaxIdleConns        = 100
	poolMaxIdleConnsPerHost = 32
	poolMaxConnsPerHost     = 0 // 0 = unlimited active; idle is what we cap
	poolIdleConnTimeout     = 90 * time.Second

	// poolHardCap bounds the number of distinct endpoint identities cached.
	// Beyond this, new endpoints transparently fall back to the per-call
	// client. ~50KB per *http.Transport => ~13MB worst case at full cap.
	poolHardCap = 256
)

// Type alias re-exports so callers can replace the plumbing import with this
// package without touching local type references.
//
// (We re-import plumbing types in do.go to keep this file free of
// imports beyond the standard library.)

// disabled reports whether the pool is bypassed. Resolved once at startup;
// changing the env after process start has no effect.
var disabled = strings.EqualFold(getenv("SNOWPLOW_HTTP_POOL"), "off")

// Override hooks for tests. Tests may set these to swap env behavior or to
// force pool-disabled for a single test.
var getenv = osGetenv

// pooledClient wraps a cached *http.Client. The client is goroutine-safe;
// readers of pool.Load may use it concurrently with each other. We never
// mutate the client after store.
type pooledClient struct {
	cli *http.Client
}

var (
	pool     sync.Map     // key string -> *pooledClient
	poolSize atomic.Int64 // number of entries currently in pool
)

// resetForTest fully resets pool state. Test-only.
func resetForTest() {
	pool.Range(func(k, _ any) bool {
		pool.Delete(k)
		return true
	})
	poolSize.Store(0)
}

// poolEntries returns the current entry count. Test-only convenience.
func poolEntries() int64 { return poolSize.Load() }
