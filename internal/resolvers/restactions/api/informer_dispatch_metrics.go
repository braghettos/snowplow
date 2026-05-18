// informer_dispatch_metrics.go — Tag 0.30.96: stable serve-rate counters
// for the 0.30.95 resolver pivot (`dispatchViaInformer`).
//
// 0.30.95 shipped the pivot with per-call `log.Debug` lines only
// (`informer_dispatch.list_served`, `.get_served`, `.fallthrough.*`).
// The Phase 6 bench proved those are unreadable at 50K request volume —
// the serve rate could not be measured. 0.30.96 adds package-level
// atomic counters + a STABLE single-line `informer_dispatch.summary` so
// the WHOLE pivot (resolver path here + the `objects.Get` path) is
// observable on the re-bench.
//
// Counters here are incremented inside `dispatchViaInformer`. The summary
// goroutine is lazy-started on first dispatch (sync.Once-bounded — one
// goroutine for the process lifetime, never started when
// RESOLVER_USE_INFORMER stays off). This mirrors `objects` package's
// `startObjectsGetSummary` and `cache`'s `startResolvedCacheSummary`.

package api

import (
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Pivot serve-rate counters. Package-level atomics — safe to Add/Load
// without external locking.
var (
	// dispatchInformerListServed counts LIST calls answered from the
	// informer indexer (marshalled into the apiserver LIST envelope).
	dispatchInformerListServed atomic.Uint64
	// dispatchInformerGetServed counts GET-by-name calls answered from
	// the informer indexer.
	dispatchInformerGetServed atomic.Uint64
	// dispatchInformerFallthrough counts calls that took the apiserver
	// branch under the active pivot — write verb, subresource, non-
	// apiserver path, passthrough / cache-off, metadata-only GVR,
	// not-synced informer, GET-miss, or marshal failure.
	dispatchInformerFallthrough atomic.Uint64
	// dispatchInformerRBACDropped counts INDIVIDUAL list items dropped
	// by the Tag-0.30.100 post-LIST per-item RBAC filter — the user
	// lacked a `list` grant for the item's namespace. A non-zero value
	// is the expected steady state for narrow-RBAC users; it is the
	// falsifier signal that the pivot is no longer over-exposing.
	dispatchInformerRBACDropped atomic.Uint64
	// dispatchInformerSyncWaitServed counts Gate-6 dispatches that were
	// served from cache AFTER a Ship 0.30.121 R2-b bounded sync-wait — a
	// GVR that was unsynced on entry became servable within
	// RESOLVER_SYNC_WAIT_MS. A non-zero value means the R2-b knob is
	// converting would-be apiserver LISTs into cache serves; it stays 0
	// when the knob is at its default (0 = disabled).
	dispatchInformerSyncWaitServed atomic.Uint64
)

// DispatchInformerStats is an atomic snapshot of the pivot serve-rate
// counters. Fields may drift by a single in-flight call — fine for log
// aggregation. Exported so tests can assert increments deterministically.
type DispatchInformerStats struct {
	ListServed     uint64
	GetServed      uint64
	Fallthrough    uint64
	RBACDropped    uint64
	SyncWaitServed uint64
}

// DispatchInformerStatsSnapshot returns the current counter values.
func DispatchInformerStatsSnapshot() DispatchInformerStats {
	return DispatchInformerStats{
		ListServed:     dispatchInformerListServed.Load(),
		GetServed:      dispatchInformerGetServed.Load(),
		Fallthrough:    dispatchInformerFallthrough.Load(),
		RBACDropped:    dispatchInformerRBACDropped.Load(),
		SyncWaitServed: dispatchInformerSyncWaitServed.Load(),
	}
}

// Summary goroutine knobs. Mirrors resolved.go's
// `RESOLVED_CACHE_SUMMARY_EVERY_SECONDS` pattern.
const (
	envDispatchSummaryEvery       = "RESOLVER_DISPATCH_SUMMARY_EVERY_SECONDS"
	defaultDispatchSummarySeconds = 60
)

var dispatchSummaryOnce sync.Once

// startDispatchSummary launches a single bounded goroutine that emits an
// `informer_dispatch.summary` INFO line every N seconds. Lifecycle bound:
// a sync.Once guarantees exactly one goroutine for the process lifetime;
// it does constant work per tick and is never stopped (process-scoped,
// same contract as `startResolvedCacheSummary`).
//
// Started lazily on the first `dispatchViaInformer` call so the goroutine
// never exists when RESOLVER_USE_INFORMER stays off.
func startDispatchSummary() {
	dispatchSummaryOnce.Do(func() {
		every := time.Duration(dispatchSummaryEverySeconds()) * time.Second
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for range t.C {
				s := DispatchInformerStatsSnapshot()
				// STABLE single-line falsifier shape (greppable):
				//   informer_dispatch.summary list_served=N get_served=M
				//   apiserver_fallthrough=K
				slog.Info("informer_dispatch.summary",
					slog.String("subsystem", "cache"),
					slog.Uint64("list_served", s.ListServed),
					slog.Uint64("get_served", s.GetServed),
					slog.Uint64("apiserver_fallthrough", s.Fallthrough),
					slog.Uint64("rbac_dropped", s.RBACDropped),
					slog.Uint64("sync_wait_served", s.SyncWaitServed),
				)
			}
		}()
	})
}

// dispatchSummaryEverySeconds resolves the summary interval from the env
// knob, falling back to the default on unset / non-int / non-positive.
func dispatchSummaryEverySeconds() int {
	v := os.Getenv(envDispatchSummaryEvery)
	if v == "" {
		return defaultDispatchSummarySeconds
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultDispatchSummarySeconds
	}
	return n
}
