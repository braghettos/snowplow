// internal_dispatch_tls_test.go — 0.30.104 falsifier.
//
// Reproduces the 0.30.103 Phase-1 TLS-CA regression and proves the
// 0.30.104 fix. The bug: Phase 1's SA-credentialed walk dispatches its
// inner K8s api[] calls (the `/api/v1/namespaces` LIST) through plumbing's
// httpcall.Do, which builds the HTTP client from the plumbing Endpoint
// shape. plumbing's transport.go `tlsConfigFor` applies a custom CA pool
// ONLY inside the `HasCertAuth()` branch — a TOKEN-auth endpoint (exactly
// the snowplow SA endpoint) returns at the `!ep.HasCertAuth()` early-exit
// and its CertificateAuthorityData is NEVER installed into RootCAs. The
// client then verifies against the system root store, which does not
// contain the cluster's self-signed CA:
//
//	tls: failed to verify certificate: x509: certificate signed by
//	unknown authority
//
// 0.30.104 fix: when the context carries an internal-dispatch *rest.Config
// (cache.WithInternalRESTConfig — Phase 1's SA walk attaches the
// rest.InClusterConfig() config, which carries the cluster CA verbatim),
// the api-stage K8s GET/LIST dispatch routes through a client-go REST
// client built from that *rest.Config instead of plumbing's httpcall.Do.
// client-go's transport installs the CA correctly.
//
// CRITICAL — this unit test is necessary but NOT sufficient. It runs
// against a httptest TLS server with a synthetic CA; it structurally
// cannot exercise the real cluster CA or a real apiserver TLS handshake.
// The two prior Phase-1-SA fixes (0.30.102 base64, 0.30.103) both passed
// unit tests and failed on-cluster. 0.30.104 is validated on-cluster.

package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/http/response"
	"github.com/krateoplatformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

// newTLSAPIServer starts an httptest TLS server that answers a
// /api/v1/namespaces LIST with a minimal apiserver-shaped envelope. Its
// auto-generated certificate is signed by a CA that is NOT in the system
// root store — the same trust posture as a real cluster's self-signed
// apiserver CA. Returns the server and its CA PEM bytes.
//
// The LIST body carries TWO real items. This is load-bearing for the
// fix-path test: client-go parses the response into an
// *unstructured.UnstructuredList whose item objects live in the typed
// `Items` field — `list.Object` does NOT carry an `items` key. A fix
// that marshals `list.Object` would emit an items-less envelope; the
// fix-path test asserts the served bytes contain BOTH item names so an
// items-less envelope FAILS the test (the bug found on the first
// 0.30.104 on-cluster smoke check, where `.namespaces.items` was null
// and Phase 1 discovered no composition GVR).
const fixtureNS1 = "krateo-system"
const fixtureNS2 = "demo-system"

func newTLSAPIServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"apiVersion":"v1","kind":"NamespaceList","metadata":{"resourceVersion":"1"},"items":[`+
			`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"`+fixtureNS1+`"}},`+
			`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"`+fixtureNS2+`"}}]}`)
	}))
	t.Cleanup(srv.Close)

	// httptest's TLS server certificate is self-signed; srv.Certificate()
	// IS that cert and acts as its own CA for our purposes.
	caPEM := pemEncodeCert(srv.Certificate())
	return srv, caPEM
}

func pemEncodeCert(cert *x509.Certificate) []byte {
	// PEM block: "-----BEGIN CERTIFICATE-----\n<base64>\n-----END...".
	const hdr = "-----BEGIN CERTIFICATE-----\n"
	const ftr = "\n-----END CERTIFICATE-----\n"
	var sb strings.Builder
	sb.WriteString(hdr)
	enc := stdBase64WrapPEM(cert.Raw)
	sb.WriteString(enc)
	sb.WriteString(ftr)
	return []byte(sb.String())
}

// stdBase64WrapPEM mirrors encoding/pem's 64-col line wrapping.
func stdBase64WrapPEM(der []byte) string {
	const lineLen = 64
	b64 := base64Std(der)
	var sb strings.Builder
	for i := 0; i < len(b64); i += lineLen {
		end := i + lineLen
		if end > len(b64) {
			end = len(b64)
		}
		sb.WriteString(b64[i:end])
		if end < len(b64) {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

func base64Std(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out strings.Builder
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n |= uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out.WriteByte(alphabet[(n>>18)&0x3F])
		out.WriteByte(alphabet[(n>>12)&0x3F])
		if rem > 1 {
			out.WriteByte(alphabet[(n>>6)&0x3F])
		} else {
			out.WriteByte('=')
		}
		if rem > 2 {
			out.WriteByte(alphabet[n&0x3F])
		} else {
			out.WriteByte('=')
		}
	}
	return out.String()
}

// TestPlumbingHttpcall_TokenAuthEndpoint_DropsCA is the NEGATIVE CONTROL.
// It reproduces the 0.30.103 bug directly: a token-auth Endpoint that
// carries the cluster CA, dispatched through plumbing's httpcall.Do, gets
// an HTTP client that does NOT trust that CA — the TLS handshake fails
// with "certificate signed by unknown authority".
//
// If a future plumbing bump fixes tlsConfigFor to honour HasCA() for
// token-auth endpoints, this test FAILS — flagging that the 0.30.104
// snowplow-side workaround can be removed. That is the intended falsifier
// behaviour: the test asserts the bug it was written to work around.
func TestPlumbingHttpcall_TokenAuthEndpoint_DropsCA(t *testing.T) {
	srv, caPEM := newTLSAPIServer(t)

	// A token-auth endpoint carrying the cluster CA — the exact shape
	// dynamic.ServiceAccountEndpoint() produces (raw-PEM CA, bearer token,
	// no client cert).
	ep := &endpoints.Endpoint{
		ServerURL:                srv.URL,
		Token:                    "fake-sa-jwt",
		CertificateAuthorityData: string(caPEM),
	}
	if ep.HasCertAuth() {
		t.Fatal("precondition: SA endpoint must NOT be cert-auth")
	}
	if !ep.HasCA() {
		t.Fatal("precondition: SA endpoint must carry a CA")
	}

	res := httpcall.Do(context.Background(), httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: "/api/v1/namespaces"},
		Endpoint:    ep,
	})

	if res.Status != response.StatusFailure {
		t.Fatalf("NEGATIVE CONTROL BROKEN: expected plumbing httpcall.Do to "+
			"fail TLS verification for a token-auth endpoint (the 0.30.103 "+
			"bug), got status=%v message=%q. If plumbing fixed tlsConfigFor "+
			"to honour HasCA() for token-auth endpoints, the 0.30.104 "+
			"snowplow-side workaround can be removed.", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "x509") &&
		!strings.Contains(res.Message, "certificate") {
		t.Fatalf("NEGATIVE CONTROL: expected a TLS/x509 verification error, "+
			"got %q", res.Message)
	}
	t.Logf("negative control confirmed — plumbing httpcall.Do drops the CA "+
		"for token-auth endpoints: %s", res.Message)
}

// TestInternalRESTConfigDispatch_TrustsClusterCA proves the 0.30.104 fix:
// a *rest.Config carrying the cluster CA (the shape rest.InClusterConfig()
// returns) dispatches the same /api/v1/namespaces LIST and SUCCEEDS — the
// client-go transport installs the CA into RootCAs correctly.
//
// It ALSO guards the LIST-envelope-shape bug found on the first 0.30.104
// on-cluster smoke check: the served bytes MUST carry an `items` array
// containing every namespace. client-go parses the apiserver response
// into an *unstructured.UnstructuredList whose item objects live in the
// typed `Items` field — `list.Object` has NO `items` key. Marshalling
// `list.Object` (the original 0.30.104 code) yields an items-less
// envelope; the walk's iterator filter `[.namespaces.items[] | ...]`
// then evaluates against a null `items` and Phase 1 discovers no
// composition GVR. The fix marshals `list.UnstructuredContent()`, which
// folds Items back into an `items` array. This test FAILS against the
// items-less marshal.
//
// This exercises dispatchViaInternalRESTConfig — the api-stage sibling of
// dispatchViaInformer — which the 0.30.104 fix wires ahead of
// httpcall.Do whenever cache.WithInternalRESTConfig is on the context.
func TestInternalRESTConfigDispatch_TrustsClusterCA(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)
	srv, caPEM := newTLSAPIServer(t)

	rc := &rest.Config{
		Host:        srv.URL,
		BearerToken: "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}
	ctx := cache.WithInternalRESTConfig(context.Background(), rc)

	raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: "/api/v1/namespaces"},
	})
	if err != nil {
		t.Fatalf("FIX BROKEN: dispatch via in-cluster *rest.Config failed the "+
			"TLS handshake against a CA it carries verbatim: %v", err)
	}
	if !served {
		t.Fatal("FIX BROKEN: expected the apiserver-path GET to be served via " +
			"the internal *rest.Config dispatcher")
	}
	if !strings.Contains(string(raw), "NamespaceList") {
		t.Fatalf("FIX BROKEN: served body is not the apiserver LIST envelope: %q",
			string(raw))
	}

	// The LIST-envelope-shape guard: the served bytes MUST contain a
	// non-null `items` array with every namespace. An items-less
	// envelope (json.Marshal(list.Object)) fails here.
	var envelope struct {
		Kind  string `json:"kind"`
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if uErr := json.Unmarshal(raw, &envelope); uErr != nil {
		t.Fatalf("FIX BROKEN: served bytes are not valid JSON: %v", uErr)
	}
	if len(envelope.Items) != 2 {
		t.Fatalf("LIST-ENVELOPE BUG: served LIST envelope must carry the 2 "+
			"namespace items in an `items` array — got %d. The 0.30.104 "+
			"first-smoke-check bug: marshalling list.Object drops items "+
			"(items live in UnstructuredList.Items, not list.Object); the "+
			"walk's `.namespaces.items[]` iterator then sees null and "+
			"Phase 1 discovers no composition GVR. served bytes: %q",
			len(envelope.Items), string(raw))
	}
	got := map[string]bool{}
	for _, it := range envelope.Items {
		got[it.Metadata.Name] = true
	}
	if !got[fixtureNS1] || !got[fixtureNS2] {
		t.Fatalf("LIST-ENVELOPE BUG: served `items` is missing a namespace — "+
			"want %q + %q, got %v", fixtureNS1, fixtureNS2, got)
	}
	t.Logf("fix confirmed — client-go transport from rest.Config{CAData} "+
		"trusts the cluster CA AND the served LIST envelope carries all "+
		"%d items: %d bytes served", len(envelope.Items), len(raw))
}

// TestInternalRESTConfigDispatch_NoConfigFallsThrough confirms the
// behaviour-neutral invariant: with NO internal *rest.Config on the
// context (every ordinary per-user request), dispatchViaInternalRESTConfig
// returns served=false so the call takes the unchanged httpcall.Do path.
func TestInternalRESTConfigDispatch_NoConfigFallsThrough(t *testing.T) {
	raw, served, err := dispatchViaInternalRESTConfig(context.Background(),
		httpcall.RequestOptions{
			RequestInfo: httpcall.RequestInfo{Path: "/api/v1/namespaces"},
		})
	if err != nil {
		t.Fatalf("expected no error on the no-internal-config fall-through, got %v", err)
	}
	if served {
		t.Fatal("BEHAVIOUR-NEUTRAL VIOLATED: dispatchViaInternalRESTConfig must " +
			"return served=false when no internal *rest.Config is on the context " +
			"— ordinary per-user requests must take the unchanged httpcall.Do path")
	}
	if raw != nil {
		t.Fatalf("expected nil bytes on fall-through, got %d bytes", len(raw))
	}
}

// TestInternalRESTConfigDispatch_NonAPIServerPathFallsThrough confirms an
// external / non-apiserver path is NOT routed through the internal
// *rest.Config dispatcher even when the config is present — the internal
// dispatcher only owns apiserver-shaped GVR paths.
func TestInternalRESTConfigDispatch_NonAPIServerPathFallsThrough(t *testing.T) {
	rc := &rest.Config{Host: "https://kubernetes.default.svc"}
	ctx := cache.WithInternalRESTConfig(context.Background(), rc)

	_, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{Path: "https://example.com/external"},
	})
	if err != nil {
		t.Fatalf("expected no error for a non-apiserver path, got %v", err)
	}
	if served {
		t.Fatal("expected a non-apiserver path to fall through to httpcall.Do")
	}
}
