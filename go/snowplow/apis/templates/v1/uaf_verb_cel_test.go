// uaf_verb_cel_test.go — A-4 (1.12.3) ADMISSION falsifier for the read-verb
// bound on userAccessFilter.verb.
//
// WHY A REAL CEL DRIVE, NOT A STRING GREP. Asserting that the generated YAML
// CONTAINS the rule text proves only that a marker was typed; it cannot tell a
// correct rule from one that parses but never rejects anything (a mis-scoped
// `self`, a `has()` short-circuit that always yields true, a typo'd field
// path). Per feedback_falsifier_must_drive_real_boundary_not_install_crossed_state
// this arm drives the REAL admission boundary: it loads the COMMITTED generated
// CRD, builds the same structural schema the apiserver builds, and runs the
// apiserver's own CEL validator over whole RESTAction objects. A rule that does
// not actually reject `verb: create` fails here.
//
// The arms:
//
//	(i)  verb: create  → REJECTED, and the rejection cites the A-4 rule (not one
//	     of the three pre-existing userAccessFilter rules, which would mean the
//	     arm is passing for the wrong reason).
//	(ii) verb: list    → ACCEPTED (no CEL error at all) — proves the rule is not
//	     an indiscriminate deny.
//	(iii) the full write-verb corpus + an upper-case "GET" → all REJECTED.
//	(iv) the full read-verb corpus (get/list/watch) → all ACCEPTED.
//	(v)  a stage with NO userAccessFilter → ACCEPTED, proving the rule's
//	     !has() guard does not leak onto the ~99% of api-steps that have no
//	     filter at all.
//
// RED on origin/main: without the fourth XValidation marker, arm (i) admits
// `verb: create` and fails with "want REJECTED, got ACCEPTED".

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

// celPerCallLimit / celCostBudget mirror the apiserver's own defaults closely
// enough for a handful of rules over a two-stage object; they only bound
// runtime cost, never which rules run.
const (
	celPerCallLimit = uint64(1_000_000)
	celCostBudget   = int64(10_000_000)
)

// loadRESTActionCELValidator builds the apiserver's CEL validator from the
// COMMITTED generated CRD — the same artifact the snowplow-crds chart ships,
// so this arm fails if the marker is present in core.go but the YAML was never
// regenerated (the CI drift guard's twin, from the admission side).
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

// restActionWithUAFVerb builds a complete, otherwise-VALID RESTAction carrying a
// single api-step whose userAccessFilter checks `verb`. Everything else is
// deliberately well-formed (GET stage, no exportJwt, a non-empty resource) so
// the ONLY rule that can possibly fire is the A-4 verb bound — if any other
// userAccessFilter rule fires, the arm's message-matching catches it rather
// than silently passing.
func restActionWithUAFVerb(verb string) map[string]any {
	return map[string]any{
		"apiVersion": "templates.krateo.io/v1",
		"kind":       "RESTAction",
		"metadata":   map[string]any{"name": "a4-probe", "namespace": "demo-system"},
		"spec": map[string]any{
			"api": []any{
				map[string]any{
					"name": "compositions",
					"path": "/apis/composition.krateo.io/v1alpha1/compositions",
					"verb": "GET",
					"userAccessFilter": map[string]any{
						"group":         "composition.krateo.io",
						"resource":      "compositions",
						"verb":          verb,
						"namespaceFrom": ".metadata.namespace",
					},
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
		"metadata":   map[string]any{"name": "a4-probe", "namespace": "demo-system"},
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
		t.Fatal("A-4 setup: the generated CRD carries NO CEL rules at all — the validator is nil, so no admission bound exists")
	}
	errs, _ := validator.Validate(context.Background(), field.NewPath(""), structural, obj, nil, celCostBudget)
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return strings.Join(msgs, "\n")
}

// a4RuleFingerprint is a distinctive fragment of the A-4 message. Matching it
// (rather than merely "some error occurred") is what stops the arm passing
// because one of the three PRE-EXISTING userAccessFilter rules fired instead.
const a4RuleFingerprint = "must be a READ verb"

// TestA4_UAFVerbNonRead_RejectedByCRDSchema — the make-or-break admission arm.
func TestA4_UAFVerbNonRead_RejectedByCRDSchema(t *testing.T) {
	// (i) create → REJECTED, by the A-4 rule specifically.
	got := validateCEL(t, restActionWithUAFVerb("create"))
	if got == "" {
		t.Fatalf("A-4 (i): an api-step with userAccessFilter.verb=create must be REJECTED by the CRD schema; the CEL validator ADMITTED it. Without the read-verb XValidation the refilter checks a WRITE grant on a read path — every object the requester may create is admitted into the filtered response (scope inversion)")
	}
	if !strings.Contains(got, a4RuleFingerprint) {
		t.Fatalf("A-4 (i): verb=create was rejected, but NOT by the A-4 read-verb rule — the arm would pass for the wrong reason. Rejection text:\n%s", got)
	}

	// (ii) list → ACCEPTED. Discriminates an indiscriminate deny.
	if got := validateCEL(t, restActionWithUAFVerb("list")); got != "" {
		t.Fatalf("A-4 (ii): userAccessFilter.verb=list is a legitimate read filter and must be ACCEPTED; the schema rejected it:\n%s", got)
	}
}

// TestA4_UAFVerbCorpus_ReadAcceptedWriteRejected — the corpus arm. Every write
// verb the CRD previously admitted must now be rejected, and every read verb
// must still be accepted. "GET" (upper-case) is in the REJECT set on purpose:
// the RBAC evaluator matches verbs by exact lower-case equality, so an
// upper-case verb matches no PolicyRule and silently denies every item —
// rejecting it at admission surfaces the typo instead of shipping a filter that
// returns an empty list forever.
func TestA4_UAFVerbCorpus_ReadAcceptedWriteRejected(t *testing.T) {
	rejected := []string{
		"create", "update", "patch", "delete", "deletecollection",
		"*", "GET", "List", "impersonate", "escalate", "bind",
	}
	for _, verb := range rejected {
		got := validateCEL(t, restActionWithUAFVerb(verb))
		if got == "" {
			t.Errorf("A-4 corpus: userAccessFilter.verb=%q must be REJECTED (not a lower-case read verb); the schema ADMITTED it", verb)
			continue
		}
		if !strings.Contains(got, a4RuleFingerprint) {
			t.Errorf("A-4 corpus: verb=%q was rejected by some OTHER rule, not the A-4 read-verb bound:\n%s", verb, got)
		}
	}

	for _, verb := range []string{"get", "list", "watch"} {
		if got := validateCEL(t, restActionWithUAFVerb(verb)); got != "" {
			t.Errorf("A-4 corpus: userAccessFilter.verb=%q is a read verb and must be ACCEPTED; got:\n%s", verb, got)
		}
	}
}

// TestA4_NoUserAccessFilter_Unaffected — the !has() boundary. An api-step with
// no userAccessFilter at all (the overwhelming majority of the corpus) must be
// admitted unchanged; a rule missing its !has() guard would reject every one of
// them and take the whole product down.
func TestA4_NoUserAccessFilter_Unaffected(t *testing.T) {
	if got := validateCEL(t, restActionNoUAF()); got != "" {
		t.Fatalf("A-4 boundary: an api-step with NO userAccessFilter must be ACCEPTED unchanged; the schema rejected it:\n%s", got)
	}
}
