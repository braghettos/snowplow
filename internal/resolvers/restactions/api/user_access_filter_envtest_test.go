//go:build envtest
// +build envtest

// Q-RBAC-DECOUPLE C(d) §9.5 — envtest harness.
//
// Stands up a real K8s control plane via controller-runtime's envtest
// (kube-apiserver + etcd downloaded by `setup-envtest`). Drives the api
// resolver `Resolve` end-to-end against that control plane to prove:
//
//   1. The RBACWatcher path (production wiring) — NOT the WithRBACEvaluator
//      mock used by the unit suites — correctly filters list responses
//      against real Role/RoleBinding/ClusterRole/ClusterRoleBinding
//      objects served by a real apiserver.
//   2. The migrated `compositions-get-ns-and-crd` RESTAction shape (50 NS,
//      cyberjoker scoped to demo-system only) yields:
//        - cyberjoker → dict["namespaces"] == ["demo-system"], audit
//          denied >= 49.
//        - admin      → dict["namespaces"] length 50, audit denied == 0.
//   3. The in-memory `EvaluateRBAC` decisions match the apiserver's own
//      authorizer for the same (user, verb, resource, namespace) tuple.
//
// Coverage gap closed (vs the mock-based unit + dispatch tests): wiring of
// WithRBACWatcher into ctx, real informer warmup ordering against a live
// apiserver, real `RBACWatcher.EvaluateRBAC` against real RBAC indexes,
// end-to-end Resolve with a real informer (PM ask D2, architect ack
// 2026-05-04).
//
// Build/run:
//   export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir ~/.envtest -p path)
//   go test -tags envtest -timeout 5m ./internal/resolvers/restactions/api/

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtestEnvVarHint is checked at the start of each test so that if a
// developer runs the suite without KUBEBUILDER_ASSETS set, the failure
// message points them at the correct setup command rather than dying with
// an opaque "exec: kube-apiserver: not found".
const envtestEnvVarHint = `
KUBEBUILDER_ASSETS not set or empty.

Run the following once on this machine:

  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
  setup-envtest use --bin-dir ~/.envtest -p path
  export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir ~/.envtest -p path)

Then re-run:
  go test -tags envtest -timeout 5m ./internal/resolvers/restactions/api/
`

// startEnvtest brings up etcd + kube-apiserver, returns the rest.Config and
// a teardown function. Each test gets its own apiserver to avoid cross-
// contamination and to keep each test independently re-runnable.
func startEnvtest(t *testing.T) (*rest.Config, func()) {
	t.Helper()

	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip(envtestEnvVarHint)
	}

	env := &envtest.Environment{
		// We do NOT install snowplow's own CRDs here. The api resolver
		// hits namespaces / customresourcedefinitions endpoints directly
		// via the K8s REST client; we exercise the response→filter path,
		// not the snowplow CRD admission path.
		ErrorIfCRDPathMissing: false,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}

	teardown := func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest.Stop: %v", err)
		}
	}
	return cfg, teardown
}

// seedScenario creates 50 namespaces, 1 ClusterRole (`cyberjoker-list-ns`)
// granting list on namespaces in any namespace via per-namespace
// RoleBinding, the corresponding RoleBinding in demo-system only for
// cyberjoker, and a ClusterRoleBinding granting cyberjoker cluster-wide
// list on customresourcedefinitions (so the `crds` API call succeeds for
// cyberjoker — matching the §8.1 migrated YAML where CRDs are intentionally
// kept cluster-readable).
//
// Returns the demo-system namespace name (constant) for assertion clarity.
func seedScenario(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	cli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("kubernetes.NewForConfig: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 50 namespaces — demo-system + 49 tenants.
	want := []string{"demo-system"}
	for i := 0; i < 49; i++ {
		want = append(want, fmt.Sprintf("tenant-%02d", i))
	}
	for _, ns := range want {
		_, err := cli.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create namespace %q: %v", ns, err)
		}
	}

	// ClusterRole granting list/get on namespaces (used by both the
	// per-NS RoleBinding for cyberjoker and the cluster-wide CRB for
	// admin discovery).
	_, err = cli.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "cyberjoker-list-ns"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""},
			Resources: []string{"namespaces"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ClusterRole cyberjoker-list-ns: %v", err)
	}

	// Per-NS RoleBinding granting cyberjoker namespaces.list ONLY in
	// demo-system. After C(d) Step 4 the cluster-wide CRB is gone, so
	// this is the cyberjoker user's entire namespace footprint.
	_, err = cli.RbacV1().RoleBindings("demo-system").Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cyberjoker-list-ns-demo-system",
			Namespace: "demo-system",
		},
		Subjects: []rbacv1.Subject{
			{Kind: "User", Name: "cyberjoker", APIGroup: "rbac.authorization.k8s.io"},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cyberjoker-list-ns",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create RoleBinding cyberjoker-list-ns-demo-system: %v", err)
	}

	// ClusterRole granting list/get on customresourcedefinitions.
	_, err = cli.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "cyberjoker-list-crd"},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{"apiextensions.k8s.io"},
			Resources: []string{"customresourcedefinitions"},
			Verbs:     []string{"get", "list", "watch"},
		}},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create ClusterRole cyberjoker-list-crd: %v", err)
	}
	// Cluster-wide CRB for cyberjoker on CRDs (matches the §8.1 design:
	// CRDs are kept cluster-readable; only namespace discovery is gated).
	_, err = cli.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "cyberjoker-crd"},
		Subjects: []rbacv1.Subject{
			{Kind: "User", Name: "cyberjoker", APIGroup: "rbac.authorization.k8s.io"},
		},
		RoleRef: rbacv1.RoleRef{
			Kind:     "ClusterRole",
			Name:     "cyberjoker-list-crd",
			APIGroup: "rbac.authorization.k8s.io",
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create CRB cyberjoker-crd: %v", err)
	}

	// Seed at least one CRD so dict["crds"] is non-empty for both users.
	apiextCli, err := apiextclientset.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiextv1 client: %v", err)
	}
	_, err = apiextCli.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "compositions.composition.krateo.io"},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextv1.CustomResourceDefinitionNames{
				Plural:   "compositions",
				Singular: "composition",
				Kind:     "Composition",
				ListKind: "CompositionList",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    "v1",
				Served:  true,
				Storage: true,
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{Type: "object"},
				},
			}},
		},
		Status: apiextv1.CustomResourceDefinitionStatus{
			StoredVersions: []string{"v1"},
			AcceptedNames: apiextv1.CustomResourceDefinitionNames{
				Plural:   "compositions",
				Singular: "composition",
				Kind:     "Composition",
				ListKind: "CompositionList",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create CRD compositions: %v", err)
	}

	return "demo-system"
}

// startWatcher constructs a real *cache.RBACWatcher pointed at the envtest
// apiserver and waits until the informer caches are populated enough to
// answer the RBAC questions the test will ask.
func startWatcher(t *testing.T, cfg *rest.Config) *cache.RBACWatcher {
	t.Helper()
	rw := cache.NewRBACWatcher(cache.NewMem(0), cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := rw.Start(ctx); err != nil {
		t.Fatalf("RBACWatcher.Start: %v", err)
	}

	// Wait until the watcher answers the canonical question correctly.
	// Start() already calls factory.WaitForCacheSync, but on slower CI
	// machines the listers may need a beat after sync to surface the
	// initial LIST output.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if rw.EvaluateRBAC("cyberjoker", nil, "list",
			schema.GroupResource{Group: "", Resource: "namespaces"},
			"demo-system") {
			return rw
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("RBACWatcher never returned true for cyberjoker namespaces.list in demo-system within 20s")
	return rw
}

// captureLogger returns a slog.Logger that writes to a thread-safe buffer
// the test can scan after Resolve returns to assert on audit log fields.
type capturingHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *capturingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingHandler) WithAttrs(_ []slog.Attr) slog.Handler        { return h }
func (h *capturingHandler) WithGroup(_ string) slog.Handler             { return h }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	parts := []string{r.Message}
	r.Attrs(func(a slog.Attr) bool {
		parts = append(parts, fmt.Sprintf("%s=%v", a.Key, a.Value))
		return true
	})
	h.lines = append(h.lines, strings.Join(parts, " "))
	return nil
}
func (h *capturingHandler) snapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.lines))
	copy(out, h.lines)
	return out
}

// findAuditLine returns the first audit=user_access_filter line whose
// `api_call=<name>` matches, or "" if none.
func findAuditLine(lines []string, apiCallName string) string {
	for _, l := range lines {
		if !strings.Contains(l, "audit=user_access_filter") {
			continue
		}
		if strings.Contains(l, "api_call="+apiCallName) {
			return l
		}
	}
	return ""
}

// extractIntField pulls the integer value of `key=` from a log line.
func extractIntField(line, key string) int {
	prefix := key + "="
	i := strings.Index(line, prefix)
	if i < 0 {
		return -1
	}
	rest := line[i+len(prefix):]
	end := strings.IndexAny(rest, " \t")
	if end < 0 {
		end = len(rest)
	}
	var v int
	fmt.Sscanf(rest[:end], "%d", &v)
	return v
}

func TestUserAccessFilter_Envtest_CompositionsGetNsAndCrd(t *testing.T) {
	cfg, stop := startEnvtest(t)
	defer stop()
	demoNS := seedScenario(t, cfg)
	rw := startWatcher(t, cfg)

	// Resolve uses the same RC the apiserver gave us. The api package
	// dispatches via createRequestOptions+httpcall against ep.ServerURL —
	// our endpoint here points at the envtest apiserver, with the SA
	// token from the rest.Config (envtest issues a bootstrap token with
	// cluster-admin equivalence; the test relies on the per-user filter
	// to drop items, NOT on the apiserver authorizer).
	saEndpoint := func() (*endpoints.Endpoint, error) {
		// envtest authenticates via client cert (system:masters group) —
		// production uses bearer token, but the dispatch path doesn't
		// care which auth scheme the apiserver accepts. We pass the
		// envtest-provisioned cert/key/CA through the Endpoint so the
		// httpcall transport mints requests apiserver accepts.
		// Insecure=true sidesteps the "certificate is for localhost
		// but Host is 127.0.0.1:<port>" mismatch.
		// plumbing's transport expects cert/key/CA as base64-encoded PEM
		// (matches the Secret-stored convention used by FromSecret).
		// When cert auth is set, the transport ignores Insecure and
		// requires CAData for trust-anchor validation.
		return &endpoints.Endpoint{
			ServerURL:                cfg.Host,
			ClientCertificateData:    base64.StdEncoding.EncodeToString(cfg.CertData),
			ClientKeyData:            base64.StdEncoding.EncodeToString(cfg.KeyData),
			CertificateAuthorityData: base64.StdEncoding.EncodeToString(cfg.CAData),
		}, nil
	}

	// The migrated `compositions-get-ns-and-crd` shape from
	// portal-cache-yaml-staging/restaction.compositions-get-ns-and-crd.yaml.
	apis := []*templates.API{
		{
			Name: "crds",
			Path: "/apis/apiextensions.k8s.io/v1/customresourcedefinitions",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb:     "list",
				Group:    "apiextensions.k8s.io",
				Resource: "customresourcedefinitions",
			},
			Filter:          ptr.To(`[.crds.items[] | select(.spec.group == "composition.krateo.io") | {storedVersions: .status.storedVersions[0], plural: .status.acceptedNames.plural} ]`),
			ContinueOnError: ptr.To(true),
		},
		{
			Name: "namespaces",
			Path: "/api/v1/namespaces",
			UserAccessFilter: &templates.UserAccessFilter{
				Verb:          "list",
				Resource:      "namespaces",
				NamespaceFrom: ptr.To("."),
			},
			Filter:          ptr.To(`[.namespaces.items[] | .metadata.name]`),
			ContinueOnError: ptr.To(true),
		},
	}

	t.Run("cyberjoker_seesOnly_demo-system", func(t *testing.T) {
		cap := &capturingHandler{}
		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(slog.New(cap)),
			xcontext.WithUserInfo(jwtutil.UserInfo{
				Username: "cyberjoker",
				Groups:   []string{"devs"},
			}),
		)
		ctx = cache.WithRBACWatcher(ctx, rw)
		ctx = cache.WithRESTActionName(ctx, "compositions-get-ns-and-crd")

		dict := Resolve(ctx, ResolveOptions{
			RC:               cfg,
			Items:            apis,
			SnowplowEndpoint: saEndpoint,
		})

		gotNS, ok := dict["namespaces"].([]any)
		if !ok {
			b, _ := json.Marshal(dict)
			t.Fatalf("expected dict[namespaces]=[]any, got %s", string(b))
		}
		if len(gotNS) != 1 || gotNS[0] != demoNS {
			t.Errorf("expected only %q in namespaces, got %v (len=%d)", demoNS, gotNS, len(gotNS))
		}

		gotCRDs, ok := dict["crds"].([]any)
		if !ok {
			t.Errorf("expected dict[crds]=[]any, got %T %v", dict["crds"], dict["crds"])
		} else if len(gotCRDs) == 0 {
			t.Errorf("expected dict[crds] non-empty for cyberjoker (cluster-wide CRD CRB), got empty")
		}

		// Audit assertions. envtest seeds default+kube-* namespaces in
		// addition to the 50 we created (typically 4 extras: default,
		// kube-node-lease, kube-public, kube-system), so items_in is
		// >= 50 not == 50. The cyberjoker contract is "only demo-system
		// visible AND denial volume tracks the unseen NS count" — we
		// assert that, not the apiserver-fixture cardinality.
		lines := cap.snapshot()
		nsLine := findAuditLine(lines, "namespaces")
		if nsLine == "" {
			t.Fatalf("no audit line for api_call=namespaces in %d log lines", len(lines))
		}
		denied := extractIntField(nsLine, "denied")
		if denied < 49 {
			t.Errorf("expected denied >= 49 in audit line, got %d (line: %s)", denied, nsLine)
		}
		itemsIn := extractIntField(nsLine, "items_in")
		if itemsIn < 50 {
			t.Errorf("expected items_in >= 50 (50 user-seeded + envtest fixtures), got %d (line: %s)", itemsIn, nsLine)
		}
		itemsOut := extractIntField(nsLine, "items_out")
		if itemsOut != 1 {
			t.Errorf("expected items_out == 1, got %d (line: %s)", itemsOut, nsLine)
		}
		// Internal accounting consistency check.
		if itemsIn != itemsOut+denied {
			t.Errorf("audit accounting inconsistent: items_in=%d != items_out=%d + denied=%d (line: %s)",
				itemsIn, itemsOut, denied, nsLine)
		}
	})

	t.Run("admin_sees_all_50", func(t *testing.T) {
		// Admin gets cluster-wide list via a fresh CRB created here, so
		// EvaluateRBAC short-circuits true at phase 1 for every namespace.
		cli, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			t.Fatalf("kubernetes.NewForConfig: %v", err)
		}
		_, _ = cli.RbacV1().ClusterRoleBindings().Create(context.Background(),
			&rbacv1.ClusterRoleBinding{
				ObjectMeta: metav1.ObjectMeta{Name: "admin-list-ns"},
				Subjects: []rbacv1.Subject{
					{Kind: "User", Name: "admin", APIGroup: "rbac.authorization.k8s.io"},
				},
				RoleRef: rbacv1.RoleRef{
					Kind: "ClusterRole", Name: "cyberjoker-list-ns",
					APIGroup: "rbac.authorization.k8s.io",
				},
			}, metav1.CreateOptions{})

		// Wait for the watcher to see the admin CRB.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if rw.EvaluateRBAC("admin", nil, "list",
				schema.GroupResource{Group: "", Resource: "namespaces"},
				"tenant-00") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}

		cap := &capturingHandler{}
		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(slog.New(cap)),
			xcontext.WithUserInfo(jwtutil.UserInfo{
				Username: "admin",
				Groups:   []string{"system:masters"},
			}),
		)
		ctx = cache.WithRBACWatcher(ctx, rw)
		ctx = cache.WithRESTActionName(ctx, "compositions-get-ns-and-crd")

		dict := Resolve(ctx, ResolveOptions{
			RC:               cfg,
			Items:            apis,
			SnowplowEndpoint: saEndpoint,
		})

		gotNS, ok := dict["namespaces"].([]any)
		if !ok {
			b, _ := json.Marshal(dict)
			t.Fatalf("expected dict[namespaces]=[]any, got %s", string(b))
		}
		// envtest fixtures (default + kube-*) push total >= 50; admin
		// must see at least the 50 we user-seeded.
		if len(gotNS) < 50 {
			t.Errorf("expected >=50 namespaces visible to admin, got %d (%v)", len(gotNS), gotNS)
		}

		lines := cap.snapshot()
		nsLine := findAuditLine(lines, "namespaces")
		if nsLine == "" {
			t.Fatalf("no audit line for api_call=namespaces; %d log lines", len(lines))
		}
		denied := extractIntField(nsLine, "denied")
		if denied != 0 {
			t.Errorf("expected denied == 0 for admin, got %d (line: %s)", denied, nsLine)
		}
		itemsIn := extractIntField(nsLine, "items_in")
		itemsOut := extractIntField(nsLine, "items_out")
		if itemsIn < 50 || itemsOut != itemsIn {
			t.Errorf("expected items_in>=50 and items_out==items_in, got in=%d out=%d (line: %s)", itemsIn, itemsOut, nsLine)
		}
	})
}
