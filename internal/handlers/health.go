package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"github.com/krateoplatformops/snowplow/internal/handlers/dispatchers"
	"k8s.io/client-go/rest"
)

// @Summary Liveness Endpoint
// @Description Health HealthCheck
// @ID health
// @Produce  json
// @Success 200 {object} serviceInfo
// @Router /health [get]
func HealthCheck(serviceName, build string, nsgetter func() (string, error)) http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		if !env.TestMode() {
			if _, err := rest.InClusterConfig(); err != nil {
				response.ServiceUnavailable(wri, err)
				return
			}
		}

		if nsgetter == nil {
			nsgetter = kubeutil.ServiceAccountNamespace
		}

		ns, _ := nsgetter()

		data := serviceInfo{
			Name:      serviceName,
			Build:     build,
			Namespace: ns,
		}

		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		json.NewEncoder(wri).Encode(data)
	}
}

// ReadinessCheck returns 200 only after the initial L1 pre-warm is complete.
// Used as the Kubernetes readiness probe so traffic isn't routed to a pod
// with a cold cache.
func ReadinessCheck() http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		if !cache.Disabled() && !dispatchers.IsPreWarmComplete() {
			http.Error(wri, `{"status":"warming up"}`, http.StatusServiceUnavailable)
			return
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		wri.Write([]byte(`{"status":"ready"}`))
	}
}

type serviceInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Build     string `json:"build"`
}
