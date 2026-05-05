// Q-RBAC-DECOUPLE C(d) v6 — TLS handshake regression test
// (audit 2026-05-04).
//
// This test drives the production codepath that produced the v5 D1
// failure (`tls: failed to verify certificate: x509: certificate signed
// by unknown authority`) and asserts:
//
//  1. THE BUG IS REAL on v5 baseline: `httpcall.Do(token-auth + CA)`
//     fails the TLS handshake against a server whose cert is signed by
//     the CA in the endpoint's `CertificateAuthorityData`. Plumbing's
//     `tlsConfigFor` early-returns at `if !ep.HasCertAuth()` and never
//     wires `RootCAs`; the distroless system pool is effectively empty;
//     verification fails. Live evidence on `0.25.298`:
//     "Get https://kubernetes.default.svc/...: tls: failed to verify
//     certificate: x509: certificate signed by unknown authority"
//
//  2. THE BUG IS ABSENT on the v6 client-go path (`Path B`): the
//     equivalent dispatch via `dynamic.Client` from a `rest.Config`
//     carrying the same `CAData` + `BearerToken` succeeds the TLS
//     handshake. Client-go's `rest.TLSConfigFor` correctly consumes
//     `CAData` regardless of whether `CertData/KeyData` are set —
//     there's no `HasCertAuth()` early-return bug. So the same trust
//     material that plumbing silently dropped, client-go honors.
//
// Test names use the `OnV5Baseline` / `OnV6` suffixes so the failure
// messages explicitly cite which codepath the assertion is for.
//
// CRITICAL CONTRACT: case `TestHttpcallDo_TokenAuthWithCA_FailsOnV5Baseline`
// MUST FAIL on a *future* fix to plumbing's `tlsConfigFor` — at which
// point this test becomes a positive assertion that v6 is no longer
// load-bearing. Today, both cases together prove the bug is structurally
// bypassed by Path B even though plumbing remains buggy.

package httpcall_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/dynamic"
	"github.com/krateoplatformops/snowplow/internal/httpcall"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// generateTLSFixture builds a self-signed CA + server cert with SAN
// entries DNS=["kubernetes.default.svc"] and IP=[127.0.0.1]. The DNS SAN
// matches the production-shape SA ca.crt; the IP SAN lets the test bind
// to localhost without needing DNS overrides at the OS level.
//
// Returns (caPEM, certPEM, keyPEM, *tls.Certificate-for-server).
func generateTLSFixture(t *testing.T) (caPEM, certPEM, keyPEM []byte, srvCert tls.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "snowplow-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"kubernetes.default.svc", "localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	caPEM = certPEM
	srvCert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return
}

// startTestTLSServer spins up an httptest.Server bound to a self-signed
// cert with the SAN list above. Returns the server URL.
func startTestTLSServer(t *testing.T, srvCert tls.Certificate, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{srvCert},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestHttpcallDo_TokenAuthWithCA_FailsOnV5Baseline asserts the v5 D1 bug
// is still present in the unchanged `httpcall.Do` codepath: a token-auth
// + CA endpoint cannot complete the TLS handshake against a server whose
// cert is signed by the endpoint's CA, because plumbing's tlsConfigFor
// silently drops CertificateAuthorityData when HasCertAuth() returns
// false (token-auth has no client cert).
//
// This test is a NEGATIVE-BASELINE assertion: when this test FAILS in
// the future (e.g. plumbing ships a fix), v6's Path B becomes
// non-load-bearing for the SA dispatch case — that's a green-light to
// remove the v6 plumbing entirely. Until then, this test proves Path B
// is structurally necessary.
//
// The test does NOT assert a specific error string (Go's TLS error
// messages vary across versions), only that:
//   - the dispatch returns response.StatusFailure
//   - the message contains "tls" or "x509" (any verification failure is
//     evidence of the CA-drop bug)
func TestHttpcallDo_TokenAuthWithCA_FailsOnV5Baseline(t *testing.T) {
	caPEM, _, _, srvCert := generateTLSFixture(t)

	// Server returns a small JSON body so the request would succeed if
	// the TLS handshake passed. We never expect to reach this handler on
	// v5 baseline — TLS fails first.
	srv := startTestTLSServer(t, srvCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	ep := &endpoints.Endpoint{
		// Use the test server's actual URL (e.g. https://127.0.0.1:NNN)
		// so DNS resolution doesn't mask the TLS-layer assertion.
		ServerURL:                srv.URL,
		Token:                    "test-bearer-token",
		CertificateAuthorityData: string(caPEM),
		Insecure:                 false,
	}

	verb := http.MethodGet
	res := httpcall.Do(context.Background(), httpcall.RequestOptions{
		Endpoint: ep,
		RequestInfo: httpcall.RequestInfo{
			Path: "/",
			Verb: ptr.To(verb),
		},
	})

	if res == nil {
		t.Fatalf("expected non-nil response.Status from httpcall.Do")
	}
	if res.Status != response.StatusFailure {
		t.Fatalf("v5 baseline FAILED to reproduce: expected StatusFailure (TLS handshake should fail because plumbing drops CA on token-auth), got Status=%q Code=%d Message=%q.\n"+
			"If this test starts passing, plumbing's tlsConfigFor likely shipped a fix and v6 Path B may no longer be load-bearing for SA dispatch.",
			res.Status, res.Code, res.Message)
	}
	msg := strings.ToLower(res.Message)
	if !strings.Contains(msg, "tls") && !strings.Contains(msg, "x509") && !strings.Contains(msg, "certificate") {
		t.Fatalf("v5 baseline reproduced a failure but not the expected TLS/x509 shape: Message=%q", res.Message)
	}
	t.Logf("v5 baseline reproduced: TLS handshake fails through httpcall.Do → plumbing/tlsConfigFor on token-auth+CA endpoint.\nMessage: %s", res.Message)
}

// TestHttpcallDo_TokenAuthWithCA_PoolPreserves verifies that the failure
// is reproducible on a fresh pool entry too — guards against accidental
// future caching of a working transport that would mask the bug.
func TestHttpcallDo_TokenAuthWithCA_PoolPreserves(t *testing.T) {
	caPEM, _, _, srvCert := generateTLSFixture(t)
	srv := startTestTLSServer(t, srvCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	ep := &endpoints.Endpoint{
		ServerURL:                srv.URL,
		Token:                    "tok-pool-test",
		CertificateAuthorityData: string(caPEM),
	}
	verb := http.MethodGet
	for i := 0; i < 3; i++ {
		res := httpcall.Do(context.Background(), httpcall.RequestOptions{
			Endpoint: ep,
			RequestInfo: httpcall.RequestInfo{
				Path: "/",
				Verb: ptr.To(verb),
			},
		})
		if res.Status != response.StatusFailure {
			t.Fatalf("iteration %d: expected StatusFailure (v5 plumbing CA-drop bug), got %q", i, res.Status)
		}
	}
}

// TestPathBSADispatch_TokenAuthWithCA_HandshakeOK asserts the v6 Path B
// codepath structurally bypasses the v5 D1 bug. Building a `rest.Config`
// from the SAME materials (token + CA) that the v5 endpoint carries and
// dispatching via `dynamic.Client` (the production v6 path) succeeds the
// TLS handshake.
//
// We can't run a full `dynamic.Client.List()` against a non-K8s server
// because the dynamic client needs to discover REST mappings first. So
// instead we drive `rest.RESTClientFor` directly with the same rest.Config
// — that's the layer that plumbing's tlsConfigFor was supposed to mirror,
// and the layer that client-go's dynamic client builds on top of.
//
// The TLS handshake assertion fires inside `rest.HTTPClientFor` via
// `transport.New(config)` → `transport.TLSConfigFor(config)`. If CAData
// is loaded correctly, the test request returns either HTTP 404 (the
// test server doesn't implement K8s API verbs) or any non-TLS error.
// Critically: it does NOT return a TLS x509 error.
func TestPathBSADispatch_TokenAuthWithCA_HandshakeOK(t *testing.T) {
	caPEM, _, _, srvCert := generateTLSFixture(t)

	srv := startTestTLSServer(t, srvCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mimic apiserver-style 404 for unknown paths.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","code":404}`))
	}))

	rc := &rest.Config{
		Host:        srv.URL,
		BearerToken: "test-bearer-token",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}

	// rest.HTTPClientFor builds the same TLS config that dynamic.NewClient
	// uses. If RootCAs is loaded correctly, the handshake succeeds.
	cli, err := rest.HTTPClientFor(rc)
	if err != nil {
		t.Fatalf("rest.HTTPClientFor failed (BUG): %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	resp, err := cli.Do(req)
	if err != nil {
		// An error here that mentions tls/x509 means the CA pool failed
		// to load — i.e., the v6 fix is broken. Any other error is fine.
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "x509") || strings.Contains(errStr, "tls: failed to verify") {
			t.Fatalf("v6 Path B BROKEN: TLS handshake failed via rest.HTTPClientFor: %v", err)
		}
		// Other errors (timeout, server hang up) are fine for this test —
		// the TLS handshake completed, which is what we're asserting.
		t.Logf("non-TLS error (acceptable): %v", err)
		return
	}
	defer resp.Body.Close()
	t.Logf("v6 Path B succeeded: TLS handshake OK, server responded with HTTP %d", resp.StatusCode)
}

// TestPathBSADispatch_DynamicClientHandshakeOK adds a stronger assertion
// driven through `dynamic.NewClient` — the actual v6 production code
// path used by `dispatchSAViaClientGo`. We don't expect List() to
// succeed (no real K8s API on the test server), but we DO expect the
// failure mode NOT to be a TLS verification error. Any client-go error
// that surfaces means the TLS handshake completed — which is the v6
// regression catcher.
func TestPathBSADispatch_DynamicClientHandshakeOK(t *testing.T) {
	caPEM, _, _, srvCert := generateTLSFixture(t)

	// Server pretends to be an apiserver: respond to discovery with
	// minimal valid shape so dynamic.Client can move past discovery.
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`))
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"namespaces","singularName":"","namespaced":false,"kind":"Namespace","verbs":["get","list"]}]}`))
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIGroupList","groups":[]}`))
	})
	mux.HandleFunc("/api/v1/namespaces", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NamespaceList","apiVersion":"v1","metadata":{"resourceVersion":"1"},"items":[]}`))
	})

	srv := startTestTLSServer(t, srvCert, mux)

	rc := &rest.Config{
		Host:        srv.URL,
		BearerToken: "test-bearer-token",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}

	cli, err := dynamic.NewClient(rc)
	if err != nil {
		t.Fatalf("dynamic.NewClient: %v", err)
	}

	// Drive a list call. Either it succeeds (test server returns empty
	// NamespaceList) or it fails with a non-TLS error.
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	_, listErr := cli.List(context.Background(), dynamic.Options{GVR: gvr})
	if listErr != nil {
		errStr := strings.ToLower(listErr.Error())
		if strings.Contains(errStr, "x509") || strings.Contains(errStr, "tls: failed to verify") {
			t.Fatalf("v6 Path B BROKEN: dynamic.Client.List failed TLS handshake: %v", listErr)
		}
		// Anything that's not a TLS error proves the handshake
		// completed and CA was loaded correctly.
		// API errors (e.g. 404, 500) are fine; we only catch TLS.
		var statusErr *apierrors.StatusError
		if errors.As(listErr, &statusErr) {
			t.Logf("v6 Path B handshake OK; got expected non-TLS apiserver-style error: code=%d message=%s", statusErr.ErrStatus.Code, statusErr.ErrStatus.Message)
			return
		}
		t.Logf("v6 Path B handshake OK; got non-TLS error (acceptable): %v", listErr)
		return
	}
	t.Logf("v6 Path B succeeded end-to-end: dynamic.Client.List returned without TLS error")
}

// TestPathBSADispatch_NoCAFails asserts the negative path: removing
// CAData causes the TLS handshake to fail. This guards against an
// accidental `Insecure: true` regression masking missing CA wiring.
func TestPathBSADispatch_NoCAFails(t *testing.T) {
	_, _, _, srvCert := generateTLSFixture(t)

	srv := startTestTLSServer(t, srvCert, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rc := &rest.Config{
		Host:        srv.URL,
		BearerToken: "tok",
		// No CAData → must fail (and must NOT be Insecure).
		TLSClientConfig: rest.TLSClientConfig{},
	}

	cli, err := rest.HTTPClientFor(rc)
	if err != nil {
		// Some Go versions reject this rest.Config at construction;
		// either way the contract is "no implicit insecure".
		return
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
	resp, err := cli.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("BUG: missing CAData should not allow successful HTTPS handshake against self-signed cert (security regression)")
	}
	t.Logf("no-CA negative case OK: %v", err)
}

// _ ensures metav1 is referenced (avoids unused import in scenarios
// where the dynamic-client list shape changes). metav1 is the package
// dynamic.Client.List uses to decode list options.
var _ = metav1.ListOptions{}
