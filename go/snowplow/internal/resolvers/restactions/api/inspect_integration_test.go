//go:build integration
// +build integration

// inspect_integration_test.go — the kind falsifiers for InspectReadSet that
// the in-process tests structurally cannot cover (design §7):
//
//   #1 (DECISIVE, PM nit): the UAF `verb: deletecollection` RESTAction
//      round-trips through REAL CEL admission (apply against the kind
//      cluster's RESTActions CRD) — proving the "admission accepts a free-form
//      verb" premise is itself true — and InspectReadSet emits the
//      deletecollection verb verbatim. The in-process companion
//      (TestInspect_UAFVerbVerbatim_FreeFormVerb) proves only the EMIT half on
//      a hand-built struct; THIS proves admission accepts it.
//
//   #5 (dispatch-free / before-resolve): InspectReadSet returns the complete,
//      correct read-set using ONLY the SA *rest.Config (discovery), with NONE
//      of any caller's RBAC perms — the whole reason the endpoint can run
//      before any binding exists. Run against the live kind apiserver
//      discovery, no caller token is involved in the enumeration at all.
//
// Reuses the package TestMain (resolve_test.go): kind cluster + the RESTActions
// CRD (current CEL from crds/templates.krateo.io_restactions.yaml) + namespace.

package api

import (
	"context"
	"testing"

	"github.com/krateo-platformops/snowplow/apis"
	v1 "github.com/krateo-platformops/snowplow/apis/templates/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// TestInspect_RealAdmission_UAFFreeFormVerb is falsifier #1 + #5.
func TestInspect_RealAdmission_UAFFreeFormVerb(t *testing.T) {
	f := features.New("RBACInspect").
		Assess("admission accepts a deletecollection UAF AND InspectReadSet emits it verbatim",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				r, err := resources.New(cfg.Client().RESTConfig())
				if err != nil {
					t.Fatal(err)
				}
				apis.AddToScheme(r.GetScheme())
				r.WithNamespace(namespace)

				// The RESTAction with a UAF whose verb is the free-form
				// `deletecollection` on namespaces. core.go:32 only requires a
				// non-empty verb + the resource/resourcesFrom XOR; core.go:30
				// bounds self.verb (the HTTP-stage method) to GET/HEAD, NOT the
				// UAF verb. So admission MUST accept this — the apply succeeding
				// is the falsifier-#1 admission half.
				ra := &v1.RESTAction{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rbac-inspect-deletecollection",
						Namespace: namespace,
					},
					Spec: v1.RESTActionSpec{
						API: []*v1.API{
							{
								Name: "ns",
								Path: "/api/v1/namespaces",
								Verb: ptrString("GET"),
								UserAccessFilter: &v1.UserAccessFilterSpec{
									Verb:     "deletecollection",
									Group:    "",
									Resource: "namespaces",
								},
							},
						},
					},
				}
				if err := r.Create(ctx, ra); err != nil && !errors.IsAlreadyExists(err) {
					t.Fatalf("FALSIFIER #1 ADMISSION HALF FAILED: the apiserver REJECTED a "+
						"RESTAction whose userAccessFilter.verb is the free-form "+
						"'deletecollection' — the design's whole premise (CEL bounds only "+
						"self.verb, not the UAF verb) would be false: %v", err)
				}

				// Re-read the ADMITTED CR (round-tripped through the apiserver),
				// not the struct we built — so we inspect exactly what admission
				// stored.
				var admitted v1.RESTAction
				if err := r.Get(ctx, "rbac-inspect-deletecollection", namespace, &admitted); err != nil {
					t.Fatalf("get admitted RESTAction: %v", err)
				}

				// #5: wire the SA seam to the kind cluster's RESTConfig — the
				// enumeration uses ONLY this (discovery), NEVER a caller token.
				withInspectSARESTConfig(t, cfg.Client().RESTConfig())

				rows, unresolved, err := InspectReadSet(ctx, &admitted, nil)
				if err != nil {
					t.Fatalf("InspectReadSet errored: %v", err)
				}
				if len(unresolved) != 0 {
					t.Fatalf("expected zero unresolved stages, got %+v", unresolved)
				}

				row, ok := findRow(rows, "", "namespaces", "deletecollection")
				if !ok {
					t.Fatalf("FALSIFIER #1 EMIT HALF FAILED: read-set missing "+
						"{group:\"\", resource:\"namespaces\", verb:\"deletecollection\"} — a "+
						"verb-less/get-only emit would UNDER-GRANT this UAF at the first "+
						"/call. got rows=%+v", rows)
				}
				if row.Verb != "deletecollection" {
					t.Fatalf("FALSIFIER #1 FAIL: UAF row verb must be the admitted UAF verb "+
						"verbatim (deletecollection), got %q", row.Verb)
				}
				t.Logf("FALSIFIER #1 + #5 PASS: admission accepted deletecollection UAF; "+
					"InspectReadSet (SA discovery only, no caller perms) emitted %d rows "+
					"including %+v", len(rows), row)
				return ctx
			}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, _ := resources.New(cfg.Client().RESTConfig())
			apis.AddToScheme(r.GetScheme())
			r.WithNamespace(namespace)
			ra := &v1.RESTAction{ObjectMeta: metav1.ObjectMeta{Name: "rbac-inspect-deletecollection", Namespace: namespace}}
			_ = r.Delete(ctx, ra)
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

func ptrString(s string) *string { return &s }

// compile-time assurance the SA seam is a *rest.Config builder.
var _ func() (*rest.Config, error) = saRESTConfigForInspectFn
