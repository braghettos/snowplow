// cache_mode.go — Tag 0.30.93 (Revision 18): cache-mode discriminator for
// `EnsureResourceType` routing.
//
// The §0.30.93 binding is to route high-cardinality, "DepTracker-only"
// GVRs onto a metadata-only informer (PartialObjectMetadata) instead of
// the default dynamic full-Unstructured informer. The motivation is the
// 0.30.92 OOM finding (helm rev 108): 49K compositions × ~20 KiB
// post-strip residual = ~1 GiB on the indexer alone, plus a resolver
// LIST cascade racing the initial sync ⇒ container hit its 2 Gi limit.
//
// The discriminator is **annotation-driven**, NOT per-Resource. Per
// `feedback_no_special_cases.md` the rule lives in cluster state
// (`krateo.io/cache-mode: metadata` on a CRD), and snowplow only carries
// a static seed of GVR-pattern matchers for the Krateo Composition
// family (which is generated at runtime by `core-provider`; per
// `project_no_upstream_authority.md` we cannot patch core-provider to
// emit the annotation today).
//
// Two-tier predicate (plan §"Revision 18 implementation outline" item 2):
//
//  1. ANNOTATION (long-term primary). Snowplow lists CRDs at startup via
//     the apiextensions client; any CRD carrying the annotation
//     `krateo.io/cache-mode: metadata` is registered in the
//     metadata-only set. Customer CRDs without it use the default full
//     informer.
//
//  2. STATIC-SEED FALLBACK (operationally-safe-today). Snowplow ships a
//     small list of GVR-pattern matchers (Group + Resource-prefix)
//     covering Krateo's Composition family. The seed is GVR-pattern
//     (not exact-GVR) to tolerate the per-CompositionDefinition version
//     suffix (`v1-2-2`, `v12-8-3`, ...). The seed remains live even
//     after the annotation ships upstream — `shouldUseMetadataOnly(gvr)
//     = annotated(gvr) OR matchesSeed(gvr)`.
//
// RBAC GVRs (Role, RoleBinding, ClusterRole, ClusterRoleBinding) are
// NEVER metadata-only: the typed-RBAC indexer needs `spec.rules[]` and
// `subjects[]` from the full object. The predicate enforces this by
// returning false for the rbac.authorization.k8s.io API group.
//
// Concurrency: the package-level discoverer is a sync.Map keyed by GVR;
// reads on the predicate hot path are lock-free. Discovery runs once at
// startup (one apiextensions LIST) and is non-blocking on the dispatcher
// hot path — predicate falls back to the static seed if discovery has
// not completed.
//
// Per `feedback_no_special_cases.md`: every per-Resource decision is
// expressed as predicate-input (annotation OR seed-pattern), NOT as a
// per-Resource switch statement in the routing code path.

package cache

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

// cacheModeAnnotation is the CRD annotation key opting a GVR into the
// metadata-only informer path. Per plan §"Revision 18 implementation
// outline" item 2.1.
const cacheModeAnnotation = "krateo.io/cache-mode"

// cacheModeAnnotationValueMetadata is the annotation value (must match
// exactly — empty or any other value keeps the default full informer).
const cacheModeAnnotationValueMetadata = "metadata"

// gvrPattern matches a GVR by Group + Resource-prefix. Used by the
// static seed so a single entry covers every CompositionDefinition
// version of a given Composition family (e.g.
// `githubscaffoldingwithcompositionpages.composition.krateo.io` regardless
// of the per-version Resource suffix `v1-2-2`, `v12-8-3`, ...).
//
// The matcher is `gvr.Group == Group && strings.HasPrefix(gvr.Resource,
// ResourcePrefix)`. Version is intentionally NOT a discriminator: the
// metadata-only routing applies regardless of CRD version.
type gvrPattern struct {
	Group          string
	ResourcePrefix string
}

// metadataOnlyGVRSeed is intentionally empty. The static-seed mechanism
// is retained so a future pattern can be added if a structural reason
// emerges, but per Diego's 2026-05-15 directive — and the original
// 2026-05-14 rejection of the composition.krateo.io seed as "still
// business logic hardcoded" — no hardcoded GVR patterns ship in this
// list. Metadata-only routing is opt-in via the CRD annotation
// `krateo.io/cache-mode: metadata` only.
//
// Trade-off accepted: composition.krateo.io now defaults to full
// Unstructured informer. At 50K composition scale this adds ~1 GiB to
// the indexer footprint. The 8 GiB chart limit (4 GiB request)
// provides headroom; the 0.30.92 OOM at 2 GiB no longer applies. The
// resolver pivot to lister-only reads benefits directly — without this
// removal, compositions-list would fall back to apiserver. With it,
// every K8s read shape A-D in the resolver can serve from the lister.
//
// Per `feedback_no_special_cases.md`: the predicate stays a uniform
// pattern matcher; what changes is that the data feeding it is now
// empty. Adding patterns here is reserved for structural needs (e.g.,
// a customer CRD whose annotation cannot be set upstream and that
// presents an OOM-class footprint) — not for product-family carve-outs.
var metadataOnlyGVRSeed = []gvrPattern{}

// annotatedGVRs is the runtime-discovered set of GVRs whose CRD carries
// `krateo.io/cache-mode: metadata`. Populated once at startup by
// `DiscoverMetadataOnlyAnnotations`; reads are lock-free via sync.Map.
//
// We use sync.Map (not a plain map under rw.mu) because:
//   - Discovery writes happen once, at startup, on the same goroutine
//     as the watcher constructor. Reads happen on every hot-path call
//     to shouldUseMetadataOnly (i.e. once per first-touch of a GVR).
//   - sync.Map's read-mostly path is allocation-free and lock-free.
//   - We do NOT want the routing predicate to contend on rw.mu — the
//     watcher already holds that lock during EnsureResourceType's
//     singleflight, and a second lock would introduce a deadlock risk.
//
// Keyed by schema.GroupVersionResource (struct comparable in Go).
var annotatedGVRs sync.Map // map[schema.GroupVersionResource]struct{}

// shouldUseMetadataOnly returns true when the GVR should be routed onto
// the PartialObjectMetadata informer (10× smaller per-object footprint;
// satisfies the DepTracker but NOT typed-RBAC nor resolver GetObject
// reads).
//
// Decision rule:
//
//   1. RBAC GVRs are NEVER metadata-only (typed-RBAC needs spec/rules).
//   2. bytes-override groups are NEVER metadata-only (Ship H1 — see
//      below): the bytes representation needs the FULL object.
//   3. If the annotated set contains this exact GVR, return true.
//   4. If any seed pattern matches (Group + Resource-prefix), return true.
//   5. Otherwise, return false (default: full Unstructured informer).
//
// Per the binding constraint in `feedback_no_special_cases.md`: this
// function is the only place per-GVR routing logic lives. `EnsureResourceType`
// is a uniform plumbing call that consults the predicate — there is no
// per-Resource if-elif chain in the watcher code.
//
// Safe for concurrent use. Fast path (~ns per call) on the watcher hot
// path; consults sync.Map.Load and a small fixed loop over seed
// patterns (currently 1 entry).
func shouldUseMetadataOnly(gvr schema.GroupVersionResource) bool {
	// Rule 1: RBAC GVRs are never metadata-only. The typed-RBAC
	// indexer at `internal/cache/strip.go` reads spec.rules + subjects
	// off the full typed object on every event; metadata-only would
	// silently break RBAC evaluation. We hardcode the rbac API group
	// (NOT per-Resource — the whole `rbac.authorization.k8s.io` group
	// is full-informer-only by construction).
	if gvr.Group == "rbac.authorization.k8s.io" {
		return false
	}

	// Rule 2 (Ship H1 B1 — predicate updated for the Ship H5 routing
	// inversion): a bytes-streaming GVR is NEVER metadata-only. The two
	// routings are MUTUALLY EXCLUSIVE BY DESIGN and this is the single
	// point that encodes it:
	//
	//   - metadata-only stores *metav1.PartialObjectMetadata — it
	//     DROPS spec/status. It also SKIPS SetTransform
	//     (addResourceTypeMetadataOnlyLocked, watcher.go) — so the
	//     bytes-override, which lives inside the TransformFunc, would
	//     never run.
	//   - the bytes representation KEEPS every field (the resolver
	//     needs the full spec/status; metadata-only would break the
	//     resolver).
	//
	// H1 keyed this on the per-group bytesResourceOverrides allow-list.
	// Ship H5 inverted routing — bytes-streaming is the default for
	// every non-typed-RBAC GVR — and deleted the allow-list. The guard
	// is re-expressed on the SAME single predicate the routing now uses:
	// `!isStreamingException(gvr)` is true for exactly the GVRs that
	// stream to bytes. This is the faithful evolution of the B1 guard,
	// not a change to shouldUseMetadataOnly's structure: post-H5 only
	// the 4 typed-RBAC GVRs are non-streaming, and Rule 1 already
	// returns false for those — so the metadata-only path is inert
	// (no GVR reaches a `return true`). The metadata-only mechanism is
	// superseded; a later dead-code-removal ship deletes it. H5 leaves
	// it inert and does not restructure shouldUseMetadataOnly.
	//
	// Checked BEFORE the annotation/seed rules so it strictly wins —
	// a `krateo.io/cache-mode: metadata` annotation cannot route a
	// streaming GVR to the metadata-only path.
	if !isStreamingException(gvr) {
		return false
	}

	// Rule 3: annotation-driven (runtime-discovered).
	if _, ok := annotatedGVRs.Load(gvr); ok {
		return true
	}

	// Rule 4: static-seed (GVR-pattern).
	for _, pat := range metadataOnlyGVRSeed {
		if matchesSeed(gvr, pat) {
			return true
		}
	}

	// Rule 5: default — full informer.
	return false
}

// matchesSeed implements the GVR-pattern match: same Group + Resource
// starts with ResourcePrefix.
//
// Empty ResourcePrefix matches every Resource in the group (the current
// seed entry's behaviour for `composition.krateo.io`). Empty Group only
// matches the core group ("") — not exposed today but defined for
// future seed entries.
func matchesSeed(gvr schema.GroupVersionResource, pat gvrPattern) bool {
	if gvr.Group != pat.Group {
		return false
	}
	return strings.HasPrefix(gvr.Resource, pat.ResourcePrefix)
}

// DiscoverMetadataOnlyAnnotations populates the annotated-GVR set by
// listing CRDs via the apiextensions client and inspecting their
// `krateo.io/cache-mode` annotation. Idempotent: re-calling overwrites
// any prior state (the set is the union of new discovery + the existing
// static seed at predicate-evaluation time).
//
// Operationally non-fatal: any error here (apiextensions LIST failure,
// missing CRD informer kind, etc.) leaves the annotated set empty.
// `shouldUseMetadataOnly` still returns true for seed-matching GVRs, so
// the OOM-safety property holds without a working discovery client.
//
// Called once at startup from main.go after `cache.SetGlobal(w)`. Must
// run in a bounded context — discovery walks every CRD in the cluster
// (~hundreds typical, low single thousands worst-case at customer
// scale); a single apiextensions LIST is bounded by listPageLimit
// paging.
//
// Per the plan §"Revision 18 implementation outline" item 2.1 +
// `feedback_no_special_cases.md`: the annotation key is the
// discriminator, NOT the Resource name. Customer CRDs without the
// annotation are unaffected.
func DiscoverMetadataOnlyAnnotations(ctx context.Context, cfg *rest.Config) {
	if cfg == nil {
		slog.Debug("cache.discover_metadata_only.skip",
			slog.String("subsystem", "cache"),
			slog.String("reason", "nil rest.Config — annotation discovery skipped, seed-only routing"))
		return
	}
	clientset, err := apiextensionsclientset.NewForConfig(cfg)
	if err != nil {
		slog.Warn("cache.discover_metadata_only.client_construct_failed",
			slog.String("subsystem", "cache"),
			slog.String("error", err.Error()),
			slog.String("hint", "annotation discovery offline; seed-only routing remains active"))
		return
	}
	discoverMetadataOnlyAnnotationsWithClient(ctx, clientset.ApiextensionsV1().CustomResourceDefinitions())
}

// crdLister is the minimal API surface DiscoverMetadataOnlyAnnotations
// needs from the apiextensions client. Extracted as an interface so
// unit tests can inject a fake LIST without spinning the full
// apiextensions client.
//
// The single method matches the apiextensions clientset signature for
// `CustomResourceDefinitions().List(ctx, opts)`.
type crdLister interface {
	List(ctx context.Context, opts metav1.ListOptions) (*apiextensionsv1.CustomResourceDefinitionList, error)
}

// discoverMetadataOnlyAnnotationsWithClient is the testable inner loop.
// Iterates the CRD LIST (bounded paging), extracts the GVRs of any CRD
// carrying `krateo.io/cache-mode: metadata`, and stores them in
// annotatedGVRs.
//
// Per-CRD annotation read uses GetAnnotations() (lives on ObjectMeta);
// per-CRD GVR derivation expands each `spec.versions[]` so version
// fan-out is explicit (a CRD with v1+v1beta1 produces two GVR entries).
//
// Concurrency: this function is the sole writer of annotatedGVRs. It
// MUST run once at startup. Subsequent calls overwrite — useful for
// tests; in production the watcher constructor only calls it once.
func discoverMetadataOnlyAnnotationsWithClient(ctx context.Context, lister crdLister) {
	var continueToken string
	var discovered int
	for {
		list, err := lister.List(ctx, metav1.ListOptions{
			Limit:    listPageLimit,
			Continue: continueToken,
		})
		if err != nil {
			slog.Warn("cache.discover_metadata_only.list_failed",
				slog.String("subsystem", "cache"),
				slog.String("error", err.Error()),
				slog.Int("discovered_before_failure", discovered),
				slog.String("hint", "partial annotation set; seed remains active for OOM-safety"))
			return
		}
		for i := range list.Items {
			crd := &list.Items[i]
			ann := crd.GetAnnotations()
			if ann[cacheModeAnnotation] != cacheModeAnnotationValueMetadata {
				continue
			}
			// Expand spec.versions[] into individual GVR entries; one
			// CRD ⇒ multiple GVRs when multiple versions are served.
			group := crd.Spec.Group
			resource := crd.Spec.Names.Plural
			for j := range crd.Spec.Versions {
				v := &crd.Spec.Versions[j]
				if !v.Served {
					continue
				}
				gvr := schema.GroupVersionResource{
					Group:    group,
					Version:  v.Name,
					Resource: resource,
				}
				annotatedGVRs.Store(gvr, struct{}{})
				discovered++
				slog.Info("cache.discover_metadata_only.found",
					slog.String("subsystem", "cache"),
					slog.String("gvr", gvr.String()),
					slog.String("crd", crd.Name),
					slog.String("reason", "annotation"))
			}
		}
		continueToken = list.GetContinue()
		if continueToken == "" {
			break
		}
	}
	slog.Info("cache.discover_metadata_only.complete",
		slog.String("subsystem", "cache"),
		slog.Int("annotated_gvrs", discovered),
		slog.Int("seed_patterns", len(metadataOnlyGVRSeed)),
		slog.String("hint", "metadata-only routing active for annotated set ∪ seed"))
}

// resetMetadataOnlyAnnotationsForTest clears the annotated-GVR set.
// Test-only entry point so unit tests can run hermetically. NOT
// exported beyond the package — production code MUST NOT clear the
// runtime state.
//
// Per `feedback_no_special_cases.md` we keep the test helper unexported
// so it cannot leak into a per-Resource production override.
func resetMetadataOnlyAnnotationsForTest() {
	annotatedGVRs.Range(func(k, _ interface{}) bool {
		annotatedGVRs.Delete(k)
		return true
	})
}
