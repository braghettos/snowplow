package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/krateoplatformops/plumbing/kubeutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Q-PREWARM-R2 — probe handler surgery (spec §1, R2.1+R2.2).
//
// Three changes vs the pre-R2 implementation:
//
//   - /health is a pure in-process liveness check. No more
//     rest.InClusterConfig() per probe. Under prewarm CPU saturation
//     even a 10ms cgroup file stat could miss the 1s probe timeout
//     when all P's were busy in JQ; the result was an unbreakable
//     restart loop. /health now writes 200 + "ok" with zero
//     filesystem or kube-API touches.
//
//   - /ready decouples from dispatchers.IsPreWarmComplete().
//     Gates only on cache.IsInformerReady() — set after the Phase 3
//     informer WaitForSync completes (~10-30s at 50K). L1 misses
//     fall through to the foreground singleflight resolve, so cold-
//     L1 requests are slow-but-correct rather than rejected with
//     503. Prewarm continues in the background regardless.
//
//   - Service metadata (name/build/namespace) moves to /info.
//     Operators that scraped /health for build info now scrape
//     /info. The kube probes do not depend on /info.
//
// These handlers are intended to be registered on a SECOND
// http.Server bound to PROBE_PORT (default 8082) with no
// middleware chain — see main.go probeServer wiring (R2.3).

// HealthCheck returns a pure in-process liveness handler.
//
// The handler does NOT touch the kube API server, the filesystem,
// or any user code. It exists solely to confirm the binary is
// running and able to schedule a goroutine to write a 5-byte
// response. Anything richer belongs on /info.
//
// Signature is preserved (serviceName, build, nsgetter) for
// backwards compatibility with the existing main.go wiring; the
// extra arguments are ignored. Callers that want the rich JSON
// payload should register InfoHandler at /info instead.
func HealthCheck(serviceName, build string, nsgetter func() (string, error)) http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		wri.Header().Set("Content-Type", "text/plain; charset=utf-8")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write([]byte("ok"))
	}
}

// InfoHandler returns the rich service-info JSON that pre-R2
// /health used to return. Mounted on /info on the main mux so
// operators and dashboards can still scrape build/namespace
// without competing with the kube probes.
func InfoHandler(serviceName, build string, nsgetter func() (string, error)) http.HandlerFunc {
	if nsgetter == nil {
		nsgetter = kubeutil.ServiceAccountNamespace
	}
	return func(wri http.ResponseWriter, req *http.Request) {
		ns, _ := nsgetter()

		data := serviceInfo{
			Name:      serviceName,
			Build:     build,
			Namespace: ns,
		}

		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(wri).Encode(data)
	}
}

// ReadinessCheck returns 200 once the resource informers have
// completed their initial LIST/WATCH sync (Phase 3 in
// startBackgroundServices). Pre-R2 this gated on
// dispatchers.IsPreWarmComplete(), which under production load
// (50K + 1004 users) never flipped — the pod was permanently
// NotReady and entered a restart loop. Post-R2 the pod becomes
// Ready as soon as informers are populated; L1 prewarm keeps
// running in the background and L1 misses fall through to the
// foreground resolve singleflight.
//
// When CACHE_ENABLED=false the cache subsystem is bypassed
// entirely; informers are not started, so there is nothing to
// wait for and the pod is Ready as soon as the handler is
// reachable.
func ReadinessCheck() http.HandlerFunc {
	return func(wri http.ResponseWriter, req *http.Request) {
		if !cache.Disabled() && !cache.IsInformerReady() {
			http.Error(wri, `{"status":"warming up"}`, http.StatusServiceUnavailable)
			return
		}
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		_, _ = wri.Write([]byte(`{"status":"ready"}`))
	}
}

type serviceInfo struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Build     string `json:"build"`
}
