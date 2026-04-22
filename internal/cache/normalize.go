package cache

// NormalizeInPlace recursively converts int64/int32/int → float64 in-place.
// Safe only when the caller owns the data (e.g., informer SetTransform).
// Zero allocation for maps/slices — only replaces integer values.
func NormalizeInPlace(v any) {
	switch val := v.(type) {
	case map[string]interface{}:
		for k, vv := range val {
			switch n := vv.(type) {
			case int64:
				val[k] = float64(n)
			case int32:
				val[k] = float64(n)
			case int:
				val[k] = float64(n)
			case map[string]interface{}:
				NormalizeInPlace(n)
			case []interface{}:
				normalizeSliceInPlace(n)
			}
		}
	case []interface{}:
		normalizeSliceInPlace(val)
	}
}

func normalizeSliceInPlace(s []interface{}) {
	for i, vv := range s {
		switch n := vv.(type) {
		case int64:
			s[i] = float64(n)
		case int32:
			s[i] = float64(n)
		case int:
			s[i] = float64(n)
		case map[string]interface{}:
			NormalizeInPlace(n)
		case []interface{}:
			normalizeSliceInPlace(n)
		}
	}
}
