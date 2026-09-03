//go:build integration
// +build integration

// inspect_integration_test.go — the kind falsifiers for InspectReadSet that
// the in-process tests structurally cannot cover (design §7):
//
//   #1 (DECISIVE, PM nit) — INVERTED BY A-4 (1.12.3). This falsifier used to
//      prove that admission ACCEPTS a free-form UAF verb (`deletecollection`),
//      because CEL bounded only self.verb (the HTTP-stage method) and left
//      userAccessFilter.verb unconstrained. A-4 closed that: a fourth
//      XValidation on the API stage bounds userAccessFilter.verb to
//      get/list/watch, because the verb is threaded verbatim into
//      rbac.EvaluateRBAC and a write verb would make the read path keep the
//      objects the requester may MUTATE — a scope inversion.
//
//      So the arm now asserts the OPPOSITE admission outcome: REAL CEL
//      admission must REJECT `verb: deletecollection`. It is the kind-level
//      twin of apis/templates/v1/uaf_verb_cel_test.go, which runs the
//      apiserver's own CEL validator in-process over the committed generated
//      CRD; this one proves a real apiserver, serving the CRD the
//      snowplow-crds chart ships, reaches the same verdict.
//
//      The EMIT half it also carried — InspectReadSet emits the UAF's OWN verb,
//      not a hardcoded `get` — is unchanged and still needed, so it now runs on
//      an ADMISSIBLE non-get read verb (`watch`). The in-process companion is
//      TestInspect_UAFVerbVerbatim_ReadVerbNotHardcodedGet.
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

// uafInspectRA builds the falsifier's RESTAction with a given UAF verb.
func uafInspectRA(name, uafVerb string) *v1.RESTAction {
	return &v1.RESTAction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.RESTActionSpec{
			API: []*v1.API{
				{
					Name: "ns",
					Path: "/api/v1/namespaces",
					Verb: ptrString("GET"),
					UserAccessFilter: &v1.UserAccessFilterSpec{
						Verb:     uafVerb,
						Group:    "",
						Resource: "namespaces",
					},
				},
			},
		},
	}
}

// TestInspect_RealAdmission_UAFVerbBound is falsifier #1 (A-4 inverted) + #5.
func TestInspect_RealAdmission_UAFVerbBound(t *testing.T) {
	const (
		rejectName = "rbac-inspect-deletecollection"
		acceptName = "rbac-inspect-watch"
	)
	f := features.New("RBACInspect").
		Assess("A-4: real admission REJECTS a write-verb UAF and ACCEPTS a read-verb one, whose verb InspectReadSet emits verbatim",
			func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
				r, err := resources.New(cfg.Client().RESTConfig())
				if err != nil {
					t.Fatal(err)
				}
				apis.AddToScheme(r.GetScheme())
				r.WithNamespace(namespace)

				// A-4 ADMISSION HALF (inverted). Before 1.12.3 core.go bounded
				// only self.verb (the HTTP-stage method) and left the UAF verb
				// free-form, so this apply SUCCEEDED. The fourth XValidation now
				// bounds userAccessFilter.verb to get/list/watch, so a real
				// apiserver serving the shipped CRD MUST reject it. This is the
				// kind-level twin of apis/templates/v1/uaf_verb_cel_test.go.
				err = r.Create(ctx, uafInspectRA(rejectName, "deletecollection"))
				if err == nil {
					t.Fatalf("A-4 ADMISSION HALF FAILED: the apiserver ADMITTED a RESTAction "+
						"whose userAccessFilter.verb is the WRITE verb 'deletecollection'. The "+
						"refilter threads that verb into rbac.EvaluateRBAC, so the read path "+
						"would keep every namespace the requester may DELETE rather than the "+
						"ones they may read — a scope inversion. The read-verb XValidation on "+
						"the API stage is missing from the CRD the cluster is serving (stale "+
						"crds/ or an unregenerated chart template)")
				}
				if errors.IsAlreadyExists(err) {
					t.Fatalf("A-4 setup: %q already exists from an earlier run; teardown did not "+
						"run. Cannot distinguish rejection from a leftover object: %v", rejectName, err)
				}
				if !errors.IsInvalid(err) {
					t.Fatalf("A-4: expected an Invalid (CEL validation) rejection for a "+
						"write-verb userAccessFilter, got a different error: %v", err)
				}
				t.Logf("A-4 ADMISSION HALF PASS: apiserver rejected deletecollection UAF: %v", err)

				// EMIT HALF — unchanged property, on an ADMISSIBLE non-get read
				// verb. `watch` is neither the hardcoded `get` a regression would
				// emit nor the `list` a plain collection stage emits, so the row
				// lookup below still discriminates.
				if err := r.Create(ctx, uafInspectRA(acceptName, "watch")); err != nil && !errors.IsAlreadyExists(err) {
					t.Fatalf("A-4: the apiserver REJECTED a RESTAction whose "+
						"userAccessFilter.verb is the READ verb 'watch' — the bound must admit "+
						"get/list/watch, so this is an over-broad rule: %v", err)
				}

				// Re-read the ADMITTED CR (round-tripped through the apiserver),
				// not the struct we built — so we inspect exactly what admission
				// stored.
				var admitted v1.RESTAction
				if err := r.Get(ctx, acceptName, namespace, &admitted); err != nil {
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

				row, ok := findRow(rows, "", "namespaces", "watch")
				if !ok {
					t.Fatalf("FALSIFIER #1 EMIT HALF FAILED: read-set missing "+
						"{group:\"\", resource:\"namespaces\", verb:\"watch\"} — a "+
						"verb-less/get-only emit would UNDER-GRANT this UAF at the first "+
						"/call. got rows=%+v", rows)
				}
				if row.Verb != "watch" {
					t.Fatalf("FALSIFIER #1 FAIL: UAF row verb must be the admitted UAF verb "+
						"verbatim (watch), got %q", row.Verb)
				}
				t.Logf("FALSIFIER #1 + #5 PASS: admission rejected the write verb and accepted "+
					"the read verb; InspectReadSet (SA discovery only, no caller perms) emitted "+
					"%d rows including %+v", len(rows), row)
				return ctx
			}).
		Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			r, _ := resources.New(cfg.Client().RESTConfig())
			apis.AddToScheme(r.GetScheme())
			r.WithNamespace(namespace)
			for _, n := range []string{rejectName, acceptName} {
				_ = r.Delete(ctx, &v1.RESTAction{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: namespace}})
			}
			return ctx
		}).
		Feature()
	testenv.Test(t, f)
}

func ptrString(s string) *string { return &s }

// compile-time assurance the SA seam is a *rest.Config builder.
var _ func() (*rest.Config, error) = saRESTConfigForInspectFn
