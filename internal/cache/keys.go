package cache

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	WatchedGVRsKey   = "snowplow:watched-gvrs"
	ActiveUsersKey   = "snowplow:active-users"
	L1ReadyKey       = "snowplow:l1:ready"
	notFoundSentinel = `{"__snowplow_not_found__":true}`
)

// MarkL1Ready writes a Unix-epoch timestamp to the L1 ready sentinel key.
// External consumers (e.g. e2e tests) can poll this key to deterministically
// know when the most recent L1 warmup or refresh cycle completed.
func MarkL1Ready(ctx context.Context, c *RedisCache) {
	if c == nil {
		return
	}
	_ = c.SetString(ctx, L1ReadyKey, strconv.FormatInt(time.Now().Unix(), 10))
}

// L1ReadyTimestamp returns the Unix epoch stored in the L1 ready key, or 0.
func L1ReadyTimestamp(ctx context.Context, c *RedisCache) int64 {
	if c == nil {
		return 0
	}
	raw, _, err := c.GetRaw(ctx, L1ReadyKey)
	if err != nil || len(raw) == 0 {
		return 0
	}
	ts, _ := strconv.ParseInt(string(raw), 10, 64)
	return ts
}

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

// UserRBACIndexKey returns the Redis SET key that tracks all RBAC cache keys
// for a given user. Used for O(1) invalidation instead of SCAN.
func UserRBACIndexKey(username string) string {
	return "snowplow:rbac-idx:" + username
}

// UserResolvedIndexKey returns the Redis SET key that tracks all resolved (L1)
// cache keys for a given user. Used for O(1) invalidation instead of SCAN.
func UserResolvedIndexKey(username string) string {
	return "snowplow:resolved-idx:" + username
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

// L1GVRKey returns the Redis SET key that maps a GVR to all L1 (resolved)
// cache entries that depend on it. Used for targeted invalidation.
func L1GVRKey(gvrKey string) string {
	return "snowplow:l1gvr:" + gvrKey
}

// UserConfigKey builds the per-user cache key for the Endpoint fetched from
// the user's -clientconfig Secret. Invalidated by UserSecretWatcher when the
// Secret changes.
func UserConfigKey(username string) string {
	return "snowplow:userconfig:" + username
}

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

// ParseCallPath extracts GVR, namespace, and name from a snowplow /call URL
// (e.g. /call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=foo&namespace=bar).
func ParseCallPath(rawPath string) (gvr schema.GroupVersionResource, namespace, name string) {
	idx := strings.Index(rawPath, "?")
	if idx < 0 {
		return
	}
	prefix := rawPath[:idx]
	if prefix != "/call" && prefix != "call" {
		return
	}
	q, err := url.ParseQuery(rawPath[idx+1:])
	if err != nil {
		return
	}
	apiVersion := q.Get("apiVersion")
	resource := q.Get("resource")
	if apiVersion == "" || resource == "" {
		return
	}
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 {
		gvr.Group = parts[0]
		gvr.Version = parts[1]
	} else {
		gvr.Version = parts[0]
	}
	gvr.Resource = resource
	namespace = q.Get("namespace")
	name = q.Get("name")
	return
}

// ResolvedKeyInfo holds the parsed components of a resolved (L1) cache key.
type ResolvedKeyInfo struct {
	Username string
	GVR      schema.GroupVersionResource
	NS       string
	Name     string
	Page     int
	PerPage  int
}

// ParseResolvedKey parses an L1 resolved cache key into its components.
// Key format: snowplow:resolved:{user}:{group/version/resource}:{ns}:{name}[:p{page}-pp{perPage}]
func ParseResolvedKey(key string) (ResolvedKeyInfo, bool) {
	parts := strings.SplitN(key, ":", 7)
	if len(parts) < 6 || parts[0] != "snowplow" || parts[1] != "resolved" {
		return ResolvedKeyInfo{}, false
	}
	info := ResolvedKeyInfo{
		Username: parts[2],
		GVR:      ParseGVRKey(parts[3]),
		NS:       parts[4],
		Name:     parts[5],
		Page:     -1,
		PerPage:  -1,
	}
	if len(parts) == 7 {
		fmt.Sscanf(parts[6], "p%d-pp%d", &info.Page, &info.PerPage)
	}
	if info.GVR.Resource == "" || info.Name == "" {
		return ResolvedKeyInfo{}, false
	}
	return info, true
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
