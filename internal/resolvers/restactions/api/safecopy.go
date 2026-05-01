package api

import (
	"encoding/json"
)

// safeCopyJSON returns a deep-copied JSON-shaped tree with all numeric
// leaves coerced to float64.
//
// Why this exists
//
// The informer-hit fast path in Resolve passes a *map[string]any tree* to
// gojq for filter evaluation. gojq's normalizeNumbers/deleteEmpty mutate
// input maps in-place, so we cannot share the informer's read-only tree
// across request goroutines — each call needs its own mutable copy.
//
// The previous implementation used json.Marshal followed by json.Unmarshal
// to obtain the safe copy. That round-trip was identified by 50K-scale
// pprof as the dominant allocator (≈25% of total alloc, ≈21 GB/s sustained
// allocation rate, ≈32% CPU on gcAssistAlloc). v0.25.283 attempted to
// replace it with k8s.io DeepCopyJSON; that variant panicked in production
// with `invalid type: int64 (1)` because gojq's normalizeNumbers does not
// accept int64 — only float64 and json.Number. The marshal+unmarshal had
// silently coerced every int64 to float64 because JSON has only one
// number type.
//
// safeCopyJSON walks the tree directly (avoiding the json encoder/decoder
// allocation cost) and explicitly coerces every numeric leaf to float64,
// preserving the gojq compatibility that the round-trip provided. The
// resulting tree is independent of the input — gojq is free to mutate it.
//
// Accepted leaf types: string, bool, nil, float64, float32, json.Number,
// int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64.
// All numeric types are normalized to float64 (json.Number via Float64()).
// Unknown types are returned unchanged. This matches the relaxed semantics
// of the old marshal+unmarshal: anything json.Marshal would have rendered
// as a JSON number ends up as a float64; anything else passes through.
func safeCopyJSON(v any) any {
	switch val := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(val))
		for k, child := range val {
			m[k] = safeCopyJSON(child)
		}
		return m
	case []any:
		s := make([]any, len(val))
		for i, child := range val {
			s[i] = safeCopyJSON(child)
		}
		return s
	case nil, string, bool, float64:
		return val
	case float32:
		return float64(val)
	case json.Number:
		f, err := val.Float64()
		if err != nil {
			// Number too large to fit float64 — fall back to the string
			// form. gojq accepts json.Number, so this preserves precision
			// for the rare case at the cost of one extra type check at
			// the leaf.
			return val
		}
		return f
	case int:
		return float64(val)
	case int8:
		return float64(val)
	case int16:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case uint:
		return float64(val)
	case uint8:
		return float64(val)
	case uint16:
		return float64(val)
	case uint32:
		return float64(val)
	case uint64:
		return float64(val)
	default:
		return val
	}
}
