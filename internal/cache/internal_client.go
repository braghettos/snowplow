// internal_client.go — 0.30.103 bug fix: the apiserver client-config
// builder that the resolver's object/resourceRef fetch sites use instead
// of calling kubeconfig.NewClientConfig directly.
//
// THE BUG (0.30.102 Tag B Phase 1, reproduced flag-ON on every boot):
//
//   Phase 1's SA-credentialed resolution walk fetches the routesloaders
//   navigation root (and its RESTAction CRs) through the standard widget
//   resolver under the snowplow service-account identity. The walk
//   injects the SA endpoint into the context; the resolver's object-fetch
//   sites then rebuilt a kube client from that endpoint via
//   plumbing's kubeconfig.NewClientConfig(ctx, ep).
//
//   kubeconfig.NewClientConfig marshals the endpoint into a kubeconfig
//   document and hands it to client-go's clientcmd loader. That path is:
//     - CERT-AUTH-ONLY — the kubeconfig user block has no token field, so
//       a token-bearing endpoint loses its only credential; and
//     - base64-AWARE — clientcmd base64-decodes certificate-authority-data.
//
//   The per-user `<user>-clientconfig` Secret works there because the
//   authn signup flow stores credentials base64-encoded and per-user auth
//   is cert-based. But the snowplow SA's own in-cluster credentials — the
//   projected /var/run/secrets/kubernetes.io/serviceaccount/token (a raw
//   JWT) and ca.crt (raw PEM) — are NOT base64-encoded, and the SA
//   authenticates by TOKEN, not client cert. Routing the SA endpoint
//   through kubeconfig.NewClientConfig therefore fails immediately:
//
//     phase1.warmup.root_resolve_failed
//       err="illegal base64 data at input byte 0"
//
//   the raw-PEM CA's leading '-' is not in the base64 alphabet. Effect:
//   roots_resolved=0/1, Phase 1 pre-warms nothing, the headline 0.30.102
//   feature is a no-op.
//
// THE FIX: the SA cannot be expressed as a kubeconfig-loadable endpoint.
// Its *rest.Config must be built directly from the raw in-cluster
// credentials (rest.InClusterConfig, which reads the raw token/ca.crt
// files with the correct semantics). Phase 1 already holds that
// *rest.Config; it attaches it to the context via WithInternalRESTConfig.
// ClientConfigFor consults the context first and returns that pre-built
// *rest.Config verbatim — bypassing the base64/cert-only kubeconfig path.
// Ordinary per-user requests never set it and take the unchanged
// kubeconfig.NewClientConfig path, so this is behavior-neutral for them.
//
// feedback_no_special_cases.md: this is a uniform mechanism — any
// internal driver can hand the resolver a pre-built *rest.Config; the
// builder is one code path keyed on context state, never on a resource.

package cache

import (
	"context"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"k8s.io/client-go/rest"
)

// ClientConfigFor returns the apiserver *rest.Config the caller should use
// to build a kube client for the given context.
//
//   - If the context carries an internal-dispatch *rest.Config (attached
//     by WithInternalRESTConfig — Phase 1's SA-credentialed walk), that
//     pre-built config is returned verbatim. This is the load-bearing fix
//     path: it bypasses kubeconfig.NewClientConfig, which cannot carry the
//     SA's raw-PEM CA or bearer token.
//   - Otherwise (every ordinary per-user request) it delegates to
//     plumbing's kubeconfig.NewClientConfig(ctx, ep), unchanged — the
//     per-user `<user>-clientconfig` endpoint is cert-based and
//     base64-encoded, exactly what that path expects.
//
// The internal *rest.Config is shared, read-only — callers MUST NOT mutate
// the returned pointer's fields. plumbing's kubeconfig.NewClientConfig
// already returns a freshly-built config per call on the per-user path.
func ClientConfigFor(ctx context.Context, ep endpoints.Endpoint) (*rest.Config, error) {
	if v, ok := InternalRESTConfigFromContext(ctx); ok {
		if rc, rcOK := v.(*rest.Config); rcOK && rc != nil {
			return rc, nil
		}
		// A context carried an internal REST config of the wrong shape /
		// a nil pointer. Fall through to the per-user path rather than
		// fail — but the per-user path has no `<sa>-clientconfig` Secret
		// for a synthetic SA identity and will fail loudly there, which is
		// the diagnosable outcome. A future internal driver passing the
		// wrong type is caught by the falsifier test in this package.
	}
	return kubeconfig.NewClientConfig(ctx, ep)
}
