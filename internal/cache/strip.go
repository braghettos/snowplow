package cache

import "encoding/json"

// StripBulkyAnnotations removes kubectl.kubernetes.io/last-applied-configuration
// from every metadata.annotations map found recursively in the JSON payload.
// This annotation is a full JSON-stringified copy of each object's spec and can
// account for >50% of the response size for list-type RESTActions.
// On any error the original bytes are returned unchanged.
func StripBulkyAnnotations(raw []byte) []byte {
	var obj any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}
	if !stripAnnotationRecursive(obj) {
		return raw
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return out
}

const lastAppliedKey = "kubectl.kubernetes.io/last-applied-configuration"

// stripAnnotationRecursive walks maps and slices, deleting the
// last-applied-configuration annotation from any metadata.annotations map.
// Returns true if any deletion was made.
func stripAnnotationRecursive(v any) bool {
	changed := false
	switch val := v.(type) {
	case map[string]any:
		if metadata, ok := val["metadata"].(map[string]any); ok {
			if annotations, ok := metadata["annotations"].(map[string]any); ok {
				if _, exists := annotations[lastAppliedKey]; exists {
					delete(annotations, lastAppliedKey)
					changed = true
				}
			}
		}
		for _, child := range val {
			if stripAnnotationRecursive(child) {
				changed = true
			}
		}
	case []any:
		for _, item := range val {
			if stripAnnotationRecursive(item) {
				changed = true
			}
		}
	}
	return changed
}
