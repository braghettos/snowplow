//go:build envtest
// +build envtest

// Q-RBAC-DECOUPLE C(d) v4 §6.5 — anti-defect envtest for the NEW
// widget-L1 transitive leak (Q-RBACC-DEFECT-4).
//
// CRITICAL CONTRACT (per the v4 spec §6.5): this test MUST FAIL on the
// v3 implementation tagged 0.25.296 (proves it catches the defect) and
// PASS on the v4 implementation. The v3 / v3-pre-Fix-W path was:
//
//   first request (widget L1 miss) → widgets.Resolve runs the widget
//                                    pipeline; the apiref slot is resolved
//                                    via apiref.Resolve which (on its own
//                                    L1 miss) returns the FIRST user's
//                                    refiltered view. The widget body
//                                    inlines that view and is written to
//                                    widget L1 keyed by binding identity.
//   second request (widget L1 hit, same identity, different user) →
//                                    dispatcher serves the FIRST user's
//                                    cached widget body verbatim. The
//                                    second user receives the FIRST user's
//                                    apiref data inside the widget shell.
//                                    Silent RBAC leak.
//
// In v4 (Fix-W):
//
//   widget L1 write is GATED by tracker.UAFTouching. apiref.Resolve
//   marks the parent (widget) tracker UAFTouching when the resolved
//   RESTAction has any api[] entry with userAccessFilter. With the
//   flag set, the widget dispatcher SKIPS the L1 write — every request
//   for the widget falls through to Path D and resolves per-user.
//
// Build/run:
//
//   export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir ~/.envtest -p path)
//   go test -tags envtest -timeout 120s -run TestWidgetDispatcher_UAFTransitiveLeak ./internal/handlers/dispatchers/
//
// The envtest binaries provide a real K8s apiserver so the widget
// dispatcher's ValidateObjectStatus + dynamic discovery succeed against
// a real Panel CRD installed by the test setup. The snowplow service
// is exposed via httptest.NewServer wrapping the real widgetsHandler.

package dispatchers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/endpoints"
	"github.com/krateoplatformops/plumbing/env"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/plumbing/ptr"
	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// widgetGVRForTest is the canonical Widget GVR. envtest installs this
// CRD before the test runs.
var widgetGVRForTest = schema.GroupVersionResource{
	Group:    "widgets.templates.krateo.io",
	Version:  "v1beta1",
	Resource: "panels",
}

// widgetUAFTestEnv bundles the envtest control plane + the snowplow
// httptest service so the test can drive widgetsHandler.ServeHTTP via
// a real client over HTTP.
type widgetUAFTestEnv struct {
	t          *testing.T
	cfg        *rest.Config
	c          *cache.MemCache
	upstream   *httptest.Server // serves the K8s namespaces list shape
	server     *httptest.Server // wraps widgetsHandler
	upstreamCt *atomic.Int32
	identityH  string
	cr         *templates.RESTAction
	widget     *unstructured.Unstructured
	authnNS    string
	stopFns    []func()
}

func (e *widgetUAFTestEnv) close() {
	for i := len(e.stopFns) - 1; i >= 0; i-- {
		e.stopFns[i]()
	}
}

const widgetUAFEnvtestHint = `
KUBEBUILDER_ASSETS not set or empty.

Run the following once on this machine:

  go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
  setup-envtest use --bin-dir ~/.envtest -p path
  export KUBEBUILDER_ASSETS=$(setup-envtest use --bin-dir ~/.envtest -p path)

Then re-run:
  go test -tags envtest -timeout 120s -run TestWidgetDispatcher_UAFTransitiveLeak ./internal/handlers/dispatchers/
`

// startWidgetUAFEnvtest brings up etcd + kube-apiserver. Returns the
// rest.Config and a teardown func.
func startWidgetUAFEnvtest(t *testing.T) (*rest.Config, func()) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip(widgetUAFEnvtestHint)
	}
	envt := &envtest.Environment{ErrorIfCRDPathMissing: false}
	cfg, err := envt.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	return cfg, func() {
		if err := envt.Stop(); err != nil {
			t.Logf("envtest.Stop: %v", err)
		}
	}
}

// installPanelCRD registers a minimal Panel CRD against envtest's
// apiserver. The schema is permissive — the dispatcher's
// ValidateObjectStatus only needs a real CRD to look up via discovery
// + apiextensions GET; it then validates `status.widgetData` against
// a permissive object schema.
func installPanelCRD(t *testing.T, cfg *rest.Config) {
	t.Helper()
	cli, err := apiextclientset.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("apiext client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	crd := &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: widgetGVRForTest.Resource + "." + widgetGVRForTest.Group,
		},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: widgetGVRForTest.Group,
			Names: apiextv1.CustomResourceDefinitionNames{
				Plural:   widgetGVRForTest.Resource,
				Singular: "panel",
				Kind:     "Panel",
				ListKind: "PanelList",
			},
			Scope: apiextv1.NamespaceScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{{
				Name:    widgetGVRForTest.Version,
				Served:  true,
				Storage: true,
				Schema: &apiextv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
						Properties: map[string]apiextv1.JSONSchemaProps{
							"spec": {
								Type:                   "object",
								XPreserveUnknownFields: ptr.To(true),
							},
							"status": {
								Type:                   "object",
								XPreserveUnknownFields: ptr.To(true),
								Properties: map[string]apiextv1.JSONSchemaProps{
									"widgetData": {
										Type:                   "object",
										XPreserveUnknownFields: ptr.To(true),
									},
								},
							},
						},
					},
				},
				Subresources: &apiextv1.CustomResourceSubresources{
					Status: &apiextv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}
	if _, err := cli.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create Panel CRD: %v", err)
		}
	}
	// envtest needs a moment to register the new CRD with discovery.
	time.Sleep(500 * time.Millisecond)
}

func newWidgetUAFTestEnv(t *testing.T, namespacesInUpstream []string) *widgetUAFTestEnv {
	t.Helper()
	cfg, stopEnvt := startWidgetUAFEnvtest(t)
	installPanelCRD(t, cfg)

	env.SetTestMode(true)
	t.Cleanup(func() { env.SetTestMode(false) })

	upstreamCt := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCt.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(k8sNamespacesListBody(namespacesInUpstream)))
	}))

	cr := &templates.RESTAction{
		Spec: templates.RESTActionSpec{
			API: []*templates.API{{
				Name: "ns",
				Path: "/api/v1/namespaces",
				UserAccessFilter: &templates.UserAccessFilter{
					Verb:          "list",
					Resource:      "namespaces",
					NamespaceFrom: ptr.To("."),
				},
				Filter: ptr.To(`[.ns.items[].metadata.name]`),
			}},
			Filter: ptr.To(`{namespaces: .ns}`),
		},
	}
	cr.Name = "ns-restaction"
	cr.Namespace = "krateo-system"
	cr.APIVersion = "templates.krateo.io/v1"
	cr.Kind = "RESTAction"

	c := cache.NewMem(0)
	if err := c.Set(context.Background(),
		cache.GetKey(restActionGVRForTest, cr.Namespace, cr.Name),
		mustToUnstructured(t, cr)); err != nil {
		t.Fatalf("seed RESTAction in L2: %v", err)
	}

	widget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgetGVRForTest.GroupVersion().String(),
		"kind":       "Panel",
		"metadata": map[string]any{
			"name":      "ns-panel",
			"namespace": "krateo-system",
		},
		"spec": map[string]any{
			"apiRef": map[string]any{
				"name":      cr.Name,
				"namespace": cr.Namespace,
			},
			// widgetData is needed by ValidateObjectStatus (line 67 of
			// schema.go) — provide a placeholder; widgetDataTemplate
			// fills `namespaces` from the apiref's `.namespaces`
			// status subtree (the per-user-filtered list).
			"widgetData": map[string]any{
				"namespaces": []any{},
			},
			// widgetDataTemplate flows the apiref-resolved data source
			// into status.widgetData. The dispatcher's writeup calls
			// widgetdatatemplate.Resolve which evaluates each item's
			// expression against `ds` (the apiref output map) and
			// writes the result at the given path.
			"widgetDataTemplate": []any{
				map[string]any{
					"forPath":    "namespaces",
					"expression": "${.namespaces}",
				},
			},
		},
	}}
	if err := c.Set(context.Background(),
		cache.GetKey(widgetGVRForTest, "krateo-system", "ns-panel"),
		widget); err != nil {
		t.Fatalf("seed Widget in L2: %v", err)
	}

	const identityH = "shared-binding-id-widget-H"

	snowplowEndpointFn := func() (*endpoints.Endpoint, error) {
		return &endpoints.Endpoint{
			ServerURL: upstream.URL,
			Token:     "SNOWPLOW-SA-TEST",
		}, nil
	}

	authnNS := "krateo-system"
	handler := Widgets()

	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := r.Header.Get("X-Test-User")
		var rbac cache.RBACEvaluator
		switch r.Header.Get("X-Test-RBAC") {
		case "user-a":
			rbac = &missTestStubRBAC{allow: map[string]map[string]bool{
				"user-a": {"default": true, "shared-ns": true},
			}}
		case "user-b":
			rbac = &missTestStubRBAC{allow: map[string]map[string]bool{
				"user-b": {"kube-system": true, "shared-ns": true},
			}}
		}

		ctx := xcontext.BuildContext(r.Context(),
			xcontext.WithLogger(slog.Default()),
			xcontext.WithUserInfo(jwtutil.UserInfo{Username: username, Groups: []string{"devs"}}),
			xcontext.WithUserConfig(endpoints.Endpoint{ServerURL: upstream.URL}),
		)
		ctx = cache.WithCache(ctx, c)
		ctx = cache.WithRBACEvaluator(ctx, rbac)
		ctx = cache.WithBindingIdentity(ctx, identityH)
		ctx = cache.WithSnowplowEndpoint(ctx, func() (any, error) {
			return snowplowEndpointFn()
		})
		// Inject the envtest cfg as the SArc + RC so api.Resolve and
		// widgets.Resolve's ValidateObjectStatus path can both run
		// without requiring an in-cluster mount.
		ctx = cache.WithTestRestConfig(ctx, cfg)

		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	server := httptest.NewServer(wrapped)

	return &widgetUAFTestEnv{
		t:          t,
		cfg:        cfg,
		c:          c,
		upstream:   upstream,
		server:     server,
		upstreamCt: upstreamCt,
		identityH:  identityH,
		cr:         cr,
		widget:     widget,
		authnNS:    authnNS,
		stopFns:    []func(){upstream.Close, server.Close, stopEnvt},
	}
}

// widgetUAFRequest issues a GET to the snowplow widgets dispatcher.
func widgetUAFRequest(t *testing.T, e *widgetUAFTestEnv, user, rbac string) (int, []byte) {
	t.Helper()
	q := fmt.Sprintf(
		"%s/call?apiVersion=%s/%s&resource=%s&namespace=%s&name=%s",
		e.server.URL,
		widgetGVRForTest.Group, widgetGVRForTest.Version,
		widgetGVRForTest.Resource,
		"krateo-system", "ns-panel",
	)
	req, err := http.NewRequest(http.MethodGet, q, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Test-User", user)
	req.Header.Set("X-Test-RBAC", rbac)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// extractWidgetNS extracts the namespace list embedded in the widget
// body's apiRef-resolved data source. The widget pipeline flows the
// apiref status into status.widgetData via apiRef → widgetData merge;
// the array we want lives at status.widgetData.namespaces (or a
// flatter path for the minimal Panel shape).
func extractWidgetNS(body []byte) []string {
	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	st, ok := top["status"].(map[string]any)
	if !ok {
		return nil
	}
	if wd, ok := st["widgetData"].(map[string]any); ok {
		if arr, ok := wd["namespaces"].([]any); ok {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
			return out
		}
	}
	if arr, ok := st["namespaces"].([]any); ok {
		out := make([]string, 0, len(arr))
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// TestWidgetDispatcher_UAFTransitiveLeak_NoCrossUser is the §6.5 anti-
// defect contract for Q-RBACC-DEFECT-4.
func TestWidgetDispatcher_UAFTransitiveLeak_NoCrossUser(t *testing.T) {
	upstreamNS := []string{"default", "kube-system", "shared-ns", "tenant-a"}
	e := newWidgetUAFTestEnv(t, upstreamNS)
	defer e.close()

	codeA, bodyA := widgetUAFRequest(t, e, "user-a", "user-a")
	if codeA != http.StatusOK {
		t.Fatalf("user-a widget request: status %d, body=%s", codeA, string(bodyA))
	}
	if bytes.Contains(bodyA, []byte(`"schema_version"`)) {
		t.Errorf("user-a widget body contains v3 wrapper marker; defense-in-depth assertion failed\nbody=%s", truncate(bodyA, 800))
	}

	widgetL1Key := cache.ResolvedKey(e.identityH, widgetGVRForTest,
		"krateo-system", "ns-panel", -1, -1)
	if _, hit, _ := e.c.GetRaw(context.Background(), widgetL1Key); hit {
		t.Errorf("Q-RBACC-DEFECT-4: widget L1 entry was written despite UAF-touching apiref; v4 Fix-W should have skipped the write\nkey=%s", widgetL1Key)
	}

	codeB, bodyB := widgetUAFRequest(t, e, "user-b", "user-b")
	if codeB != http.StatusOK {
		t.Fatalf("user-b widget request: status %d, body=%s", codeB, string(bodyB))
	}
	if bytes.Equal(bodyA, bodyB) {
		t.Errorf("Q-RBACC-DEFECT-4: user-a and user-b received byte-identical widget bodies despite disjoint RBAC; widget L1 served first user's body to second user")
	}

	gotA := extractWidgetNS(bodyA)
	gotB := extractWidgetNS(bodyB)
	if !equalUnordered(gotA, []string{"default", "shared-ns"}) {
		t.Errorf("user-a widget: expected namespaces=[default shared-ns], got %v\nbody=%s", gotA, truncate(bodyA, 800))
	}
	if !equalUnordered(gotB, []string{"kube-system", "shared-ns"}) {
		t.Errorf("user-b widget: expected namespaces=[kube-system shared-ns], got %v\nbody=%s", gotB, truncate(bodyB, 800))
	}

	if bytes.Contains(bodyA, []byte(`"kube-system"`)) {
		t.Errorf("user-a widget body leaks `kube-system` (user-b's RBAC); cross-user contamination\nbody=%s", truncate(bodyA, 800))
	}
	if bytes.Contains(bodyB, []byte(`"default"`)) {
		t.Errorf("user-b widget body leaks `default` (user-a's RBAC); cross-user contamination\nbody=%s", truncate(bodyB, 800))
	}

	raL1Key := cache.ResolvedKey(e.identityH, restActionGVRForTest,
		e.cr.Namespace, e.cr.Name, -1, -1)
	cachedRA, raHit, _ := e.c.GetRaw(context.Background(), raL1Key)
	if !raHit {
		t.Errorf("RESTAction L1 entry expected to be written by apiref.Resolve; got hit=false (key=%s)", raL1Key)
	}
	if !bytes.Contains(cachedRA, []byte(`"schema_version":"v3"`)) {
		t.Errorf("RESTAction L1 entry should be the v3 wrapper; got\n%s", truncate(cachedRA, 600))
	}
}

// TestWidgetDispatcher_NonUAF_WidgetL1WriteAllowed asserts the
// negative case: a widget whose apiref points at a NON-UAF
// RESTAction must still write to widget L1 (the Fix-W gate must NOT
// trip when no UAF is involved).
func TestWidgetDispatcher_NonUAF_WidgetL1WriteAllowed(t *testing.T) {
	upstreamNS := []string{"default", "kube-system"}
	e := newWidgetUAFTestEnv(t, upstreamNS)
	defer e.close()

	e.cr.Spec.API[0].UserAccessFilter = nil
	if err := e.c.Set(context.Background(),
		cache.GetKey(restActionGVRForTest, e.cr.Namespace, e.cr.Name),
		mustToUnstructured(t, e.cr)); err != nil {
		t.Fatalf("re-seed RESTAction: %v", err)
	}

	codeA, bodyA := widgetUAFRequest(t, e, "user-a", "user-a")
	if codeA != http.StatusOK {
		t.Fatalf("widget request: status %d, body=%s", codeA, string(bodyA))
	}
	_ = bodyA

	widgetL1Key := cache.ResolvedKey(e.identityH, widgetGVRForTest,
		"krateo-system", "ns-panel", -1, -1)
	if _, hit, _ := e.c.GetRaw(context.Background(), widgetL1Key); !hit {
		t.Errorf("widget L1 entry MUST be written when the apiref RESTAction has NO UAF (Fix-W should not trip); got hit=false\nkey=%s", widgetL1Key)
	}
}

// _ = filepath, runtime — placeholders so the imports survive minor
// future tweaks; remove if unused after stabilization.
var (
	_ = filepath.Join
	_ = runtime.GOOS
)
