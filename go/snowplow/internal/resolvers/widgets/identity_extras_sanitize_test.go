// identity_extras_sanitize_test.go — A-3 (1.12.3) unit arms for
// sanitizeUndeclaredIdentityExtras, the strip/keep decision that keeps
// client-supplied identity out of the resolve input.
//
// The end-to-end arms (served body + persisted shared cell) live in
// internal/handlers/dispatchers/a3_identity_extras_quarantine_test.go. These
// pin the decision matrix itself, plus the one property only a resolve-level
// test can show: the strip happens BEFORE the DeclaredIdentity injection merge,
// so a declaring widget still gets the JWT value and an undeclared one gets
// nothing.

package widgets

import (
	"reflect"
	"testing"

	"context"

	xcontext "github.com/krateo-platformops/plumbing/context"
	"github.com/krateo-platformops/plumbing/jwtutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func a3CR(identityContext, keyExtras []string) map[string]any {
	toAny := func(ss []string) []any {
		out := make([]any, len(ss))
		for i, s := range ss {
			out[i] = s
		}
		return out
	}
	spec := map[string]any{}
	if len(identityContext) > 0 {
		spec["identityContext"] = toAny(identityContext)
	}
	if len(keyExtras) > 0 {
		spec["keyExtras"] = toAny(keyExtras)
	}
	return map[string]any{"spec": spec}
}

// TestA3_SanitizeMatrix is the strip/keep decision table.
func TestA3_SanitizeMatrix(t *testing.T) {
	cases := []struct {
		name    string
		cr      map[string]any
		in      map[string]any
		want    map[string]any
		whyKeep string
	}{
		{
			name: "undeclared widget strips every identity key",
			cr:   a3CR(nil, nil),
			in:   map[string]any{"username": "evil", "groups": []any{"g"}, "displayName": "E"},
			want: map[string]any{},
		},
		{
			name:    "non-identity extras are never touched",
			cr:      a3CR(nil, nil),
			in:      map[string]any{"foo": "bar", "name": "n"},
			want:    map[string]any{"foo": "bar", "name": "n"},
			whyKeep: "the deliberate KEY-ONLY split: an undeclared non-identity extra still reaches the resolve input and is quarantined at the Put instead",
		},
		{
			name:    "identityContext-declared key survives (injection overwrites it)",
			cr:      a3CR([]string{"username"}, nil),
			in:      map[string]any{"username": "evil", "displayName": "E"},
			want:    map[string]any{"username": "evil"},
			whyKeep: "DeclaredIdentity overwrites username with the JWT value a few lines later; displayName can never be declared in identityContext (enum filter) so it goes",
		},
		{
			name:    "keyExtras-declared identity key survives (self-quarantines)",
			cr:      a3CR(nil, []string{"username", "displayName"}),
			in:      map[string]any{"username": "evil", "displayName": "E", "groups": []any{"g"}},
			want:    map[string]any{"username": "evil", "displayName": "E"},
			whyKeep: "a keyExtras-declared key PARTITIONS the cache key, so a spoofed value lands in a cell only that same value can reach; groups is undeclared and goes",
		},
		{
			name:    "mixed: declared route params kept, undeclared identity stripped",
			cr:      a3CR(nil, []string{"name", "namespace"}),
			in:      map[string]any{"name": "demo-1", "namespace": "team-a", "username": "cyberjoker", "displayName": "CJ"},
			want:    map[string]any{"name": "demo-1", "namespace": "team-a"},
			whyKeep: "the live west4 wire shape",
		},
		{
			name: "empty extras is a no-op",
			cr:   a3CR(nil, nil),
			in:   map[string]any{},
			want: map[string]any{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeUndeclaredIdentityExtras(tc.cr, tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sanitizeUndeclaredIdentityExtras() = %#v, want %#v. %s", got, tc.want, tc.whyKeep)
			}
		})
	}
}

// TestA3_SanitizeDoesNotMutateInput — the caller's map must not be rewritten
// under it. opts.Extras can alias a map the dispatcher also uses for the KEY, so
// mutating in place would silently move the cache key.
func TestA3_SanitizeDoesNotMutateInput(t *testing.T) {
	in := map[string]any{"username": "evil", "foo": "bar"}
	before := map[string]any{"username": "evil", "foo": "bar"}
	_ = sanitizeUndeclaredIdentityExtras(a3CR(nil, nil), in)
	if !reflect.DeepEqual(in, before) {
		t.Fatalf("sanitizeUndeclaredIdentityExtras MUTATED its input: %#v (was %#v). The caller's map is also the key material — mutating it here moves the cache key", in, before)
	}
}

// TestA3_SanitizeReturnsSameMapWhenNothingToStrip — the allocation-free common
// path. The identity-free and fully-declared corpora are ~99% of traffic.
func TestA3_SanitizeReturnsSameMapWhenNothingToStrip(t *testing.T) {
	in := map[string]any{"name": "n", "namespace": "ns"}
	got := sanitizeUndeclaredIdentityExtras(a3CR(nil, []string{"name", "namespace"}), in)
	if !sameMapHeader(got, in) {
		t.Fatal("expected the SAME map back (no copy) when nothing needs stripping — the common path must not allocate")
	}
}

func sameMapHeader(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	// Mutate through one and observe through the other: same underlying map.
	const probe = "__same_map_probe__"
	a[probe] = 1
	_, seen := b[probe]
	delete(a, probe)
	return seen
}

// TestA3_StripHappensBeforeInjection drives the REAL Resolve and shows the
// ordering: an undeclared widget's jq sees NOTHING for .username (stripped),
// while a declaring widget's jq sees the JWT value (injected), not the client's.
// This is the property that makes the sanitiser's placement — inside Resolve,
// ahead of the injection merge — observable rather than incidental.
func TestA3_StripHappensBeforeInjection(t *testing.T) {
	echo := func(idc []string) map[string]any {
		cr := a3CR(idc, nil)
		cr["spec"].(map[string]any)["widgetData"] = map[string]any{}
		cr["spec"].(map[string]any)["widgetDataTemplate"] = []any{
			map[string]any{"forPath": ".echoedUser", "expression": "${ .username }"},
		}
		return cr
	}
	ctx := xcontext.BuildContext(context.Background(),
		xcontext.WithUserInfo(jwtutil.UserInfo{Username: "alice", Groups: []string{"devs"}}))

	render := func(cr map[string]any) map[string]any {
		obj, _ := Resolve(ctx, ResolveOptions{
			In:     &unstructured.Unstructured{Object: cr},
			Extras: map[string]any{"username": "SOMEONE-ELSE"},
		})
		wd, _, err := unstructured.NestedMap(obj.Object, "status", "widgetData")
		if err != nil {
			t.Fatalf("read status.widgetData: %v", err)
		}
		return wd
	}

	// Undeclared: the client value is STRIPPED, and nothing is injected, so the
	// jq expression yields no value at all.
	if got := render(echo(nil))["echoedUser"]; got == "SOMEONE-ELSE" {
		t.Fatalf("A-3: an UNDECLARED widget must not see the client-supplied username; rendered %#v", got)
	}
	// Declared: injection wins, so the jq sees the JWT's own username.
	if got := render(echo([]string{"username"}))["echoedUser"]; got != "alice" {
		t.Fatalf("A-3 ORDERING: a DECLARING widget must see the JWT username (injection runs AFTER the strip and overwrites); rendered %#v — if this is empty the sanitiser is stripping a declared key", got)
	}
}
