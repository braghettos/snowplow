// internal_client_test.go — 0.30.103 credential-real falsifier for the
// fix side: cache.ClientConfigFor + WithInternalRESTConfig.
//
// Companion to internal/dynamic/sa_credential_real_test.go (which
// reproduces the 0.30.102 "illegal base64 data" bug against the
// kubeconfig path). This file proves the FIX: a *rest.Config carrying the
// SA's real-shaped raw in-cluster credentials, attached to the context
// via WithInternalRESTConfig, is returned VERBATIM by ClientConfigFor —
// bypassing kubeconfig.NewClientConfig entirely — with the SA bearer
// token and CA preserved.
//
// All credentials here are real-shaped: a raw JWT bearer token and a raw
// PEM CA certificate. No mock identities — the 0.30.102 tests failed to
// catch the bug precisely because they used stubs/mocks.

package cache

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"k8s.io/client-go/rest"
)

const realShapedSAToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3QifQ." +
	"eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6a3JhdGVvLXN5c3RlbTpzbm93cGxvdyJ9." +
	"c2lnbmF0dXJlLWJ5dGVz"

// realShapedSACAPEM generates a real, parseable self-signed CA cert as a
// raw PEM string — the shape of /var/run/secrets/.../ca.crt.
func realShapedSACAPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "kube-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// saRESTConfig builds the SA *rest.Config the way main.go's
// rest.InClusterConfig does it: the raw bearer token in BearerToken and
// the raw PEM CA in TLSClientConfig.CAData (NO base64 round-trip — these
// are exactly the bytes rest.InClusterConfig reads from the projected SA
// volume files).
func saRESTConfig(t *testing.T) *rest.Config {
	t.Helper()
	return &rest.Config{
		Host:        "https://kubernetes.default.svc",
		BearerToken: realShapedSAToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: []byte(realShapedSACAPEM(t)),
		},
	}
}

// TestClientConfigFor_InternalRESTConfig_UsedVerbatim is the fix-side
// falsifier. With a SA *rest.Config attached via WithInternalRESTConfig,
// ClientConfigFor MUST return it verbatim — never routing the SA through
// kubeconfig.NewClientConfig (which would drop the token and reject the
// raw-PEM CA).
func TestClientConfigFor_InternalRESTConfig_UsedVerbatim(t *testing.T) {
	saRC := saRESTConfig(t)
	ctx := WithInternalRESTConfig(context.Background(), saRC)

	// The endpoint argument is the SA endpoint shape (raw-PEM CA, raw
	// token) — the SAME endpoint that fails through kubeconfig in the
	// dynamic-package negative-control test. ClientConfigFor must NOT
	// touch it: the context-injected *rest.Config wins.
	saEP := endpoints.Endpoint{
		ServerURL:                "https://kubernetes.default.svc",
		Token:                    realShapedSAToken,
		CertificateAuthorityData: realShapedSACAPEM(t),
	}

	got, err := ClientConfigFor(ctx, saEP)
	if err != nil {
		t.Fatalf("ClientConfigFor with WithInternalRESTConfig must succeed (the 0.30.103 fix); got %v", err)
	}
	if got != saRC {
		t.Fatalf("ClientConfigFor must return the context-injected *rest.Config verbatim; got a different pointer")
	}
	// The SA's real credentials must be intact — usable client config.
	if got.BearerToken != realShapedSAToken {
		t.Fatalf("SA bearer token lost — kubeconfig.NewClientConfig would have dropped it; "+
			"ClientConfigFor must preserve it. got len=%d", len(got.BearerToken))
	}
	if len(got.TLSClientConfig.CAData) == 0 {
		t.Fatalf("SA CA data lost — the usable SA client must carry the cluster CA")
	}
	if !strings.HasPrefix(string(got.TLSClientConfig.CAData), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("SA CAData must be the raw PEM the in-cluster volume carries")
	}
}

// TestClientConfigFor_NoInternalConfig_DelegatesToKubeconfig asserts the
// behavior-neutral guarantee for ordinary per-user requests: with NO
// internal *rest.Config on the context, ClientConfigFor delegates to
// kubeconfig.NewClientConfig — the unchanged per-user path. We use a
// cert-less, base64-clean endpoint so the delegate succeeds; the point is
// only that delegation happens (no internal config => no shortcut).
func TestClientConfigFor_NoInternalConfig_DelegatesToKubeconfig(t *testing.T) {
	perUserEP := endpoints.Endpoint{
		ServerURL: "https://example.com",
		// No CA, no token — kubeconfig.NewClientConfig accepts this and
		// produces a (credential-less) config. The per-user path is
		// exercised unchanged; we only assert it is the path taken.
	}
	got, err := ClientConfigFor(context.Background(), perUserEP)
	if err != nil {
		t.Fatalf("ClientConfigFor without an internal config must delegate to kubeconfig.NewClientConfig: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a non-nil *rest.Config from the per-user delegate path")
	}
	if got.Host != "https://example.com" {
		t.Fatalf("per-user delegate path must build from the endpoint; got Host=%q", got.Host)
	}
}

// TestClientConfigFor_WrongShape_FallsThrough asserts the diagnosable
// fallback: if a context carries an internal value of the WRONG type,
// ClientConfigFor does not panic — it falls through to the per-user
// delegate path (which, for a real synthetic SA identity, then fails
// loudly with a missing-Secret error — the intended diagnosable outcome).
func TestClientConfigFor_WrongShape_FallsThrough(t *testing.T) {
	ctx := WithInternalRESTConfig(context.Background(), "not-a-rest-config")
	got, err := ClientConfigFor(ctx, endpoints.Endpoint{ServerURL: "https://example.com"})
	if err != nil {
		t.Fatalf("wrong-shape internal value must fall through to the delegate, not error here: %v", err)
	}
	if got == nil || got.Host != "https://example.com" {
		t.Fatalf("expected fall-through to the per-user delegate path")
	}
}
