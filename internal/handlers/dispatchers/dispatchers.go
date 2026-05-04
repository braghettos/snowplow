package dispatchers

import (
	"net/http"

	"github.com/krateoplatformops/plumbing/endpoints"
)

// All returns the GVR → http.Handler dispatcher table used by handlers.Call.
//
// snowplowEndpointFn is the elevated-call provider plumbed to the
// restactions dispatcher (Q-RBAC-DECOUPLE C(d)). May be nil; that disables
// userAccessFilter dispatch with an explicit log line at resolve time.
func All(snowplowEndpointFn func() (*endpoints.Endpoint, error)) map[string]http.Handler {
	return map[string]http.Handler{
		"restactions.templates.krateo.io": RESTAction(snowplowEndpointFn),
		"widgets.templates.krateo.io":     Widgets(),
	}
}
