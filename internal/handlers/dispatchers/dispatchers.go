package dispatchers

import (
	"net/http"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
)

// All returns the GVR → http.Handler dispatcher table used by handlers.Call.
//
// snowplowEndpointFn is the elevated-call provider plumbed to the
// restactions dispatcher (Q-RBAC-DECOUPLE C(d)). May be nil; that disables
// userAccessFilter dispatch with an explicit log line at resolve time.
//
// Q-RBAC-DECOUPLE C(d) v6 — Path B: snowplowK8sClient is the in-cluster
// dynamic client used for SA dispatch (replaces httpcall.Do for the SA
// branch). Either field is sufficient to enable UAF dispatch.
func All(snowplowEndpointFn func() (*endpoints.Endpoint, error), snowplowK8sClient dynamic.Client) map[string]http.Handler {
	return map[string]http.Handler{
		"restactions.templates.krateo.io": RESTAction(snowplowEndpointFn, snowplowK8sClient),
		"widgets.templates.krateo.io":     Widgets(),
	}
}
