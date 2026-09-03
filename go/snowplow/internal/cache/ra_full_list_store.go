// ra_full_list_store.go — Ship 4a (0.30.198): the RAFullList cell Put +
// cost-based pin predicate + serve-outcome metrics. Kept in its own file so
// the 4a layer is wholesale-deletable (project_caching_is_provisional).

package cache

import (
	"encoding/json"
	"os"
	"strconv"
	"sync/atomic"
)

const (
	// envRAFullListPinBytesThreshold — Ship 4a (0.30.198). A RAFullList cell
	// whose encoded envelope is at least this many bytes is "EXPENSIVE" and
	// is PINNED (resident, eviction-protected) so an expensive cohort's
	// prewarmed full-list survives LRU thrash until its first visit
	// (feedback_zero_cold_navigations_hard_requirement). Cheap cells (small
	// envelopes) stay TRANSIENT — fast-cold from the warm substrate is fine.
	//
	// This is a MEASURED-COST predicate (envelope bytes), NOT an identity
	// literal (feedback_no_special_cases): every RA of every GVR is judged
	// by the same byte threshold. The default 1 MiB matches the design's
	// "envelope bytes > ~1MB" expensive-cell bar; admin's ~18 MiB
	// compositions-panels full list is far over it and pins, while a narrow
	// cohort's few-KB list stays transient.
	envRAFullListPinBytesThreshold     = "RESOLVED_CACHE_RAFULLLIST_PIN_BYTES"
	defaultRAFullListPinBytesThreshold = int64(1) * 1024 * 1024 // 1 MiB
)

// raFullListPinBytesThreshold returns the expensive-cell byte threshold.
func raFullListPinBytesThreshold() int64 {
	if v := os.Getenv(envRAFullListPinBytesThreshold); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			return n
		}
	}
	return defaultRAFullListPinBytesThreshold
}

// PutRAFullList stores the RA full-result map under key as a RAFullList cell,
// applying the cost-based pin predicate: a cell whose encoded envelope is
// >= raFullListPinBytesThreshold() is marked Pinned (resident region,
// eviction-protected); a cheaper cell is stored transient. The pin is
// HONOURED by Put only when the resident budget permits (else demoted — see
// Put). Returns the encoded bytes (for the caller's serve, avoiding a second
// marshal) and whether the cell was pinned.
//
// Inputs is stored so the refresher re-resolves the cell on a dirty-mark
// (and re-pins — see PutRAFullListPinned). A nil/marshal-fail full is a
// no-op returning (nil,false).
func (c *ResolvedCacheStore) PutRAFullList(key string, inputs ResolvedKeyInputs, full map[string]any) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	encoded, err := json.Marshal(full)
	if err != nil {
		return nil, false
	}
	pin := int64(len(encoded)) >= raFullListPinBytesThreshold()
	in := inputs // copy onto the heap for the entry
	// uaf-scope-waiver: UNREACHABLE for a refilter-narrowed body. The only caller is apiref.raFullListServe, which bypasses this whole layer (returns served=false) for a RESTAction declaring a userAccessFilter BEFORE it derives the key — so no refilter output can arrive here. Falsified by TestA1_RAFullList_UAFBypassesCell_ControlServes (no cell under the derived raKey) and the warm-cell ordering arm.
	// scope-waiver:TTLOverride: raFullList-class cell — 1.12.3 A-1 CORRECTED WAIVER. The pre-1.12.3 text claimed UAF refilter output "lands in the per-page restactions cell, never here"; that was WRONG (this cell holds the apiRef'd RA's own full resolve output, UAF stage included, keyed on BindingUID ALONE — no RBACSubGen). A UAF-bearing RA can no longer REACH this Put: raFullListServe bypasses the whole layer for it (ra_full_list.go), so every cell written here is non-UAF and needs no UAF cap. Pinned/TTL governed by the 4a resident-budget path. Revisit when 1.13.0 folds the UAF scope into the key (uaf_shortttl.go R-d-4 SITE MAP, "THE FOURTH SITE").
	c.Put(key, &ResolvedEntry{
		RawJSON: encoded,
		Inputs:  &in,
		Pinned:  pin,
	})
	return encoded, pin
}

// PutRAFullListPinned stores a RAFullList cell with an EXPLICIT pin decision
// (the PIP prewarm + refresher path, which has measured the resolve
// wall-clock and decided expensiveness from the PIPStageTiming sink rather
// than re-deriving from envelope bytes). pin=true requests the resident
// region (honoured by Put subject to the resident budget). Used so a
// prewarmed expensive cell is pinned even if its envelope happens to be
// under the byte threshold, and so the refresher RE-pins on every
// re-resolve (never demoting a pinned cell on a dirty-mark).
func (c *ResolvedCacheStore) PutRAFullListPinned(key string, inputs ResolvedKeyInputs, encoded []byte, pin bool) {
	if c == nil || encoded == nil {
		return
	}
	in := inputs
	// uaf-scope-waiver: UNREACHABLE for a refilter-narrowed body, same as PutRAFullList above. This explicit-pin path only ever re-Puts a cell that raFullListServe (gated) or the refresher (gated) already produced; a UAF RA never produces one.
	// scope-waiver:TTLOverride: raFullList-class cell (explicit-pin path) — same class as PutRAFullList above, same 1.12.3 A-1 CORRECTED reasoning: NOT "UAF output never lands here" (it did), but "a UAF-bearing RA can no longer reach this Put" — raFullListServe bypasses the layer for it, and this explicit-pin path re-Puts only cells that path (or the refresher, over its stored Inputs) already produced (uaf_shortttl.go R-d-4 SITE MAP, "THE FOURTH SITE").
	c.Put(key, &ResolvedEntry{
		RawJSON: encoded,
		Inputs:  &in,
		Pinned:  pin,
	})
}

// --- Serve-outcome metrics ---------------------------------------------
//
// Per feedback_measurement_use_expvar_not_log_tails: surface the 4a serve
// path's outcome distribution as atomic counters (also folded into the
// resolved_cache.summary line / Stats). The ratio of hit:repopulate:
// verified-slice:fallback is the 4a effectiveness signal.

type RAFullListServeOutcome int

const (
	// RAFullListServeHit — served a Go-slice over a CACHED full cell under a
	// known-sliceable verdict (the steady-state cheap path — no resolve).
	RAFullListServeHit RAFullListServeOutcome = iota
	// RAFullListServeRepopulateSlice — known-sliceable verdict but the cell
	// had been evicted; resolved unpaginated ONCE, re-Put, then Go-sliced.
	RAFullListServeRepopulateSlice
	// RAFullListServeVerifiedSlice — first sight of (RA × shape): byte-verify
	// PASSED; Put the cell + served the Go-slice.
	RAFullListServeVerifiedSlice
	// RAFullListServeFallback — byte-verify FAILED (not cleanly sliceable for
	// this shape); served the page-keyed resolve (correct, slower).
	RAFullListServeFallback
)

var (
	raFullListServeHitTotal           atomic.Uint64
	raFullListServeRepopulateTotal    atomic.Uint64
	raFullListServeVerifiedSliceTotal atomic.Uint64
	raFullListServeFallbackTotal      atomic.Uint64
)

// RecordRAFullListServe bumps the per-outcome counter.
func RecordRAFullListServe(o RAFullListServeOutcome) {
	switch o {
	case RAFullListServeHit:
		raFullListServeHitTotal.Add(1)
	case RAFullListServeRepopulateSlice:
		raFullListServeRepopulateTotal.Add(1)
	case RAFullListServeVerifiedSlice:
		raFullListServeVerifiedSliceTotal.Add(1)
	case RAFullListServeFallback:
		raFullListServeFallbackTotal.Add(1)
	}
}

// RAFullListServeStats is the read-only snapshot of the serve-outcome
// counters.
type RAFullListServeStats struct {
	Hit           uint64
	Repopulate    uint64
	VerifiedSlice uint64
	Fallback      uint64
}

// RAFullListServeSnapshot returns the current serve-outcome counters.
func RAFullListServeSnapshot() RAFullListServeStats {
	return RAFullListServeStats{
		Hit:           raFullListServeHitTotal.Load(),
		Repopulate:    raFullListServeRepopulateTotal.Load(),
		VerifiedSlice: raFullListServeVerifiedSliceTotal.Load(),
		Fallback:      raFullListServeFallbackTotal.Load(),
	}
}
