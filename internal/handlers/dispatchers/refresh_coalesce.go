// Q-REFRESH-COALESCE (0.25.328) — singleflight coalesce on the L1 refresher.
//
// PROBLEM
//
// At 50K scale the informer fan-out + cascade chain re-enqueues identical
// L1 keys (same binding-identity hash + GVR + ns + name + page) faster
// than a single worker can drain them. The workqueue layer dedupes on
// identity (the resolved-key BASE), but processItem then iterates pages
// and calls refreshSingleL1 PER raw key. Two cascades hitting the same
// downstream resource through different parents can race into
// refreshSingleL1 with identical raw keys, paying full decode + resolve
// + per-user refilter cost twice. Combined with the byte-budget evict
// path that just landed in 0.25.319, the duplicate decode pressure is
// a measurable contributor to the structural ~5 GiB heap walk-back the
// architect logged for the 0.25.319 soak.
//
// FIX
//
// Wrap the refreshSingleL1 invocation inside MakeL1Refresher's closure
// in a process-wide singleflight.Group keyed by the raw L1 key. Concurrent
// callers with the same key share ONE refreshSingleL1 execution and ONE
// (ok, cascade) result tuple. Distinct keys still execute independently
// in parallel — bounded only by the existing worker pool.
//
// KEY DESIGN
//
// The coalesce key is the raw L1 key string (k.raw inside the closure):
//
//	<binding-identity-hash>:<gvr>:<ns>:<name>:<page>
//
// This is precisely the L1 cache key. Per the Q-RBACC-DEFECT-1 history,
// userAccessFilter is per-group-set NOT per-user, so the binding-identity
// hash already encodes the cohort fingerprint — two users in the same
// cohort share the hash and therefore the singleflight slot, which is
// the desired coalesce-across-users behaviour. Two binding cohorts that
// happen to point at the same downstream resource keep distinct slots
// (different identity hashes), preserving the per-cohort refresh
// contract.
//
// FOLLOWER COUNTER
//
// `inflight` is a sync.Map<key, *atomic.Int32>. Each caller increments
// the counter on entry; if the post-increment value is >1 the caller is
// a FOLLOWER (the leader is the one whose increment took the counter
// from 0 → 1). Followers bump GlobalMetrics.RefresherInflightCoalesced.
// On exit the counter is decremented and the map entry is removed when
// it reaches 0. The map is purely an instrumentation helper — singleflight
// owns the actual dedup contract; the counter is a side-channel because
// singleflight.Do's `shared` return is true for both leader and followers
// and would over-count by exactly 1 per group.
//
// TRUST BOUNDARY
//
// refreshSingleL1 never serves bytes to an HTTP response — it writes the
// (refilter-ready) wrapper into L1 under a key that already encodes the
// binding identity. Sharing one refresh execution across followers is
// therefore safe by construction: the L1 write is identity-scoped, and
// per-user refilter happens later on the dispatcher's HIT path (which
// has its own singleflight in api.RefilterRESTActionDeduped). Errors
// from the inner call are shared with all followers — same semantic as
// the existing v3 refilter singleflight. NO behaviour change to caller-
// visible API.
package dispatchers

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"golang.org/x/sync/singleflight"
)

// refresherFlight is the package-level singleflight.Group for refreshSingleL1
// dedup. Distinct from the api-package refilterFlight (which dedups the
// dispatcher HIT-with-refilter path) and from l1cache.foregroundFlight
// (which dedups the dispatcher MISS / fresh-resolve path). All three
// groups operate on disjoint slot keys.
var refresherFlight singleflight.Group

// refresherInflight tracks concurrent waiters per coalesce key so the
// follower counter is exact. The map value is a *atomic.Int32 holding
// the current waiter count for the key. A nil map entry means no waiter
// is currently registered; the entry is created on the first caller and
// removed when the last caller decrements to zero.
var refresherInflight sync.Map // string -> *atomic.Int32

// refreshResult bundles the (ok, cascade) tuple refreshSingleL1 returns
// so it can be passed through singleflight.Do's any/error interface.
type refreshResult struct {
	ok      bool
	cascade []string
}

// refreshSingleL1Inner is the indirect call site the wrapper uses. Defaults
// to refreshSingleL1. Tests swap this to a blocking / counting variant to
// observe the singleflight dedup behaviour deterministically (the real
// inner can complete before follower goroutines join the slot, hiding
// the dedup). Production code MUST NOT mutate this var.
var refreshSingleL1Inner = refreshSingleL1

// refreshSingleL1Coalesced wraps refreshSingleL1 with singleflight dedup
// keyed by rawKey. Concurrent callers with identical rawKey share ONE
// inner execution; followers bump GlobalMetrics.RefresherInflightCoalesced.
// An empty rawKey bypasses singleflight (defensive: should not occur in
// production code, ParseResolvedKey is checked upstream).
func refreshSingleL1Coalesced(
	ctx context.Context,
	c cache.Cache,
	user jwtutil.UserInfo,
	ep endpoints.Endpoint,
	accessToken string,
	info cache.ResolvedKeyInfo,
	rawKey string,
	authnNS string,
	snowplowEndpointFn func() (*endpoints.Endpoint, error),
	snowplowK8sClient dynamic.Client,
) (bool, []string) {
	if rawKey == "" {
		return refreshSingleL1Inner(ctx, c, user, ep, accessToken, info, rawKey, authnNS, snowplowEndpointFn, snowplowK8sClient)
	}

	// Register intent; followers bump the counter exactly once.
	cv, _ := refresherInflight.LoadOrStore(rawKey, &atomic.Int32{})
	cnt := cv.(*atomic.Int32)
	if cnt.Add(1) > 1 {
		cache.GlobalMetrics.RefresherInflightCoalesced.Add(1)
	}
	defer func() {
		if cnt.Add(-1) == 0 {
			// Best-effort cleanup; if a new caller already created a
			// fresh entry under the same key the Delete is a harmless
			// no-op against the new pointer because sync.Map.Delete
			// matches by key not by value.
			refresherInflight.Delete(rawKey)
		}
	}()

	v, err, _ := refresherFlight.Do(rawKey, func() (any, error) {
		ok, cascade := refreshSingleL1Inner(ctx, c, user, ep, accessToken, info, rawKey, authnNS, snowplowEndpointFn, snowplowK8sClient)
		return refreshResult{ok: ok, cascade: cascade}, nil
	})
	// refreshSingleL1Inner does NOT return errors via the (bool, []string)
	// signature — failures surface as ok=false. The closure above always
	// returns a nil error, so err here is always nil; the assertion is
	// defensive against a future refactor that adds an error path.
	if err != nil {
		return false, nil
	}
	r, ok := v.(refreshResult)
	if !ok {
		return false, nil
	}
	return r.ok, r.cascade
}
