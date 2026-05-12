// SAFETY: This file's TestMain performs destructive setup/teardown on the
// restactions.templates.krateo.io CRD. To prevent accidental data loss when
// run against a production cluster (e.g. a developer's default GKE
// kubeconfig), TestMain refuses to run unless RBAC_TEST_ALLOW_DESTRUCTIVE=1
// is set AND the kubeconfig target is not a known production host.
//
// Background: on 2026-05-11 GKE audit logs recorded destructive DELETE
// operations against the production restactions CRD attributed to
// userAgent=rbac.test/v0.0.0. The trigger was running `go test ./...` from
// the snowplow repo with a default GKE kubeconfig active. Every RESTAction
// CR cluster-wide was garbage-collected. This guard exists to make that
// failure mode impossible.
//
// To run locally against a kind cluster:
//
//	kind create cluster --name rbac-test
//	KUBECONFIG=$(kind get kubeconfig --name rbac-test) \
//	  RBAC_TEST_ALLOW_DESTRUCTIVE=1 \
//	  go test ./internal/rbac/...
package rbac

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/krateoplatformops/plumbing/e2e"
	xenv "github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/snowplow/apis"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
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

// productionKubeconfigSubstrings is the deny-list of strings that, if present
// in the current kubeconfig's server URL or context name, mark the target as
// a production cluster. The guard refuses to run against these even with
// RBAC_TEST_ALLOW_DESTRUCTIVE=1 set.
var productionKubeconfigSubstrings = []string{
	"gke_neon-481711",         // Krateo production GKE project/cluster
	"container.googleapis.com", // any GKE control-plane
	"googleusercontent.com",    // GKE/GCP-managed endpoints
	"eks.amazonaws.com",        // EKS control-plane (defensive)
	"azmk8s.io",                // AKS control-plane (defensive)
}

// guardAgainstProductionKubeconfig inspects the current kubeconfig (the one
// the test binary would talk to if it bypassed the kind setup) and refuses
// to proceed if it points at a known production cluster. Returns true if
// the caller should abort.
func guardAgainstProductionKubeconfig() (abort bool, reason string) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := loadingRules.Load()
	if err != nil {
		// Can't read kubeconfig — that's actually fine, kind setup will
		// create its own. Allow.
		return false, ""
	}
	if cfg == nil {
		return false, ""
	}
	currentCtxName := cfg.CurrentContext
	ctx, ok := cfg.Contexts[currentCtxName]
	if !ok || ctx == nil {
		return false, ""
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok || cluster == nil {
		return false, ""
	}
	server := cluster.Server
	haystack := strings.ToLower(currentCtxName + "|" + ctx.Cluster + "|" + server)
	for _, needle := range productionKubeconfigSubstrings {
		if strings.Contains(haystack, strings.ToLower(needle)) {
			return true, fmt.Sprintf("context=%q cluster=%q server=%q matched %q",
				currentCtxName, ctx.Cluster, server, needle)
		}
	}
	return false, ""
}

func TestMain(m *testing.M) {
	// SAFETY GUARD #1: refuse to run without explicit opt-in. This file's
	// TestMain creates and tears down CRDs; if it picks up a developer's
	// default kubeconfig (e.g. production GKE), every restaction CR
	// cluster-wide will be garbage-collected. See the package-level comment
	// at the top of this file.
	if os.Getenv("RBAC_TEST_ALLOW_DESTRUCTIVE") != "1" {
		fmt.Fprint(os.Stderr,
			"\n[rbac_test] SKIPPING destructive rbac TestMain.\n"+
				"\n"+
				"This test creates and tears down CRDs on the cluster pointed at by\n"+
				"the current kubeconfig. To prevent accidental data loss it refuses\n"+
				"to run unless RBAC_TEST_ALLOW_DESTRUCTIVE=1 is set AND the target\n"+
				"is not a known production cluster.\n"+
				"\n"+
				"To run locally:\n"+
				"  kind create cluster --name rbac-test\n"+
				"  KUBECONFIG=$(kind get kubeconfig --name rbac-test) \\\n"+
				"    RBAC_TEST_ALLOW_DESTRUCTIVE=1 \\\n"+
				"    go test ./internal/rbac/...\n")
		// Exit 0 so `go test ./...` keeps going across other packages.
		// The package's other (non-destructive) tests are in internal/rbac/evaltest.
		os.Exit(0)
	}

	// SAFETY GUARD #2: belt-and-suspenders. Even with the opt-in flag, refuse
	// to run if the kubeconfig points at a production-pattern host. This is
	// the line of defense against typos like "I set the flag but forgot to
	// also set KUBECONFIG=...".
	if abort, reason := guardAgainstProductionKubeconfig(); abort {
		fmt.Fprintf(os.Stderr,
			"\n[rbac_test] REFUSING to run against production kubeconfig.\n"+
				"\n"+
				"RBAC_TEST_ALLOW_DESTRUCTIVE=1 is set, but the current kubeconfig\n"+
				"matches the production deny-list: %s.\n"+
				"\n"+
				"Switch KUBECONFIG to a kind/envtest cluster and re-run. Example:\n"+
				"  kind create cluster --name rbac-test\n"+
				"  KUBECONFIG=$(kind get kubeconfig --name rbac-test) \\\n"+
				"    RBAC_TEST_ALLOW_DESTRUCTIVE=1 \\\n"+
				"    go test ./internal/rbac/...\n",
			reason)
		// Exit 0 so this is a skip, not a CI failure. The destructive intent
		// was real (flag set) but the target was wrong; we treat that as a
		// hard skip rather than a hard fail to avoid breaking `go test ./...`.
		os.Exit(0)
	}

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

			// TODO: add a wait.For conditional helper that can
			// check and wait for the existence of a CRD resource
			time.Sleep(2 * time.Second)
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

func TestUserCan(t *testing.T) {
	const (
		signKey = "abbracadabbra"
	)

	os.Setenv("DEBUG", "0")

	f := features.New("Setup").
		Setup(e2e.Logger("test")).
		Setup(e2e.SignUp(e2e.SignUpOptions{
			Username:   "cyberjoker",
			Groups:     []string{"devs"},
			Namespace:  namespace,
			JWTSignKey: signKey,
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
				decoder.CreateHandler(r),
				decoder.MutateNamespace(namespace),
			)
			if err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("User cannot list secrets",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				gr := schema.GroupResource{Group: "", Resource: "secrets"}
				ok := UserCan(ctx, UserCanOptions{Verb: "list", GroupResource: gr, Namespace: namespace})
				if ok {
					t.Fatalf("user should NOT be able to list %s", gr)
				}
				return ctx
			}).
		Assess("User can list restactions",
			func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
				gr := schema.GroupResource{Group: "templates.krateo.io", Resource: "restactions"}
				ok := UserCan(ctx, UserCanOptions{Verb: "list", GroupResource: gr, Namespace: namespace})
				if !ok {
					t.Fatalf("user should be able to list %s", gr)
				}
				return ctx
			}).
		Feature()

	testenv.Test(t, f)
}
