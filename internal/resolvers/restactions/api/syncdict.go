package api

import (
	"sync"
)

// syncDict is the V3_SYNCMAP variant's replacement for the
// dictMu-guarded `map[string]any` at the api_l1 and informer_direct
// merge sites. Per architect's design v2.1 § V3 (file
// /tmp/snowplow-runs/dictmu-microbench-design-2026-05-03.md) and the
// S-50 single-pod bench finding (V0_BASE_HOIST + V3_SYNCMAP joint
// winners), V3 uses a sync.Map keyed by api-id with each value held in
// a heap-allocated *holder so sync.Map.CompareAndSwap can compare
// pointer identities (sync.Map.CAS uses == on `any` and slices are
// not comparable, so a naked []any value would panic at runtime).
//
// The CAS-loop pattern matches the bench's V3_SYNCMAP implementation
// (see q-con-1-bench fa74572: dictmu_bench_test.go syncMapDict). Each
// merge produces a fresh holder via copy-on-append per design § 9.3
// (avoids torn-slice reads against concurrent gojq snapshots).
//
// gojq-purity-required: the syncDict stores the post-Compute values
// only — every value passed to Merge has already been through
// jsonHandler*Compute (which itself runs on safeCopyJSON-wrapped
// inputs at the informer_direct site). The discipline is preserved at
// the call site, not here.

// holder wraps a value in a heap-allocated pointer so sync.Map
// CompareAndSwap can compare pointers (slices are not == comparable).
// One alloc per RMW; acceptable for V3.
type holder struct{ v any }

// syncDict maintains both:
//   - a sync.Map for the high-contention merge sites (api_l1,
//     informer_direct), accessed via Merge.
//   - a plain map[string]any for legacy-shape sites (snapshot read,
//     err_write, slice/extras), accessed via the embedded sync.Mutex
//     in the parent resolveAPI scope. Those sites still go through
//     instrumentedDictWrite as before — the V3 change is scoped to
//     Merge only.
//
// At the end of Resolve, Drain copies the sync.Map contents into the
// returned map (this is what the caller — restactions.Resolve —
// consumes), preserving the existing return contract of Resolve.
type syncDict struct {
	hot   sync.Map     // *holder values, keyed by api-id
	cold  map[string]any // legacy slot for slice/extras/err keys
	coldMu sync.Mutex
}

func newSyncDict(initial map[string]any) *syncDict {
	d := &syncDict{cold: make(map[string]any, len(initial))}
	for k, v := range initial {
		d.cold[k] = v
	}
	return d
}

// Merge implements the api_l1 / informer_direct write semantics:
//   - if key absent: store val.
//   - if key holds []any: append wrapAsSlice(val).
//   - otherwise: produce []any{prev, val} or expanded if val is []any.
//
// Uses a CAS loop on sync.Map for the per-key read-modify-write.
// copy-on-append on the slice path eliminates the torn-slice race that
// design § 9.3 flags.
func (d *syncDict) Merge(key string, val any) {
	first := &holder{v: val}
	got, loaded := d.hot.LoadOrStore(key, first)
	if !loaded {
		return
	}
	for {
		prev := got.(*holder)
		next := mergeValues(prev.v, val)
		nh := &holder{v: next}
		if d.hot.CompareAndSwap(key, prev, nh) {
			return
		}
		// CAS lost the race; reload and retry.
		got, _ = d.hot.Load(key)
		if got == nil {
			// Extremely unlikely (Delete during Merge); reinsert.
			if _, ok := d.hot.LoadOrStore(key, &holder{v: val}); !ok {
				return
			}
			got, _ = d.hot.Load(key)
		}
	}
}

// mergeValues mirrors mergeIntoDict semantics from handler.go but is
// pure (no map mutation) and produces a fresh slice via copy-on-append.
func mergeValues(prev, val any) any {
	switch existing := prev.(type) {
	case []any:
		v := wrapAsSlice(val)
		if len(v) == 0 {
			return existing
		}
		// copy-on-append — fresh backing array.
		out := make([]any, len(existing), len(existing)+len(v))
		copy(out, existing)
		return append(out, v...)
	default:
		switch v := val.(type) {
		case []any:
			out := make([]any, 0, 1+len(v))
			out = append(out, existing)
			return append(out, v...)
		default:
			return []any{existing, v}
		}
	}
}

// SetCold stores into the cold slot under the cold mutex. Used for
// dict["slice"], extras, and err writes — sites that retain their
// legacy mutex shape per design § 6 (V3 split: hot path on sync.Map,
// cold path on mutex).
func (d *syncDict) SetCold(key string, val any) {
	d.coldMu.Lock()
	d.cold[key] = val
	d.coldMu.Unlock()
}

// GetCold returns the cold value (or nil, false) under the cold mutex.
func (d *syncDict) GetCold(key string) (any, bool) {
	d.coldMu.Lock()
	v, ok := d.cold[key]
	d.coldMu.Unlock()
	return v, ok
}

// Drain produces the final map[string]any consumed by the caller.
// Hot keys are coalesced via sync.Map.Range; cold keys override hot
// keys when both exist (cold is the legacy slot for non-merge writes
// like err_write — should never collide with hot keys in practice but
// the override is the safer rule).
func (d *syncDict) Drain() map[string]any {
	out := make(map[string]any)
	d.hot.Range(func(k, v any) bool {
		if h, ok := v.(*holder); ok {
			out[k.(string)] = h.v
		}
		return true
	})
	d.coldMu.Lock()
	for k, v := range d.cold {
		out[k] = v
	}
	d.coldMu.Unlock()
	return out
}

// SnapshotForJQ returns a copy of the dict suitable for the snapshot
// read site (createRequestOptions reads the dict and runs jqutil.ForEach).
// Equivalent to Drain — same semantic — but documents the read-only
// usage at the snapshot site.
func (d *syncDict) SnapshotForJQ() map[string]any {
	return d.Drain()
}

// HotLen returns the number of hot keys; used by the OTel span
// `dict.size_before` attribute at the merge sites.
func (d *syncDict) HotLen() int {
	n := 0
	d.hot.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}
