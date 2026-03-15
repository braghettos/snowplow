package cache

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	WatchedGVRsKey   = "snowplow:watched-gvrs"
	ActiveUsersKey   = "snowplow:active-users"
	notFoundSentinel = `{"__snowplow_not_found__":true}`
)

func GVRToKey(gvr schema.GroupVersionResource) string {
	g := gvr.Group
	if g == "" {
		g = "core"
	}
	return g + "/" + gvr.Version + "/" + gvr.Resource
}

func ParseGVRKey(s string) schema.GroupVersionResource {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 3 {
		return schema.GroupVersionResource{}
	}
	g := parts[0]
	if g == "core" {
		g = ""
	}
	return schema.GroupVersionResource{Group: g, Version: parts[1], Resource: parts[2]}
}

// GetKey builds the shared (user-agnostic) cache key for a single-resource GET.
func GetKey(gvr schema.GroupVersionResource, namespace, name string) string {
	return fmt.Sprintf("snowplow:get:%s:%s:%s", GVRToKey(gvr), namespace, name)
}

// ListKey builds the shared cache key for a LIST operation.
func ListKey(gvr schema.GroupVersionResource, namespace string) string {
	return fmt.Sprintf("snowplow:list:%s:%s", GVRToKey(gvr), namespace)
}

// DiscoveryKey builds the shared cache key for an API discovery result.
func DiscoveryKey(category string) string {
	return fmt.Sprintf("snowplow:discovery:%s", category)
}

func GetKeyPattern(gvr schema.GroupVersionResource) string {
	return fmt.Sprintf("snowplow:get:%s:*", GVRToKey(gvr))
}

func ListKeyPattern(gvr schema.GroupVersionResource) string {
	return fmt.Sprintf("snowplow:list:%s:*", GVRToKey(gvr))
}

// RBACKey builds the per-user cache key for a SelfSubjectAccessReview result.
func RBACKey(username, verb string, gr schema.GroupResource, namespace string) string {
	g := gr.Group
	if g == "" {
		g = "core"
	}
	return fmt.Sprintf("snowplow:rbac:%s:%s:%s/%s:%s", username, verb, g, gr.Resource, namespace)
}

func RBACKeyPattern(username string) string {
	return fmt.Sprintf("snowplow:rbac:%s:*", username)
}

// HTTPKey builds the shared (user-agnostic) cache key for a raw HTTP GET call.
func HTTPKey(method, path string) string {
	h := sha256.Sum256([]byte(path))
	return fmt.Sprintf("snowplow:http:%s:%x", strings.ToUpper(method), h[:8])
}

// HTTPUserKey builds a per-user cache key for HTTP GET calls made inside the
// RESTAction/widget resolution pipeline. These calls use the user's JWT, so
// responses may differ per user (RBAC-filtered namespace lists, widget lists,
// etc.). The path hash includes query parameters to avoid collisions.
func HTTPUserKey(username, method, path string) string {
	h := sha256.Sum256([]byte(path))
	return fmt.Sprintf("snowplow:http:%s:%s:%x", username, strings.ToUpper(method), h[:8])
}

// ResolvedKey builds the per-user cache key for a fully-resolved dispatcher
// output (widget or RESTAction). Caching at this level eliminates both the
// HTTP fan-out AND all JQ evaluations for repeated requests.
//
// Pagination is included in the key so paginated requests get isolated entries.
// Pass page=0 and perPage=0 for unpaginated resources (the common case).
func ResolvedKey(username string, gvr schema.GroupVersionResource, namespace, name string, page, perPage int) string {
	base := fmt.Sprintf("snowplow:resolved:%s:%s:%s:%s", username, GVRToKey(gvr), namespace, name)
	if page > 0 || perPage > 0 {
		return fmt.Sprintf("%s:p%d-pp%d", base, page, perPage)
	}
	return base
}

// AllResolvedPattern matches every dispatcher-level resolved cache entry.
// Used by the resource watcher to bulk-invalidate stale resolved outputs
// when any watched Kubernetes resource changes.
const AllResolvedPattern = "snowplow:resolved:*"

// AllHTTPPattern matches every per-user HTTP response cache entry created
// during RESTAction resolution. These must be invalidated alongside resolved
// keys when a watched resource changes, because the HTTP-cached responses
// contain raw Kubernetes API data that feeds into the resolution pipeline.
const AllHTTPPattern = "snowplow:http:*"

// IsNotFoundRaw returns true if raw is the negative-cache sentinel.
func IsNotFoundRaw(raw []byte) bool {
	return string(raw) == notFoundSentinel
}

// ParseGetKey parses a snowplow:get cache key into its components.
func ParseGetKey(key string) (gvr schema.GroupVersionResource, namespace, name string, ok bool) {
	parts := strings.SplitN(key, ":", 5)
	if len(parts) != 5 || parts[0] != "snowplow" || parts[1] != "get" {
		return
	}
	return ParseGVRKey(parts[2]), parts[3], parts[4], true
}

// ParseListKey parses a snowplow:list cache key into its components.
func ParseListKey(key string) (gvr schema.GroupVersionResource, namespace string, ok bool) {
	parts := strings.SplitN(key, ":", 4)
	if len(parts) != 4 || parts[0] != "snowplow" || parts[1] != "list" {
		return
	}
	return ParseGVRKey(parts[2]), parts[3], true
}

// GVRFromKey extracts the GVR from a snowplow:get or snowplow:list cache key.
func GVRFromKey(key string) schema.GroupVersionResource {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) < 3 {
		return schema.GroupVersionResource{}
	}
	typ := parts[1]
	if typ != "get" && typ != "list" {
		return schema.GroupVersionResource{}
	}
	gvrPart := strings.SplitN(parts[2], ":", 2)[0]
	return ParseGVRKey(gvrPart)
}

// ParseK8sAPIPath parses a Kubernetes API server path into GVR, namespace, name.
func ParseK8sAPIPath(path string) (gvr schema.GroupVersionResource, namespace, name string) {
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	path = strings.TrimPrefix(path, "/")
	segs := strings.Split(path, "/")

	switch {
	case len(segs) >= 2 && segs[0] == "api":
		gvr.Version = segs[1]
		if len(segs) >= 5 && segs[2] == "namespaces" {
			namespace = segs[3]
			gvr.Resource = segs[4]
			if len(segs) >= 6 {
				name = segs[5]
			}
		} else if len(segs) >= 3 {
			gvr.Resource = segs[2]
			if len(segs) >= 4 {
				name = segs[3]
			}
		}
	case len(segs) >= 3 && segs[0] == "apis":
		gvr.Group = segs[1]
		gvr.Version = segs[2]
		if len(segs) >= 6 && segs[3] == "namespaces" {
			namespace = segs[4]
			gvr.Resource = segs[5]
			if len(segs) >= 7 {
				name = segs[6]
			}
		} else if len(segs) >= 4 {
			gvr.Resource = segs[3]
			if len(segs) >= 5 {
				name = segs[4]
			}
		}
	}
	return
}
