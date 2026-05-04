package api

import (
	"fmt"
	"os"
	"strings"

	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/client-go/rest"
)

// SnowplowEndpointFromConfig converts an in-cluster *rest.Config into a
// plumbing endpoints.Endpoint suitable for the snowplow-elevated dispatch
// path used by api[] entries that declare UserAccessFilter.
//
// The conversion is intentionally minimal: it copies the API server URL,
// any CA material (CAData prevails over CAFile when both are set), and
// the bearer token (BearerTokenFile is preferred and re-read at every
// call so projected SA token rotation is picked up automatically — see
// Q-RBACC-IMPL-8).
//
// Callers are expected to wrap this helper in a closure that captures
// the *rest.Config once at startup and exposes the no-arg
// `func() (*endpoints.Endpoint, error)` shape consumed by
// api.ResolveOptions.SnowplowEndpoint.
func SnowplowEndpointFromConfig(rc *rest.Config) (*endpoints.Endpoint, error) {
	if rc == nil {
		return nil, fmt.Errorf("nil rest.Config")
	}

	var token string
	switch {
	case rc.BearerTokenFile != "":
		// Re-read on every call. The kubelet rotates projected SA tokens
		// roughly every hour by writing a new tmpfs file; reading once
		// at startup would pin the original token until pod restart.
		// tmpfs reads are ~10 µs so the per-call cost is negligible.
		b, err := os.ReadFile(rc.BearerTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read SA token %q: %w", rc.BearerTokenFile, err)
		}
		token = strings.TrimSpace(string(b))
	case rc.BearerToken != "":
		token = rc.BearerToken
	default:
		return nil, fmt.Errorf("rest.Config has neither BearerToken nor BearerTokenFile")
	}

	var caPEM string
	switch {
	case len(rc.TLSClientConfig.CAData) > 0:
		caPEM = string(rc.TLSClientConfig.CAData)
	case rc.TLSClientConfig.CAFile != "":
		b, err := os.ReadFile(rc.TLSClientConfig.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read SA CA %q: %w", rc.TLSClientConfig.CAFile, err)
		}
		caPEM = string(b)
	}

	return &endpoints.Endpoint{
		ServerURL:                rc.Host,
		CertificateAuthorityData: caPEM,
		Token:                    token,
		Insecure:                 rc.TLSClientConfig.Insecure,
	}, nil
}
