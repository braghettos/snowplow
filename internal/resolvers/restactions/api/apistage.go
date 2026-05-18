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
// EVERYTHING here is gated by cache.ApistageL1Enabled() — default OFF.
// Flag-off the resolver is byte-identical to 0.30.118 (AC-F1.1).

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	xcontext "github.com/krateoplatformops/plumbing/context"
	httpcall "github.com/krateoplatformops/plumbing/http/request"
	"github.com/krateoplatformops/plumbing/ptr"
	"github.com/krateoplatformops/snowplow/internal/cache"
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
) ([]byte, bool) {
	gvr, _, name, parseOK := cache.ParseAPIServerPathToDep(call.GetPath())
	if !parseOK {
		// Not an apiserver GVR path — the content layer never produced
		// this; pass the bytes through un-gated (defensive — the caller
		// only invokes the gate for content-served calls).
		return raw, true
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

// gateListItems runs filterListByRBAC over an ALREADY-PARSED item slice
// and re-marshals the narrowed set into the apiserver-shaped envelope —
// Ship 0.30.121 R3. This is the unmarshal-free gate path: identical
// parse->filter->marshalAsList pipeline to gateListEnvelope, only the
// json.Unmarshal has been hoisted out to the Put site (parseListEnvelope).
func gateListItems(ctx context.Context, gvr schema.GroupVersionResource, parsed parsedListEnvelope) ([]byte, bool) {
	kept, identityOK := filterListByRBAC(ctx, gvr, parsed.items)
	if !identityOK {
		// FAIL-CLOSED: no identity — same contract as the inline gate's
		// served=false (caller falls through to the apiserver).
		return nil, false
	}
	gated, err := marshalAsList(parsed.apiVersion, parsed.kind, kept)
	if err != nil {
		return nil, false
	}
	return gated, true
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
func gateListEnvelope(ctx context.Context, gvr schema.GroupVersionResource, raw []byte) ([]byte, bool) {
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
func gateGetEnvelope(ctx context.Context, gvr schema.GroupVersionResource, raw []byte) ([]byte, bool) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	u := &unstructured.Unstructured{Object: obj}
	if !filterGetByRBAC(ctx, gvr, u) {
		return nil, false
	}
	return raw, true
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
// Returns (raw, served, ok):
//   - ok==false  — the content layer cannot serve this call (it is not
//     pivot-servable: a write verb, an external URL, a metadata-only
//     GVR, a pre-sync informer, a non-apiserver path). The caller falls
//     through to the non-pivot dispatch branches, byte-identical to a
//     flag-off resolve.
//   - ok==true, served==false — fail-closed: no identity on ctx, or a
//     GET the requester is denied. The caller falls through to the
//     apiserver branch, whose per-user token narrows correctly.
//   - ok==true, served==true — `raw` is the RBAC-GATED envelope to feed
//     the stage's ResponseHandler.
//
// Pipeline (per the F1 spec):
//  1. content key from call.Path via ParseAPIServerPathToDep.
//  2. content Get — HIT: stored raw envelope. MISS: dispatch UN-GATED
//     (WithApistageContentResolve) + Put the raw envelope.
//  3. gate the raw envelope with the request identity (the single F1
//     gate site — runs on hit AND miss).
//
// On the prewarm path (cache.ApistagePrewarmFromContext, wired by F2)
// step 3 is skipped — the prewarm resolve has no requester; it only
// populates the content entry. F1 leaves the skip-point; F2 sets the
// marker.
func apistageContentServe(
	ctx context.Context,
	store *cache.ResolvedCacheStore,
	call httpcall.RequestOptions,
) (raw []byte, served bool, ok bool) {
	log := xcontext.Logger(ctx)

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
	var envelope []byte
	var parsed parsedListEnvelope
	var haveParsed bool
	if entry, hit := store.Get(contentKey); hit && entry != nil {
		envelope = entry.RawJSON
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
		newEntry := &cache.ResolvedEntry{
			RawJSON: dispatched,
			Inputs:  ptrTo(contentKeyInputs(gvr, ns, name)),
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
	// takes gateContentEnvelope's RawJSON path. Output is byte-identical.
	var gated []byte
	var gateOK bool
	if haveParsed {
		gated, gateOK = gateListItems(ctx, gvr, parsed)
	} else {
		gated, gateOK = gateContentEnvelope(ctx, callPathVerb{path: call.Path}, envelope)
	}
	if !gateOK {
		// Fail-closed — no identity / GET denied. Fall through to apiserver.
		return nil, false, true
	}
	return gated, true, true
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

