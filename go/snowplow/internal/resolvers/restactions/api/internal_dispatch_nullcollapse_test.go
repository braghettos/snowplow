// internal_dispatch_nullcollapse_test.go — Task #118 falsifier.
//
// DIAGNOSTIC-ONLY (NO behavior change). When a by-name RA api-step path
// template collapses to a cluster-wide LIST because a null/empty path
// segment (name and/or namespace — typically a missing `extras` value)
// elided the name, dispatchViaInternalRESTConfig emits a WARN so the
// operator sees the caller supplied a null where a name was expected —
// instead of snowplow silently LISTing N objects and the downstream jq
// choking (the blueprint-formdef "split cannot be applied to: null" class
// traced in task #117).
//
// The WHOLE point of this ship is the DISCRIMINATION RULE: the WARN must
// distinguish
//
//   - Unintended collapse (WARN fires): the resolved path carries the
//     fingerprint of a jq-templated segment that folded to empty — a
//     TRAILING SLASH (`.../compositiondefinitions/`) or an EMPTY INTERIOR
//     SEGMENT (`.../namespaces//...`). gojq evaluates `string + null ==
//     string`, so a `... + "/<resource>/" + .name` template with .name null
//     leaves the framing slash. ParseAPIServerPathToDep strips the trailing
//     slash and parses name=="" — the intended by-name GET became a LIST.
//
//   - Intentional LIST (stays silent): the RA authored NO name segment
//     (`.../compositiondefinitions`, or `.../namespaces/<ns>/<resource>`),
//     so the resolved path has neither a trailing slash nor an empty
//     interior segment. A legitimate "list all of kind X" step MUST NOT
//     warn — else every legitimate LIST spams the log.
//
// The discrimination signal was verified EMPIRICALLY against the real
// jqutil.Eval used by the resolver (setup.go:evalJQ): the concat idiom
// (`... + "/<resource>/" + .name`) with a null .name renders exactly the
// trailing-slash / empty-interior-segment shapes; an authored nameless LIST
// and a resolved by-name GET do not. (The interpolation idiom `\(.name)`
// renders the literal "null" and takes the by-name GET branch instead, so
// it never reaches the LIST branch that this WARN guards.)
//
// RED ARM: a naive `if name==""` WARN — one that fires on EVERY LIST — is
// proven wrong by TestNullCollapseWARN_RedArm_NaiveNameEmptyWarnsOnLegitLIST:
// it would fire on the intentional-LIST case, which pathHasNullPathSegment
// (the shipped discriminator) does NOT.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	httpcall "github.com/krateo-platformops/plumbing/http/request"
	"github.com/krateo-platformops/snowplow/internal/cache"
	"k8s.io/client-go/rest"
)

const nullCollapseWARNMsg = "internal_dispatch.list.unintended_collapse"

// TestPathHasNullPathSegment_Discriminates is the pure-unit falsifier for the
// discrimination rule. Each case is a REAL resolved-path shape produced by the
// resolver's jq path templates (verified against jqutil.Eval): collapsed
// by-name templates carry a trailing slash / empty interior segment, intended
// LISTs and resolved GETs do not.
func TestPathHasNullPathSegment_Discriminates(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool // true => unintended collapse (WARN); false => legit / not-collapse
	}{
		// --- Unintended collapse: WARN MUST fire -------------------------
		{
			name: "concat name-null, ns present -> trailing slash",
			path: "/apis/core.krateo.io/v1alpha1/namespaces/krateo-system/compositiondefinitions/",
			want: true,
		},
		{
			name: "concat both-null -> empty interior + trailing slash",
			path: "/apis/core.krateo.io/v1alpha1/namespaces//compositiondefinitions/",
			want: true,
		},
		{
			name: "concat ns-null, name present -> empty interior segment",
			path: "/apis/core.krateo.io/v1alpha1/namespaces//compositiondefinitions/foo",
			want: true,
		},
		{
			name: "collapse carries an ?extras query string (stripped first)",
			path: "/apis/core.krateo.io/v1alpha1/namespaces/krateo-system/compositiondefinitions/?extras=%7B%7D",
			want: true,
		},
		{
			name: "core-group namespaced name-null -> trailing slash",
			path: "/api/v1/namespaces/demo/configmaps/",
			want: true,
		},

		// --- Intentional LIST / resolved GET: MUST stay silent -----------
		{
			name: "authored cluster-scoped nameless LIST",
			path: "/apis/core.krateo.io/v1alpha1/compositiondefinitions",
			want: false,
		},
		{
			name: "authored namespaced nameless LIST",
			path: "/apis/core.krateo.io/v1alpha1/namespaces/krateo-system/compositiondefinitions",
			want: false,
		},
		{
			name: "resolved by-name GET (name present)",
			path: "/apis/core.krateo.io/v1alpha1/namespaces/krateo-system/compositiondefinitions/foo",
			want: false,
		},
		{
			name: "core-group namespaced LIST",
			path: "/api/v1/namespaces/demo/configmaps",
			want: false,
		},
		{
			name: "legit LIST with an ?extras query string",
			path: "/apis/core.krateo.io/v1alpha1/compositiondefinitions?extras=%7B%22username%22%3A%22u%22%7D",
			want: false,
		},
		{
			// A SEPARATE null class (disc-architect #118 caveat, confirmed
			// against krateo-platformops/gojq v0.12.21): when a downstream step's jq
			// `split()` hits a null it THROWS, and evalJQ swallows the error
			// into the path string (setup.go:92-94), so out.Path becomes the
			// literal jq error message. It has NO slash / no `//`, so
			// ParseAPIServerPathToDep returns parseOK=false and it never
			// reaches the LIST branch at all (it falls through to the
			// external/apiserver GET). pathHasNullPathSegment MUST NOT fire on
			// it — it is a DIFFERENT symptom (jq-error-leaked-into-path), NOT
			// #118's LIST-collapse, and folding it into this WARN was
			// explicitly ruled out. This arm pins that silence so a future
			// substring-flavoured predicate cannot accidentally claim it.
			name: "jq-error-string leaked into path (NOT a LIST collapse)",
			path: "split cannot be applied to: null",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathHasNullPathSegment(tc.path); got != tc.want {
				t.Fatalf("pathHasNullPathSegment(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestNullCollapseWARN_EndToEnd drives the REAL LIST dispatch through the
// paged fixture for BOTH path shapes and asserts:
//
//   - (K) a COLLAPSED path (trailing slash) => the unintended-collapse WARN
//     fires exactly once, carrying the resolved path;
//   - (M) an INTENTIONAL LIST path (no trailing slash) => the WARN does NOT
//     fire (only the pre-existing paged_list.completed WARN);
//   - (NO BEHAVIOR CHANGE) the served envelope bytes are IDENTICAL for the
//     two shapes — the dispatch LISTs exactly the same either way; only a
//     log line differs.
func TestNullCollapseWARN_EndToEnd(t *testing.T) {
	resetInternalClientCacheForTest()
	t.Cleanup(resetInternalClientCacheForTest)

	const totalItems = 30
	pageSize := int(internalDispatchListPageLimit)
	fixture, caPEM := newPagedListFixture(t, totalItems, pageSize)

	rc := &rest.Config{
		Host:        fixture.server.URL,
		BearerToken: "fake-sa-jwt",
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caPEM,
		},
	}

	// The fixture's GVR-derived URL is
	//   /apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages
	// client-go maps a cluster-scoped LIST (name=="") to exactly that URL for
	// BOTH the collapsed and the intentional path shapes below (both parse to
	// the same GVR + name==""), so the fixture serves the same response and
	// the ONLY difference is pathHasNullPathSegment(call.Path).
	const intentionalPath = "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages"
	const collapsedPath = "/apis/composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages/" // trailing slash = folded name segment

	dispatch := func(t *testing.T, path string) (raw []byte, logOut string) {
		t.Helper()
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
		ctx := withSlogLogger(cache.WithInternalRESTConfig(context.Background(), rc), logger)

		raw, served, err := dispatchViaInternalRESTConfig(ctx, httpcall.RequestOptions{
			RequestInfo: httpcall.RequestInfo{Path: path},
		})
		if err != nil {
			t.Fatalf("dispatch(%q) returned error: %v", path, err)
		}
		if !served {
			t.Fatalf("dispatch(%q): expected served=true for the LIST path", path)
		}
		return raw, logBuf.String()
	}

	// --- (M) INTENTIONAL LIST: WARN MUST NOT fire ------------------------
	rawIntentional, logIntentional := dispatch(t, intentionalPath)
	if strings.Contains(logIntentional, nullCollapseWARNMsg) {
		t.Fatalf("ARM-M FAIL: the unintended-collapse WARN fired on an INTENTIONAL "+
			"LIST path (no trailing slash / no empty segment). It must stay "+
			"silent for legitimate LISTs. Log:\n%s", logIntentional)
	}

	// --- (K) COLLAPSED PATH: WARN MUST fire exactly once -----------------
	rawCollapsed, logCollapsed := dispatch(t, collapsedPath)
	if got := strings.Count(logCollapsed, nullCollapseWARNMsg); got != 1 {
		t.Fatalf("ARM-K FAIL: the unintended-collapse WARN fired %d times on a "+
			"COLLAPSED path (trailing slash from a folded name segment), "+
			"expected exactly 1. Log:\n%s", got, logCollapsed)
	}
	// The WARN must carry the resolved path so the operator can see the shape.
	if !strings.Contains(logCollapsed, collapsedPath) {
		t.Fatalf("ARM-K FAIL: the WARN did not carry the resolved_path %q. Log:\n%s",
			collapsedPath, logCollapsed)
	}

	// --- NO BEHAVIOR CHANGE: served bytes identical ----------------------
	if !bytes.Equal(rawIntentional, rawCollapsed) {
		t.Fatalf("BEHAVIOR-CHANGE FAIL: the served envelope differs between the "+
			"intentional and collapsed path shapes. The WARN must be diagnostic "+
			"ONLY — the dispatch must LIST byte-identically either way.\n"+
			"intentional (%d bytes):\n%s\ncollapsed (%d bytes):\n%s",
			len(rawIntentional), rawIntentional, len(rawCollapsed), rawCollapsed)
	}
	// Sanity: the served envelope actually carries the items (guards against
	// the two paths silently both returning empty and trivially matching).
	var env struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rawCollapsed, &env); err != nil {
		t.Fatalf("served bytes not valid JSON: %v", err)
	}
	if len(env.Items) != totalItems {
		t.Fatalf("served list carries %d items, expected %d", len(env.Items), totalItems)
	}
}

// TestNullCollapseWARN_RedArm_NaiveNameEmptyWarnsOnLegitLIST proves the
// discrimination is load-bearing: a NAIVE implementation that warns whenever
// name=="" (i.e. on EVERY LIST) would fire on the intentional-LIST case. This
// test asserts the SHIPPED discriminator (pathHasNullPathSegment) does NOT
// treat the intentional LIST as a collapse, while the naive predicate would.
// If a future edit weakens pathHasNullPathSegment to "always true on the LIST
// branch", the ARM-M assertion in TestNullCollapseWARN_EndToEnd fails AND this
// test fails — the naive-vs-discriminating gap is pinned.
func TestNullCollapseWARN_RedArm_NaiveNameEmptyWarnsOnLegitLIST(t *testing.T) {
	// Two paths that BOTH reach the LIST branch (name==""):
	intentional := "/apis/core.krateo.io/v1alpha1/compositiondefinitions"
	collapsed := "/apis/core.krateo.io/v1alpha1/namespaces/krateo-system/compositiondefinitions/"

	// The naive predicate the RED arm models: warn on every LIST.
	naiveWouldWarn := func(_ string) bool { return true /* name=="" on both */ }

	// RED: the naive predicate FAILS to discriminate — it warns on the legit LIST.
	if !naiveWouldWarn(intentional) {
		t.Fatalf("test setup wrong: naive predicate should warn on everything")
	}

	// GREEN: the shipped discriminator stays silent on the intentional LIST
	// and fires on the collapsed path.
	if pathHasNullPathSegment(intentional) {
		t.Fatalf("DISCRIMINATION FAIL: pathHasNullPathSegment warned on an "+
			"intentional nameless LIST %q — this is the naive-predicate bug the "+
			"RED arm guards against.", intentional)
	}
	if !pathHasNullPathSegment(collapsed) {
		t.Fatalf("DISCRIMINATION FAIL: pathHasNullPathSegment did NOT fire on a "+
			"collapsed by-name path %q (trailing slash from a folded name "+
			"segment).", collapsed)
	}
}
