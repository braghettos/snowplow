// Q-PREWARM-R2 — informer-ready signal
//
// Decouples the kubernetes /ready probe from L1 prewarm completion. Pod
// becomes Ready as soon as informers complete their initial LIST/WATCH
// sync (Phase 3 in startBackgroundServices). Prewarm continues to fill
// L1 in the background; cache misses fall through to the foreground
// singleflight resolve path, so the pod can serve cold-L1 requests
// without blocking on the multi-minute prewarm loop.
//
// See /tmp/snowplow-runs/q-cold-1-cd-v6-phase6-50k-postPR2PR3-2026-05-05/
// Q-PREWARM-R2R5-SPEC.md §1 (R2.1).
package cache

import "sync/atomic"

// informersReady flips to true after ResourceWatcher.WaitForSync succeeds
// in main.go's startBackgroundServices. Read by the /ready handler via
// IsInformerReady. Process-global because there is exactly one
// ResourceWatcher per snowplow process.
var informersReady atomic.Bool

// MarkInformersReady is called once by main after WaitForSync succeeds.
// Safe to call multiple times; only the first call is observable.
func MarkInformersReady() {
	informersReady.Store(true)
}

// IsInformerReady returns true once the resource informers have completed
// their initial sync. Used by the /ready probe handler — pod becomes
// Ready as soon as informers are populated, regardless of L1 prewarm
// state.
func IsInformerReady() bool {
	return informersReady.Load()
}

// ResetInformersReadyForTest restores the informer-ready flag to its
// initial false state. Test-only — production code MUST NOT call this.
// Public so tests in sibling packages (e.g. internal/handlers) can
// reset state between subtests; the alternative is per-package
// shadow flags that drift from the production singleton.
func ResetInformersReadyForTest() {
	informersReady.Store(false)
}
