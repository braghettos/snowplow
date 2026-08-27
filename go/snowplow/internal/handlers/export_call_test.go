package handlers

import (
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// CallWithScopeResolver is a TEST-ONLY constructor (Issue #156) that builds
// the /call handler with an injected scope resolver instead of the default
// SA-discovery-singleton-backed one. It exists so the kind-backed
// integration arm (call_test.go, external handlers_test package) can supply
// a resolver built from the kind cluster's REST config — the test process
// runs OUTSIDE the cluster and has no projected SA volume, so the default
// dynamic.SharedSAScopeForGVR path is unavailable there.
//
// This lives in an _test.go file, so it is NOT part of the production build
// surface — production main.go always uses handlers.Call().
func CallWithScopeResolver(resolver func(schema.GroupVersionResource) (bool, error)) http.Handler {
	return &callHandler{
		scopeResolver: resolver,
	}
}
