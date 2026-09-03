// uaf_verb_cel_test.go — A-4 (1.12.3) ADMISSION arm for userAccessFilter.verb.
//
// HISTORY, because the assertion here is the OPPOSITE of the one this file
// shipped with a few hours ago. A-4's first cut added a fourth XValidation
// bounding userAccessFilter.verb to get/list/watch. Review found three LIVE
// portal RESTActions that use `verb: create` legitimately (testdata/
// portal_uaf_corpus.yaml, portal @ b2a558d): the api-step returns NAMESPACES
// and the filter checks `create` on a DIFFERENT resource, so it answers "which
// namespaces may I create X in" — a form picker, not a scope inversion. The CRD
// rule would have failed the portal upgrade outright. It was DROPPED for
// 1.12.3; the read-verb check survives at runtime as a WARN-ONLY signal
// (refilter.go uafVerbIsRead), and 1.13.0 will enforce the narrower
// SAME-RESOURCE rule.
//
// So this arm now pins the ABSENCE of a verb bound: the committed CRD must
// ADMIT the live portal corpus. It is a regression gate on a real outage — if
// someone re-adds the blanket rule, the portal upgrade breaks and this fails
// first, naming the three files.
//
// WHY A REAL CEL DRIVE, NOT A STRING GREP. Grepping the YAML for a rule that is
// absent proves nothing about whether some OTHER rule rejects these objects.
// Per feedback_falsifier_must_drive_real_boundary_not_install_crossed_state this
// loads the COMMITTED generated CRD, builds the structural schema the apiserver
// builds, and runs the apiserver's own CEL validator over whole RESTAction
// objects. The kind-backed twin, against a genuine apiserver, is
// TestInspect_RealAdmission_UAFFreeFormVerb in internal/resolvers/restactions/api.

package v1

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"
)

// celPerCallLimit / celCostBudget only bound runtime cost, never which rules run.
const (
	celPerCallLimit = uint64(1_000_000)
	celCostBudget   = int64(10_000_000)
)

// portalUAFCase mirrors one entry of testdata/portal_uaf_corpus.yaml.
type portalUAFCase struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	APIStepName      string `json:"apiStepName"`
	APIStepPath      string `json:"apiStepPath"`
	APIStepVerb      string `json:"apiStepVerb"`
	UserAccessFilter struct {
		Verb          string `json:"verb"`
		Group         string `json:"group"`
		Resource      string `json:"resource"`
		ResourcesFrom string `json:"resourcesFrom"`
		NamespaceFrom string `json:"namespaceFrom"`
	} `json:"userAccessFilter"`
}

// loadPortalUAFCorpus reads the vendored live-portal stanzas. Shared with the
// refilter-side arm in internal/resolvers/restactions/api, which reads the same
// file, so the two halves of the corpus gate can never drift apart.
func loadPortalUAFCorpus(t *testing.T, relToTestdata string) []portalUAFCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(relToTestdata, "portal_uaf_corpus.yaml"))
	if err != nil {
		t.Fatalf("read portal UAF corpus: %v", err)
	}
	var doc struct {
		Cases []portalUAFCase `json:"cases"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal portal UAF corpus: %v", err)
	}
	if len(doc.Cases) != 3 {
		t.Fatalf("portal UAF corpus must carry the 3 live stanzas; got %d", len(doc.Cases))
	}
	return doc.Cases
}

// loadRESTActionCELValidator builds the apiserver's CEL validator from the
// COMMITTED generated CRD — the same artifact the snowplow-crds chart ships, so
// this arm fails if core.go and the YAML ever disagree.
func loadRESTActionCELValidator(t *testing.T) (*schemacel.Validator, *structuralschema.Structural) {
	t.Helper()

	crdPath := filepath.Join("..", "..", "..", "crds", "templates.krateo.io_restactions.yaml")
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read generated CRD %s: %v", crdPath, err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("unmarshal generated CRD: %v", err)
	}
	if len(crd.Spec.Versions) == 0 || crd.Spec.Versions[0].Schema == nil {
		t.Fatalf("generated CRD carries no versioned schema")
	}

	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &internal, nil); err != nil {
		t.Fatalf("convert schema to internal form: %v", err)
	}

	structural, err := structuralschema.NewStructural(&internal)
	if err != nil {
		t.Fatalf("build structural schema: %v", err)
	}

	return schemacel.NewValidator(structural, true, celPerCallLimit), structural
}

// restActionFromCase builds a complete RESTAction around one vendored stanza.
func restActionFromCase(c portalUAFCase) map[string]any {
	uaf := map[string]any{
		"verb":          c.UserAccessFilter.Verb,
		"group":         c.UserAccessFilter.Group,
		"namespaceFrom": c.UserAccessFilter.NamespaceFrom,
	}
	// resource / resourcesFrom are XOR'd by an existing rule — set exactly the
	// one the live stanza sets.
	if c.UserAccessFilter.Resource != "" {
		uaf["resource"] = c.UserAccessFilter.Resource
	}
	if c.UserAccessFilter.ResourcesFrom != "" {
		uaf["resourcesFrom"] = c.UserAccessFilter.ResourcesFrom
	}
	return map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata":   map[string]any{"name": "a4-portal-" + c.Name, "namespace": "krateo-system"},
		"spec": map[string]any{
			"api": []any{
				map[string]any{
					"name":             c.APIStepName,
					"path":             c.APIStepPath,
					"verb":             c.APIStepVerb,
					"userAccessFilter": uaf,
				},
			},
		},
	}
}

// restActionNoUAF is the boundary shape: an api-step with NO userAccessFilter.
func restActionNoUAF() map[string]any {
	return map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata":   map[string]any{"name": "a4-probe", "namespace": "krateo-system"},
		"spec": map[string]any{
			"api": []any{
				map[string]any{"name": "plain", "path": "/healthz", "verb": "GET"},
			},
		},
	}
}

// validateCEL runs every XValidation rule in the schema over obj and returns the
// joined error text ("" when the object is ADMITTED).
func validateCEL(t *testing.T, obj map[string]any) string {
	t.Helper()
	validator, structural := loadRESTActionCELValidator(t)
	if validator == nil {
		t.Fatal("A-4 setup: the generated CRD carries NO CEL rules at all — the three pre-existing userAccessFilter guards have gone missing, so this harness is not exercising admission")
	}
	errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celCostBudget)
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

// TestA4_PortalCorpus_CreateVerbUAFs_Admitted — the regression gate on the real
// outage. Each live portal stanza must be ADMITTED by the committed CRD.
func TestA4_PortalCorpus_CreateVerbUAFs_Admitted(t *testing.T) {
	for _, c := range loadPortalUAFCorpus(t, filepath.Join("..", "..", "..", "testdata")) {
		if got := validateCEL(t, restActionFromCase(c)); got != "" {
			t.Errorf("A-4 PORTAL CORPUS: the LIVE portal RESTAction %q (%s) was REJECTED by the "+
				"committed CRD schema. Its userAccessFilter uses verb %q to answer \"which "+
				"namespaces may I create this in\" — the api-step returns NAMESPACES and the "+
				"filter checks a DIFFERENT resource, so it is not the scope inversion A-4 "+
				"targets. A blanket read-verb bound on userAccessFilter.verb fails the portal "+
				"upgrade and empties three namespace pickers. Rejection:\n%s",
				c.Name, c.Source, c.UserAccessFilter.Verb, got)
		}
	}
}

// TestA4_UAFVerbBound_NotReintroduced states the same invariant at the schema
// level, so the failure names the cause rather than only the symptom: no rule on
// the API stage may constrain userAccessFilter.verb by value.
func TestA4_UAFVerbBound_NotReintroduced(t *testing.T) {
	for _, verb := range []string{"create", "update", "patch", "delete", "deletecollection"} {
		obj := restActionFromCase(portalUAFCase{
			Name: "synthetic", APIStepName: "ns", APIStepPath: "/api/v1/namespaces", APIStepVerb: "GET",
			UserAccessFilter: struct {
				Verb          string `json:"verb"`
				Group         string `json:"group"`
				Resource      string `json:"resource"`
				ResourcesFrom string `json:"resourcesFrom"`
				NamespaceFrom string `json:"namespaceFrom"`
			}{Verb: verb, Group: "core.krateo.io", Resource: "compositiondefinitions", NamespaceFrom: ".metadata.name"},
		})
		if got := validateCEL(t, obj); got != "" {
			t.Errorf("A-4: userAccessFilter.verb=%q must still be ADMITTED in 1.12.3 — the blanket "+
				"read-verb XValidation was DROPPED because it breaks the live portal. Non-read "+
				"verbs are handled at RUNTIME as a warn-only signal (refilter.go), and 1.13.0 "+
				"enforces the narrower SAME-RESOURCE rule. Rejection:\n%s", verb, got)
		}
	}
}

// TestA4_PreExistingUAFRulesStillFire — the paired arm proving this file is not
// vacuous. The THREE pre-existing userAccessFilter guards must still reject what
// they always rejected; dropping the fourth rule must not have loosened them.
func TestA4_PreExistingUAFRulesStillFire(t *testing.T) {
	base := func() map[string]any {
		return restActionFromCase(portalUAFCase{
			Name: "synthetic", APIStepName: "ns", APIStepPath: "/api/v1/namespaces", APIStepVerb: "GET",
			UserAccessFilter: struct {
				Verb          string `json:"verb"`
				Group         string `json:"group"`
				Resource      string `json:"resource"`
				ResourcesFrom string `json:"resourcesFrom"`
				NamespaceFrom string `json:"namespaceFrom"`
			}{Verb: "list", Group: "core.krateo.io", Resource: "compositiondefinitions", NamespaceFrom: ".metadata.name"},
		})
	}
	step := func(o map[string]any) map[string]any {
		return o["spec"].(map[string]any)["api"].([]any)[0].(map[string]any)
	}

	// Rule 1 — a UAF on a WRITE HTTP stage.
	o := base()
	step(o)["verb"] = "POST"
	if got := validateCEL(t, o); got == "" {
		t.Error("A-4: rule 1 stopped firing — a userAccessFilter on a POST HTTP stage must still be rejected")
	}

	// Rule 2 — exportJwt alongside a UAF.
	o = base()
	step(o)["exportJwt"] = true
	if got := validateCEL(t, o); got == "" {
		t.Error("A-4: rule 2 stopped firing — exportJwt alongside a userAccessFilter must still be rejected")
	}

	// Rule 3 — an empty verb.
	o = base()
	step(o)["userAccessFilter"].(map[string]any)["verb"] = ""
	if got := validateCEL(t, o); got == "" {
		t.Error("A-4: rule 3 stopped firing — an empty userAccessFilter.verb must still be rejected")
	}
}

// TestA4_NoUserAccessFilter_Unaffected — the !has() boundary for the three
// surviving rules: an api-step with no userAccessFilter must be admitted.
func TestA4_NoUserAccessFilter_Unaffected(t *testing.T) {
	if got := validateCEL(t, restActionNoUAF()); got != "" {
		t.Fatalf("A-4 boundary: an api-step with NO userAccessFilter must be ACCEPTED unchanged; the schema rejected it:\n%s", got)
	}
}
