// Q-RBAC-DECOUPLE C(d) v3 — singleflight refilter dedup (spec §2.6).
//
// PROBLEM
//
// When 80 burst requests hit a cold-start binding-identity, all 80 race the
// same RefilterRESTAction(compositions-list, cyberjoker). Without dedup each
// goroutine unmarshals the (potentially 12 MB) cached wrapper, walks the
// per-api dict, evaluates RBAC for every NS, and re-runs the outer JQ
// filter. At 80× concurrency that is ~2.4 GB transient allocation and ~4 s
// of CPU peak — the thundering-herd shape that triggers on the customer's
// documented cluster-restart pattern.
//
// FIX
//
// Wrap RefilterRESTAction in a per-(L1-key, requesting-user, groups-hash)
// singleflight.Group. Concurrent callers with identical key share ONE
// execution and ONE result. The existing l1cache.foregroundFlight only
// dedups the MISS path (fresh resolve under singleflight); v3's HIT-with-
// refilter path needs its own group because it runs on the dispatcher side
// AFTER the L1 lookup, downstream of any miss-path dedup.
//
// KEY DESIGN  (Q-RBACC-V3-IMPL-3)
//
//	<L1-key>|<user.Username>|<groups-hash>
//
// • L1-key isolates per-RESTAction-resource caches so two different
//   compositions-list calls don't collide on the singleflight slot.
// • Username segregates per-user views so two users sharing a binding
//   identity (the very scenario that produced Q-RBACC-DEFECT-1) NEVER share
//   a refilter result.
// • groups-hash (sha256 of sorted groups, hex) prevents stale dedup if a
//   user's groups change mid-session via OAuth refresh — without it, an old
//   in-flight refilter run with the previous group set could be served to
//   a request that just got a refreshed (different) group set.
//
// The singleflight.Group is process-wide (package-level var). Once the
// underlying call returns, the group entry is released automatically by
// singleflight semantics — no manual cleanup needed.
//
// TRUST BOUNDARY
//
// The wrapper is byte-for-byte transparent to RefilterRESTAction's
// trust-boundary contract: any error from the inner call propagates to ALL
// dedup'd callers, each of which falls through to the miss path. Because
// only ONE goroutine executes the inner function, only one audit log line
// fires per (key, user, groups-hash) burst — that is intentional and
// correct: the burst represents a single RBAC decision evaluated once.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"golang.org/x/sync/singleflight"
)

// refilterFlight is the package-level singleflight.Group for the
// HIT-with-refilter path. Distinct from l1cache.foregroundFlight (which
// dedups MISSes / fresh resolves).
var refilterFlight singleflight.Group

// refilterFlightInner is the indirect call site the wrapper uses. Defaults
// to RefilterRESTAction. Tests swap this to a blocking / counting variant
// to observe the singleflight dedup behaviour deterministically (the real
// inner is microseconds-fast for small dicts and would frequently complete
// before follower goroutines join the singleflight slot, hiding the
// dedup). Production code MUST NOT mutate this var.
var refilterFlightInner = RefilterRESTAction

// hashGroups returns a stable hex-encoded sha256 of the sorted, joined
// group list. Sorting makes the hash invariant under group-order changes
// in OAuth claims (the same groups in any order produce the same hash).
//
// Why sha256: collision-resistant for practical use (no need for any
// cryptographic property — just a stable fingerprint). Hex-encoded so the
// hash is safe to embed inside a singleflight key (no '|' delimiter
// collision).
func hashGroups(groups []string) string {
	if len(groups) == 0 {
		return "no_groups"
	}
	cp := make([]string, len(groups))
	copy(cp, groups)
	sort.Strings(cp)
	h := sha256.Sum256([]byte(strings.Join(cp, "\x00")))
	return hex.EncodeToString(h[:])
}

// refilterFlightKey assembles the singleflight key from the L1 key, the
// requesting user's username, and the groups-hash. Returns "" if the
// context is missing UserInfo — callers MUST treat an empty key as a
// signal to bypass the singleflight (the inner call will fail fast on the
// missing UserInfo anyway, but we don't want to dedup distinct "no-user"
// requests against each other).
func refilterFlightKey(ctx context.Context, l1Key string) string {
	user, err := xcontext.UserInfo(ctx)
	if err != nil {
		return ""
	}
	return l1Key + "|" + user.Username + "|" + hashGroups(user.Groups)
}

// RefilterRESTActionDeduped is the singleflight-wrapped variant of
// RefilterRESTAction. Concurrent callers with the same (l1Key, username,
// groups-hash) execute the inner function exactly ONCE and share the
// result; distinct keys execute independently in parallel.
//
// Callers MUST pass the L1 cache key (the same string used to GetRaw the
// `raw` bytes). An empty l1Key disables singleflight and falls through to
// a direct RefilterRESTAction call — used by tests and the no-cache code
// path. Missing UserInfo in ctx ALSO disables singleflight (the inner
// call will return an error anyway).
//
// SECURITY: this wrapper does NOT short-circuit on cached errors. Each
// burst still gets the same (err, nil) tuple from the inner call, and
// each goroutine independently falls through to the miss path. The
// shared-error semantics are an explicit design choice — the alternative
// (re-running on every error) would defeat the dedup under transient
// failure storms.
func RefilterRESTActionDeduped(ctx context.Context, c cache.Cache, raw []byte, l1Key string) ([]byte, error) {
	key := refilterFlightKey(ctx, l1Key)
	if key == "" {
		return refilterFlightInner(ctx, c, raw)
	}

	// singleflight.Do blocks until the inner func returns; concurrent
	// callers with the same key share the result without re-running. The
	// shared bool (third return) is intentionally unused here — callers
	// don't need to know whether they were the leader or a follower.
	v, err, _ := refilterFlight.Do(key, func() (any, error) {
		return refilterFlightInner(ctx, c, raw)
	})
	if err != nil {
		return nil, err
	}
	out, ok := v.([]byte)
	if !ok {
		// Should be impossible: inner returns ([]byte, error). Defensive
		// nil return with a typed error so the dispatcher falls through
		// to the miss path instead of panicking on a bad assertion.
		return nil, errRefilterFlightTypeAssert{}
	}
	return out, nil
}

type errRefilterFlightTypeAssert struct{}

func (errRefilterFlightTypeAssert) Error() string {
	return "v3 refilter singleflight: inner returned non-[]byte value (programming error)"
}
