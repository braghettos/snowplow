//go:build integration
// +build integration

package objects

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/e2e"
	xenv "github.com/krateo-platformops/plumbing/env"
	"github.com/krateo-platformops/snowplow/apis"
	templatesv1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	serializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
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

const (
	crdPath      = "../../crds"
	testdataPath = "../../testdata"
)

func TestMain(m *testing.M) {
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

			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac*.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			// Wait for the freshly-installed RESTAction CRD
			// (templates.krateo.io/v1) to be discoverable before any test
			// proceeds. SetupCRDs registers the group/version, but the
			// controller-runtime client's RESTMapper does a full
			// ServerPreferredResources on first use; against a just-installed
			// CRD that discovery transiently returns
			// "templates.krateo.io/v1: no matches ... Resource=" — the
			// apiserver lists the group before its resource list is
			// populated. ResourceListN lists RESTActions; ResourceListMatchN
			// returns (false,nil) on a List error (conditions.go), so it
			// retries until the group/version is fully established. We assert
			// a minimum of 0 because no RESTAction CRs exist yet at TestMain
			// time (the per-test Setup decodes them later) — a List that
			// succeeds at all proves discovery has settled. This replaces the
			// fixed time.Sleep race (the TODO here) that left CI flaky and is
			// exactly the discovery gate that fixed TestResolveAPI.
			// AddToScheme so the typed RESTActionList is mappable by r's
			// client (the per-test Setup adds it too, but the wait runs here).
			if err := apis.AddToScheme(r.GetScheme()); err != nil {
				return ctx, err
			}
			if err := wait.For(
				conditions.New(r).ResourceListN(&templatesv1.RESTActionList{}, 0),
				wait.WithTimeout(60*time.Second),
				wait.WithInterval(time.Second),
			); err != nil {
				return ctx, fmt.Errorf("waiting for RESTAction CRD discovery to settle: %w", err)
			}
			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		envfuncs.DestroyCluster(clusterName),
		e2e.Coverage(),
	)

	os.Exit(testenv.Run(m))
}

func TestGet(t *testing.T) {
	const keyID = "test-kid"

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

	os.Setenv("DEBUG", "0")

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
			return ctx
		}).
		Assess("Get RESTAction", getRESTAction("typicode")).
		Feature()

	testenv.Test(t, f)
}

func getRESTAction(name string) func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r, err := resources.New(c.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}
		r.WithNamespace(namespace)
		apis.AddToScheme(r.GetScheme())

		res := Get(ctx, templatesv1.ObjectReference{
			Reference: templatesv1.Reference{
				Name:      name,
				Namespace: namespace,
			},
			Resource:   "restactions",
			APIVersion: "templates.krateo.io/v1",
		})
		if res.Err != nil {
			t.Fatal(res.Err)
		}

		s := serializer.NewSerializerWithOptions(serializer.DefaultMetaFactory,
			r.GetScheme(), r.GetScheme(),
			serializer.SerializerOptions{
				Yaml:   true,
				Pretty: true,
				Strict: false,
			})

		if err := s.Encode(res.Unstructured, os.Stdout); err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to encode YAML", slog.Any("err", err))
			t.Fail()
		}

		return ctx
	}
}
