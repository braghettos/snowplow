// phase1_credential_real_test.go — 0.30.103 credential-real falsifier at
// the Phase 1 WALK level.
//
// WHY THIS FILE EXISTS (the key 0.30.102 lesson): phase1_walk_test.go's
// falsifiers PASSED while the "illegal base64 data" bug shipped, because
// their resolver is a STUB that only calls EnsureResourceType — it never
// builds a real SA kube client. The bug lives entirely in the
// SA-credential construction, which a stub resolver skips.
//
// This file drives phase1WarmupWith with a resolver that performs the
// REAL credential step resolveRoutesLoaderRoot performs in production:
//   - it attaches the SA *rest.Config to the context with
//     cache.WithInternalRESTConfig (the 0.30.103 wiring), and
//   - it builds the SA client config with cache.ClientConfigFor — the
//     exact call objects.Get / resourcesrefs.Resolve now make.
// Only if that real client build SUCCEEDS does the resolver register the
// navigated GVR informer — modeling the production contract that an
// informer registers as a side effect of a SUCCESSFUL resolve.
//
// Credentials are real-shaped: a raw JWT bearer token and a raw PEM CA.
//
// NEGATIVE CONTROL: a resolver that takes the 0.30.102 path —
// kubeconfig.NewClientConfig on the SA endpoint directly — fails the
// client build, so roots_resolved==0 and the navigated GVR is NEVER
// registered. POSITIVE CONTROL: the 0.30.103 cache.ClientConfigFor path
// builds the client, roots_resolved>0, and the navigated GVR informer IS
// registered by Phase 1.

package dispatchers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/kubeconfig"
	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
)

const phase1RealSAToken = "eyJhbGciOiJSUzI1NiIsImtpZCI6InRlc3QifQ." +
	"eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQ6a3JhdGVvLXN5c3RlbTpzbm93cGxvdyJ9." +
	"c2lnbmF0dXJlLWJ5dGVz"

// phase1RealSACAPEM generates a real, parseable self-signed CA cert as a
// raw PEM string — the shape of the projected SA ca.crt file.
func phase1RealSACAPEM(t *testing.T) string {
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

// phase1RealSACreds returns the SA endpoint (raw-PEM CA, raw token — the
// in-cluster shape) and the SA *rest.Config (raw bearer token + raw-PEM
// CAData — the shape rest.InClusterConfig produces).
func phase1RealSACreds(t *testing.T) (endpoints.Endpoint, *rest.Config) {
	t.Helper()
	caPEM := phase1RealSACAPEM(t)
	ep := endpoints.Endpoint{
		ServerURL:                "https://kubernetes.default.svc",
		Token:                    phase1RealSAToken,
		CertificateAuthorityData: caPEM,
	}
	rc := &rest.Config{
		Host:        "https://kubernetes.default.svc",
		BearerToken: phase1RealSAToken,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: []byte(caPEM),
		},
	}
	return ep, rc
}

// TestPhase1_CredentialReal_FixResolvesRoot is the POSITIVE control.
//
// The resolver performs the real 0.30.103 credential step: it injects the
// SA *rest.Config via cache.WithInternalRESTConfig (exactly as
// resolveRoutesLoaderRoot does) and builds the SA client config with
// cache.ClientConfigFor. The build SUCCEEDS, so the resolver registers
// the navigated GVR informer.
//
// PASS: roots_resolved > 0 AND the navigated GVR informer is registered
// by Phase 1.
func TestPhase1_CredentialReal_FixResolvesRoot(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetAutoDiscoverGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetAutoDiscoverGroupsForTest)

	saEP, saRC := phase1RealSACreds(t)

	lister := func(ctx context.Context) ([]*unstructured.Unstructured, error) {
		return []*unstructured.Unstructured{routesLoaderCR("ns-a", "main")}, nil
	}

	resolved := 0
	resolver := func(ctx context.Context, root *unstructured.Unstructured) error {
		// The REAL 0.30.103 credential wiring resolveRoutesLoaderRoot does.
		rctx := cache.WithInternalRESTConfig(ctx, saRC)
		// The REAL call objects.Get / resourcesrefs.Resolve now make to
		// build the SA kube client. With the fix this returns the injected
		// SA *rest.Config; without it, it would route saEP through
		// kubeconfig.NewClientConfig and fail with "illegal base64 data".
		rc, err := cache.ClientConfigFor(rctx, saEP)
		if err != nil {
			return fmt.Errorf("phase1 SA client build failed: %w", err)
		}
		if rc.BearerToken != phase1RealSAToken {
			return fmt.Errorf("SA client lost its bearer token — unusable client")
		}
		if len(rc.TLSClientConfig.CAData) == 0 {
			return fmt.Errorf("SA client lost its CA — unusable client")
		}
		// A successful resolve registers the navigated GVR's informer
		// (production: lazyRegisterInnerCallPaths fires as the resolver
		// walks the inner-call paths).
		rw.EnsureResourceType(gvrReached)
		resolved++
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := phase1WarmupWith(ctx, rw, lister, resolver); err != nil {
		t.Fatalf("phase1WarmupWith returned error with the 0.30.103 fix: %v", err)
	}

	if resolved == 0 {
		t.Fatalf("credential-real falsifier FAIL: roots_resolved==0 — the Phase 1 walk could not "+
			"build a usable SA client; the 0.30.102 bug is not fixed")
	}
	if !rw.IsRegistered(gvrReached) {
		t.Fatalf("credential-real falsifier FAIL: a successful root resolve must register the "+
			"navigated GVR informer; %v missing", gvrReached)
	}
	if !cache.IsPhase1Done() {
		t.Fatalf("Phase1Done must be set after a successful credential-real Phase1Warmup")
	}
}

// TestPhase1_CredentialReal_UnfixedPathFailsRoot is the NEGATIVE control:
// it proves the 0.30.102 code path genuinely breaks Phase 1.
//
// The resolver here takes the UNFIXED path — kubeconfig.NewClientConfig
// on the SA endpoint directly (what objects.Get / resourcesrefs.Resolve
// did in 0.30.102). The SA's raw-PEM CA fails base64-decode, the client
// build fails, the resolve errors, and the navigated GVR informer is
// NEVER registered.
//
// PASS (the bug, reproduced at the walk level): roots_resolved==0, the
// navigated GVR informer is absent, and the failure carries the
// "illegal base64 data" signature.
func TestPhase1_CredentialReal_UnfixedPathFailsRoot(t *testing.T) {
	rw := phase1TestWatcher(t)
	cache.ResetPhase1DoneForTest()
	cache.ResetAutoDiscoverGroupsForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	t.Cleanup(cache.ResetAutoDiscoverGroupsForTest)

	saEP, _ := phase1RealSACreds(t)

	lister := func(ctx context.Context) ([]*unstructured.Unstructured, error) {
		return []*unstructured.Unstructured{routesLoaderCR("ns-a", "main")}, nil
	}

	resolved := 0
	var lastErr error
	resolver := func(ctx context.Context, root *unstructured.Unstructured) error {
		// THE 0.30.102 PATH: build the SA kube client straight through
		// kubeconfig.NewClientConfig — no WithInternalRESTConfig.
		_, err := kubeconfig.NewClientConfig(ctx, saEP)
		if err != nil {
			lastErr = err
			return fmt.Errorf("phase1 SA client build failed: %w", err)
		}
		rw.EnsureResourceType(gvrReached)
		resolved++
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// phase1WarmupWith returns the first walk error — expected non-nil.
	walkErr := phase1WarmupWith(ctx, rw, lister, resolver)
	if walkErr == nil {
		t.Fatalf("negative control FAIL: the unfixed kubeconfig path did not error — "+
			"the 0.30.102 bug is not reproduced at the walk level")
	}

	if resolved != 0 {
		t.Fatalf("negative control FAIL: roots_resolved=%d — the unfixed path must NOT resolve any root", resolved)
	}
	if rw.IsRegistered(gvrReached) {
		t.Fatalf("negative control FAIL: the navigated GVR informer was registered despite a "+
			"failed root resolve — Phase 1 must not warm it on the unfixed path")
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "illegal base64 data") {
		t.Fatalf("negative control: expected the reported 0.30.102 'illegal base64 data' "+
			"signature; got: %v", lastErr)
	}
	t.Logf("0.30.102 bug reproduced at the walk level: roots_resolved=0, navigated GVR unregistered, err=%v", lastErr)
}
