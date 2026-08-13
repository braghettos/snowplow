//go:build integration
// +build integration

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/snowplow/apis"
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"

	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
	"sigs.k8s.io/e2e-framework/support/kind"
)

var (
	testenv     env.Environment
	clusterName string
	namespace   string
)

func TestMain(m *testing.M) {
	// This package lives at internal/resolvers/restactions/api — four
	// levels below the repo root — so the crds/ and testdata/ dirs are
	// ../../../../, NOT ../../../ (which resolves to the nonexistent
	// internal/crds + internal/testdata). With the wrong depth,
	// SetupCRDs + decoder glob a missing directory: fs.Glob returns zero
	// matches and a nil error, so the RESTAction CRD is never installed
	// and no fixtures are created — every later Get/List then fails with
	// "no matches for templates.krateo.io/v1". (The sibling restactions/
	// and widgets/ test packages are only three levels deep, which is
	// why ../../../ works for them but is wrong here.)
	const (
		crdPath      = "../../../../crds"
		testdataPath = "../../../../testdata"
	)

	xenv.SetTestMode(true)

	namespace = "demo-system"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.SetupCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		e2e.CreateNamespace(namespace),

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac.restactions.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			time.Sleep(2 * time.Second)
			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		envfuncs.DestroyCluster(clusterName),
	)

	os.Exit(testenv.Run(m))
}

func TestResolveAPI(t *testing.T) {
	const (
		// Four levels up to the repo root (see the depth note in TestMain).
		testdataPath = "../../../../testdata"
		keyID        = "test-kid"
	)

	// authn now signs with RS256; e2e.SignUp expects a PEM-encoded RSA private
	// key rather than a symmetric secret.
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating RSA key: %v", err)
	}
	signKeyPEM := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	}))

	os.Setenv("DEBUG", "1")

	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(e2e.SignUp(e2e.SignUpOptions{
			Username:   "cyberjoker",
			Groups:     []string{"devs"},
			Namespace:  namespace,
			JWTSignKey: signKeyPEM,
			JWTKeyID:   keyID,
		})).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {

			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fail()
			}

			apis.AddToScheme(r.GetScheme())

			r.WithNamespace(namespace)

			err = decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "restactions")), "*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}

			// Wait for the RESTAction CRD to be discoverable before any
			// Get below. SetupCRDs (TestMain) registers
			// templates.krateo.io/v1, but the controller-runtime client's
			// RESTMapper does a full ServerPreferredResources on first
			// use; against a freshly-installed CRD that discovery
			// transiently returns "templates.krateo.io/v1: no matches …
			// Resource=" — the apiserver lists the group before its
			// resource list is populated. A List that succeeds (the
			// condition retries on List error — conditions.go
			// ResourceListMatchN returns (false,nil) on List failure)
			// proves the group/version is fully established and the
			// objects just applied are visible. This replaces the
			// fixed time.Sleep race that left CI flaky (the TODO in
			// every TestMain) with an explicit discovery gate.
			if err := wait.For(
				conditions.New(r).ResourceListN(&v1.RESTActionList{}, 1),
				wait.WithTimeout(60*time.Second),
				wait.WithInterval(time.Second),
			); err != nil {
				t.Fatalf("waiting for RESTAction discovery/list to settle: %v", err)
			}
			return ctx
		}).
		Assess("Resolve API", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				t.Fatal(err)
			}
			r.WithNamespace(namespace)
			apis.AddToScheme(r.GetScheme())

			cr := v1.RESTAction{}
			// Discovery is warm (Setup waited for the RESTAction List), so
			// a Get failure here is a real error — make it fatal at its
			// own site. (Previously this was a non-fatal t.Fail() and the
			// leftover err was re-checked AFTER Resolve, which returns no
			// error — so a transient discovery failure on this Get
			// surfaced misleadingly as a Resolve failure at line 132.)
			if err := r.Get(ctx, "kube-get", namespace, &cr); err != nil {
				t.Fatalf("get RESTAction kube-get: %v", err)
			}

			res := Resolve(ctx, ResolveOptions{
				RC:      cfg.Client().RESTConfig(),
				AuthnNS: cfg.Namespace(),
				Items:   cr.Spec.API,
			})

			enc := json.NewEncoder(os.Stderr)
			enc.SetIndent("", "  ")
			if err := enc.Encode(res); err != nil {
				t.Fatal(err)
			}

			return ctx
		}).Feature()

	testenv.Test(t, f)
}
