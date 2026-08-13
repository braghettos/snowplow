//go:build integration
// +build integration

package widgets

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
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/objects"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	crdPath      = "../../../crds"
	testdataPath = "../../../testdata"
)

func TestMain(m *testing.M) {
	xenv.SetTestMode(true)

	namespace = "demo-system"
	clusterName = "krateo"
	testenv = env.New()

	testenv.Setup(
		envfuncs.CreateCluster(kind.NewProvider(), clusterName),
		envfuncs.SetupCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		envfuncs.SetupCRDs(filepath.Join(testdataPath, "widgets"), "widgets.templates.krateo.io_buttons.yaml"),
		e2e.CreateNamespace(namespace),

		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			r, err := resources.New(cfg.Client().RESTConfig())
			if err != nil {
				return ctx, err
			}
			r.WithNamespace(namespace)

			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac.widgets.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac.restactions.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac.pods.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			// The asserted widget (button-with-resourcesrefstemplate) has
			// an apiRef RESTAction (cluster-namespaces) whose stage LISTs
			// /api/v1/namespaces cluster-wide, then a resourcesRefsTemplate
			// that iterates ${ .namespaces }. cyberjoker (group devs) must
			// be allowed to list namespaces or that stage is forbidden,
			// .namespaces is absent, and the iterator hard-fails ("query
			// .namespaces must return a JSON array"). rbac.namespaces.yaml
			// grants devs cluster-wide namespaces get/list — the sibling
			// restactions_test.go picks it up via its rbac*.yaml glob; this
			// package enumerates the rbac files individually and was simply
			// missing this one.
			err = decoder.ApplyWithManifestDir(ctx, r, testdataPath, "rbac.namespaces.yaml", []resources.CreateOption{})
			if err != nil {
				return ctx, err
			}

			// Wait for the freshly-installed Button CRD
			// (widgets.templates.krateo.io/v1beta1) to be discoverable before
			// any test proceeds. SetupCRDs registers the group/version, but
			// the controller-runtime client's RESTMapper does a full
			// ServerPreferredResources on first use; against a just-installed
			// CRD that discovery transiently returns "no matches ...
			// Resource=" — the apiserver lists the group before its resource
			// list is populated. ResourceListMatchN returns (false,nil) on a
			// List error (conditions.go), so it retries until the
			// group/version is fully established.
			//
			// Buttons is the package-distinguishing CRD this TestMain
			// installs (the asserted widget is a Button that the resolver
			// fetches via objects.Get(buttons …)); the sibling restactions
			// CRD is installed in the same SetupCRDs step and settles in
			// lockstep, and is exercised by the per-test apiRef resolve. There
			// is no typed Go Button/ButtonList in apis/ (the widgets group is
			// consumed unstructured throughout the resolver), so the
			// package-correct ObjectList is an *unstructured.UnstructuredList
			// stamped with the ButtonList GVK — listing it proves the widgets
			// group/version is discoverable without registering a typed
			// kind. We assert a minimum of 0 because no Button CRs exist yet
			// at TestMain time (the per-test Setup decodes button.*.yaml
			// later) — a List that succeeds at all proves discovery has
			// settled. This replaces the fixed time.Sleep race (the TODO
			// here) that left CI flaky and is exactly the discovery gate that
			// fixed TestResolveAPI.
			buttons := &unstructured.UnstructuredList{}
			buttons.SetGroupVersionKind(schema.GroupVersionKind{
				Group:   "widgets.templates.krateo.io",
				Version: "v1beta1",
				Kind:    "ButtonList",
			})
			if err := wait.For(
				conditions.New(r).ResourceListN(buttons, 0),
				wait.WithTimeout(60*time.Second),
				wait.WithInterval(time.Second),
			); err != nil {
				return ctx, fmt.Errorf("waiting for Button CRD discovery to settle: %w", err)
			}
			return ctx, nil
		},
	).Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		envfuncs.TeardownCRDs(filepath.Join(testdataPath, "widgets"), "widgets.templates.krateo.io_buttons.yaml"),
		envfuncs.DestroyCluster(clusterName),
		e2e.Coverage(),
	)

	os.Exit(testenv.Run(m))
}

func TestResolveWidgets(t *testing.T) {
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
				ctx, os.DirFS(filepath.Join(testdataPath, "widgets")), "button.*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		//Assess("Resolve Simple Widget", resolveWidget("button-sample")).
		//Assess("Resolve Widget with RESTAction reference", resolveWidget("button-with-api")).
		//Assess("Resolve Widget with Actions", resolveWidget("button-with-actions")).
		//Assess("Resolve Widget with API and Actions", resolveWidget("button-with-api-and-actions")).
		Assess("Resolve Widget with ResourcesRefsTemplate", resolveWidget("button-with-resourcesrefstemplate")).
		Feature()

	testenv.Test(t, f)
}

func resolveWidget(name string) func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r, err := resources.New(c.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}
		r.WithNamespace(namespace)
		apis.AddToScheme(r.GetScheme())

		res := objects.Get(ctx, v1.ObjectReference{
			Reference: v1.Reference{
				Name: name, Namespace: namespace,
			},
			Resource: "buttons", APIVersion: "widgets.templates.krateo.io/v1beta1",
		})
		if res.Err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to get object", slog.Any("err", res.Err))
			t.Fail()
		}

		obj, err := Resolve(ctx, ResolveOptions{
			RC: c.Client().RESTConfig(),
			// AuthnNS must point at the namespace holding the signed-up
			// user's <user>-clientconfig Secret (demo-system, created by
			// e2e.SignUp). Without it the apiRef RESTAction's per-user
			// endpoint resolves a Secret ref with an EMPTY namespace —
			// "an empty namespace may not be set when a resource name is
			// provided" — so the apiRef stage yields nothing, the
			// resourcesRefsTemplate iterator gets a non-array, and Resolve
			// returns a hard error. The sibling restactions_test.go passes
			// AuthnNS for exactly this reason.
			AuthnNS: namespace,
			In:      res.Unstructured,
		})
		if err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to resolve object", slog.Any("err", err))
			t.Fail()
		}

		s := serializer.NewSerializerWithOptions(serializer.DefaultMetaFactory,
			r.GetScheme(), r.GetScheme(),
			serializer.SerializerOptions{
				Yaml:   true,
				Pretty: true,
				Strict: false,
			})

		if err := s.Encode(obj, os.Stderr); err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to encode YAML", slog.Any("err", err))
			t.Fail()
		}

		return ctx
	}
}
