package httpcall

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/ptr"
)

// ── identity key ────────────────────────────────────────────────────────────

func TestEndpointKey_SameInputs_SameKey(t *testing.T) {
	a := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: "tok"}
	b := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: "tok"}
	if endpointKey(a) != endpointKey(b) {
		t.Fatalf("equal endpoints produced different keys")
	}
}

func TestEndpointKey_DifferentURL_DifferentKey(t *testing.T) {
	a := &endpoints.Endpoint{ServerURL: "https://api.example.com"}
	b := &endpoints.Endpoint{ServerURL: "https://api.other.com"}
	if endpointKey(a) == endpointKey(b) {
		t.Fatalf("different URLs produced equal keys")
	}
}

func TestEndpointKey_DifferentCert_DifferentKey(t *testing.T) {
	a := &endpoints.Endpoint{ServerURL: "https://api.example.com", CertificateAuthorityData: "AAAA"}
	b := &endpoints.Endpoint{ServerURL: "https://api.example.com", CertificateAuthorityData: "BBBB"}
	if endpointKey(a) == endpointKey(b) {
		t.Fatalf("different CA data produced equal keys")
	}
}

func TestEndpointKey_DifferentToken_DifferentKey(t *testing.T) {
	a := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: "alpha"}
	b := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: "beta"}
	if endpointKey(a) == endpointKey(b) {
		t.Fatalf("different tokens produced equal keys")
	}
}

func TestEndpointKey_BasicAuth_DifferentPassword_DifferentKey(t *testing.T) {
	a := &endpoints.Endpoint{ServerURL: "https://x", Username: "u", Password: "p1"}
	b := &endpoints.Endpoint{ServerURL: "https://x", Username: "u", Password: "p2"}
	if endpointKey(a) == endpointKey(b) {
		t.Fatalf("different passwords produced equal keys")
	}
}

func TestEndpointKey_TokenNotInPlaintext(t *testing.T) {
	secret := "super-secret-token-9F3kZq"
	ep := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: secret}
	k := endpointKey(ep)
	if strings.Contains(k, secret) {
		t.Fatalf("endpoint key contains plaintext token")
	}
	if strings.Contains(k, "secret") {
		t.Fatalf("endpoint key leaks part of plaintext token")
	}
}

func TestEndpointKey_BasicAuth_NotInPlaintext(t *testing.T) {
	user, pass := "alice", "hunter2"
	ep := &endpoints.Endpoint{ServerURL: "https://api.example.com", Username: user, Password: pass}
	k := endpointKey(ep)
	if strings.Contains(k, user) || strings.Contains(k, pass) {
		t.Fatalf("endpoint key contains plaintext credentials")
	}
}

func TestEndpointKey_AwsKey_NotInPlaintext(t *testing.T) {
	ep := &endpoints.Endpoint{
		ServerURL:    "https://s3.amazonaws.com",
		AwsAccessKey: "AKIAEXAMPLE12345",
		AwsSecretKey: "secretkey",
		AwsRegion:    "us-east-1",
		AwsService:   "s3",
	}
	k := endpointKey(ep)
	if strings.Contains(k, "AKIAEXAMPLE12345") || strings.Contains(k, "secretkey") {
		t.Fatalf("endpoint key contains plaintext AWS credentials")
	}
}

// ── pool reuse ──────────────────────────────────────────────────────────────

func TestPool_ReusesClientAcrossCalls(t *testing.T) {
	resetForTest()
	defer resetForTest()

	ep := &endpoints.Endpoint{ServerURL: "https://api.example.com", Token: "t"}

	c1, ok1 := clientFor(ep)
	c2, ok2 := clientFor(ep)
	if !ok1 || !ok2 {
		t.Fatalf("clientFor returned not-ok: %v %v", ok1, ok2)
	}
	if c1 != c2 {
		t.Fatalf("pool returned different *http.Client for the same endpoint")
	}
	if got := poolEntries(); got != 1 {
		t.Fatalf("expected 1 pool entry, got %d", got)
	}
}

func TestPool_DistinctEndpointsGetDistinctClients(t *testing.T) {
	resetForTest()
	defer resetForTest()

	a := &endpoints.Endpoint{ServerURL: "https://a.example.com"}
	b := &endpoints.Endpoint{ServerURL: "https://b.example.com"}

	ca, _ := clientFor(a)
	cb, _ := clientFor(b)
	if ca == cb {
		t.Fatalf("distinct endpoints produced same client")
	}
	if got := poolEntries(); got != 2 {
		t.Fatalf("expected 2 pool entries, got %d", got)
	}
}

// ── hard cap ─────────────────────────────────────────────────────────────────

func TestPool_HardCapBypassesPool(t *testing.T) {
	resetForTest()
	defer resetForTest()

	// Fill the pool to exactly poolHardCap.
	for i := 0; i < poolHardCap; i++ {
		ep := &endpoints.Endpoint{ServerURL: fmt.Sprintf("https://h%d.example.com", i)}
		_, ok := clientFor(ep)
		if !ok {
			t.Fatalf("clientFor unexpectedly bypassed at i=%d (cap=%d)", i, poolHardCap)
		}
	}
	if got := poolEntries(); got != int64(poolHardCap) {
		t.Fatalf("expected %d entries, got %d", poolHardCap, got)
	}

	// The (poolHardCap+1)th distinct endpoint must bypass — ok=false.
	overflow := &endpoints.Endpoint{ServerURL: "https://overflow.example.com"}
	if _, ok := clientFor(overflow); ok {
		t.Fatalf("overflow endpoint was admitted to pool; expected bypass")
	}
	if got := poolEntries(); got != int64(poolHardCap) {
		t.Fatalf("entry count changed on overflow: got %d", got)
	}
}

// ── kill switch ──────────────────────────────────────────────────────────────

func TestKillSwitch_DisablesPool(t *testing.T) {
	resetForTest()
	defer resetForTest()

	// Save and restore module-level disabled flag and getenv.
	origDisabled := disabled
	origGetenv := getenv
	t.Cleanup(func() {
		disabled = origDisabled
		getenv = origGetenv
	})

	disabled = true

	// Server tracks how many distinct *http.Conn show up. With pool off, two
	// calls go through plumbing's per-call client which uses fresh transports
	// each time. We only assert the request reached the server (200 OK) and
	// that the pool stayed empty.
	called := atomic.Int64{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep := &endpoints.Endpoint{ServerURL: srv.URL, Insecure: true}

	for i := 0; i < 3; i++ {
		_ = Do(context.Background(), RequestOptions{
			Endpoint: ep,
			RequestInfo: RequestInfo{
				Verb: ptr.To(http.MethodGet),
				Path: "/",
			},
			ResponseHandler: func(r io.ReadCloser) error {
				_, _ = io.Copy(io.Discard, r)
				return nil
			},
		})
	}

	if poolEntries() != 0 {
		t.Fatalf("pool was populated despite kill-switch: %d entries", poolEntries())
	}
	if called.Load() == 0 {
		t.Fatalf("server was never called; kill-switch path is broken")
	}
}

// ── integration: pool reuses transport across requests ──────────────────────

func TestPool_IntegrationReusesTransport(t *testing.T) {
	resetForTest()
	defer resetForTest()

	origDisabled := disabled
	t.Cleanup(func() { disabled = origDisabled })
	disabled = false

	// httptest.NewTLSServer with a self-signed cert; the endpoint must be
	// Insecure=true so plumbing's TLS config sets InsecureSkipVerify.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	// Sanity: TLS is configured.
	if srv.TLS == nil {
		t.Fatalf("expected TLS server")
	}
	_ = (*tls.Config)(nil) // touch tls import

	ep := &endpoints.Endpoint{ServerURL: srv.URL, Insecure: true}

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := Do(context.Background(), RequestOptions{
				Endpoint: ep,
				RequestInfo: RequestInfo{
					Verb: ptr.To(http.MethodGet),
					Path: "/",
				},
			})
			if st == nil {
				t.Errorf("nil response status")
			}
		}()
	}
	wg.Wait()

	if got := poolEntries(); got != 1 {
		t.Fatalf("expected exactly 1 pool entry across %d concurrent calls, got %d", n, got)
	}
}
