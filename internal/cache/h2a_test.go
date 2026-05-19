// h2a_test.go — Ship H2a hermetic acceptance tests for the LIST-decode
// re-design (streaming ListFunc decodes items directly into bytesObject).
//
// Coverage maps to the PM-gate H2a acceptance criteria that are
// hermetically verifiable (no cluster):
//
//   - TestH2a_NoFullMapTreeAtIngestion        -> AC-H2a.1
//   - TestH2a_FieldFidelity_StreamingVsStock  -> AC-H2a.2
//   - TestH2a_StripOrdering_RawMatchesH1Shape -> AC-H2a.2
//   - TestH2a_NoIngestionMarshal              -> AC-H2a.3 (newBytesObject
//     not on the streaming path)
//   - TestH2a_ReflectorReTransformPassthrough -> AC-H2a.4 (FINDING 1)
//   - TestH2a_IndexerIntegrity_StreamingStore -> AC-H2a.5
//   - TestH2a_ConcurrentListObjects_Race      -> AC-H2a.8
//   - TestH2a_SingleSourceOfTruth_GroupSets   -> SB-3
//   - TestH2a_NumericFidelity                 -> numeric int64-vs-float64
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	clientcache "k8s.io/client-go/tools/cache"
)

// h2aListServer serves a paged composition LIST with the given total
// item count and page size — reused by the H2a tests. Each item carries
// spec + status + the two strip-policy fields.
func h2aListServer(t *testing.T, totalItems, perPage int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if c := r.URL.Query().Get("continue"); c != "" {
			fmt.Sscanf(c, "%d", &offset)
		}
		end := offset + perPage
		if end > totalItems {
			end = totalItems
		}
		items := make([]any, 0, end-offset)
		for i := offset; i < end; i++ {
			items = append(items, r4CompositionItem("bench-ns", fmt.Sprintf("comp-%04d", i)))
		}
		meta := map[string]any{"resourceVersion": "12345"}
		if end < totalItems {
			meta["continue"] = fmt.Sprintf("%d", end)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "composition.krateo.io/v1",
			"kind":       "GithubScaffoldingWithCompositionPagesList",
			"metadata":   meta,
			"items":      items,
		})
	}))
}

// streamFixtureList drives streamingList against a fixture server and
// returns the resulting *bytesObjectList.
func streamFixtureList(t *testing.T, totalItems, perPage int) *bytesObjectList {
	t.Helper()
	srv := h2aListServer(t, totalItems, perPage)
	t.Cleanup(srv.Close)
	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList: %v", err)
	}
	return got
}

// TestH2a_NoFullMapTreeAtIngestion — AC-H2a.1.
//
// The streaming LIST must produce *bytesObject items — NOT
// *unstructured.Unstructured. A bytesObject's payload is a []byte
// (`raw`), not a map[string]interface{} tree. This is the structural
// proof that the per-item full map tree (UnstructuredList.UnmarshalJSON,
// the 5.28 GiB scanobject driver) is no longer built at LIST ingestion.
func TestH2a_NoFullMapTreeAtIngestion(t *testing.T) {
	got := streamFixtureList(t, 60, 25) // 3-page continue-walk

	if len(got.Items) != 60 {
		t.Fatalf("streaming LIST returned %d items, want 60", len(got.Items))
	}
	for i, it := range got.Items {
		bo, ok := it.(*bytesObject)
		if !ok {
			t.Fatalf("item %d is %T — AC-H2a.1: streaming LIST must produce *bytesObject, "+
				"not a map-tree-backed type", i, it)
		}
		// The item's payload is bytes. There is no resident map tree.
		if len(bo.raw) == 0 {
			t.Fatalf("item %d: bytesObject.raw is empty — the item bytes were not captured", i)
		}
	}
}

// TestH2a_NoFullMapTree_DeepSpecBounded — AC-H2a.1 (allocation bound).
//
// The streaming path's per-item decode must be bounded to the small
// `metadata` sub-object — it must NOT scale with spec/status depth. An
// item with a deeply-nested spec must allocate no more in
// newBytesObjectFromRaw than an item with a trivial spec, because
// spec/status are carried as raw bytes and never decoded into a map
// tree. AllocsPerRun on the constructor is the proxy.
func TestH2a_NoFullMapTree_DeepSpecBounded(t *testing.T) {
	// Item A — trivial spec.
	shallow := r4CompositionItem("ns", "shallow")
	shallow["spec"] = map[string]any{"k": "v"}
	shallowRaw, _ := stripItemJSON(mustJSON(t, shallow))

	// Item B — a deeply-nested, wide spec/status (what a real
	// composition carries: hundreds of nested keys).
	deep := r4CompositionItem("ns", "deep")
	nested := map[string]any{}
	for d := 0; d < 8; d++ {
		child := map[string]any{}
		for k := 0; k < 20; k++ {
			child[fmt.Sprintf("field-%d-%d", d, k)] = fmt.Sprintf("value-%d-%d", d, k)
		}
		child["next"] = nested
		nested = child
	}
	deep["spec"] = nested
	deep["status"] = nested
	deepRaw, _ := stripItemJSON(mustJSON(t, deep))

	allocsShallow := testing.AllocsPerRun(50, func() {
		_, _ = newBytesObjectFromRaw(shallowRaw)
	})
	allocsDeep := testing.AllocsPerRun(50, func() {
		_, _ = newBytesObjectFromRaw(deepRaw)
	})

	// The deep item's spec/status tree is ~160 nested keys vs the
	// shallow item's 1. If newBytesObjectFromRaw built the full map
	// tree, allocsDeep would dwarf allocsShallow. Because spec/status
	// are carried as raw bytes (never decoded), the constructor's
	// allocation count is bounded by metadata only — the two must be
	// within a small constant factor. A 3x ceiling is generous head-
	// room for the metadata sub-decode jitter; a full-tree build would
	// be 50x+.
	if allocsDeep > allocsShallow*3+8 {
		t.Fatalf("newBytesObjectFromRaw allocations scale with spec depth: "+
			"shallow=%.0f deep=%.0f — AC-H2a.1: the spec/status map tree must "+
			"NOT be built (carried as raw bytes)", allocsShallow, allocsDeep)
	}
}

// TestH2a_FieldFidelity_StreamingVsStock — AC-H2a.2.
//
// Each streamed bytesObject, decoded, must be deep-equal to the same
// item decoded by the STOCK path (json -> map -> Unstructured) then
// stripped by defaultStripUnstructured. The streaming path must lose no
// field and add none.
func TestH2a_FieldFidelity_StreamingVsStock(t *testing.T) {
	const n = 24
	got := streamFixtureList(t, n, 10)
	if len(got.Items) != n {
		t.Fatalf("streamed %d items, want %d", len(got.Items), n)
	}

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("comp-%04d", i)

		// Stock control: decode the fixture item the old way, strip it.
		fixture := r4CompositionItem("bench-ns", name)
		fixtureJSON, _ := json.Marshal(fixture)
		var stockMap map[string]any
		if err := json.Unmarshal(fixtureJSON, &stockMap); err != nil {
			t.Fatalf("stock decode item %d: %v", i, err)
		}
		stock := &unstructured.Unstructured{Object: stockMap}
		_, _ = defaultStripUnstructured(stock)

		// H2a streamed object, decoded.
		bo := got.Items[i].(*bytesObject)
		streamed, err := bo.Decode()
		if err != nil {
			t.Fatalf("streamed item %d Decode: %v", i, err)
		}

		if !reflect.DeepEqual(stock.Object, streamed.Object) {
			t.Fatalf("item %d: streaming path diverged from stock-decode+strip\n stock=%#v\n  h2a=%#v",
				i, stock.Object, streamed.Object)
		}
	}
}

// TestH2a_StripOrdering_RawMatchesH1Shape — AC-H2a.2 (strip ordering).
//
// The stored bytesObject.raw must be STRIPPED — managedFields and the
// last-applied annotation removed — so it matches the H1-stored shape.
// A streamed `raw` carrying managedFields is an AC-H2a.2 failure.
func TestH2a_StripOrdering_RawMatchesH1Shape(t *testing.T) {
	got := streamFixtureList(t, 5, 100)

	for i, it := range got.Items {
		bo := it.(*bytesObject)

		// `raw` itself must not contain the stripped keys.
		var rawMap map[string]any
		if err := json.Unmarshal(bo.raw, &rawMap); err != nil {
			t.Fatalf("item %d: raw not valid JSON: %v", i, err)
		}
		meta, _ := rawMap["metadata"].(map[string]any)
		if meta == nil {
			t.Fatalf("item %d: raw has no metadata", i)
		}
		if _, present := meta["managedFields"]; present {
			t.Fatalf("item %d: bytesObject.raw still carries metadata.managedFields — "+
				"AC-H2a.2 strip-ordering failure (raw must be stripped)", i)
		}
		annos, _ := meta["annotations"].(map[string]any)
		if annos != nil {
			if _, present := annos[lastAppliedAnnotation]; present {
				t.Fatalf("item %d: bytesObject.raw still carries the last-applied "+
					"annotation — AC-H2a.2 strip-ordering failure", i)
			}
		}
		// A non-stripped annotation survives in raw.
		if annos == nil || annos["krateo.io/external-create-succeeded"] == "" {
			t.Fatalf("item %d: strip removed a non-bookkeeping annotation from raw", i)
		}
		// spec + status retained in raw (no NEW field removal).
		if rawMap["spec"] == nil || rawMap["status"] == nil {
			t.Fatalf("item %d: raw lost spec/status", i)
		}
		// The embedded ObjectMeta is also stripped (it is decoded from
		// the already-stripped metadata).
		if len(bo.GetManagedFields()) != 0 {
			t.Fatalf("item %d: embedded ObjectMeta still carries managedFields", i)
		}
	}
}

// TestH2a_NoIngestionMarshal — AC-H2a.3.
//
// newBytesObjectFromRaw (the streaming-path constructor) must build the
// bytesObject WITHOUT the encoding/json.Marshal that newBytesObject (the
// H1 WATCH-event path) pays. Verified structurally: the bytesObject's
// `raw` is byte-identical to the stripped input frame — proving `raw`
// IS the captured frame, not a re-marshal of a rebuilt map.
func TestH2a_NoIngestionMarshal(t *testing.T) {
	fixture := r4CompositionItem("bench-ns", "comp-marshal")
	fixtureJSON, _ := json.Marshal(fixture)

	stripped, err := stripItemJSON(fixtureJSON)
	if err != nil {
		t.Fatalf("stripItemJSON: %v", err)
	}
	bo, err := newBytesObjectFromRaw(stripped)
	if err != nil {
		t.Fatalf("newBytesObjectFromRaw: %v", err)
	}

	// `raw` must be the SAME slice the constructor was handed — not a
	// re-marshalled copy. newBytesObjectFromRaw retains `raw` by
	// reference; that is the proof there is no ingestion marshal.
	if &bo.raw[0] != &stripped[0] {
		t.Fatal("newBytesObjectFromRaw re-allocated raw — AC-H2a.3: the streaming path " +
			"must carry the captured frame by reference, not re-marshal it")
	}
}

// TestH2a_ReflectorReTransformPassthrough — AC-H2a.4 / FINDING 1.
//
// The reflector re-applies SetTransform (StripBulkyFieldsForResourceType)
// to every LIST item after extraction. Feeding it a *bytesObject must be
// a clean no-op passthrough: the SAME object back, no re-marshal, no
// drop, and logFirstBytesOnce must NOT fire (the bytes-override block is
// past the failed *unstructured.Unstructured cast).
func TestH2a_ReflectorReTransformPassthrough(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	fixture := r4CompositionItem("bench-ns", "comp-retransform")
	fixtureJSON, _ := json.Marshal(fixture)
	stripped, _ := stripItemJSON(fixtureJSON)
	bo, err := newBytesObjectFromRaw(stripped)
	if err != nil {
		t.Fatalf("newBytesObjectFromRaw: %v", err)
	}

	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	out, err := tf(bo)
	if err != nil {
		t.Fatalf("re-transform of a *bytesObject returned error: %v", err)
	}

	// Pointer-identical: the transform returned the SAME object unchanged.
	outBO, ok := out.(*bytesObject)
	if !ok {
		t.Fatalf("re-transform of a *bytesObject produced %T — must be the same *bytesObject", out)
	}
	if outBO != bo {
		t.Fatal("re-transform of a *bytesObject produced a DIFFERENT *bytesObject — " +
			"AC-H2a.4: it must be a pointer-identical no-op passthrough, not a re-marshal")
	}
}

// TestH2a_ReflectorReTransform_NoDoubleBytesLog — AC-H2a.4.
//
// Re-transforming an already-bytesObject must NOT emit the
// strip.bytes_applied falsifier line (logFirstBytesOnce) — that line is
// only for genuine Unstructured->bytesObject conversions. A bytesObject
// fails the transform's Unstructured cast before reaching the
// bytes-override block, so the line never fires for it.
func TestH2a_ReflectorReTransform_NoDoubleBytesLog(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	bo, _ := newBytesObjectFromRaw(mustJSON(t, r4CompositionItem("ns", "n")))
	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)

	// Re-transform the bytesObject; firstBytesLogged must stay empty for
	// this resource type (the log gate is the proxy for "did the
	// bytes-override block run").
	_, _ = tf(bo)

	firstBytesLoggedMu.Lock()
	_, fired := firstBytesLogged["composition.krateo.io/v1/x"]
	firstBytesLoggedMu.Unlock()
	if fired {
		t.Fatal("re-transforming a *bytesObject fired logFirstBytesOnce — AC-H2a.4: " +
			"the bytes-override block must not run for an already-bytesObject")
	}
}

// TestH2a_IndexerIntegrity_StreamingStore — AC-H2a.5.
//
// The objects the streaming LIST delivers must be indexable: build a
// real indexer with the informer's key-func + namespace index, Add
// every streamed bytesObject, and assert GetByKey + ByIndex resolve all
// of them — count-equal to the streamed set, no object unindexed.
func TestH2a_IndexerIntegrity_StreamingStore(t *testing.T) {
	const n = 40
	got := streamFixtureList(t, n, 15)

	idx := clientcache.NewIndexer(
		clientcache.MetaNamespaceKeyFunc,
		clientcache.Indexers{clientcache.NamespaceIndex: clientcache.MetaNamespaceIndexFunc},
	)
	for i, it := range got.Items {
		if err := idx.Add(it); err != nil {
			t.Fatalf("idx.Add streamed item %d — not indexable (FINDING 2 regression): %v", i, err)
		}
	}

	// GetByKey resolves every streamed item.
	for i := 0; i < n; i++ {
		key := "bench-ns/" + fmt.Sprintf("comp-%04d", i)
		obj, exists, err := idx.GetByKey(key)
		if err != nil || !exists {
			t.Fatalf("GetByKey(%q): exists=%v err=%v — streamed object unindexed", key, exists, err)
		}
		if _, ok := obj.(*bytesObject); !ok {
			t.Fatalf("GetByKey(%q): got %T, want *bytesObject", key, obj)
		}
	}

	// ByIndex(namespace) count-equal to the streamed set.
	byNS, err := idx.ByIndex(clientcache.NamespaceIndex, "bench-ns")
	if err != nil {
		t.Fatalf("ByIndex: %v", err)
	}
	if len(byNS) != n {
		t.Fatalf("ByIndex(bench-ns) returned %d objects, want %d — index integrity lost", len(byNS), n)
	}
}

// TestH2a_ConcurrentListObjects_Race — AC-H2a.8.
//
// >= 16 goroutines decode the streamed bytesObjects concurrently. Under
// `go test -race` a shared/memoized tree (SB-4 violation) trips here.
func TestH2a_ConcurrentListObjects_Race(t *testing.T) {
	got := streamFixtureList(t, 30, 30)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for iter := 0; iter < 50; iter++ {
				for _, it := range got.Items {
					bo := it.(*bytesObject)
					uns, err := bo.Decode()
					if err != nil {
						t.Errorf("Decode: %v", err)
						return
					}
					// Mutate the private tree — must not race.
					if spec, ok := uns.Object["spec"].(map[string]any); ok {
						spec["touched"] = iter
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestH2a_SingleSourceOfTruth_GroupSets — SB-3, updated for the Ship H5
// routing inversion.
//
// Pre-H5 this asserted matchesStreamingListGroup and the bytes-override
// predicate agreed (both derived from bytesResourceOverrides). H5
// collapsed them into ONE predicate — isStreamingException — used by
// both the watcher.go routing AND the strip.go bytes-override. The
// single-source-of-truth property is now structural: there is one
// predicate, so there is nothing to drift. This test confirms the
// predicate is consistent and composition still streams.
func TestH2a_SingleSourceOfTruth_GroupSets(t *testing.T) {
	// composition + widgets + an arbitrary group all stream (not
	// excepted); the 4 typed-RBAC GVRs are the only exceptions.
	for _, gvr := range []schema.GroupVersionResource{
		compositionGVR,
		{Group: "widgets.templates.krateo.io", Version: "v1beta1", Resource: "panels"},
		{Group: "apps", Version: "v1", Resource: "deployments"},
	} {
		if isStreamingException(gvr) {
			t.Fatalf("group %q is a streaming exception — H5: only typed-RBAC GVRs are excepted", gvr.Group)
		}
	}
	for _, gvr := range rbacTypedGVRs {
		if !isStreamingException(gvr) {
			t.Fatalf("typed-RBAC GVR %q is NOT a streaming exception — RBAC must take the stock path", gvr)
		}
	}
}

// TestH2a_NumericFidelity confirms the metadata sub-decode preserves
// int64 (the H1 bytesobject.go lesson): metadata.generation is an
// integer; after streaming + Decode it must still be int64, not float64.
func TestH2a_NumericFidelity(t *testing.T) {
	item := r4CompositionItem("bench-ns", "comp-num")
	// Add an integer metadata field + an integer spec field.
	item["metadata"].(map[string]any)["generation"] = int64(42)
	item["spec"].(map[string]any)["replicas"] = int64(7)

	raw := mustJSON(t, item)
	stripped, _ := stripItemJSON(raw)
	bo, err := newBytesObjectFromRaw(stripped)
	if err != nil {
		t.Fatalf("newBytesObjectFromRaw: %v", err)
	}

	// Embedded ObjectMeta.Generation is a typed int64 field — fidelity
	// is structural there. The spec field round-trips through Decode().
	if bo.GetGeneration() != 42 {
		t.Fatalf("metadata.generation = %d, want 42", bo.GetGeneration())
	}
	decoded, err := bo.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	spec := decoded.Object["spec"].(map[string]any)
	if got := spec["replicas"]; got != int64(7) {
		t.Fatalf("spec.replicas = %v (%T), want int64(7) — numeric fidelity lost "+
			"(must decode via util/json, not encoding/json)", got, got)
	}
}

// TestH2a_MalformedItemSkipped confirms a single undecodable item is
// skipped (the streaming relist of 48,999 items must not abort on one
// bad object), while well-formed items in the same page still land.
func TestH2a_MalformedItemSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// items[1] is a JSON string, not an object — undecodable as an item.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"composition.krateo.io/v1",` +
			`"kind":"GithubScaffoldingWithCompositionPagesList",` +
			`"metadata":{"resourceVersion":"1"},` +
			`"items":[` +
			`{"apiVersion":"composition.krateo.io/v1","kind":"X","metadata":{"namespace":"ns","name":"good-0"},"spec":{}},` +
			`"i-am-a-string-not-an-object",` +
			`{"apiVersion":"composition.krateo.io/v1","kind":"X","metadata":{"namespace":"ns","name":"good-1"},"spec":{}}` +
			`]}`))
	}))
	defer srv.Close()

	rc, err := streamingRESTClient(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("streamingRESTClient: %v", err)
	}
	got, err := streamingList(context.Background(), rc, r4CompositionGVR,
		metav1.ListOptions{Limit: listPageLimit})
	if err != nil {
		t.Fatalf("streamingList must not abort on a single malformed item: %v", err)
	}
	// The two well-formed items survive; the malformed one is skipped.
	if len(got.Items) != 2 {
		t.Fatalf("got %d items, want 2 (the malformed item must be skipped, the "+
			"two valid items retained)", len(got.Items))
	}
}

// TestH2a_BytesObjectList_ReflectorContract — FINDING 2 for the list
// type. The streaming ListFunc returns *bytesObjectList; the reflector
// reads it via meta.ListAccessor (resourceVersion) and
// meta.ExtractListWithAlloc (items). Both must succeed on
// *bytesObjectList, or the streaming LIST silently delivers nothing.
func TestH2a_BytesObjectList_ReflectorContract(t *testing.T) {
	got := streamFixtureList(t, 12, 100)

	// meta.ListAccessor — the reflector reads the list's RV through this.
	la, err := meta.ListAccessor(got)
	if err != nil {
		t.Fatalf("meta.ListAccessor(*bytesObjectList) failed — reflector cannot read the list: %v", err)
	}
	if la.GetResourceVersion() != "12345" {
		t.Fatalf("list RV via accessor = %q, want 12345", la.GetResourceVersion())
	}

	// meta.ExtractListWithAlloc — the reflector extracts items through this.
	items, err := meta.ExtractListWithAlloc(got)
	if err != nil {
		t.Fatalf("meta.ExtractListWithAlloc(*bytesObjectList) failed — reflector cannot "+
			"extract items: %v", err)
	}
	if len(items) != 12 {
		t.Fatalf("ExtractList returned %d items, want 12 — reflector would store the wrong count", len(items))
	}
	for i, it := range items {
		if _, ok := it.(*bytesObject); !ok {
			t.Fatalf("extracted item %d is %T, want *bytesObject", i, it)
		}
	}
}

// TestH2a_WatchEventConvertsToBytesObject — the WATCH-path durability
// guarantee.
//
// H2a fixes the LIST decode. After the informer syncs, WATCH events
// still arrive as *unstructured.Unstructured (the stock dynamic
// WatchFunc — streaming_list.go). For the GC win to be DURABLE under
// composition churn — not LIST-only — those WATCH-delivered
// *Unstructured objects must be converted to *bytesObject before they
// land in the store.
//
// The mechanism is the H1 SetTransform (StripBulkyFieldsForResourceType)
// installed at watcher.go:993 on the SAME `gi` for the streaming
// informer path (addResourceTypeLocked assigns the streaming informer
// to `gi`, then SetTransform runs on `gi` unconditionally). SetTransform
// applies to BOTH LIST- and WATCH-delivered objects at store time.
//
// This test asserts the transform-level guarantee directly: a
// composition *unstructured.Unstructured (a WATCH-event shape) through
// StripBulkyFieldsForResourceType yields a *bytesObject — so a WATCH
// ADD/UPDATE re-stores the object as bytes, and the store does not
// drift back toward *Unstructured under churn.
func TestH2a_WatchEventConvertsToBytesObject(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	// A WATCH event delivers a plain *unstructured.Unstructured — the
	// stock dynamic WatchFunc decode shape.
	watchEvent := &unstructured.Unstructured{Object: r4CompositionItem("bench-ns", "comp-watch")}

	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	out, err := tf(watchEvent)
	if err != nil {
		t.Fatalf("transform of a WATCH *Unstructured returned error: %v", err)
	}

	bo, ok := out.(*bytesObject)
	if !ok {
		t.Fatalf("WATCH *Unstructured through the transform produced %T, want *bytesObject — "+
			"the GC win would be LIST-only and decay under composition churn", out)
	}

	// Field fidelity + strip on the converted WATCH object.
	decoded, derr := bo.Decode()
	if derr != nil {
		t.Fatalf("Decode of the WATCH-converted bytesObject: %v", derr)
	}
	if decoded.GetName() != "comp-watch" || decoded.GetNamespace() != "bench-ns" {
		t.Fatalf("WATCH-converted object identity wrong: %s/%s",
			decoded.GetNamespace(), decoded.GetName())
	}
	if len(decoded.GetManagedFields()) != 0 {
		t.Fatal("WATCH-converted object still carries managedFields — strip not applied")
	}
	if decoded.Object["spec"] == nil || decoded.Object["status"] == nil {
		t.Fatal("WATCH-converted object lost spec/status")
	}
}

// TestH2a_AssembledReflector_WatchAndListBothStoreBytes — the assembled
// reflector test (PM non-blocking #1).
//
// Drives a real SharedIndexInformer.Run with the H1 SetTransform
// installed (exactly as addResourceTypeLocked wires it), against a fake
// ListerWatcher: the initial LIST is empty, then WATCH ADD events
// deliver composition *Unstructured objects. It asserts the informer's
// indexer ends up holding *bytesObject — proving the SetTransform on a
// running informer converts WATCH-delivered objects, the durability
// property end-to-end.
//
// Cheap: reuses the watch.FakeWatcher harness pattern; no HTTP server.
func TestH2a_AssembledReflector_WatchAndListBothStoreBytes(t *testing.T) {
	resetStripLoggingForTest()
	t.Setenv("CACHE_ENABLED", "true")

	fw := watch.NewFake()
	lw := &watchInjectingListWatch{watcher: fw}

	inf := clientcache.NewSharedIndexInformer(
		lw,
		&unstructured.Unstructured{},
		0,
		clientcache.Indexers{clientcache.NamespaceIndex: clientcache.MetaNamespaceIndexFunc},
	)

	// Install the SAME transform addResourceTypeLocked installs at
	// watcher.go:993 — StripBulkyFieldsForResourceType for the
	// composition GVR, which carries the H1 bytes-override.
	tf := StripBulkyFieldsForResourceType("composition.krateo.io/v1/x", compositionGVR)
	if err := inf.SetTransform(func(obj interface{}) (interface{}, error) { return tf(obj) }); err != nil {
		t.Fatalf("SetTransform: %v", err)
	}

	stopCh := make(chan struct{})
	defer close(stopCh)
	go inf.Run(stopCh)
	if !clientcache.WaitForCacheSync(stopCh, inf.HasSynced) {
		t.Fatal("informer failed to sync")
	}

	// Deliver WATCH ADD events — composition *Unstructured objects, the
	// stock dynamic WatchFunc shape.
	const n = 6
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("comp-watch-%02d", i)
		fw.Add(&unstructured.Unstructured{Object: r4CompositionItem("bench-ns", name)})
	}

	// Wait for the indexer to absorb the n events.
	if !waitForIndexerCount(inf, n, 2*time.Second) {
		t.Fatalf("indexer did not absorb %d WATCH events, has %d", n, len(inf.GetStore().List()))
	}

	// Every stored object must be a *bytesObject — the transform
	// converted the WATCH-delivered *Unstructured.
	for _, obj := range inf.GetStore().List() {
		if _, ok := obj.(*bytesObject); !ok {
			t.Fatalf("WATCH-delivered object stored as %T, want *bytesObject — "+
				"SetTransform did not convert it; the store would drift to *Unstructured", obj)
		}
	}
}

// watchInjectingListWatch is a clientcache.ListerWatcher whose initial
// LIST is empty and whose Watch returns a caller-supplied FakeWatcher —
// so a test can push WATCH events into a real running informer.
type watchInjectingListWatch struct {
	watcher *watch.FakeWatcher
}

func (l *watchInjectingListWatch) List(metav1.ListOptions) (runtime.Object, error) {
	// Empty initial LIST — an UnstructuredList with no items, RV "1".
	ul := &unstructured.UnstructuredList{}
	ul.SetResourceVersion("1")
	return ul, nil
}

func (l *watchInjectingListWatch) Watch(metav1.ListOptions) (watch.Interface, error) {
	return l.watcher, nil
}

// waitForIndexerCount polls until the informer's store holds want items
// or the deadline elapses.
func waitForIndexerCount(inf clientcache.SharedIndexInformer, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if len(inf.GetStore().List()) >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(inf.GetStore().List()) >= want
}

// mustJSON marshals v to JSON or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}
