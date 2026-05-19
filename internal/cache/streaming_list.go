// streaming_list.go — Ship 0.30.122 R4 Lever 1: a streaming ListWatch
// for the composition GVR's dynamic informer.
//
// THE OOM (0.30.121): with PREWARM_ENABLED=true the dynamic informer's
// initial relist of the 48,999-composition fixture ran at pod startup.
// The standard NewFilteredDynamicInformer ListFunc calls
// dynamic.Interface.List, which inside client-go:
//   (a) io.ReadAll's the whole HTTP response body of a page;
//   (b) unmarshals it into a full []map[string]any (the pre-transform
//       Unstructured set), via a RawMessage intermediate;
//   (c) hands the materialised *UnstructuredList back to the reflector,
//       which only THEN applies the SetTransform strip.
// Every page's full pre-transform objects are alive simultaneously with
// their post-transform copies — the transient duplication that peaked at
// ~17.7 GiB and OOMKilled the pod 4x. Field projection / metadata-only
// is OFF THE TABLE (Diego's hard constraint: full spec+status retained),
// so R4 bounds the *relist transient* instead.
//
// THE FIX (Option A — PM-confirmed): a custom ListFunc that
//   1. issues the paged LIST itself — a `continue`-token walk, identical
//      LIST semantics to client-go's ListPager;
//   2. streams each page's HTTP response body through a json.Decoder —
//      Token() down to the `items` array, then captures each element's
//      RAW JSON BYTES one at a time (never io.ReadAll of the whole
//      body);
//   3. (H2a) builds a *bytesObject directly from each item's raw bytes
//      — decoding ONLY the small `metadata` sub-object, never the
//      spec/status map tree;
//   4. drops the per-element raw slice's decoder buffer before decoding
//      the next element;
//   5. accumulates only *bytesObject values into the returned
//      *bytesObjectList.
//
// Ship H2a (the LIST-decode re-design). H1 (0.30.131) converted the
// resident store to bytesObject but FAILED its scanobject HARD STOP
// (62%->77%): the re-diagnosis traces proved the scanobject driver is
// the LIST-decode map[string]interface{} tree — UnstructuredList.
// UnmarshalJSON, 5.28 GiB / 56% of heap — NOT the resident store H1
// fixed. H1 also ADDED an 845 MB ingestion json.Marshal (newBytesObject
// re-marshalling an already-built Unstructured).
//
// H2a eliminates BOTH: the streaming ListFunc no longer builds a
// per-item map[string]interface{} (step 2-3 above) — it captures the
// item's raw JSON frame and constructs the bytesObject directly via
// newBytesObjectFromRaw (metadata-only sub-decode). The
// UnstructuredList.UnmarshalJSON driver and H1's ingestion marshal are
// both gone from the LIST path.
//
// The reflector still re-applies SetTransform (StripBulkyFieldsFor-
// ResourceType) on the returned items — a *bytesObject fails that
// transform's `obj.(*unstructured.Unstructured)` cast and is returned
// UNCHANGED (a clean idempotent passthrough — AC-H2a.4): no re-marshal,
// no drop. The strip policy itself is applied at the JSON-bytes level
// inside the streaming decoder (stripItemJSON), so the stored `raw`
// matches the H1-stored shape exactly (managedFields + last-applied
// removed). The WatchFunc is the standard dynamic watch, unchanged —
// the re-diagnosis proved the watch decoder is 0.091% of allocations,
// not a driver; WATCH events still flow through H1's newBytesObject.
//
// Gated by RESOLVER_COMPOSITION_STREAMING_LIST (default ON). Flag off, or
// no *rest.Config wired, reverts the composition GVR to the standard
// NewFilteredDynamicInformer (AC-7).

package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/rest"
	clientcache "k8s.io/client-go/tools/cache"
)

// envCompositionStreamingList gates the streaming ListWatch.
// Default ON — the streaming path IS the fix. Set to "false" to revert
// to the standard NewFilteredDynamicInformer.
//
// Ship H5 — SCOPE NOTE: since the H5 routing inversion this toggle
// governs the streaming path for EVERY dynamic informer, not only the
// composition group. The env-var name `RESOLVER_COMPOSITION_STREAMING_LIST`
// is therefore a misnomer post-H5 — a rename is deliberately deferred
// (it touches the Helm chart configmap, out of H5's code-only scope;
// feedback_chart_only_for_snowplow). A later ship renames it (e.g.
// STREAMING_LIST_ENABLED). The behaviour is correct; only the name
// lags.
const envCompositionStreamingList = "RESOLVER_COMPOSITION_STREAMING_LIST"

// compositionStreamingListEnabled reports whether the streaming
// ListWatch is enabled (default true). Post-H5 it gates streaming for
// every dynamic informer (see envCompositionStreamingList's scope note).
func compositionStreamingListEnabled() bool {
	return boolFromEnv(envCompositionStreamingList, true)
}

// NOTE — the streaming ListWatch reuses the shared listOptionsTweak /
// global listPageLimit (watcher.go); there is NO streaming-only
// page-limit knob, by design. Under the streaming json.Decoder the
// per-page transient is OBJECT-sized (the decoder's token buffer holds
// ~one composition object at a time), NOT page-sized — so a 500-object
// page and a 100-object page produce identical snowplow memory. A
// separate page-limit constant/config would buy zero memory and only
// multiply round-trips, so none is added.

// Ship H5 — the streaming-routing predicate is no longer a positive
// group allow-list. `matchesStreamingListGroup` is DELETED: streaming
// is the default for every dynamic GVR, and the routing decision is
// `!isStreamingException(gvr)` (strip.go) — one predicate shared by the
// watcher.go informer routing AND the strip.go bytes-override re-gate.
// See isStreamingException for the full inversion rationale.

// streamingDynamicInformer is the R4 GenericInformer wrapper — the exact
// shape of client-go's unexported dynamicInformer, but built around a
// ListWatch whose ListFunc streams. Satisfies informers.GenericInformer
// so addResourceTypeLocked can store it in rw.informers transparently.
type streamingDynamicInformer struct {
	informer clientcache.SharedIndexInformer
	gvr      schema.GroupVersionResource
}

func (d *streamingDynamicInformer) Informer() clientcache.SharedIndexInformer {
	return d.informer
}

// Lister returns a GenericLister over this informer's indexer.
//
// Ship H1 — B2 SILENT-DROP GUARD (made unconditional at Ship H5):
// dynamiclister.New produces a lister whose List/Get type-assert every
// indexer value to *unstructured.Unstructured. A streamingDynamicInformer's
// store holds *bytesObject — its streaming ListFunc builds bytesObjects
// directly (H2a) and the strip.go bytes-override converts its WATCH
// events too. A dynamiclister would therefore SILENTLY DROP every
// bytesObject — an empty/short list, no crash, no log: the FINDING 1
// silent-drop defect class, on a path the five watcher.go cast sites do
// not cover.
//
// Ship H5 — the panic is now UNCONDITIONAL. Pre-H5 a streamingDynamic-
// Informer was constructed only for the bytesResourceOverrides allow-list,
// so the guard keyed on group membership. Post-H5 the routing inversion
// means a streamingDynamicInformer is constructed ONLY for non-exception
// (bytes-streaming) GVRs — every streaming informer's store is bytes-
// backed without exception. So Lister() on this type is always the
// silent-drop trap; the guard panics unconditionally.
//
// Verified at H1 ship time, re-checked at H5: there are ZERO production
// callers of Lister() — every cache read goes through GetObject /
// ListObjects / GetTypedObject / ListTypedObjects (which read
// GetIndexer() directly and decode-on-access). So this panic is
// unreachable today.
//
// It is retained as a LOUD trap: a future caller invoking Lister() fails
// immediately with a diagnostic pointing at the fix — rather than
// silently returning a truncated list. A future caller that genuinely
// needs a lister here must add a bytesObject-aware lister (decode-on-
// access). Per feedback_no_park_broken_behind_flag: a known silent-drop
// trap is made loud, not left latent.
func (d *streamingDynamicInformer) Lister() clientcache.GenericLister {
	panic("cache: streamingDynamicInformer.Lister() called for " +
		d.gvr.String() + " — a streaming informer's store is *bytesObject-backed " +
		"and dynamiclister would silently drop every value. Read via " +
		"ResourceWatcher.ListObjects / GetObject (decode-on-access) or add a " +
		"bytesObject-aware lister; do NOT use dynamiclister here.")
}

var _ informers.GenericInformer = &streamingDynamicInformer{}

// newStreamingDynamicInformer builds a standalone dynamic informer for
// gvr whose ListFunc streams (R4 Lever 1). Returns (nil, false) when the
// streaming path cannot be constructed (no *rest.Config, REST client
// build failure) so the caller can fall back to the standard informer.
//
// The WatchFunc is byte-identical to NewFilteredDynamicInformer's — only
// the ListFunc is replaced. tweak is the shared listOptionsTweak so the
// streaming LIST carries the SAME paging policy as every other informer
// (no streaming-only page-limit constant — see the NOTE above).
func newStreamingDynamicInformer(
	rc *rest.Config,
	dyn dynamic.Interface,
	gvr schema.GroupVersionResource,
	indexers clientcache.Indexers,
	tweak func(*metav1.ListOptions),
) (informers.GenericInformer, bool) {
	if rc == nil || dyn == nil {
		return nil, false
	}
	restClient, err := streamingRESTClient(rc)
	if err != nil {
		slog.Warn("cache.streaming_list.rest_client_failed",
			slog.String("subsystem", "cache"),
			slog.String("gvr", gvr.String()),
			slog.String("error", err.Error()),
			slog.String("effect", "falling back to the standard dynamic informer for this GVR"),
		)
		return nil, false
	}

	lw := &clientcache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			if tweak != nil {
				tweak(&options)
			}
			list, err := streamingList(context.TODO(), restClient, gvr, options)
			if err != nil {
				// Return an untyped nil on the error path — never a
				// typed-nil *bytesObjectList wrapped in a non-nil
				// runtime.Object interface (the reflector checks err
				// first, but an explicit untyped nil is unambiguous).
				return nil, err
			}
			return list, nil
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			// Standard dynamic watch — unchanged from
			// NewFilteredDynamicInformer. A WATCH stream is already
			// incremental (one event at a time); only the initial LIST
			// needed the streaming treatment.
			if tweak != nil {
				tweak(&options)
			}
			return dyn.Resource(gvr).Namespace(metav1.NamespaceAll).Watch(context.TODO(), options)
		},
	}

	informer := clientcache.NewSharedIndexInformerWithOptions(
		lw,
		&unstructured.Unstructured{},
		clientcache.SharedIndexInformerOptions{
			ResyncPeriod:      0, // pure event-driven — matches the factory
			Indexers:          indexers,
			ObjectDescription: gvr.String(),
		},
	)
	return &streamingDynamicInformer{informer: informer, gvr: gvr}, true
}

// streamingRESTClient builds a rest.RESTClient for raw apiserver GETs
// from rc. It mirrors how dynamic.NewForConfig configures its client —
// an unversioned REST client with a no-op codec, since streamingList
// reads the body itself rather than letting client-go decode it.
func streamingRESTClient(rc *rest.Config) (*rest.RESTClient, error) {
	cfg := rest.CopyConfig(rc)
	cfg.GroupVersion = &schema.GroupVersion{}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(runtime.NewScheme()).WithoutConversion()
	if cfg.UserAgent == "" {
		cfg.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	return rest.RESTClientFor(cfg)
}

// streamingList issues the paged LIST for gvr and streams every page's
// response body through a json.Decoder, building one *bytesObject per
// item directly from its raw JSON frame (H2a). The returned
// *bytesObjectList holds only bytes-backed objects; no full
// pre-transform map[string]interface{} copy of the 48,999-object set is
// ever alive, and no per-item map tree is built at all.
//
// Paging: a `continue`-token walk, identical semantics to client-go's
// ListPager — each page carries opts.Limit (listPageLimit) and the
// previous page's metadata.continue token until the apiserver returns an
// empty continue.
func streamingList(
	ctx context.Context,
	rc *rest.RESTClient,
	gvr schema.GroupVersionResource,
	opts metav1.ListOptions,
) (*bytesObjectList, error) {
	out := &bytesObjectList{}
	pages := 0
	totalItems := 0

	for {
		pageOpts := opts // value copy — only the continue token changes per page
		body, err := streamingListPageBody(ctx, rc, gvr, pageOpts)
		if err != nil {
			return nil, err
		}

		cont, rv, apiVersion, kind, perr := decodeListPageStreaming(body, out, &totalItems)
		// Always close the body before evaluating the parse error so a
		// failed page cannot leak the HTTP connection.
		_ = body.Close()
		if perr != nil {
			return nil, fmt.Errorf("streaming list %s page %d: %w", gvr.String(), pages, perr)
		}

		// BLOCKER 2 (re-review) — never serve an error envelope as a
		// (truncated) list. rest.Request.Stream already returns an error
		// for a non-2xx response, but a 200 response can still carry a
		// metav1 `kind: Status` object (a proxy, a watch-cache edge, a
		// misbehaving apiserver). That envelope has no `items`, so the
		// streaming decoder would silently produce a zero/partial list
		// with continueToken=="" — the loop would break and streamingList
		// would return (out, nil). A Status on page 50 of ~490 → the
		// informer relists to a TRUNCATED composition set and serves it
		// as authoritative (the S4-class "partial looks complete" trap).
		// Fail the WHOLE list instead — never break-and-return-partial.
		if kind == "Status" {
			return nil, fmt.Errorf("streaming list %s page %d: apiserver returned a Status "+
				"envelope, not a List — failing the whole list rather than serving a "+
				"truncated set", gvr.String(), pages)
		}
		pages++

		// Carry envelope identity onto the result list ONCE, from the
		// FIRST page only (the same one-shot pattern for all three).
		// bytesObjectList embeds metav1.TypeMeta (APIVersion/Kind are
		// plain exported fields) + metav1.ListMeta (ResourceVersion via
		// Get/SetResourceVersion).
		if out.APIVersion == "" && apiVersion != "" {
			out.APIVersion = apiVersion
		}
		if out.Kind == "" && kind != "" {
			out.Kind = kind
		}
		// BLOCKER 1 (re-review) — the list's resourceVersion MUST be the
		// FIRST page's, not last-write-wins. For a paged collection LIST
		// the apiserver pins a snapshot RV at the first page (embedded in
		// the continue token); every page reports that same snapshot RV.
		// Last-write-wins is fragile: if any page returns a differing RV
		// (a watch-cache edge, a 410-retry) the list would end with the
		// wrong RV and the informer's subsequent WATCH would start from
		// it → missed/duplicated events, silent cache drift. Capture it
		// once, from page 1, with the same `== ""` guard as apiVersion/kind.
		if out.GetResourceVersion() == "" && rv != "" {
			out.SetResourceVersion(rv)
		}

		if cont == "" {
			break
		}
		opts.Continue = cont
		// Subsequent pages must NOT re-send a resourceVersion — the
		// continue token fully determines the page (apiserver contract).
		opts.ResourceVersion = ""
	}

	slog.Info("cache.streaming_list.completed",
		slog.String("subsystem", "cache"),
		slog.String("gvr", gvr.String()),
		slog.Int("pages", pages),
		slog.Int("items", totalItems),
		slog.Int64("page_limit", opts.Limit),
		slog.String("note", "paged LIST streamed item-by-item — no full pre-transform materialisation"),
	)
	return out, nil
}

// streamingListPageBody issues ONE page's raw HTTP GET and returns the
// response body as a stream. The caller MUST Close it.
func streamingListPageBody(
	ctx context.Context,
	rc *rest.RESTClient,
	gvr schema.GroupVersionResource,
	opts metav1.ListOptions,
) (io.ReadCloser, error) {
	req := rc.Get().
		AbsPath(streamingListAbsPath(gvr)...).
		SpecificallyVersionedParams(&opts, paramCodec, metav1.SchemeGroupVersion)
	return req.Stream(ctx)
}

// streamingListAbsPath builds the apiserver collection path segments for
// a cluster-wide LIST of gvr. Core group ("") uses /api/<version>; a
// named group uses /apis/<group>/<version>. The composition GVR is
// always a named group, but the core branch keeps the helper general.
func streamingListAbsPath(gvr schema.GroupVersionResource) []string {
	if gvr.Group == "" {
		return []string{"api", gvr.Version, gvr.Resource}
	}
	return []string{"apis", gvr.Group, gvr.Version, gvr.Resource}
}

// decodeListPageStreaming consumes one LIST page's response body with a
// streaming json.Decoder. It walks the top-level object, and when it
// reaches the `items` array it captures ONE element's raw JSON bytes at
// a time, strips them at the JSON-bytes level, builds a *bytesObject
// directly (H2a — no map[string]interface{} tree), appends it to
// out.Items, and drops the per-element raw slice before decoding the
// next — so the page's full pre-transform set is never simultaneously
// alive AND no per-item map tree is ever built.
//
// Returns (continueToken, resourceVersion, apiVersion, kind, err).
func decodeListPageStreaming(
	body io.Reader,
	out *bytesObjectList,
	totalItems *int,
) (continueToken, resourceVersion, apiVersion, kind string, err error) {
	dec := json.NewDecoder(body)

	// Expect the opening '{' of the LIST envelope.
	tok, err := dec.Token()
	if err != nil {
		return "", "", "", "", fmt.Errorf("read list envelope open: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", "", "", "", fmt.Errorf("list envelope: expected object, got %v", tok)
	}

	for dec.More() {
		// Each top-level key.
		keyTok, kerr := dec.Token()
		if kerr != nil {
			return "", "", "", "", fmt.Errorf("read list envelope key: %w", kerr)
		}
		key, _ := keyTok.(string)

		switch key {
		case "apiVersion":
			apiVersion, err = decodeStringValue(dec)
			if err != nil {
				return "", "", "", "", err
			}
		case "kind":
			kind, err = decodeStringValue(dec)
			if err != nil {
				return "", "", "", "", err
			}
		case "metadata":
			// The list metadata carries continue + resourceVersion.
			var meta struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			}
			if derr := dec.Decode(&meta); derr != nil {
				return "", "", "", "", fmt.Errorf("decode list metadata: %w", derr)
			}
			continueToken = meta.Continue
			resourceVersion = meta.ResourceVersion
		case "items":
			// The streaming heart: walk the items array element by element.
			if derr := decodeItemsArrayStreaming(dec, out, totalItems); derr != nil {
				return "", "", "", "", derr
			}
		default:
			// Unknown top-level key — decode-and-discard into a throwaway
			// so the decoder stays positioned correctly.
			var discard json.RawMessage
			if derr := dec.Decode(&discard); derr != nil {
				return "", "", "", "", fmt.Errorf("skip list key %q: %w", key, derr)
			}
		}
	}

	// Consume the closing '}'.
	if _, cerr := dec.Token(); cerr != nil && cerr != io.EOF {
		return "", "", "", "", fmt.Errorf("read list envelope close: %w", cerr)
	}
	return continueToken, resourceVersion, apiVersion, kind, nil
}

// decodeItemsArrayStreaming consumes the `items` array — Ship H2a, the
// LIST-decode fix. The decoder is positioned just before the array. It
// reads the '[', then per iteration:
//
//  1. captures ONE element as raw JSON bytes (json.RawMessage) — the
//     decoder copies the element's bytes WITHOUT building a
//     map[string]interface{} tree;
//  2. strips that raw frame at the JSON-bytes level (stripItemJSON —
//     drops managedFields + last-applied-config, everything else
//     verbatim) so the stored `raw` matches the H1-stored shape;
//  3. builds a *bytesObject directly from the stripped raw bytes
//     (newBytesObjectFromRaw — decodes ONLY the small `metadata`
//     sub-object for the embedded ObjectMeta; spec/status are never
//     decoded);
//  4. appends the *bytesObject to out.Items and drops the per-element
//     raw slices on the next iteration.
//
// This is the H2a core: the per-item map[string]interface{} the old
// code built via `dec.Decode(&obj)` — the UnstructuredList.UnmarshalJSON
// 5.28 GiB scanobject driver — is NEVER constructed. H1's added
// ingestion json.Marshal (newBytesObject) is also bypassed: the
// bytesObject's `raw` IS the captured LIST frame bytes, not a re-marshal.
//
// A single malformed item is skipped (logged once) rather than failing
// the whole LIST — the streaming relist of 48,999 items must not abort
// on one bad object. (A page-level Status envelope IS still a whole-list
// failure — that check lives in streamingList, unchanged.)
func decodeItemsArrayStreaming(
	dec *json.Decoder,
	out *bytesObjectList,
	totalItems *int,
) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("read items array open: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("items: expected array, got %v", tok)
	}

	for dec.More() {
		// Step 1 — capture ONE element as raw bytes. dec.Decode into a
		// json.RawMessage copies the element's literal bytes; it does
		// NOT build a map tree. This is the allocation H2a replaces the
		// `map[string]any` decode with — bytes, not a pointer-dense map.
		var itemRaw json.RawMessage
		if derr := dec.Decode(&itemRaw); derr != nil {
			return fmt.Errorf("capture list item %d raw: %w", *totalItems, derr)
		}

		// Step 2 — strip at the JSON-bytes level. stripItemJSON decodes
		// ONLY metadata (small) and carries spec/status as raw bytes —
		// the spec/status map tree is never built. The result matches
		// the H1-stored shape (managedFields + last-applied removed).
		stripped, serr := stripItemJSON(itemRaw)
		if serr != nil {
			slog.Warn("cache.streaming_list.item_strip_failed",
				slog.String("subsystem", "cache"),
				slog.Int("item_index", *totalItems),
				slog.String("error", serr.Error()),
				slog.String("effect", "item skipped — streaming relist continues"),
			)
			*totalItems++
			continue
		}

		// Step 3 — build the bytesObject directly. newBytesObjectFromRaw
		// decodes ONLY the metadata sub-object; `raw` is the stripped
		// frame, carried by reference (stripItemJSON already returned a
		// fresh, non-aliased slice — SB-4).
		bo, berr := newBytesObjectFromRaw(stripped)
		if berr != nil {
			slog.Warn("cache.streaming_list.item_decode_failed",
				slog.String("subsystem", "cache"),
				slog.Int("item_index", *totalItems),
				slog.String("error", berr.Error()),
				slog.String("effect", "item skipped — streaming relist continues"),
			)
			*totalItems++
			continue
		}

		// Step 4 — append. `itemRaw` and `stripped`'s intermediate maps
		// go out of scope on the next iteration; only out.Items (the
		// bytes-backed objects) accumulates.
		out.Items = append(out.Items, bo)
		*totalItems++
	}

	// Consume the closing ']'.
	if _, cerr := dec.Token(); cerr != nil {
		return fmt.Errorf("read items array close: %w", cerr)
	}
	return nil
}

// decodeStringValue reads a single JSON string value the decoder is
// positioned on.
func decodeStringValue(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("read string value: %w", err)
	}
	s, ok := tok.(string)
	if !ok {
		return "", fmt.Errorf("expected string value, got %T", tok)
	}
	return s, nil
}

// paramCodec encodes metav1.ListOptions into URL query parameters for
// the raw REST request. metav1.ParameterCodec is the standard codec
// client-go uses for the same purpose.
var paramCodec = metav1.ParameterCodec
