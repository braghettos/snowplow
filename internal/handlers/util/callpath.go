// callpath.go — the shared `/call` query-param decoder.
//
// Ship 0.30.123 (#155): ParseCallPathToObjectRef was LIFTED VERBATIM out
// of internal/handlers/dispatchers/phase1_walk.go's parseCallPathToObjectRef
// into this leaf package so BOTH the resolver (internal/resolvers/
// restactions/api — which cannot import dispatchers, import cycle) and
// the dispatchers package can share one decoder. Pure move, zero
// behaviour change: phase1_walk.go now calls this shared function.
//
// A `/call?resource=...&apiVersion=...&name=...&namespace=...` URL is
// snowplow's own loopback endpoint shape — the frontend dispatches every
// navigation child on exactly this shape, and a RESTAction stage whose
// `path` is such a URL is a nested /call into snowplow itself. This is
// the generic /call query-param decoder — the SAME params util.ParseGVR
// / util.ParseNamespacedName read off a real HTTP request — NOT a
// hardcoded resource/path special-case (feedback_no_special_cases.md).

package util

import (
	"net/url"
	"strings"

	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// ParseCallPathToObjectRef parses a `/call?resource=...&apiVersion=...&
// name=...&namespace=...` endpoint into the ObjectReference an
// objects.Get fetch needs. Returns ok=false for any path that is not a
// /call endpoint (external link, missing resource/apiVersion).
//
// Detection is keyed on the path SHAPE only — the trimmed path ends in
// "/call" AND both `resource` and `apiVersion` query params are present.
// No resource/name/host literal is consulted.
func ParseCallPathToObjectRef(path string) (templatesv1.ObjectReference, bool) {
	u, err := url.Parse(path)
	if err != nil {
		return templatesv1.ObjectReference{}, false
	}
	// Only a /call endpoint carries a CR. The trimmed path must end in
	// "/call" (it may be host-qualified or root-relative).
	trimmed := strings.TrimRight(u.Path, "/")
	if trimmed != "" && !strings.HasSuffix(trimmed, "/call") {
		return templatesv1.ObjectReference{}, false
	}
	q := u.Query()
	resource := q.Get("resource")
	apiVersion := q.Get("apiVersion")
	if resource == "" || apiVersion == "" {
		return templatesv1.ObjectReference{}, false
	}
	return templatesv1.ObjectReference{
		Reference: templatesv1.Reference{
			Name:      q.Get("name"),
			Namespace: q.Get("namespace"),
		},
		Resource:   resource,
		APIVersion: apiVersion,
	}, true
}
