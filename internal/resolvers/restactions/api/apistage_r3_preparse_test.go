// apistage_r3_preparse_test.go — Ship 0.30.121 R3 hermetic falsifier.
//
// R3 eliminates the F1 content-gate double-unmarshal (~1.73 GiB
// alloc_space on the 50K bench): pre-F1 gateListEnvelope re-unmarshalled
// the stored LIST envelope on EVERY content-Get-hit. R3 parses the items
// ONCE at the content-entry Put site (parseListEnvelope) and stores them
// on the ResolvedEntry; the gate then runs filterListByRBAC directly
// over the stored items via gateListItems and skips json.Unmarshal.
//
// THE INVARIANT (architect's R3 spec): the gated bytes from the
// pre-parsed-Items path MUST be byte-identical to the un-gated-dispatch-
// then-gate path — only the unmarshal TIMING moves; the
// parse->filter->marshalAsList pipeline is otherwise identical.
//
// TestR3_PreParsedGateByteIdentical drives BOTH gate paths over the SAME
// raw LIST envelope under an IDENTICAL RBAC context and asserts the
// output bytes are equal. It reuses the F1 watcher harness (newF1Watcher)
// for a real, synced RBAC store — no live cluster.

package api

import (
	"context"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
	"github.com/krateoplatformops/plumbing/jwtutil"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestR3_PreParsedGateByteIdentical is the R3 hermetic falsifier. It
// builds a raw cluster-wide widgets LIST envelope, then gates it two
// ways under the SAME narrow-user identity:
//
//   path A — gateListEnvelope(raw): the un-gated-dispatch-then-gate
//            path; unmarshals the envelope inside the gate (pre-0.30.121
//            behaviour, retained as the fallback).
//   path B — gateListItems(parseListEnvelope(raw)): the R3 pre-parsed
//            path; the unmarshal is hoisted to parseListEnvelope (the
//            Put-site call) and the gate runs over the parsed items.
//
// The two outputs MUST be byte-identical — R3 only moves the unmarshal,
// it must not change a single output byte.
func TestR3_PreParsedGateByteIdentical(t *testing.T) {
	rw := newF1Watcher(t)

	// A real raw LIST envelope: marshalAsList over every seeded widget,
	// exactly the shape dispatchViaInformer stores in a content entry.
	items, servable := rw.ListObjectsServable(f1WidgetsGVR, "")
	if !servable {
		t.Fatalf("setup: widgets GVR not servable after sync")
	}
	raw, err := marshalAsList(apiVersionForGVR(f1WidgetsGVR),
		listKindForResource(f1WidgetsGVR.Resource), items)
	if err != nil {
		t.Fatalf("setup: marshalAsList: %v", err)
	}

	// Identical RBAC context for both gate paths — the narrow user, who
	// is authorized for only f1NarrowNamespaces. A real filterListByRBAC
	// verdict (not a stub) exercises the actual gate.
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: f1NarrowUser}),
	)

	// Path A — the unmarshal-inside-gate fallback path.
	gatedA, okA := gateListEnvelope(ctx, f1WidgetsGVR, raw)
	if !okA {
		t.Fatalf("path A (gateListEnvelope) returned served=false; want a gated envelope")
	}

	// Path B — the R3 pre-parsed path: unmarshal ONCE (parseListEnvelope,
	// the Put-site call), then gate over the parsed items.
	parsed, parseOK := parseListEnvelope(f1WidgetsGVR, raw)
	if !parseOK {
		t.Fatalf("path B: parseListEnvelope failed on a well-formed envelope")
	}
	gatedB, okB := gateListItems(ctx, f1WidgetsGVR, parsed)
	if !okB {
		t.Fatalf("path B (gateListItems) returned served=false; want a gated envelope")
	}

	// THE INVARIANT — byte-identical.
	if string(gatedA) != string(gatedB) {
		t.Fatalf("R3 byte-identity FAILED: the pre-parsed-Items gate path produced "+
			"different bytes from the unmarshal-inside-gate path.\n"+
			" gateListEnvelope: %s\n gateListItems:    %s", gatedA, gatedB)
	}

	// And both must be non-empty narrowed content (the narrow user IS
	// authorized for 2 namespaces — a 200-with-empty-body would be a
	// serving failure, AC-2 in spirit).
	if len(gatedA) == 0 {
		t.Fatalf("R3: gated output is empty — the narrow user is authorized for "+
			"%d namespaces and must see their widgets", len(f1NarrowNamespaces))
	}
}

// TestR3_EntryItemsAccountedInBytes asserts the LRU byte accounting
// counts the R3 pre-parsed Items — MANDATORY per the architect, or the
// byte cap silently under-counts every apistage LIST content entry. An
// entry carrying Items must weigh strictly MORE than the same entry with
// only RawJSON.
func TestR3_EntryItemsAccountedInBytes(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")
	cache.ResetResolvedCacheForTest()
	t.Cleanup(cache.ResetResolvedCacheForTest)

	store := cache.ResolvedCache()
	if store == nil {
		t.Fatalf("resolved cache nil under RESOLVED_CACHE_ENABLED=true")
	}

	raw := []byte(`{"apiVersion":"widgets.krateo.io/v1","kind":"WidgetList",` +
		`"items":[{"metadata":{"name":"w1"}},{"metadata":{"name":"w2"}}]}`)
	apistageInputs := &cache.ResolvedKeyInputs{
		CacheEntryClass: cache.CacheEntryClassApistage,
		Group:           "widgets.krateo.io",
		Version:         "v1",
		Resource:        "widgets",
	}

	// Entry 1 — RawJSON only (no pre-parsed Items).
	plain := &cache.ResolvedEntry{RawJSON: raw, Inputs: apistageInputs}
	store.Put("r3-plain", plain)
	plainBytes := store.Bytes()

	// Entry 2 — same RawJSON PLUS pre-parsed Items. f1WidgetsGVR is a
	// package-level var initialized at load — safe to use here.
	parsed, ok := parseListEnvelope(f1WidgetsGVR, raw)
	if !ok {
		t.Fatalf("parseListEnvelope failed on a well-formed envelope")
	}
	withItems := &cache.ResolvedEntry{
		RawJSON:         raw,
		Inputs:          apistageInputs,
		Items:           parsed.items,
		ItemsAPIVersion: parsed.apiVersion,
		ItemsKind:       parsed.kind,
	}
	store.Put("r3-with-items", withItems)
	bothBytes := store.Bytes()

	// The Items-carrying entry must add strictly more than its RawJSON
	// length alone — i.e. the Items term is accounted.
	itemsDelta := bothBytes - plainBytes
	if itemsDelta <= int64(len(raw)) {
		t.Fatalf("R3 byte accounting UNDER-COUNTS: adding an entry WITH pre-parsed "+
			"Items grew curBytes by %d, which is <= the bare RawJSON length %d. "+
			"The Items tree is not being counted — the LRU cap will overshoot.",
			itemsDelta, len(raw))
	}
}
