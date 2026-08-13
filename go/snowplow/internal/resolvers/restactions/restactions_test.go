//go:build integration
// +build integration

package restactions

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

	corev1 "k8s.io/api/core/v1"
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
	// fixtureSrv is the suite-local recorded-response server (B hermetify).
	// Started in TestMain, closed at Finish; the per-test Setup rewrites the
	// github/httpbin/typicode endpointRef Secrets' server-url to fixtureSrv.URL()
	// so those three RAs resolve against recorded JSON, not live third-party hosts
	// (docs/flaky-integration-gate-fix-design-2026-07-02.md). httpfixture_test.go.
	fixtureSrv *fixtureServer
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

	// B (hermetify): start the recorded-response server before the suite; the
	// github/httpbin/typicode RAs resolve against it (Secret server-url rewritten
	// at Setup) so the gate never touches a live third-party host. Closed at Finish.
	fixtureSrv = newFixtureServer()

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
				conditions.New(r).ResourceListN(&v1.RESTActionList{}, 0),
				wait.WithTimeout(60*time.Second),
				wait.WithInterval(time.Second),
			); err != nil {
				return ctx, fmt.Errorf("waiting for RESTAction CRD discovery to settle: %w", err)
			}
			return ctx, nil
		},
	).Finish(
		func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
			// B (hermetify): stop the recorded-response server.
			if fixtureSrv != nil {
				fixtureSrv.Close()
			}
			return ctx, nil
		},
		envfuncs.DeleteNamespace(namespace),
		envfuncs.TeardownCRDs(crdPath, "templates.krateo.io_restactions.yaml"),
		envfuncs.DestroyCluster(clusterName),
		e2e.Coverage(),
	)

	os.Exit(testenv.Run(m))
}

func TestRESTAction(t *testing.T) {
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

			// B (hermetify): point the three formerly-live endpointRef Secrets at
			// the recorded-response server so github/httpbin/typicode resolve
			// deterministically against local JSON, never a live third-party host
			// (docs/flaky-integration-gate-fix-design-2026-07-02.md). The fixtures'
			// authored URLs (api.github.com / httpbin.org / jsonplaceholder) stay
			// human-readable defaults but are overwritten here before any resolve.
			// Done AFTER the manifest-apply so the Secrets exist to patch.
			for _, secretName := range []string{
				"github-endpoint", "httpbin-endpoint", "typicode-endpoint",
			} {
				var sec corev1.Secret
				if err := r.Get(ctx, secretName, namespace, &sec); err != nil {
					t.Fatalf("hermetify: get endpoint Secret %q: %v", secretName, err)
				}
				if sec.StringData == nil {
					sec.StringData = map[string]string{}
				}
				// stringData is write-only on apply; on a live object the value is
				// in Data (base64). Overwrite via StringData (apiserver re-encodes)
				// so the update is authoritative regardless of how it was created.
				sec.StringData["server-url"] = fixtureSrv.URL()
				if err := r.Update(ctx, &sec); err != nil {
					t.Fatalf("hermetify: rewrite server-url on Secret %q: %v", secretName, err)
				}
			}
			return ctx
		}).
		Assess("Resolve GitHub", resolveRESTAction("github")).
		Assess("Resolve HttpBin", resolveRESTAction("httpbin")).
		Assess("Resolve Typicode", resolveRESTAction("typicode")).
		Assess("Resolve Cluster PODs", resolveRESTAction("cluster-pods")).
		Assess("Resolve Kube Get", resolveRESTAction("kube-get")).
		Assess("Resolve Cluster Namespaces", resolveRESTAction("cluster-namespaces")).
		// B (hermetify) falsifier arm — decisive "zero live egress" proof. The
		// three formerly-live RAs dispatch a KNOWN number of api-steps to the
		// fixture server:
		//   typicode: /users (1) + /todos × first-3-users (3)           = 4
		//   github:   /runs (1)                                         = 1
		//   httpbin:  /get (1) + /post (1)                              = 2
		//                                                          total = 7
		// github fires ONLY its /runs collection step, NOT the /runs/{id}/timing
		// children: the resolver wraps each step's decoded body under pig[stepName]
		// before running that step's jq filter (handler.go jsonHandlerCore), so the
		// `all` step filter `.workflow_runs | map(…)` evaluates `.workflow_runs`
		// against the WRAPPER object `{all: {…}}` → null → the filter errors
		// non-fatally (logged, body left raw) → the dependent `jobs` iterator
		// `.all | sort_by(.created_at)` then runs over the raw github_runs OBJECT's
		// values → `.created_at` on a number → iterator yields nothing → 0 timing
		// dispatches. This is a PRE-EXISTING resolver semantic, identical whether
		// the step resolves against live github or the local fixture (the fixture
		// body is byte-shaped like github's real /runs response), so it is NOT a
		// hermetify artefact and NOT in scope to "fix" here. The honest full-
		// hermetic dispatch count is therefore 7, not the pre-flight projection of
		// 9 (which wrongly assumed the step filters resolve).
		// If those requests reached the LOCAL fixture server (not the internet),
		// its hit counter is >= 7. A live-dispatch regression (Secret URL not
		// overridden) leaves the counter at 0 → this arm FAILS. This is the
		// design's counter-based alternative to running under `unshare -n`
		// (kind needs docker egress, so full network isolation is impractical).
		Assess("Hermetic — all live dispatch served locally", func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			fixtureSrv.assertServedAtLeast(t, 7)
			return ctx
		}).
		Feature()

	testenv.Test(t, f)
}

func resolveRESTAction(name string) func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	return func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		r, err := resources.New(c.Client().RESTConfig())
		if err != nil {
			t.Fail()
		}
		r.WithNamespace(namespace)
		apis.AddToScheme(r.GetScheme())

		cr := v1.RESTAction{}
		err = r.Get(ctx, name, namespace, &cr)
		if err != nil {
			t.Fail()
		}

		res, err := Resolve(ctx, ResolveOptions{
			In:      &cr,
			SArc:    c.Client().RESTConfig(),
			AuthnNS: namespace,
		})
		if err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to resolve rest action", slog.Any("err", err))
			t.Fail()
		}

		res.Kind = "RESTAction"
		res.APIVersion = v1.SchemeGroupVersion.String()

		s := serializer.NewSerializerWithOptions(serializer.DefaultMetaFactory,
			r.GetScheme(), r.GetScheme(),
			serializer.SerializerOptions{
				Yaml:   true,
				Pretty: true,
				Strict: false,
			})

		if err := s.Encode(res, os.Stdout); err != nil {
			log := xcontext.Logger(ctx)
			log.Error("unable to encode YAML", slog.Any("err", err))
			t.Fail()
		}

		return ctx
	}
}
