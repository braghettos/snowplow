// sa_client.go — Tag 0.30.9 Sub-scope A: snowplow ServiceAccount
// endpoint provider for the userAccessFilter dispatch path.
//
// When a RestAction API stage declares userAccessFilter, snowplow
// dispatches the inner K8s call using its OWN ServiceAccount token
// (cluster-wide read) instead of the per-user-clientconfig token.
// The returned result set is then in-process-refiltered through
// EvaluateRBAC so the caller only sees objects they are RBAC-permitted
// to read.
//
// Two design constraints (binding):
//   1. feedback_no_special_cases.md — no per-resource policy here.
//      The SA endpoint is a single, uniform fallback; the resolver
//      decides WHEN to use it based on userAccessFilter presence.
//   2. feedback_l1_invalidation_delete_only.md is unaffected — the
//      SA endpoint is the read path, not the dep-tracker.
//
// Concurrency: the singleton is built lazily on first call and
// cached process-wide. After construction the *Endpoint is immutable;
// callers MUST NOT mutate the returned pointer's fields.
//
// Memory: one *Endpoint per process. Negligible.

package dynamic

import (
	"fmt"
	"os"
	"sync"

	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/client-go/rest"
)

// Standard in-cluster ServiceAccount projected paths (see
// https://kubernetes.io/docs/tasks/run-application/access-api-from-pod/).
// These mirror the locations rest.InClusterConfig reads.
//
// They are package vars (not consts) ONLY so the credential-real
// falsifier test can point ServiceAccountEndpoint at a temp dir holding
// real-shaped credentials (a raw JWT token + a raw PEM CA) — the 0.30.102
// unit tests shipped the "illegal base64 data" bug precisely because they
// only exercised the no-files error path, never a real SA credential.
// Production NEVER reassigns these; the values are the fixed projected
// SA volume paths.
var (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// saEndpointSingleton is the process-wide cached SA endpoint. Built
// lazily on first call to ServiceAccountEndpoint(); error path
// re-tries on every call (so a recoverable file-read failure at
// startup doesn't poison the lifetime of the process).
var (
	saEndpointMu       sync.Mutex
	saEndpointInstance *endpoints.Endpoint
)

// ServiceAccountEndpoint returns the snowplow ServiceAccount-backed
// Endpoint used by the userAccessFilter dispatch path. The endpoint
// targets the in-cluster apiserver ("https://kubernetes.default.svc")
// and carries snowplow's projected SA token (cluster-wide read).
//
// Caches the result on first success — subsequent calls return the
// same pointer. On failure (e.g., running outside a cluster, missing
// token / CA files), returns an error and does NOT cache, so a later
// call can retry (intended for unit tests that synthesise the files
// after pod-init — never relevant in production where the SA volume
// is always mounted).
//
// The function is goroutine-safe.
//
// Per Revision 17 + plan §"Sub-scope A — UAF" binding: this is the
// ONLY way snowplow obtains cluster-wide-read credentials at
// dispatch time. The endpoint is NOT user-bound (deliberately) so
// the refilter step is the load-bearing security gate per
// feedback_no_shortcuts_or_workarounds.md.
func ServiceAccountEndpoint() (*endpoints.Endpoint, error) {
	saEndpointMu.Lock()
	defer saEndpointMu.Unlock()

	if saEndpointInstance != nil {
		return saEndpointInstance, nil
	}

	tokenBytes, err := os.ReadFile(saTokenPath)
	if err != nil {
		return nil, fmt.Errorf("dynamic.sa: read SA token at %s: %w", saTokenPath, err)
	}
	if len(tokenBytes) == 0 {
		return nil, fmt.Errorf("dynamic.sa: SA token at %s is empty", saTokenPath)
	}

	caBytes, err := os.ReadFile(saCAPath)
	if err != nil {
		return nil, fmt.Errorf("dynamic.sa: read SA CA at %s: %w", saCAPath, err)
	}

	ep := &endpoints.Endpoint{
		ServerURL:                "https://kubernetes.default.svc",
		Token:                    string(tokenBytes),
		CertificateAuthorityData: string(caBytes),
		Insecure:                 false,
	}
	saEndpointInstance = ep
	return ep, nil
}

// ServiceAccountRESTConfig returns a *rest.Config backed by snowplow's
// in-cluster ServiceAccount. Used by callers that need a real
// kubernetes.Clientset (e.g., the typed dynamic client) rather than
// the Endpoint shape that the httpcall resolver consumes.
//
// Returns whatever rest.InClusterConfig errors when running outside
// a cluster — the production caller path always lives inside a pod.
func ServiceAccountRESTConfig() (*rest.Config, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("dynamic.sa: rest.InClusterConfig: %w", err)
	}
	return rc, nil
}

// resetSAEndpointForTest clears the singleton so each test sees a
// fresh state. Exported via the _test.go shim only; production code
// MUST NOT call this.
func resetSAEndpointForTest() {
	saEndpointMu.Lock()
	defer saEndpointMu.Unlock()
	saEndpointInstance = nil
}
