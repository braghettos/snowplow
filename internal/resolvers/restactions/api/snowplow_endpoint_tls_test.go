package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/env"
	"k8s.io/client-go/rest"
)

// TestSnowplowEndpointTLS — Q-RBAC-DECOUPLE C(d) v5 — D1 fix
// (audit 2026-05-05).
//
// The pre-v5 production code path copied rc.Host literally into
// ep.ServerURL. On GKE Autopilot, rest.InClusterConfig sets
// rc.Host = "https://<cluster-IP>:443" (e.g. https://34.118.224.1:443).
// The SA ca.crt SAN list contains "kubernetes", "kubernetes.default",
// "kubernetes.default.svc", "kubernetes.default.svc.cluster.local" but
// no IP SAN — every TLS handshake against the cluster IP fails x509.
//
// v5 pins ServerURL to https://kubernetes.default.svc unless TestMode is
// on. This test:
//
//  1. Stands up an httptest TLS server bound to a self-signed cert whose
//     SAN list mimics the real SA ca.crt: includes the
//     `kubernetes.default.svc` hostname, no IP SAN.
//  2. Constructs a *rest.Config with rc.Host = "https://34.118.224.1:443"
//     (production-shape — deliberately NOT in the cert's SAN).
//  3. With TestMode=false, asserts SnowplowEndpointFromConfig pins
//     ep.ServerURL to https://kubernetes.default.svc, AND a real HTTPS
//     roundtrip against ep.ServerURL succeeds (DNS overridden via
//     custom DialContext to point at the test server).
//  4. With TestMode=true, asserts the pin is bypassed (so the existing
//     6-case unit suite at snowplow_endpoint_test.go is preserved).
//  5. Exercises a parameterized set of rc.Host values to prove the pin
//     is unconditional in production mode.
//  6. Re-asserts the NoTokenError regression — pinning must not affect
//     the failure path.
//
// CRITICAL CONTRACT: case TLS_SANMismatchHost_Pinned_HandshakeOK MUST
// FAIL on the v4 baseline (where rc.Host is copied literally and TLS
// then attempts to verify against the cluster IP, which is absent from
// the SAN list). It PASSES on v5.
func TestSnowplowEndpointTLS(t *testing.T) {
	// Self-signed cert with SANs mimicking the real SA ca.crt: hostname
	// SANs only, NO IP SANs. The test server binds to 127.0.0.1; we
	// override DNS for both the pinned hostname AND the production-shape
	// rc.Host so they resolve to the test server's actual port.
	caPEM, certPEM, keyPEM := generateTestCA(t, []string{
		"kubernetes",
		"kubernetes.default",
		"kubernetes.default.svc",
		"kubernetes.default.svc.cluster.local",
	}, nil)

	// httptest.NewUnstartedServer + manual TLS config so we control the
	// cert (httptest.NewTLSServer uses its own auto-generated cert).
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"host":"` + r.Host + `"}`))
	}))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	srvAddr := srv.Listener.Addr().String() // 127.0.0.1:NNNN

	// httpClientWithDNSOverride builds an http.Client that:
	//   - rewrites DialContext for the given host:port pairs to srvAddr
	//   - trusts the test CA (NOT the system roots)
	//   - performs full TLS verification (no InsecureSkipVerify)
	httpClientWithDNSOverride := func(t *testing.T, overrides map[string]string) *http.Client {
		t.Helper()
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			t.Fatalf("failed to add test CA to pool")
		}
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
				// MinVersion explicit to silence linters; the test
				// server's negotiated version doesn't affect SAN check.
				MinVersion: tls.VersionTLS12,
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if rewrite, ok := overrides[addr]; ok {
					return dialer.DialContext(ctx, network, rewrite)
				}
				return dialer.DialContext(ctx, network, addr)
			},
		}
		return &http.Client{Transport: tr, Timeout: 10 * time.Second}
	}

	t.Run("TLS_SANMismatchHost_Pinned_HandshakeOK", func(t *testing.T) {
		// Production mode: pin must be active.
		env.SetTestMode(false)
		t.Cleanup(func() { env.SetTestMode(false) })

		caFile := writeTempFile(t, caPEM)
		tokFile := writeTempFile(t, []byte("sa-token-value"))

		rc := &rest.Config{
			// Production-shape: cluster IP, NOT in the cert SAN list.
			Host:            "https://34.118.224.1:443",
			BearerTokenFile: tokFile,
			TLSClientConfig: rest.TLSClientConfig{CAFile: caFile},
		}

		ep, err := SnowplowEndpointFromConfig(rc)
		if err != nil {
			t.Fatalf("SnowplowEndpointFromConfig: %v", err)
		}

		// (a) ep.ServerURL is pinned to the literal hostname, NOT rc.Host.
		// On v4 (no pin) this assertion FAILS — that's the catcher.
		const want = "https://kubernetes.default.svc"
		if ep.ServerURL != want {
			t.Fatalf("ep.ServerURL=%q want %q (v4-baseline failure indicates the D1 pin is missing)",
				ep.ServerURL, want)
		}

		// (b) A real HTTPS roundtrip against ep.ServerURL succeeds:
		//   - DNS override sends kubernetes.default.svc:443 → srvAddr
		//   - TLS verifies against the cert's SAN list (matches "kubernetes.default.svc")
		client := httpClientWithDNSOverride(t, map[string]string{
			"kubernetes.default.svc:443": srvAddr,
		})
		resp, err := client.Get(ep.ServerURL + "/")
		if err != nil {
			t.Fatalf("HTTPS roundtrip against pinned host failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status=%d want 200", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), `"ok":true`) {
			t.Fatalf("body=%q does not contain ok marker", string(body))
		}
	})

	t.Run("TLS_V4Baseline_RcHostHandshakeFails", func(t *testing.T) {
		// Negative-baseline assertion: confirm that if the pin were
		// REMOVED (we simulate by talking directly to rc.Host with a
		// real client), TLS handshake would fail x509.
		//
		// This proves the v5 pin is load-bearing — without it, the same
		// roundtrip against the production-shape host fails.
		client := httpClientWithDNSOverride(t, map[string]string{
			"34.118.224.1:443": srvAddr,
		})
		resp, err := client.Get("https://34.118.224.1:443/")
		if err == nil {
			resp.Body.Close()
			t.Fatalf("expected TLS handshake failure against 34.118.224.1 (no IP SAN); got success")
		}
		// Best-effort error-message check (Go's x509 errors may vary by
		// version; we accept several forms).
		errStr := err.Error()
		// On Go 1.21+ this is "tls: failed to verify certificate" with
		// inner "x509: cannot validate certificate for 34.118.224.1
		// because it doesn't contain any IP SANs"; on older versions it
		// may be just "x509: ..."; on some macOS toolchains it surfaces
		// as the wrapped form. Accept any.
		if !strings.Contains(errStr, "x509") && !strings.Contains(errStr, "tls:") {
			t.Fatalf("expected x509/tls verify error; got: %v", err)
		}
		t.Logf("v4 baseline (rc.Host raw) TLS error confirms pin is load-bearing: %v", err)
	})

	t.Run("TLS_TestModeRespected", func(t *testing.T) {
		// With TestMode=true, the pin must be bypassed so existing test
		// fixtures keep asserting against rc.Host. This preserves the
		// 6-case suite at snowplow_endpoint_test.go.
		env.SetTestMode(true)
		t.Cleanup(func() { env.SetTestMode(false) })

		caFile := writeTempFile(t, caPEM)
		tokFile := writeTempFile(t, []byte("tok"))
		rc := &rest.Config{
			Host:            "https://34.118.224.1:443",
			BearerTokenFile: tokFile,
			TLSClientConfig: rest.TLSClientConfig{CAFile: caFile},
		}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err != nil {
			t.Fatalf("SnowplowEndpointFromConfig: %v", err)
		}
		if ep.ServerURL != rc.Host {
			t.Fatalf("TestMode=true: ep.ServerURL=%q want rc.Host=%q (gate is broken)",
				ep.ServerURL, rc.Host)
		}
	})

	t.Run("TLS_ProductionMode_AlwaysPinned", func(t *testing.T) {
		env.SetTestMode(false)
		t.Cleanup(func() { env.SetTestMode(false) })

		hosts := []string{
			"https://kubernetes.default.svc:443",
			"https://10.0.0.1:443",
			"https://34.118.224.1:443",
			"https://k8s-internal.example.com:443",
		}
		for _, h := range hosts {
			h := h
			t.Run(h, func(t *testing.T) {
				rc := &rest.Config{
					Host:            h,
					BearerToken:     "tok",
					TLSClientConfig: rest.TLSClientConfig{Insecure: false},
				}
				ep, err := SnowplowEndpointFromConfig(rc)
				if err != nil {
					t.Fatalf("SnowplowEndpointFromConfig: %v", err)
				}
				const want = "https://kubernetes.default.svc"
				if ep.ServerURL != want {
					t.Fatalf("rc.Host=%q ep.ServerURL=%q want %q",
						h, ep.ServerURL, want)
				}
			})
		}
	})

	t.Run("TLS_NoToken_StillFails", func(t *testing.T) {
		// Regression: NoTokenError case must continue to error
		// regardless of TestMode / pinning.
		env.SetTestMode(false)
		t.Cleanup(func() { env.SetTestMode(false) })

		rc := &rest.Config{Host: "https://34.118.224.1:443"}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err == nil {
			t.Fatalf("expected NoTokenError; got ep=%+v", ep)
		}
		if ep != nil {
			t.Fatalf("expected nil endpoint on error; got %+v", ep)
		}
	})
}

// generateTestCA generates a self-signed certificate authority and a
// matching server cert valid for the given DNS SAN names and IP SANs.
// Returns (caPEM, certPEM, keyPEM).
//
// The CA self-signs the server cert so a single PEM blob (caPEM == the
// cert's signer) can be used both as the chain root for verification AND
// as the server's presented cert. This keeps the test fixture small.
func generateTestCA(t *testing.T, dnsNames []string, ipSANs []net.IP) (caPEM, certPEM, keyPEM []byte) {
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
		DNSNames:              dnsNames,
		IPAddresses:           ipSANs,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	// Marshal the EC key as PKCS#8 so tls.X509KeyPair can read it via the
	// generic "PRIVATE KEY" header.
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	// CA == self-signed cert (single-cert chain).
	caPEM = certPEM
	return
}

// writeTempFile writes data to a t.TempDir() file and returns the path.
func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
