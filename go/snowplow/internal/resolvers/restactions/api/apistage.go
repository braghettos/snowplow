// apistage.go — Ship F1 (0.30.119): content-keyed api-stage L1.
//
// Ship E (0.30.116) keyed the api-stage L1 per-STAGE and per-USER, storing
// each stage's post-filter, N-way-merged dict[id]. Ship F1 reshapes it to
// the CONTENT-KEYED model:
//
//   - Per-K8s-CALL granularity. The content entry is the raw apiserver
//     envelope of ONE dispatch (dispatchViaInformer's return), keyed
//     (gvr, namespace, name-or-empty). A stage with an iterator fans into
//     N calls -> N content entries. One entry per distinct (gvr, ns,
//     [name]) is SHARED across stages, RESTActions, and users.
//
//   - IDENTITY-FREE. K8s RBAC is a binary gate on (gvr, ns, [name]) units
//     — it never filters items or shapes content, so the raw envelope of
//     a K8s call is identity-invariant. ComputeKey drops Username/Groups
//     for CacheEntryClassApistage (a per-class key shape, not a
//     per-resource switch — feedback_no_special_cases). The per-user
//     narrowing moves to the serve-time RBAC gate (resolve.go).
//
//   - Stores the RAW, UN-GATED envelope. The dispatch is un-gated
//     (cache.WithApistageContentResolve makes dispatchViaInformer skip its
//     inline filterListByRBAC/filterGetByRBAC); the per-user gate runs at
//     a single site in resolve.go, on the Get-hit AND the miss path,
//     before the stage filter produces dict[id]. Narrowed content is
//     never stored — no cross-user leak.
//
// EVERYTHING here is gated by cache.ApistageL1Enabled() — on with the
// cache subsystem (folded under the master gate per #57). With the
// resolved-output store off the resolver is byte-identical to 0.30.118
// (AC-F1.1).

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	xcontext "github.com/krateo-platformops/plumbing/context"
	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/plumbing/ptr"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// contentKeyInputs assembles the cache.ResolvedKeyInputs for ONE K8s call
// — the Ship F1 content-keyed unit. The key is (gvr, namespace,
// name-or-empty) under CacheEntryClassApistage; ComputeKey drops the
// identity fields for that class, so the entry is identity-free and
// shared by every user the serve-time gate admits.
//
// name=="" is a LIST call; a non-empty name is a GET-by-name. The
// (gvr, ns, name) tuple fully identifies the K8s call — the stage
// filter (applied post-Get) and the stage-input (which determines WHICH
// calls createRequestOptions emits, each already captured by its own
// tuple) deliberately do NOT enter the content key.
//
// Username/Groups are left zero — ComputeKey ignores them for the
// apistage class. They are omitted entirely rather than threaded-and-
// ignored so the content nature of the key is explicit at the call site.
func contentKeyInputs(gvr schema.GroupVersionResource, namespace, name string) cache.ResolvedKeyInputs {
	return cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           gvr.Group,
		Version:         gvr.Version,
		Resource:        gvr.Resource,
		Namespace:       namespace,
		Name:            name,
	}
}

// gateContentEnvelope applies the serve-time per-user RBAC gate to a raw
// apiserver envelope retrieved from (or about to be stored in) the
// identity-free content layer — the Ship F1 single gate site. It runs on
// BOTH the content-Get-hit path AND the un-gated-dispatch-miss path,
// before the envelope reaches the stage's jsonHandler/filter, so the
// hit-path leak is closed (pre-F1 the inline gate fired only on a miss).
//
// `call` identifies the K8s call: a LIST when ParseAPIServerPathToDep
// yields name=="", a GET-by-name otherwise. The gate is the exact same
// filterListByRBAC / filterGetByRBAC the inline dispatch used pre-F1 —
// pure functions of (raw items, request identity, live RBAC store) — so
// gating the stored raw envelope yields byte-identical narrowing to a
// fresh inline-gated dispatch (AC-F1.2).
//
// Returns (gatedEnvelope, served):
//   - served==false — fail-closed: no identity on ctx, or a GET denied.
//     The caller treats this exactly as dispatchViaInformer's
//     served=false — fall through to the apiserver branch, whose
//     per-user token narrows correctly.
//   - served==true  — gatedEnvelope is the RBAC-narrowed bytes to feed
//     the stage's ResponseHandler.
func gateContentEnvelope(
	ctx context.Context,
	call interface {
		GetPath() string
		GetVerb() string
	},
	raw []byte,
	uafActive bool,
) (any, bool) {
	gvr, _, name, parseOK := cache.ParseAPIServerPathToDep(call.GetPath())
	if !parseOK {
		// Not an apiserver GVR path — the content layer never produced
		// this; pass the bytes through un-gated (defensive — the caller
		// only invokes the gate for content-served calls). Decode to a
		// value so the return type matches the gated paths (Ship 0.30.128
		// P-CORE-2).
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false
		}
		return v, true
	}
	// SECURITY (#58, UAF-AWARE — symmetric with the pre-parsed-LIST branch):
	// a UAF step is served UN-narrowed here; the per-user narrowing is the
	// UAF refilter DOWNSTREAM (which diverges from raw filterGet/ListByRBAC).
	// Applying raw RBAC to a UAF step would strip the UAF-intended scope.
	// So for a UAF step (LIST or GET-by-name) decode + pass through; only a
	// NON-UAF step takes the raw-RBAC gate (which is the over-serve fix for
	// LIST + the unchanged GET narrowing for no-UAF GET).
	if uafActive {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, false
		}
		return v, true
	}
	if name == "" {
		// LIST envelope: {apiVersion, kind, items:[...]}.
		return gateListEnvelope(ctx, gvr, raw)
	}
	// GET-by-name: raw is a single object.
	return gateGetEnvelope(ctx, gvr, raw)
}

// parsedListEnvelope is the Ship 0.30.121 R3 pre-parsed form of a LIST
// content envelope: the items decoded ONCE into []*unstructured plus the
// envelope's apiVersion/kind. parseListEnvelope produces it at the
// content-entry Put site; gateListItems consumes it on every Get-hit so
// the per-hit json.Unmarshal (the ~1.73 GiB double-unmarshal) is gone.
type parsedListEnvelope struct {
	items      []*unstructured.Unstructured
	apiVersion string
	kind       string
}

// parseListEnvelope decodes a raw LIST envelope ONCE — Ship 0.30.121 R3.
// Called at the content-entry Put site so the parsed items can be stored
// on the ResolvedEntry; the content-gate then runs filterListByRBAC over
// the stored items and skips re-unmarshalling on every hit. Returns ok=false
// on a malformed envelope (the caller stores RawJSON only — the gate then
// falls back to the unmarshal path, byte-identical to pre-0.30.121).
func parseListEnvelope(gvr schema.GroupVersionResource, raw []byte) (parsedListEnvelope, bool) {
	var envelope struct {
		APIVersion string           `json:"apiVersion"`
		Kind       string           `json:"kind"`
		Items      []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return parsedListEnvelope{}, false
	}
	items := make([]*unstructured.Unstructured, 0, len(envelope.Items))
	for _, it := range envelope.Items {
		// Ship 2a (0.30.209) — strip managedFields ONCE here, at the
		// item-materialisation site where `it` is still PRIVATE (freshly
		// json.Unmarshal'd, not yet aliased by any serve). After this the
		// shared entry.Items carry no managedFields, so the serve path
		// needs no per-serve removeManagedFields walk (dropped in
		// resolve.go) — that walk wrote the SHARED item maps once the
		// envelope went shallow, racing concurrent reads.
		stripManagedFields(it)
		items = append(items, &unstructured.Unstructured{Object: it})
	}
	apiVersion := envelope.APIVersion
	if apiVersion == "" {
		apiVersion = apiVersionForGVR(gvr)
	}
	kind := envelope.Kind
	if kind == "" {
		kind = listKindForResource(gvr.Resource)
	}
	return parsedListEnvelope{items: items, apiVersion: apiVersion, kind: kind}, true
}

// stripManagedFields removes metadata.managedFields from a single item
// map IN PLACE. It MUST be called only at item-materialisation sites
// (parseListEnvelope, validateClusterListShape, gateGetEnvelope,
// gateListEnvelope), where the item map is freshly json.Unmarshal'd and
// still private — never on a map already aliased by a serve. After this,
// the shared entry.Items maps carry no managedFields, which is the
// invariant the Ship 2a shallow envelope relies on to drop the per-serve
// removeManagedFields walk (resolve.go).
//
// managedFields is a wire field on every apiserver object's metadata; it
// is large, server-managed, and never consumed by widgets/RESTActions, so
// snowplow has always stripped it before serving. Pre-Ship-2a the strip
// happened per-serve on a private deep copy (removeManagedFields(dict));
// Ship 2a moves it to load-time so the shared items are stripped once.
func stripManagedFields(obj map[string]any) {
	if obj == nil {
		return
	}
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		return
	}
	delete(md, "managedFields")
}

// gateListItems runs filterListByRBAC over an ALREADY-PARSED item slice
// and assembles the narrowed set into the apiserver-shaped envelope as a
// DECODED structured value (map[string]any) — Ship 0.30.128 P-CORE-2.
//
// Pre-0.30.128 this re-marshalled the narrowed set to []byte
// (marshalAsList) so the stage jsonHandler could io.ReadAll +
// json.Unmarshal it straight back — a redundant marshal+unmarshal on
// EVERY content-cache hit. It now returns the envelope value directly;
// jsonHandlerValue consumes it with no marshal and no unmarshal. The
// envelope map is built exactly as marshalAsList builds it
// (apiVersion/kind/items), so the eventual served body is byte-identical
// (AC-128.4) — only the decode timing moves.
func gateListItems(ctx context.Context, gvr schema.GroupVersionResource, parsed parsedListEnvelope) (any, bool) {
	kept, identityOK := filterListByRBAC(ctx, gvr, parsed.items)
	if !identityOK {
		// FAIL-CLOSED: no identity — same contract as the inline gate's
		// served=false (caller falls through to the apiserver).
		return nil, false
	}
	return listEnvelopeValue(parsed.apiVersion, parsed.kind, kept), true
}

// listEnvelopeValue assembles the apiserver-shaped LIST envelope as a
// decoded map[string]any — the structured equivalent of marshalAsList's
// input, returned WITHOUT the json.Marshal. Field set + ordering of the
// map are irrelevant to the served body: the stage filter / jsonHandler
// operate on the decoded value and re-encode canonically downstream.
//
// Ship 2a (0.30.209) — SHALLOW envelope. Background: the `items` here is
// the cached content entry's R3 pre-parsed Items, whose it.Object
// map[string]any are OWNED by entry.Items and SHARED across every
// content-Get-hit. From 0.30.130 to 0.30.208 this site deep-copied the
// assembled envelope (maps.DeepCopyJSON, then CopyJSONMap) so each serve
// got a PRIVATE tree gojq could mutate freely — the per-call isolation
// the pre-0.30.128 marshal+unmarshal round-trip provided. That deep copy
// was 45.9% of serve-path allocations / ~20% GC at scale (155 MB/serve),
// and existed ONLY to absorb gojq's in-place writers of the input:
// normalizeNumbers (gone — upstream removed it in v0.12.18; the fork is
// v0.12.19) and deleteEmpty (delpaths/del), whose copy-on-write left
// ALIASED sibling sub-trees and recursed-and-wrote them in place.
//
// Ship 2a removes that last writer: the gojq fork's deleteEmpty is now
// allocator-aware (gojq/func.go) — it CoW-copies any non-gojq-allocated
// node instead of writing it, exactly like the update* family. With NO
// gojq path able to write the input, this site hands back a SHALLOW
// envelope: a fresh outer map + a fresh []any whose elements ALIAS the
// shared it.Object. The served tree is read-only and per-request (it
// flows to the HTTP response; it is never Put into a shared cache), so
// aliasing the shared items is safe. The -race destructive-serve
// falsifier is the hard gate (ship2a CoW falsifier +
// apistage_concurrent_isolation_test). P-CORE-1 (decode-once-at-entry-
// load) is untouched.
func listEnvelopeValue(apiVersion, listKind string, items []*unstructured.Unstructured) map[string]any {
	itemList := make([]any, 0, len(items))
	for _, it := range items {
		if it != nil {
			// SHALLOW: alias the shared it.Object (read-only). gojq cannot
			// mutate it — its only input-writer (deleteEmpty) is now
			// allocator-aware (CoW for non-allocated nodes).
			itemList = append(itemList, it.Object)
		}
	}
	return map[string]any{
		"apiVersion": apiVersion,
		"kind":       listKind,
		"items":      itemList,
	}
}

// gateListEnvelope unmarshals a LIST envelope, runs filterListByRBAC over
// its items with the request identity, and re-marshals the narrowed set
// into the SAME apiserver-shaped envelope (marshalAsList) so the stage
// jsonHandler/filter sees an identical shape to a fresh dispatch.
//
// Ship 0.30.121 R3 — this remains the fallback unmarshal path, used when
// the content entry carries no pre-parsed Items (a malformed-at-Put
// envelope, or a refresh path that stored RawJSON only). The hot path
// (a content-Get-hit on an entry with Items) goes through gateListItems
// and never reaches here. Output is byte-identical between the two.
func gateListEnvelope(ctx context.Context, gvr schema.GroupVersionResource, raw []byte) (any, bool) {
	parsed, ok := parseListEnvelope(gvr, raw)
	if !ok {
		// A malformed stored envelope cannot be vouched for — fail closed.
		return nil, false
	}
	return gateListItems(ctx, gvr, parsed)
}

// gateGetEnvelope runs filterGetByRBAC on a single-object envelope.
// A denied / no-identity object is fail-closed (served=false) — the
// caller falls through to the apiserver, whose per-user token 403s.
func gateGetEnvelope(ctx context.Context, gvr schema.GroupVersionResource, raw []byte) (any, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	// Ship 2a (0.30.209) — the GET-by-name path does NOT flow through the
	// LIST item-strip; strip managedFields here too so dropping the
	// per-serve removeManagedFields walk (resolve.go) does not change the
	// served wire shape for a managedFields-bearing GET. `obj` is freshly
	// json.Unmarshal'd and private at this point.
	stripManagedFields(obj)
	u := &unstructured.Unstructured{Object: obj}
	if !filterGetByRBAC(ctx, gvr, u) {
		return nil, false
	}
	// Ship D.4.2 (0.30.149) — apistage GET-by-name partial-shape
	// guard. EMPIRICALLY GROUNDED at the 0.30.148 burst evidence
	// (site=13 success-path exit): 10/250 served objects for
	// `/v1, Resource=configmaps` had `obj_apiVersion_present=false`
	// AND `obj_apiVersion_is_empty_string=false` AND
	// `obj_apiVersion_type=<nil>`. The map literally LACKS the key
	// (not present-but-empty, not present-but-null).
	//
	// Defect flow (TRACED at design §1.5):
	//   apiserver elides per-item TypeMeta on core-group LIST
	//   responses (k8s wire convention)
	//   → streaming_list.go:507 captures item bytes verbatim
	//   → bytesObject's b.raw lacks apiVersion
	//   → bytesObject.Decode produces map[string]any without
	//     apiVersion key
	//   → dispatchViaInformer's json.Marshal(obj.Object) emits bytes
	//     without apiVersion
	//   → apistage Put stores them
	//   → apistage Get + gateGetEnvelope decodes back
	//   → obj["apiVersion"] is Go nil (untyped nil from absent
	//     map key).
	//
	// PREDICATE: Go nil-check (`obj["apiVersion"] == nil`), NOT D.4's
	// `obj["apiVersion"].(string) == ""`. The narrower predicate fires
	// ONLY on the empirically-observed defect class (present=false).
	// D.4's predicate would have ALSO fired on a present-but-empty-
	// string case that DOES NOT empirically exist (0/250 site=13
	// fires had is_empty_string=true). `feedback_empirical_apiserver
	// _probe_for_predicate_design` requires the predicate's blast
	// radius bounded by empirically observed cases.
	//
	// Truth table:
	//   - key absent → obj["apiVersion"]==any(nil) → predicate fires
	//     ✓ (the empirical defect class).
	//   - key present, JSON null → obj["apiVersion"]==nil → predicate
	//     fires ✓ (defensive; hypothetical, not empirically observed
	//     but caught by the same code).
	//   - key present, string "v1" → obj["apiVersion"]=="v1" → does
	//     NOT fire ✓.
	//   - key present, empty string "" → obj["apiVersion"]=="" →
	//     does NOT fire ✓ (D.4 false-positive class; empirically
	//     never observed; D.4.2 explicitly avoids).
	//
	// Returns (nil, false) — same shape as the filterGetByRBAC deny
	// branch above. apistageContentServe sees served=false; resolve.go
	// falls through to dispatchViaInternalRESTConfig / httpcall.Do; the
	// fresh apiserver GET-by-name response (verified §1.5: kubectl
	// get --raw returns apiVersion + kind populated) feeds the
	// downstream filter with a fully-populated map. ZERO new caller-
	// side code path.
	//
	// CACHE ENTRY PRESERVED: no eviction per
	// feedback_l1_invalidation_delete_only — the partial-shape entry
	// stays in the apistage cache; the next informer UPDATE event
	// dirty-marks the entry; the refresher re-Puts the entry, which
	// will pick up the apiserver's now-fresh GET-by-name shape; the
	// guard ceases firing for that (gvr, ns, name) tuple.
	if obj["apiVersion"] == nil || obj["kind"] == nil {
		cache.RecordApiserverFallthrough(ctx, cache.ReasonApistageGetPartialShape, gvr.String())
		return nil, false
	}
	// Ship 0.30.128 P-CORE-2: return the decoded object value, not the
	// raw bytes — jsonHandlerValue consumes it with no re-unmarshal.
	return obj, true
}

// apiVersionForGVR renders a GVR's apiVersion: "v1" for the core group,
// "group/version" otherwise — the apiserver convention marshalAsList
// expects.
func apiVersionForGVR(gvr schema.GroupVersionResource) string {
	if gvr.Group == "" {
		return gvr.Version
	}
	return gvr.Group + "/" + gvr.Version
}

// callPathVerb adapts an httpcall.RequestOptions to the narrow interface
// gateContentEnvelope consumes — keeps the gate helper free of the
// plumbing httpcall import.
type callPathVerb struct {
	path string
	verb string
}

func (c callPathVerb) GetPath() string { return c.path }
func (c callPathVerb) GetVerb() string {
	if c.verb == "" {
		return http.MethodGet
	}
	return c.verb
}

// apistageContentServe is the Ship F1 content-keyed serve pipeline for
// one K8s call. It is invoked from resolve.go's g.Go worker when the
// resolver pivot is on AND the content-keyed api-stage L1 is enabled.
//
// Returns (value, served, ok):
//   - ok==false  — the content layer cannot serve this call (it is not
//     pivot-servable: a write verb, an external URL, a metadata-only
//     GVR, a pre-sync informer, a non-apiserver path). The caller falls
//     through to the non-pivot dispatch branches, byte-identical to a
//     flag-off resolve.
//   - ok==true, served==false — fail-closed: no identity on ctx, or a
//     GET the requester is denied. The caller falls through to the
//     apiserver branch, whose per-user token narrows correctly.
//   - ok==true, served==true — `value` is the RBAC-GATED envelope as an
//     already-DECODED structured value (Ship 0.30.128 P-CORE-2). The
//     caller feeds it to jsonHandlerValue — no marshal, no unmarshal.
//
// Pipeline (per the F1 spec):
//  1. content key from call.Path via ParseAPIServerPathToDep.
//  2. content Get — HIT: stored entry (R3 pre-parsed Items reused).
//     MISS: dispatch UN-GATED (WithApistageContentResolve) + Put.
//  3. gate with the request identity, returning the DECODED envelope
//     value (the single F1 gate site — runs on hit AND miss).
//
// On the prewarm path (cache.ApistagePrewarmFromContext, wired by F2)
// step 3 is skipped — the prewarm resolve has no requester; it only
// populates the content entry. F1 leaves the skip-point; F2 sets the
// marker.
// uafActive (#58): does the OWNING stage carry a userAccessFilter? It selects
// how a pre-parsed LIST content cell is gated at serve time (see the
// gate-selection comment at the serve branch below). A NON-UAF LIST is
// narrowed by the requester's RAW RBAC (filterListByRBAC via gateListItems);
// a UAF LIST is served UN-narrowed here and narrowed by the UAF refilter
// downstream — the two narrowings DIVERGE for real UAF steps (verb/resource/
// namespace-source differ), so applying raw RBAC to a UAF step would wrongly
// strip the UAF-intended scope (the Diego/arch UAF verdict).
func apistageContentServe(
	ctx context.Context,
	store *cache.ResolvedCacheStore,
	call httpcall.RequestOptions,
	uafActive bool,
) (value any, served bool, ok bool) {
	log := xcontext.Logger(ctx)

	// Ship 0.30.193 Checkpoint 3 — per-call apistageContentServe
	// instrumentation. The sink is installed ONLY on the PIP seed paths
	// (seedOneRestaction, seedOneWidget); production /call requests
	// have no sink → PIPStageTimingSinkFrom returns nil → every
	// Accumulate* below is a nil-receiver no-op (zero overhead beyond
	// one ctx.Value lookup + one nil-check per call).
	//
	// hitObserved is set by the hit branch below; on every return we
	// record (hitObserved, time.Since(t0)). For non-pivot-servable
	// short-circuit returns (write verbs, malformed paths) hitObserved
	// stays false; the sample is recorded as a "miss" (no
	// dispatch/Put — sub-µs cost). That's fine: the sentinel cohort's
	// allCompositions stage is pivot-servable on the cluster-list
	// collapse path; the early-return branches are zero-cost noise.
	pipSink := cache.PIPStageTimingSinkFrom(ctx)
	t0 := time.Now()
	var hitObserved bool
	defer func() {
		pipSink.AccumulateContentServe(hitObserved, time.Since(t0).Milliseconds())
	}()

	// Content-keyed entries describe apiserver GET/LIST calls only — the
	// same shape dispatchViaInformer can serve. A write verb or a
	// non-apiserver path is not a content unit.
	if ptr.Deref(call.Verb, http.MethodGet) != http.MethodGet {
		return nil, false, false
	}
	gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(call.Path)
	if !parseOK {
		return nil, false, false
	}
	contentKey := cache.ComputeKey(contentKeyInputs(gvr, ns, name))
	isList := name == ""

	// Step 2 — content Get / un-gated dispatch + Put.
	//
	// Ship 0.30.121 R3 — for a LIST content entry the items are parsed
	// ONCE here (at the Put site, or carried on the hit entry) so the
	// per-hit gate skips json.Unmarshal. `parsed`/`haveParsed` carry the
	// pre-parsed form when available; `envelope` carries the raw bytes
	// for the GET path and the unmarshal fallback.
	// Ship GMC / 0.30.174 — `entryRef` carries the *cache.ResolvedEntry
	// pointer (HIT: from store.Get; MISS: the just-Put entry) so the
	// per-cohort gate memo (CohortGates) can attach to the entry's own
	// lifetime.
	var envelope []byte
	var parsed parsedListEnvelope
	var haveParsed bool
	var entryRef *cache.ResolvedEntry
	// R1 Layer 1 — refresh-context content-bypass (force a MISS on a content
	// HIT whose OWN dep GVR is the GVR that TRIGGERED this refresher
	// re-resolve). Without it, a whole-RA refresh re-resolve serves this
	// stage from its STORED content bytes (the HIT branch below short-
	// circuits the fresh fetch — the content-shield, R1 §3), so the
	// re-resolve consumes a stale sibling snapshot and re-stores a degraded
	// result. forceContentMiss is true ONLY when:
	//   - ctx carries a refresh trigger GVR (WithRefreshTriggerGVR, set ONLY
	//     by the refresher's re-resolve entry point — NEVER a request-path
	//     /call, so the HIT branch is byte-identical for real traffic), AND
	//   - that trigger GVR == this content unit's own GVR (dep-edge
	//     equality — UNIFORM, no per-resource/path special-case,
	//     feedback_no_special_cases).
	// On a forced miss we fall through to the MISS branch, which re-dispatches
	// fresh + re-Puts, so the whole-RA re-resolve reads the FRESH input.
	forceContentMiss := false
	if tg, ok := cache.RefreshTriggerGVRFromContext(ctx); ok && tg == gvr {
		forceContentMiss = true
	}
	if entry, hit := store.Get(contentKey); hit && entry != nil && !forceContentMiss {
		hitObserved = true // Ship 0.30.193 C3 — observed cache hit.
		envelope = entry.RawJSON
		entryRef = entry
		// R3: a LIST entry stored with pre-parsed Items — gate directly
		// over them, no re-unmarshal. An entry without Items (legacy /
		// refresh-stored / malformed-at-Put) falls back to the RawJSON
		// unmarshal path below.
		if isList && len(entry.Items) > 0 {
			parsed = parsedListEnvelope{
				items:      entry.Items,
				apiVersion: entry.ItemsAPIVersion,
				kind:       entry.ItemsKind,
			}
			haveParsed = true
		}
		// Ship 0.30.212 — idempotent re-record on HIT. Required to converge
		// after rollout for entries Put under an earlier binary (no dep
		// edge) OR via the cluster_list collapse path. sync.Map.LoadOrStore
		// semantics make this a no-op for already-present edges — sub-µs
		// hot-path cost per Deps.Record/RecordList doc.
		if isList {
			cache.Deps().RecordList(contentKey, gvr, ns)
			// Fix #1 (1a) — stale-delete heal/re-touch
			// (docs/rca-stale-delete-compositiondefinitions-informer-2026-06-25.md).
			// A LIST served from this content HIT short-circuits BEFORE
			// dispatchViaInformer, so once the entry was Put while the GVR
			// was not-servable nothing ever re-touches the informer to
			// confirm it — the GVR latches not-servable, its data informer
			// never delivers DELETEs, and the dependent resolved-output LIST
			// entry (e.g. /blueprints) stays stale to TTL. Re-touch the
			// informer here so a registered-but-unconfirmed GVR keeps being
			// nudged onto the discovery-confirm path:
			//
			//   - EnsureResourceType is singleflight-idempotent (sub-µs map
			//     check when already registered — watcher.go:625); it does
			//     NOT change what is served (still this content HIT). This
			//     keeps the GVR registered + on the 30s discovery-refresh
			//     confirm path (servable.go:208), which confirms every
			//     registered GVR — so even a content-only-served LIST is
			//     confirmed within one tick (≤30s) instead of latching
			//     forever. That is the heal.
			//
			// NO per-HIT ConfirmResourceType here (architect N1, gate):
			// ConfirmResourceType runs an UNCACHED ServerResourcesForGroupVersion
			// discovery call, and for a GVR latched not-servable (the bug
			// scenario) `!IsServable` stays true so EVERY content-HIT LIST
			// would issue one — a discovery-storm amplifier on the same GVR
			// class as regression #42 (feedback_bounding_mechanism_discipline:
			// cost MUST be cost-proportional, not per-request-worst-case). The
			// sub-tick prime lives SOLELY in 1b (confirm-on-register, one-shot
			// per registration); here we only keep the GVR registered + on the
			// ticker, which heals within ≤1 tick. The HIT path therefore stays
			// the O(1) EnsureResourceType map check.
			//
			// LIST-ONLY (feedback_bounding_mechanism_discipline + the 1.5.1
			// boot-OOM): this is inside `if isList` (name==""), so the
			// GET-by-name child-resource fan-out (the OOM path) is NEVER
			// re-touched — no child informer is ever forced. AFTER the cache
			// read.
			if rw := cache.Global(); rw != nil {
				rw.EnsureResourceType(gvr)
			}
		} else {
			cache.Deps().Record(contentKey, gvr, ns, name)
		}
		log.Debug("apistage.content_hit",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", ns),
			slog.String("name", name),
			slog.Bool("preparsed", haveParsed),
			slog.String("key_hash", contentKey),
		)
	} else {
		// MISS — dispatch UN-GATED so the stored envelope is identity-free.
		dispatched, dispatchedOK := dispatchViaInformer(
			cache.WithApistageContentResolve(ctx), call)
		if !dispatchedOK {
			// Not pivot-servable — the content layer cannot serve it.
			return nil, false, false
		}
		envelope = dispatched
		// uaf-scope-waiver: PRE-refilter substrate, same reasoning as cluster_list.go — the raw per-K8s-call envelope, identity-free by key, narrowed only downstream on the way to the requester. Never holds refilter output.
		// scope-waiver:TTLOverride: apistage identity-free content substrate — sets its OWN data-plane CATALOG_UNSERVABLE TTLOverride CONDITIONALLY on newEntry below (not a keyed literal element); NOT the UAF-cap class (no BindingUID / no per-user refilter output) — uaf_shortttl.go R-d-4 SITE MAP.
		newEntry := &cache.ResolvedEntry{
			RawJSON: dispatched,
			Inputs:  ptrTo(contentKeyInputs(gvr, ns, name)),
		}
		// R1 Layer 2 (#36): bounded-staleness backstop. If this content
		// entry is being stored while its GVR informer is NOT servable
		// (registered-but-not-synced / watch-broken / unconfirmed — e.g. the
		// core.krateo.io discovery flap that re-latches not-servable), it may
		// be a degraded / not-fully-resolved snapshot. Stamp the short
		// CATALOG_UNSERVABLE_TTL_SECONDS so it self-evicts within the bound
		// even if a dirty-mark / refresh-ordering gap is missed. Uniform
		// over every GVR (no path/resource special-case — feedback_no_special_cases);
		// disabled (TTLOverride stays 0 → standard TTL) when the env is unset.
		if ttl := cache.CatalogUnservableTTL(); ttl > 0 && !cache.Global().IsServable(gvr) {
			newEntry.TTLOverride = ttl
		}
		// R3: parse the LIST envelope ONCE and attach the items to the
		// entry, so every future content-Get-hit gates without a
		// re-unmarshal. A malformed envelope (parseOK=false) stores
		// RawJSON only — the gate then takes the unmarshal fallback.
		if isList {
			if p, parseOK := parseListEnvelope(gvr, dispatched); parseOK {
				newEntry.Items = p.items
				newEntry.ItemsAPIVersion = p.apiVersion
				newEntry.ItemsKind = p.kind
				parsed = p
				haveParsed = true
			}
		}
		store.Put(contentKey, newEntry)
		// Ship 0.30.212 — wire informer-event invalidation for this content
		// entry. Without a dep edge an informer ADD/UPDATE/DELETE on the
		// underlying objects can never dirty-mark this cell, leaving it
		// TTL-stale-forever (F-4 defect). Symmetric with dispatcher L1
		// dep-record at resolve.go:546-562 and widget_content.go:267
		// recordWidgetDeps (AC-G.5 pattern). `isList` was computed above
		// on the same `name == ""` predicate the content layer keys on.
		// Idempotent (sync.Map.LoadOrStore) and sub-µs (two atomic.Add).
		if isList {
			cache.Deps().RecordList(contentKey, gvr, ns)
		} else {
			cache.Deps().Record(contentKey, gvr, ns, name)
		}
		entryRef = newEntry
		log.Debug("apistage.content_store",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("ns", ns),
			slog.String("name", name),
			slog.Bool("preparsed", haveParsed),
			slog.String("key_hash", contentKey),
		)
	}

	// Step 3 — the per-user RBAC gate (skipped on the F2 prewarm path).
	if cache.ApistagePrewarmFromContext(ctx) {
		// Prewarm: no requester — populate the content entry only, do not
		// gate or feed dict[id]. served=false tells the caller not to use
		// the bytes (the prewarm resolve discards dict).
		return nil, false, true
	}
	// R3: a LIST with pre-parsed items gates unmarshal-free; everything
	// else (GET-by-name, or a LIST whose envelope failed to pre-parse)
	// takes gateContentEnvelope's RawJSON path. Ship 0.30.128 P-CORE-2:
	// the gate returns the DECODED envelope value, so the caller feeds it
	// via jsonHandlerValue with no marshal + no unmarshal.
	//
	// SECURITY (#58 — cross-tenant LIST over-serve, UAF-AWARE fix). The
	// apistage content cell is populated SA-MAXIMAL / UN-GATED (the MISS
	// dispatch runs under WithApistageContentResolve, which SKIPS
	// dispatchViaInformer's inline filterListByRBAC — informer_dispatch.go
	// :482/575), so the stored items are the full SA-visible set. The
	// 0.30.242 "narrowed-at-populate, no serve-refilter" claim was WRONG
	// (the cell's apistage BindingUID is "" — contentKeyInputs never sets it
	// — so the key is identity-invariant; nothing narrows at populate). The
	// per-user narrowing MUST run at SERVE — but HOW depends on the stage:
	//
	//   - NON-UAF LIST → gateListItems (filterListByRBAC: the requester's
	//     RAW RBAC, verb=list, the LISTed GVR, item namespace). Restores the
	//     pre-0.30.242 gated serve; `serveParsedListEnvelope` (no filter) was
	//     the over-serve regression → a narrow tenant got the FULL list.
	//
	//   - UAF LIST → serveParsedListEnvelope UNCHANGED (un-narrowed here).
	//     A UAF step is called with ELEVATED privilege precisely because the
	//     requester LACKS the raw RBAC; the per-user narrowing is the UAF
	//     refilter DOWNSTREAM (refilter.go evalSingle: uaf.Verb, uaf.Resource,
	//     NamespaceFrom(item)), which DIVERGES from raw filterListByRBAC for
	//     real UAF steps (verb=create, resource swap, ns from .metadata.name).
	//     Routing a UAF LIST through gateListItems would apply raw RBAC the
	//     requester does not have → strip the UAF-intended scope to
	//     near-empty → the refilter cannot recover it → BREAK UAF. So UAF
	//     LISTs pass through un-narrowed for the refilter to handle (exactly
	//     the pre-0.30.242 UAF behaviour). The TestFalsifier58 UAF-arm (a
	//     DIVERGENT UAF) is the proof this branch is required.
	_ = entryRef
	var gated any
	var gateOK bool
	if haveParsed {
		if uafActive {
			gated, gateOK = serveParsedListEnvelope(parsed)
		} else {
			gated, gateOK = gateListItems(ctx, gvr, parsed)
		}
	} else {
		gated, gateOK = gateContentEnvelope(ctx, callPathVerb{path: call.Path}, envelope, uafActive)
	}
	if !gateOK {
		// Fail-closed — no identity / GET denied. Fall through to apiserver.
		return nil, false, true
	}
	return gated, true, true
}

// serveParsedListEnvelope returns the cell's pre-parsed list items as
// a decoded envelope value with served=true. Ship 0.30.242 H.c-layered
// Phase 2b replacement for the deleted gateListItemsWithMemo: the cell
// holds items that THIS BindingUID's authorisation already permitted
// at populate time (the cell key folds BindingUID, design §3.4), so
// the serve path does NOT re-filter.
//
// The returned envelope is a map shaped like the apiserver's LIST
// response: {"apiVersion", "kind", "items"} with items being the
// []any extraction of the parsed.items slice. The caller
// (jsonHandlerValue) treats this as the decoded envelope.
//
// served=false ONLY when parsed.items is nil/empty — defensive guard
// against a malformed cell. The fail-closed branch falls through to
// apiserver — same shape as gateContentEnvelope.
func serveParsedListEnvelope(parsed parsedListEnvelope) (any, bool) {
	if parsed.items == nil {
		return nil, false
	}
	items := make([]any, 0, len(parsed.items))
	for _, it := range parsed.items {
		if it != nil {
			items = append(items, it.Object)
		}
	}
	envelope := map[string]any{
		"apiVersion": parsed.apiVersion,
		"kind":       parsed.kind,
		"items":      items,
	}
	return envelope, true
}

// ptrTo returns a pointer to v — a local generic helper so the content
// Put can attach a fresh *ResolvedKeyInputs without an addressable temp.
func ptrTo[T any](v T) *T { return &v }

// RefreshContentEntry re-dispatches the single K8s call an api-stage
// content entry caches and returns the fresh RAW envelope — the Ship F1
// refresher seam.
//
// `inputs` is a content-keyed cache.ResolvedKeyInputs
// (CacheEntryClass=="apistage", Group/Version/Resource + Namespace +
// Name-or-empty). The refresh:
//   - reconstructs the apiserver path for that (gvr, ns, name) call;
//   - dispatches it UN-GATED via dispatchViaInformer (the same
//     WithApistageContentResolve marker the request path uses), so the
//     re-Put envelope stays identity-free;
//   - returns the raw envelope for resolveAndPopulateL1 to Put under the
//     content key.
//
// There is NO whole-RESTAction re-run and NO self-hit: a content entry
// is re-dispatched directly, never self-Got. Returns (nil, nil) when the
// call is not pivot-servable (the refresher then skips-to-TTL, not an
// error).
func RefreshContentEntry(ctx context.Context, inputs cache.ResolvedKeyInputs) ([]byte, error) {
	gvr := schema.GroupVersionResource{
		Group:    inputs.Group,
		Version:  inputs.Version,
		Resource: inputs.Resource,
	}
	path := apiserverPathFor(gvr, inputs.Namespace, inputs.Name)
	call := httpcall.RequestOptions{
		RequestInfo: httpcall.RequestInfo{
			Path: path,
			Verb: ptr.To(http.MethodGet),
		},
	}
	raw, served := dispatchViaInformer(cache.WithApistageContentResolve(ctx), call)
	if !served {
		// Not pivot-servable (pre-sync informer, metadata-only GVR, …) —
		// skip-to-TTL. Not an error: a later request re-dispatches.
		return nil, nil
	}
	return raw, nil
}

// ParseListEnvelopeForRefresh — Ship #97 (0.30.214). Pure helper the
// refresher's Put site at resolve_populate.go:255 calls when the entry
// being Put is an apistage-class LIST so it can populate
// ResolvedEntry.Items + ItemsAPIVersion + ItemsKind. Without this, the
// refresher writes RawJSON-only entries, the R3 fast-path predicate at
// apistage.go:487 (`len(entry.Items) > 0`) evaluates false on every
// subsequent content-Get-hit, and the gate falls through to
// gateListEnvelope → parseListEnvelope on the customer request goroutine
// — the 45% cum CPU long-pole captured in the 0.30.212 pre-fix profile
// (ship-97-prefix-falsifier-2026-05-31).
//
// Returns (nil, "", "", false) for:
//   - GET-by-name (inputs.Name != ""), where Items is not the right shape;
//   - a LIST whose envelope fails to parse (e.g. a non-LIST shape that
//     leaked into the apistage class — caller stores RawJSON only and
//     the gate then takes the unmarshal fallback, byte-identical to the
//     pre-fix path).
//
// Pure function over (inputs, raw) — no context. The refresher's
// WithUserInfo / SA-transport identity is unchanged (the parse runs
// inside the refresher goroutine; the resulting Items slice is the
// same Items the MISS branch at apistage.go:530-538 populates). The
// parse cost moves from "per Get-hit on request goroutines" to "per
// Put on refresher goroutines" — strictly lower rate by construction
// (Puts << Gets in steady-state) and consistent with the customer-
// priority invariant (`feedback_customer_priority_over_refresher`).
func ParseListEnvelopeForRefresh(inputs cache.ResolvedKeyInputs, raw []byte) (
	items []*unstructured.Unstructured, apiVersion, kind string, ok bool,
) {
	if inputs.Name != "" {
		// GET-by-name — not a LIST. R3 fast-path keys on LIST envelopes only.
		return nil, "", "", false
	}
	gvr := schema.GroupVersionResource{
		Group:    inputs.Group,
		Version:  inputs.Version,
		Resource: inputs.Resource,
	}
	parsed, parseOK := parseListEnvelope(gvr, raw)
	if !parseOK {
		// Malformed-at-Put envelope. Caller stores RawJSON only — the
		// content gate then takes the unmarshal fallback at hit time,
		// byte-identical to pre-fix behaviour.
		return nil, "", "", false
	}
	return parsed.items, parsed.apiVersion, parsed.kind, true
}

// apiserverPathFor reconstructs the apiserver REST path for a K8s call.
// name=="" yields a collection (LIST) path; a non-empty name yields the
// object (GET-by-name) path. Core group ("") uses the /api/v1 prefix;
// a named group uses /apis/<group>/<version>.
func apiserverPathFor(gvr schema.GroupVersionResource, namespace, name string) string {
	var b []byte
	if gvr.Group == "" {
		b = append(b, "/api/"...)
		b = append(b, gvr.Version...)
	} else {
		b = append(b, "/apis/"...)
		b = append(b, gvr.Group...)
		b = append(b, '/')
		b = append(b, gvr.Version...)
	}
	if namespace != "" {
		b = append(b, "/namespaces/"...)
		b = append(b, namespace...)
	}
	b = append(b, '/')
	b = append(b, gvr.Resource...)
	if name != "" {
		b = append(b, '/')
		b = append(b, name...)
	}
	return string(b)
}

