//go:build integration
// +build integration

package widgets

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/e2e"
	"github.com/krateo-platformops/snowplow/apis"
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"github.com/krateo-platformops/snowplow/internal/objects"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestResolveWidgets_Extras is the END-TO-END (kind) proof of the
// extras→widgets parity feature. It drives the REAL widgets.Resolve with an
// Extras map and asserts the resolved status reflects the extras-supplied
// values across all three paths the feature covers:
//
//   - apiRef-fetch parity (step 1): a widget whose apiRef RESTAction `path`
//     references an extras key resolves the right object (button-extras-apiref).
//   - widgetDataTemplate access (step 2): a widgetDataTemplate referencing an
//     extras key yields the expected status.widgetData (button-extras-static).
//   - resourcesRefsTemplate access (step 2, Diego's case): a
//     resourcesRefsTemplate referencing an extras key yields the expected
//     status.resourcesRefs (button-extras-static).
//
// It reuses the package TestMain (widgets_test.go) cluster/CRD/RBAC setup —
// the buttons + restactions CRDs and rbac.namespaces.yaml (devs → cluster-wide
// namespaces get/list) are already installed there.
func TestResolveWidgets_Extras(t *testing.T) {
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

	f := features.New("ExtrasParity").
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
				t.Fatal(err)
			}
			apis.AddToScheme(r.GetScheme())
			r.WithNamespace(namespace)

			// Decode the extras fixtures (widget CRs + the apiRef RESTAction).
			if err := decoder.DecodeEachFile(
				ctx, os.DirFS(filepath.Join(testdataPath, "widgets")), "button.extras.*.yaml",
				decoder.CreateIgnoreAlreadyExists(r),
				decoder.MutateNamespace(namespace),
			); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		// Step 2 over BOTH template paths (apiRef-less static widget).
		Assess("static widget: extras in widgetDataTemplate AND resourcesRefsTemplate",
			assertStaticExtras).
		// Step 1: apiRef RESTAction path references an extras key.
		Assess("apiRef widget: extras parametrises the RESTAction fetch path",
			assertApiRefExtras).
		// inline-extras design P — per-surface inline maps end-to-end.
		Assess("#1b/#5 inline resourcesRefsTemplateExtras reaches the rrt jq ONLY (scope isolation)",
			assertInlineRrtExtrasScoped).
		Assess("#1a inline apiRef.extras parametrises the RESTAction fetch path (no request)",
			assertInlineApiRefExtras).
		Assess("#2a request overrides inline apiRef.extras (request wins)",
			assertRequestOverridesInlineApiRef).
		Assess("#7 input-only: inline maps never echo into resolved status",
			assertInlineExtrasInputOnly).
		Feature()

	testenv.Test(t, f)
}

// TWO-REPO SPLIT (design §6 + hard constraints): the widget CRD schema
// declarations for spec.apiRef.extras + spec.resourcesRefsTemplateExtras are a
// portal-chart follow-up, OUT of this snowplow ship's scope. The kind harness
// installs the SHIPPED Button CRD (which declares neither), so a CREATE through
// the apiserver PRUNES both inline blocks before the resolver could read them
// (the apiserver logs `unknown field "spec.apiRef.extras"`). The inline
// falsifiers therefore build the widget unstructured IN-MEMORY carrying the
// inline blocks and pass it straight to the REAL widgets.Resolve — exactly the
// object shape the production fetchObject GET will return once the portal CRD
// ships the fields. Snowplow reads unstructured and tolerates absence ({}); the
// only thing the apiserver round-trip would add is the prune, which is the very
// behaviour the two-repo follow-up removes. (The RESTAction the apiRef path
// resolves IS fetched from the cluster — only the widget object is in-memory.)

// inlineStaticWidget builds the apiRef-LESS scope-isolation fixture in-memory:
// widgetDataTemplate runs BEFORE the resourcesRefsTemplateExtras fold so it sees
// `.rrtNs` as ABSENT ("ns=ABSENT"); the resourcesRefsTemplate iterator over the
// inline `names` fans out one ref per element, each namespaced by the inline
// `rrtNs`. Mirrors testdata/widgets/button.extras.inline.static.yaml.
func inlineStaticWidget() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Button",
		"metadata":   map[string]any{"name": "button-extras-inline-static", "namespace": namespace},
		"spec": map[string]any{
			"widgetData": map[string]any{
				"actions": map[string]any{}, "clickActionId": "nop",
				"label": "base-label", "icon": "fa-clock", "type": "text",
			},
			"widgetDataTemplate": []any{
				map[string]any{"forPath": "label", "expression": `${ "ns=" + (.rrtNs // "ABSENT") }`},
			},
			"resourcesRefsTemplate": []any{
				// Iterator over the rrt-inline `names` array (evaluated against
				// ds): fanning out proves `names` reached the rrt jq. Inside the
				// iterator, `.` is each element string, so namespace=${ . }.
				map[string]any{
					"iterator": `${ .names }`,
					"template": map[string]any{
						"id": `${ "ns-" + . }`, "apiVersion": "v1", "resource": "namespaces",
						"namespace": `${ . }`, "verb": "GET",
					},
				},
				// Non-iterator item referencing the rrt-inline SCALAR `rrtNs`
				// (evaluated against ds): a resolved namespace=inline-team proves
				// `rrtNs` reached the rrt jq too.
				map[string]any{
					"template": map[string]any{
						"id": `${ "from-rrtNs-" + .rrtNs }`, "apiVersion": "v1",
						"resource": "namespaces", "namespace": `${ .rrtNs }`, "verb": "GET",
					},
				},
			},
			"resourcesRefsTemplateExtras": map[string]any{
				"rrtNs": "inline-team",
				"names": []any{"alpha", "beta"},
			},
		},
	}}
}

// inlineApiRefWidget builds the apiRef fixture in-memory with
// spec.apiRef.extras.nsName. Mirrors button.extras.inline.apiref.yaml; the
// referenced RESTAction (extras-namespace-by-name) IS fetched from the cluster.
func inlineApiRefWidget() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "widgets.templates.krateo.io/v1beta1",
		"kind":       "Button",
		"metadata":   map[string]any{"name": "button-extras-inline-apiref", "namespace": namespace},
		"spec": map[string]any{
			"widgetData": map[string]any{
				"actions": map[string]any{}, "clickActionId": "nop",
				"label": "base-label", "icon": "fa-clock", "type": "text",
			},
			"apiRef": map[string]any{
				"name": "extras-namespace-by-name", "namespace": namespace,
				"extras": map[string]any{"nsName": "kube-system"},
			},
			"widgetDataTemplate": []any{
				map[string]any{"forPath": "label", "expression": `${ .ns.metadata.name }`},
			},
		},
	}}
}

// assertInlineRrtExtrasScoped resolves the in-memory inline-static widget with
// NO request extras and asserts (falsifier #1b + #5):
//   - the resourcesRefsTemplate jq saw the inline rrt keys (one ref per `names`
//     element, each namespaced by the inline `rrtNs`) — proves #1b inline-only;
//   - the widgetDataTemplate did NOT see the rrt-only key `rrtNs`
//     (status.widgetData.label == "ns=ABSENT") — proves #5 scope isolation
//     (the rrt fold happens AFTER widgetDataTemplate ran).
func assertInlineRrtExtrasScoped(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	// NO request extras — the inline resourcesRefsTemplateExtras block is the
	// sole source of rrtNs + names.
	obj := resolveInMemoryWidget(ctx, t, c, inlineStaticWidget(), nil)

	// #5 — scope isolation: the rrt-only key MUST be absent in widgetData.
	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "ns=ABSENT" {
		t.Fatalf("#5 scope-isolation FAILED: widgetDataTemplate saw the resourcesRefsTemplateExtras-only key rrtNs; "+
			"status.widgetData.label got %v want \"ns=ABSENT\" (the rrt fold must be invisible to widgetDataTemplate)", got)
	}

	// #1b — the resourcesRefsTemplate jq DID see the inline rrt keys:
	//   - the iterator over `names` fanned out one ref per element (ns-alpha,
	//     ns-beta) — proves the ARRAY key `names` reached the jq;
	//   - the scalar item resolved namespace=inline-team — proves the SCALAR
	//     key `rrtNs` reached the jq.
	items := nestedSlice(t, obj, "status", "resourcesRefs", "items")
	if len(items) != 3 {
		t.Fatalf("#1b FAILED: resourcesRefsTemplate must fan out 2 iterator refs + 1 scalar ref; got %d want 3 (items=%v)", len(items), items)
	}
	for _, el := range []string{"alpha", "beta"} {
		if !hasRefWithID(items, "ns-"+el) {
			t.Fatalf("#1b FAILED: expected a resourcesRefs item with id ns-%s (from inline names element %q); items=%v", el, el, items)
		}
		if !hasRefPathContainingNamespace(items, el) {
			t.Fatalf("#1b FAILED: expected a ref targeting namespace=%s (from inline names element); items=%v", el, items)
		}
	}
	// The scalar item proves rrtNs reached the jq.
	if !hasRefWithID(items, "from-rrtNs-inline-team") {
		t.Fatalf("#1b FAILED: expected a resourcesRefs item id from-rrtNs-inline-team (from inline rrtNs scalar); items=%v", items)
	}
	if !hasRefPathContainingNamespace(items, "inline-team") {
		t.Fatalf("#1b FAILED: expected a ref targeting namespace=inline-team (from inline rrtNs); items=%v", items)
	}
	return ctx
}

// assertInlineApiRefExtras resolves the in-memory inline-apiref widget with NO
// request extras; the inline spec.apiRef.extras.nsName="kube-system" must
// parametrise the RESTAction path so status.widgetData.label == "kube-system"
// (#1a).
func assertInlineApiRefExtras(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	obj := resolveInMemoryWidget(ctx, t, c, inlineApiRefWidget(), nil)
	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "kube-system" {
		t.Fatalf("#1a FAILED: inline apiRef.extras.nsName must parametrise the RESTAction fetch path; "+
			"status.widgetData.label got %v want kube-system (if base-label/empty, the inline map did not reach the fetch)", got)
	}
	return ctx
}

// assertRequestOverridesInlineApiRef resolves the SAME in-memory inline-apiref
// widget but WITH a request extras override (nsName=default). The REQUEST value
// must win over the inline default, so the fetched namespace is `default` and
// status.widgetData.label == "default" (#2a — the load-bearing precedence).
func assertRequestOverridesInlineApiRef(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	obj := resolveInMemoryWidget(ctx, t, c, inlineApiRefWidget(),
		map[string]any{"nsName": "default"})
	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "default" {
		t.Fatalf("#2a FAILED: the REQUEST extras nsName MUST win over the inline apiRef.extras default; "+
			"status.widgetData.label got %v want default (got kube-system ⇒ inline shadowed the request — the precedence bug)", got)
	}
	return ctx
}

// assertInlineExtrasInputOnly resolves the in-memory inline-static widget and
// asserts the inline maps never echo into the resolved status (falsifier #7):
// no status.apiRef.extras, no status.resourcesRefsTemplateExtras, no
// status.extras. Inline extras is INPUT-only (seeds ds / the api dict), exactly
// like request extras.
func assertInlineExtrasInputOnly(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	obj := resolveInMemoryWidget(ctx, t, c, inlineStaticWidget(), nil)
	status := nestedMap(t, obj, "status")
	if status == nil {
		t.Fatal("#7: resolved widget has no status")
	}
	if _, present := status["extras"]; present {
		t.Fatalf("#7 FAILED: status.extras MUST NOT be written (inline extras is input-only); status=%v", status)
	}
	if _, present := status["resourcesRefsTemplateExtras"]; present {
		t.Fatalf("#7 FAILED: status.resourcesRefsTemplateExtras MUST NOT be written; status=%v", status)
	}
	// status.apiRef (if any) must not carry an extras sub-key either.
	if ar, ok := status["apiRef"].(map[string]any); ok {
		if _, present := ar["extras"]; present {
			t.Fatalf("#7 FAILED: status.apiRef.extras MUST NOT be written; status.apiRef=%v", ar)
		}
	}
	return ctx
}

// resolveInMemoryWidget resolves an in-memory widget unstructured (carrying the
// inline-extras blocks the shipped CRD would prune on a CREATE round-trip — see
// the TWO-REPO SPLIT note above) through the REAL widgets.Resolve. AuthnNS is
// set so the apiRef per-user endpoint resolves (cf. resolveExtrasWidget).
func resolveInMemoryWidget(ctx context.Context, t *testing.T, c *envconf.Config, in *unstructured.Unstructured, extras map[string]any) *unstructured.Unstructured {
	t.Helper()
	obj, err := Resolve(ctx, ResolveOptions{
		RC:      c.Client().RESTConfig(),
		AuthnNS: namespace,
		In:      in,
		Extras:  extras,
	})
	if err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to resolve in-memory widget", slog.Any("err", err))
		t.Fatalf("resolve in-memory widget %q with extras=%v: %v", in.GetName(), extras, err)
	}
	return obj
}

// assertStaticExtras resolves button-extras-static with
// extras={tenant, region, names:[...]} and asserts the extras-derived values
// landed in status.widgetData (label/icon) and status.resourcesRefs (one ref
// per extras `names` element, each namespaced by the element).
func assertStaticExtras(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	extras := map[string]any{
		"tenant": "acme-corp",
		"region": "fa-eu",
		"names":  []any{"alpha", "beta"},
	}
	obj := resolveExtrasWidget(ctx, t, c, "button-extras-static", extras)

	// status.widgetData.label == extras.tenant ; .icon == extras.region.
	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "acme-corp" {
		t.Fatalf("status.widgetData.label: extras.tenant must flow into widgetDataTemplate; got %v want acme-corp", got)
	}
	if got := wd["icon"]; got != "fa-eu" {
		t.Fatalf("status.widgetData.icon: extras.region must flow into widgetDataTemplate; got %v want fa-eu", got)
	}

	// status.resourcesRefs.items — one ref per extras names element, each
	// namespaced by the element (Diego's resourcesRefsTemplate case). The
	// resolver rewrites each ref into a snowplow /call loopback path of the
	// shape `/call?apiVersion=v1&namespace=<el>&resource=namespaces`, and sets
	// id to the template's `"ns-"+<el>`. We assert BOTH the per-element id
	// (template-set, stable) AND that the resolved path carries the element's
	// namespace (proving the extras element drove the ref's namespace).
	items := nestedSlice(t, obj, "status", "resourcesRefs", "items")
	if len(items) != 2 {
		t.Fatalf("resourcesRefsTemplate must fan out one ref per extras names element; got %d items want 2 (items=%v)", len(items), items)
	}
	for _, el := range []string{"alpha", "beta"} {
		if !hasRefWithID(items, "ns-"+el) {
			t.Fatalf("expected a resourcesRefs item with id ns-%s (from extras.names element %q); items=%v", el, el, items)
		}
		if !hasRefPathContainingNamespace(items, el) {
			t.Fatalf("expected a resourcesRefs item whose resolved /call path targets namespace=%s (from extras.names); items=%v", el, items)
		}
	}
	return ctx
}

// assertApiRefExtras resolves button-extras-apiref with extras={nsName:"kube-system"};
// the apiRef RESTAction's `path` is ${ "/api/v1/namespaces/" + .nsName }, so a
// correct resolve GETs the kube-system namespace and surfaces its name into
// status.widgetData.label. If extras had NOT reached the path, the GET would
// target a name-less namespaces path and .ns.nsName would be absent.
func assertApiRefExtras(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
	extras := map[string]any{"nsName": "kube-system"}
	obj := resolveExtrasWidget(ctx, t, c, "button-extras-apiref", extras)

	wd := nestedMap(t, obj, "status", "widgetData")
	if got := wd["label"]; got != "kube-system" {
		t.Fatalf("apiRef path must be parametrised by extras.nsName: status.widgetData.label got %v want kube-system "+
			"(if nil/empty, extras did not reach the RESTAction fetch path)", got)
	}
	return ctx
}

// (Backward-compat / no-op-when-extras-absent is covered at the unit + cache-
// key layers: TestMergeExtras_NilOrEmptyIsNoOp, TestExtras_NoExtras_Template-
// Unchanged, and TestExtras_WidgetContentKey_EmptyEqualsNil. It is deliberately
// NOT re-asserted here: a static fixture whose widgetDataTemplate writes an
// extras-derived value into a REQUIRED string field (Button.spec label) would,
// with extras absent, write jq null and trip the Button CRD's string-type
// status validation — exercising CRD validation, not the feature's no-op
// property.)

// resolveExtrasWidget fetches the named Button and resolves it through the REAL
// widgets.Resolve with the given extras (AuthnNS set so the apiRef per-user
// endpoint resolves — see resolveWidget in widgets_test.go for the rationale).
func resolveExtrasWidget(ctx context.Context, t *testing.T, c *envconf.Config, name string, extras map[string]any) *unstructured.Unstructured {
	t.Helper()
	r, err := resources.New(c.Client().RESTConfig())
	if err != nil {
		t.Fatal(err)
	}
	r.WithNamespace(namespace)
	apis.AddToScheme(r.GetScheme())

	res := objects.Get(ctx, v1.ObjectReference{
		Reference: v1.Reference{Name: name, Namespace: namespace},
		Resource:  "buttons", APIVersion: "widgets.templates.krateo.io/v1beta1",
	})
	if res.Err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to get widget", slog.Any("err", res.Err))
		t.Fatalf("get widget %q: %v", name, res.Err)
	}

	obj, err := Resolve(ctx, ResolveOptions{
		RC:      c.Client().RESTConfig(),
		AuthnNS: namespace,
		In:      res.Unstructured,
		Extras:  extras,
	})
	if err != nil {
		log := xcontext.Logger(ctx)
		log.Error("unable to resolve widget", slog.Any("err", err))
		t.Fatalf("resolve widget %q with extras=%v: %v", name, extras, err)
	}
	return obj
}

// --- small unstructured accessors (test-local) ---

func nestedMap(t *testing.T, obj *unstructured.Unstructured, fields ...string) map[string]any {
	t.Helper()
	m, ok, err := unstructured.NestedMap(obj.Object, fields...)
	if err != nil {
		t.Fatalf("NestedMap %v: %v", fields, err)
	}
	if !ok {
		return nil
	}
	return m
}

func nestedSlice(t *testing.T, obj *unstructured.Unstructured, fields ...string) []any {
	t.Helper()
	s, ok, err := unstructured.NestedSlice(obj.Object, fields...)
	if err != nil {
		t.Fatalf("NestedSlice %v: %v", fields, err)
	}
	if !ok {
		return nil
	}
	return s
}

func hasRefWithID(items []any, wantID string) bool {
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := m["id"].(string); id == wantID {
			return true
		}
	}
	return false
}

// hasRefPathContainingNamespace reports whether some resolved ref's /call
// loopback path carries `namespace=<ns>` (the resolver rewrites a
// resourcesRefsTemplate ref into /call?apiVersion=…&namespace=<ns>&resource=…).
func hasRefPathContainingNamespace(items []any, ns string) bool {
	want := "namespace=" + ns
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if p, _ := m["path"].(string); strings.Contains(p, want) {
			return true
		}
	}
	return false
}
