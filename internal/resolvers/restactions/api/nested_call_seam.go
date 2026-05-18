// nested_call_seam.go — Ship 0.30.123 (#155): the in-process nested-/call
// resolver seam.
//
// THE IMPORT-CYCLE CONSTRAINT: this package (internal/resolvers/
// restactions/api) cannot import `restactions` or `dispatchers` — both
// import this package, so a back-import is a cycle. The actual
// nested-/call resolution (objects.Get -> checkDispatchRBAC ->
// restactions.Resolve) therefore lives in internal/handlers/dispatchers/
// nested_call.go, which CAN import everything. This file declares only
// the SEAM: a function-typed package var the dispatchers package fills
// at startup via RegisterNestedCallResolver. Mirrors the resolveOnceFn
// seam pattern (dispatchers/resolve_populate.go).
//
// When a RESTAction stage's `path` is a /call?resource=...&apiVersion=...
// loopback into snowplow's own /call endpoint, the resolver's inner-call
// worker (resolve.go) — instead of issuing an HTTP request with no
// Authorization header — invokes nestedCallResolver IN-PROCESS, carrying
// the WithUserInfo identity already on ctx. This lets a JWT-less /
// SA-credentialed resolve complete an exportJwt loopback stage (the
// 0.30.120 poison) and is the hard prerequisite for F2's startup
// SA-prewarm.

package api

import (
	"context"

	"github.com/krateoplatformops/plumbing/env"
	templatesv1 "github.com/krateoplatformops/snowplow/apis/templates/v1"
)

// envInprocessNestedCall gates the in-process nested-/call path. Default
// true: a /call-loopback stage resolves in-process. Set "false" and
// every /call stage takes the HTTP path, byte-identical to 0.30.121
// (the loopback-detection branch is skipped entirely).
const envInprocessNestedCall = "RESOLVER_INPROCESS_NESTED_CALL"

// inprocessNestedCallEnabled reports whether the in-process nested-/call
// path is enabled (default true).
func inprocessNestedCallEnabled() bool {
	return env.Bool(envInprocessNestedCall, true)
}

// NestedCallResolverFunc resolves a /call-loopback stage IN-PROCESS. ref
// is the ObjectReference decoded from the stage's /call?resource=...&
// apiVersion=... path; perPage/page/extras are the pagination + extras
// the stage carried. It returns the referenced RESTAction's resolved
// Status.Raw — byte-identical to what an HTTP /call would have returned
// as its response body — or an error.
//
// The implementation (dispatchers.ResolveNestedCall) MUST run the
// checkDispatchRBAC gate before resolving — the in-process path bypasses
// the HTTP edge, and with it the per-user apiserver RBAC enforcement, so
// the explicit gate is the single load-bearing correctness line.
type NestedCallResolverFunc func(
	ctx context.Context,
	ref templatesv1.ObjectReference,
	perPage, page int,
	extras map[string]any,
) (statusRaw []byte, err error)

// nestedCallResolver is the seam. nil until RegisterNestedCallResolver
// wires it at startup — a nil resolver is the SECOND structural fallback
// (alongside the env flag): the loopback branch is skipped and the /call
// stage takes the HTTP path. Production never reassigns it after the
// single startup wiring; tests swap it via the _test.go shim.
var nestedCallResolver NestedCallResolverFunc

// RegisterNestedCallResolver wires the in-process nested-/call resolver.
// Called once at startup from main.go —
// api.RegisterNestedCallResolver(dispatchers.ResolveNestedCall) —
// alongside the cache.RegisterRefreshFunc wiring. Idempotent in shape; a
// later call replaces the earlier wiring (used by tests).
func RegisterNestedCallResolver(fn NestedCallResolverFunc) {
	nestedCallResolver = fn
}
