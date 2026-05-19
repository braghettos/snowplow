// cache_mode_test.go — unit tests for the §0.30.93 (Revision 18)
// metadata-only routing predicate.
//
// Covers:
//   - TestShouldUseMetadataOnly_StaticSeedComposition: a GVR in the
//     composition.krateo.io group always routes to metadata-only via
//     the static seed (no annotation required).
//   - TestShouldUseMetadataOnly_RBACReturnsFalse: every RBAC GVR
//     (Role, RoleBinding, ClusterRole, ClusterRoleBinding) is forced
//     onto the full-informer path, even if some future annotation /
//     seed mis-fires.
//   - TestShouldUseMetadataOnly_AnnotationDiscovery: when discovery
//     observes a CRD carrying `krateo.io/cache-mode: metadata`, every
//     served version's GVR is routed to metadata-only.
//   - TestShouldUseMetadataOnly_DefaultFullInformer: GVRs outside the
//     seed and the annotation set route to the default full informer.
//   - TestMetadataOnlyReason_AnnotationVsSeed: log-reason labelling.
//
// Per `feedback_no_special_cases.md`: tests verify the predicate is
// purely annotation- + seed-driven; no per-Resource override path.

package cache

import (
	"context"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// TestShouldUseMetadataOnly_StaticSeedEmpty asserts that the
// composition.krateo.io family — previously hardcoded into the static
// seed per 0.30.93 — now routes to the full Unstructured informer by
// default. Per 2026-05-15 directive: no hardcoded GVR business logic
// in the seed. Customer CRDs opt into metadata-only via the
// `krateo.io/cache-mode: metadata` annotation only.
func TestShouldUseMetadataOnly_StaticSeedEmpty(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()

	cases := []struct {
		name string
		gvr  schema.GroupVersionResource
	}{
		{
			name: "githubscaffoldingwithcompositionpages v1-2-2 (was seed-matched)",
			gvr: schema.GroupVersionResource{
				Group:    "composition.krateo.io",
				Version:  "v1-2-2",
				Resource: "githubscaffoldingwithcompositionpages",
			},
		},
		{
			name: "vmmigration (was seed-matched)",
			gvr: schema.GroupVersionResource{
				Group:    "composition.krateo.io",
				Version:  "v1",
				Resource: "vmmigration",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if shouldUseMetadataOnly(tc.gvr) {
				t.Fatalf("expected shouldUseMetadataOnly(%v) = false (seed is now empty); got true", tc.gvr)
			}
		})
	}
}

// TestStaticSeedIsEmpty is the binding falsifier for the 2026-05-15
// rule: the seed slice must contain zero patterns. If a future
// contributor adds a pattern back without the architectural review
// path, this test fails loudly.
func TestStaticSeedIsEmpty(t *testing.T) {
	if len(metadataOnlyGVRSeed) != 0 {
		t.Fatalf("metadataOnlyGVRSeed must be empty per 2026-05-15 directive; got %d entries: %v",
			len(metadataOnlyGVRSeed), metadataOnlyGVRSeed)
	}
}

// TestShouldUseMetadataOnly_RBACReturnsFalse asserts that the four
// RBAC GVRs ALWAYS route to the full-informer path. The typed-RBAC
// indexer at internal/cache/strip.go reads spec.rules / subjects which
// PartialObjectMetadata does not carry; routing RBAC to metadata-only
// would silently break EvaluateRBAC.
//
// This test is the binding falsifier for the
// `feedback_no_special_cases.md` rule that RBAC routing is hardcoded
// at the API-group level (NOT per-Resource): the assertion covers all
// four RBAC GVRs uniformly.
func TestShouldUseMetadataOnly_RBACReturnsFalse(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()

	for _, gvr := range RBACResourceTypes {
		gvr := gvr
		t.Run(gvr.Resource, func(t *testing.T) {
			if shouldUseMetadataOnly(gvr) {
				t.Fatalf("RBAC GVR %v MUST NOT route metadata-only (typed-RBAC needs spec.rules)", gvr)
			}
		})
	}
}

// TestShouldUseMetadataOnly_RBACReturnsFalseEvenWithAnnotation asserts
// the API-group exclusion takes precedence over the annotation set. A
// malicious / mistaken `krateo.io/cache-mode: metadata` annotation on
// an RBAC CRD MUST NOT break RBAC evaluation.
func TestShouldUseMetadataOnly_RBACReturnsFalseEvenWithAnnotation(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()

	rbac := schema.GroupVersionResource{
		Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles",
	}
	// Forcibly inject the GVR into the annotated set as if discovery
	// had observed an annotated CRD.
	annotatedGVRs.Store(rbac, struct{}{})
	defer resetMetadataOnlyAnnotationsForTest()

	if shouldUseMetadataOnly(rbac) {
		t.Fatalf("RBAC GVR exclusion MUST override annotation set; got metadata-only=true")
	}
}

// fakeCRDLister is the test double for the apiextensions LIST. Returns
// a fixed list (paged once) so discoverMetadataOnlyAnnotationsWithClient
// can iterate without touching apiserver.
type fakeCRDLister struct {
	items []apiextensionsv1.CustomResourceDefinition
	err   error
}

func (f *fakeCRDLister) List(_ context.Context, _ metav1.ListOptions) (*apiextensionsv1.CustomResourceDefinitionList, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &apiextensionsv1.CustomResourceDefinitionList{
		Items: f.items,
	}, nil
}

// TestShouldUseMetadataOnly_AnnotationDiscovery asserts that a CRD
// carrying `krateo.io/cache-mode: metadata` produces a metadata-only
// routing decision for each served version's GVR.
//
// The annotation case is the long-term primary mechanism per plan
// §"Revision 18 implementation outline" item 2.1; the static seed is
// preserved as defensive fallback. This test specifically exercises
// the annotation branch (the CRD is in a customer group, NOT in the
// composition.krateo.io seed, so static-seed would return false).
func TestShouldUseMetadataOnly_AnnotationDiscovery(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()
	defer resetMetadataOnlyAnnotationsForTest()

	crd := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "widgets.example.com",
			Annotations: map[string]string{
				cacheModeAnnotation: cacheModeAnnotationValueMetadata,
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "widgets",
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true},
				{Name: "v1alpha1", Served: true},
				{Name: "v1beta1", Served: false}, // NOT served — must skip
			},
		},
	}
	lister := &fakeCRDLister{items: []apiextensionsv1.CustomResourceDefinition{crd}}
	discoverMetadataOnlyAnnotationsWithClient(context.Background(), lister)

	// The discovery MECHANISM still populates annotatedGVRs from the
	// CRD annotation — verified directly via the sync.Map.
	servedDiscovered := []schema.GroupVersionResource{
		{Group: "example.com", Version: "v1", Resource: "widgets"},
		{Group: "example.com", Version: "v1alpha1", Resource: "widgets"},
	}
	for _, gvr := range servedDiscovered {
		if _, ok := annotatedGVRs.Load(gvr); !ok {
			t.Fatalf("annotation discovery: %v not added to annotatedGVRs — discovery mechanism broken", gvr)
		}
		// Ship H5 — the metadata-only PATH is now inert: shouldUseMetadataOnly
		// returns false for every non-typed-RBAC GVR (Rule 2,
		// !isStreamingException), so even an annotation-discovered GVR
		// does NOT route metadata-only. The discovery mechanism is left
		// intact (a later dead-code-removal ship deletes it) but it can
		// no longer change routing — every non-RBAC GVR streams to bytes.
		if shouldUseMetadataOnly(gvr) {
			t.Fatalf("H5: annotation-discovered %v routed metadata-only — the metadata-only "+
				"path must be inert post-inversion (every non-RBAC GVR streams)", gvr)
		}
	}

	// v1beta1 was not served, so it should NOT be in the annotated set.
	notServed := schema.GroupVersionResource{Group: "example.com", Version: "v1beta1", Resource: "widgets"}
	if _, ok := annotatedGVRs.Load(notServed); ok {
		t.Fatalf("not-served version v1beta1 MUST NOT be added to annotatedGVRs")
	}
}

// TestShouldUseMetadataOnly_AnnotationDiscoveryIgnoresUnannotated
// asserts the predicate stays at the default full-informer path for
// CRDs WITHOUT the annotation, even when other CRDs in the same LIST
// carry it.
func TestShouldUseMetadataOnly_AnnotationDiscoveryIgnoresUnannotated(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()
	defer resetMetadataOnlyAnnotationsForTest()

	crdAnnotated := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "annotated.example.com",
			Annotations: map[string]string{
				cacheModeAnnotation: cacheModeAnnotationValueMetadata,
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "annotated"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true},
			},
		},
	}
	crdPlain := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "plain.example.com",
			// No annotation at all.
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "plains"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true},
			},
		},
	}
	crdWrongValue := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wrongvalue.example.com",
			Annotations: map[string]string{
				cacheModeAnnotation: "something-else",
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "wrongvalues"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Served: true},
			},
		},
	}
	lister := &fakeCRDLister{items: []apiextensionsv1.CustomResourceDefinition{crdAnnotated, crdPlain, crdWrongValue}}
	discoverMetadataOnlyAnnotationsWithClient(context.Background(), lister)

	annotated := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "annotated"}
	plain := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "plains"}
	wrong := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "wrongvalues"}

	// The discovery MECHANISM still distinguishes annotated CRDs from
	// plain / wrong-value ones — verified via annotatedGVRs directly.
	if _, ok := annotatedGVRs.Load(annotated); !ok {
		t.Fatalf("annotated CRD MUST be added to annotatedGVRs by discovery")
	}
	if _, ok := annotatedGVRs.Load(plain); ok {
		t.Fatalf("plain CRD MUST NOT be added to annotatedGVRs")
	}
	if _, ok := annotatedGVRs.Load(wrong); ok {
		t.Fatalf("wrong-value annotation MUST NOT be added to annotatedGVRs")
	}

	// Ship H5 — the metadata-only PATH is inert: shouldUseMetadataOnly
	// returns false for ALL three (every non-RBAC GVR streams). The
	// discovery mechanism is intact but can no longer change routing.
	for _, gvr := range []schema.GroupVersionResource{annotated, plain, wrong} {
		if shouldUseMetadataOnly(gvr) {
			t.Fatalf("H5: %v routed metadata-only — the metadata-only path must be inert", gvr)
		}
	}
}

// TestShouldUseMetadataOnly_DefaultFullInformer asserts that GVRs in
// arbitrary customer groups (no annotation, no seed match) take the
// default full-informer path.
func TestShouldUseMetadataOnly_DefaultFullInformer(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()

	gvr := schema.GroupVersionResource{
		Group:    "customer.example.com",
		Version:  "v1",
		Resource: "widgets",
	}
	if shouldUseMetadataOnly(gvr) {
		t.Fatalf("default routing for non-seed, non-annotated GVR MUST be full informer")
	}
}

// TestDiscoverMetadataOnlyAnnotations_NilConfigNoOp asserts the
// startup-side entry point is safe to call with a nil rest.Config
// (unit-test path where the watcher is exercised without a live
// cluster). The annotated set stays empty; the seed still routes
// Composition GVRs to metadata-only.
func TestDiscoverMetadataOnlyAnnotations_NilConfigNoOp(t *testing.T) {
	resetMetadataOnlyAnnotationsForTest()
	DiscoverMetadataOnlyAnnotations(context.Background(), nil)

	// Annotated set must be untouched.
	gvr := schema.GroupVersionResource{Group: "example.com", Version: "v1", Resource: "widgets"}
	if shouldUseMetadataOnly(gvr) {
		t.Fatalf("nil-config discovery MUST NOT populate the annotated set")
	}
}
