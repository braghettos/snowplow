package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// TestSnowplowEndpointFromConfig — §9.4 (6 cases).
//
// Cases per spec:
//   1. nil cfg → error.
//   2. BearerTokenFile + CAFile → token + CA loaded from disk.
//   3. BearerToken literal → used directly.
//   4. CAData preferred over CAFile.
//   5. No token at all → error.
//   6. Unreadable token file → error wrapped.
//
// v5 — D1 fix introduced an env.TestMode() gate on ServerURL pinning. To
// preserve the original cases' semantics (they assert ep.ServerURL == rc.Host),
// each case enables TestMode via t.Setenv. The production-path (TestMode=false)
// pinning behavior is exercised separately in snowplow_endpoint_tls_test.go.
func TestSnowplowEndpointFromConfig(t *testing.T) {
	t.Setenv("TEST_MODE", "true")
	t.Run("NilConfig", func(t *testing.T) {
		ep, err := SnowplowEndpointFromConfig(nil)
		if err == nil {
			t.Fatal("expected error for nil config")
		}
		if ep != nil {
			t.Fatalf("expected nil endpoint, got %+v", ep)
		}
	})

	t.Run("BearerTokenFile_CAFile", func(t *testing.T) {
		dir := t.TempDir()
		tokFile := filepath.Join(dir, "token")
		caFile := filepath.Join(dir, "ca.crt")
		if err := os.WriteFile(tokFile, []byte("file-token-value\n"), 0o600); err != nil {
			t.Fatalf("write token: %v", err)
		}
		caPEM := "-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"
		if err := os.WriteFile(caFile, []byte(caPEM), 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
		rc := &rest.Config{
			Host:            "https://kubernetes.default.svc",
			BearerTokenFile: tokFile,
			TLSClientConfig: rest.TLSClientConfig{CAFile: caFile},
		}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.Token != "file-token-value" { // strip-trimmed
			t.Errorf("token=%q want %q", ep.Token, "file-token-value")
		}
		if ep.CertificateAuthorityData != caPEM {
			t.Errorf("ca mismatch:\n got %q\nwant %q", ep.CertificateAuthorityData, caPEM)
		}
		if ep.ServerURL != rc.Host {
			t.Errorf("server url mismatch")
		}
	})

	t.Run("BearerTokenLiteral", func(t *testing.T) {
		rc := &rest.Config{
			Host:        "https://k8s",
			BearerToken: "literal-token",
		}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.Token != "literal-token" {
			t.Errorf("token=%q", ep.Token)
		}
		if ep.CertificateAuthorityData != "" {
			t.Errorf("expected empty CA, got %q", ep.CertificateAuthorityData)
		}
	})

	t.Run("CADataPreferredOverCAFile", func(t *testing.T) {
		dir := t.TempDir()
		caFile := filepath.Join(dir, "ca.crt")
		if err := os.WriteFile(caFile, []byte("FILE-CONTENTS"), 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
		rc := &rest.Config{
			Host:        "https://k8s",
			BearerToken: "tok",
			TLSClientConfig: rest.TLSClientConfig{
				CAData: []byte("INLINE-DATA"),
				CAFile: caFile,
			},
		}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.CertificateAuthorityData != "INLINE-DATA" {
			t.Errorf("expected CAData prevail, got %q", ep.CertificateAuthorityData)
		}
	})

	t.Run("NoTokenError", func(t *testing.T) {
		rc := &rest.Config{Host: "https://k8s"}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err == nil {
			t.Fatal("expected error for no token")
		}
		if ep != nil {
			t.Fatalf("expected nil endpoint, got %+v", ep)
		}
	})

	t.Run("UnreadableTokenFileError", func(t *testing.T) {
		rc := &rest.Config{
			Host:            "https://k8s",
			BearerTokenFile: "/nonexistent/path/to/token-" + t.Name(),
		}
		ep, err := SnowplowEndpointFromConfig(rc)
		if err == nil {
			t.Fatal("expected error for unreadable token file")
		}
		if ep != nil {
			t.Fatalf("expected nil endpoint, got %+v", ep)
		}
		if !strings.Contains(err.Error(), "read SA token") {
			t.Errorf("error should mention 'read SA token', got %q", err.Error())
		}
	})
}
