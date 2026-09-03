// a1_uaf_shared_cell_shape_test.go — 1.12.3 A-1: the RBAC GROUND TRUTH that
// makes the userAccessFilter cross-tenant leak possible.
//
// This arm proves the PREMISE of A-1, not the mitigation. It establishes, using
// the production rbac.EvaluateRBAC over a real published snapshot, that a
// perfectly ordinary two-tenant RBAC shape produces the two properties that
// together make one L1 cell serve two users divergent content:
//
//	(1) the two users' per-object (per-namespace) verdicts DIVERGE — the
//	    dependency a userAccessFilter stage re-evaluates per object; and
//	(2) every input the resolved-cache key folds from RBAC is IDENTICAL for
//	    them — the same first-match BindingUID for `get restactions`, and the
//	    same per-subject RBACSubGen.
//
// (1) AND (2) together are the defect: the key cannot tell the two users apart
// while their correct bodies differ. Asserting them here, in the package that
// owns RBAC evaluation, keeps the premise honest independently of the
// dispatcher — if a future RBAC change made (2) false (say the key started
// folding something that separates them), this test goes RED and tells you the
// shared-cell premise no longer holds, rather than letting the dispatcher-side
// acceptance arm silently become vacuous.
//
// THE ACCEPTANCE ARM ITSELF lives where the real dispatch, the real
// dispatchCacheLookupKey and the real response bytes are reachable:
// TestA1_UAFCrossUser_NoSharedCellServe in
// internal/handlers/dispatchers/a1_uaf_cross_user_isolation_test.go. It asserts
// on BOB'S SERVED BODY (per feedback_seed_group_shared_cell_needs_resolve_output
// _arm, a key-side-only arm is inadmissible); this file is its premise, not a
// substitute for it.
//
// evaltest package only — never `go test ./internal/rbac/...` against a remote
// kubeconfig (feedback_no_go_test_against_remote_kubeconfig).

package evaltest

import (
	"context"
	"testing"

	"github.com/krateo-platformops/snowplow/internal/cache"
	"github.com/krateo-platformops/snowplow/internal/rbac"
)

const (
	a1SharedGroup = "portal"
	a1TenantANS   = "tenant-a"
	a1TenantBNS   = "tenant-b"
)

// TestA1_UAFSharedCellShape_DivergentVerdictsSameKeyInputs builds the exact
// shape both red-teams used: ONE ClusterRoleBinding granting `get restactions`
// to a group both users are in, plus DIVERGENT per-namespace RoleBindings.
func TestA1_UAFSharedCellShape_DivergentVerdictsSameKeyInputs(t *testing.T) {
	newTestWatcher(t,
		// The SHARED dispatch grant — the binding whose UID the key folds.
		clusterRole("ra-reader",
			rule([]string{"templates.krateo.io"}, []string{"restactions"}, []string{"get", "list"}),
		),
		clusterRoleBindingWithUID("portal-ra-bind", "ra-reader", "uid-portal-shared",
			groupSubject(a1SharedGroup),
		),
		// The DIVERGENT per-object grants a userAccessFilter re-evaluates.
		role(a1TenantANS, "cm-reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		roleBinding(a1TenantANS, "alice-cm", "Role", "cm-reader", userSubject("alice")),
		role(a1TenantBNS, "cm-reader", rule([]string{""}, []string{"configmaps"}, []string{"get"})),
		roleBinding(a1TenantBNS, "bob-cm", "Role", "cm-reader", userSubject("bob")),
	)

	ctx := context.Background()
	groups := []string{a1SharedGroup}

	// perObject is the call refilter.go's evalSingle makes for each returned
	// object: the requester's identity against the OBJECT'S OWN namespace.
	perObject := func(user, ns string) bool {
		t.Helper()
		allowed, _, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username: user, Groups: groups,
			Verb: "get", Group: "", Resource: "configmaps", Namespace: ns,
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC(%s, %s): %v", user, ns, err)
		}
		return allowed
	}

	// dispatchBinding is the call dispatchCacheLookupKey makes: the first-match
	// binding UID for `get` on the RESTAction CR — the ONE RBAC input the
	// restactions key folds.
	dispatchBinding := func(user string) string {
		t.Helper()
		allowed, uid, err := rbac.EvaluateRBAC(ctx, rbac.EvaluateOptions{
			Username: user, Groups: groups,
			Verb: "get", Group: "templates.krateo.io", Resource: "restactions", Namespace: "krateo-system",
		})
		if err != nil {
			t.Fatalf("EvaluateRBAC(%s, restactions): %v", user, err)
		}
		if !allowed {
			t.Fatalf("PREMISE: %s must be permitted to dispatch the RESTAction (the shared CRB grants it via group %q); "+
				"a denied user derives a \"\" BindingUID and is excluded by the #95 guard for an unrelated reason",
				user, a1SharedGroup)
		}
		return uid
	}

	// --- (1) The per-object verdicts DIVERGE. -------------------------------
	if !perObject("alice", a1TenantANS) || perObject("alice", a1TenantBNS) {
		t.Fatalf("(1) alice must be permitted in %s and DENIED in %s; got %v/%v",
			a1TenantANS, a1TenantBNS, perObject("alice", a1TenantANS), perObject("alice", a1TenantBNS))
	}
	if !perObject("bob", a1TenantBNS) || perObject("bob", a1TenantANS) {
		t.Fatalf("(1) bob must be permitted in %s and DENIED in %s; got %v/%v",
			a1TenantBNS, a1TenantANS, perObject("bob", a1TenantBNS), perObject("bob", a1TenantANS))
	}

	// --- (2) Every RBAC input the key folds is IDENTICAL. -------------------
	aliceUID, bobUID := dispatchBinding("alice"), dispatchBinding("bob")
	if aliceUID == "" || bobUID == "" {
		t.Fatalf("(2) both users must derive a NON-EMPTY first-match BindingUID; alice=%q bob=%q", aliceUID, bobUID)
	}
	if aliceUID != bobUID {
		t.Fatalf("(2) the two users must share the SAME first-match binding — that sharing is what collapses their "+
			"two correct bodies onto ONE cache cell. alice=%q bob=%q", aliceUID, bobUID)
	}

	// RBACSubGen is the other RBAC-derived key component (#118 (c)). The shape is
	// symmetric — one shared group binding plus one RoleBinding each — so it
	// comes out equal, and the users are indistinguishable to the whole key.
	aliceGen := cache.RBACSubGenForSubject("alice", groups)
	bobGen := cache.RBACSubGenForSubject("bob", groups)
	if aliceGen != bobGen {
		t.Fatalf("(2) the per-subject RBACSubGen must also match for the cell to be shared; alice=%d bob=%d. "+
			"If this ever diverges legitimately, the shared-cell premise of A-1 has changed and the dispatcher-side "+
			"acceptance arm (TestA1_UAFCrossUser_NoSharedCellServe) must be re-examined before it is trusted.",
			aliceGen, bobGen)
	}

	// --- THE PREMISE, stated. ----------------------------------------------
	// Divergent bodies, indistinguishable keys. Nothing above is about the
	// mitigation; this is the defect's precondition, and it holds.
	t.Logf("A-1 premise holds: alice and bob have DIVERGENT per-object verdicts (%s vs %s) yet IDENTICAL "+
		"key-folded RBAC inputs (binding_uid=%q, rbac_subgen=%d) — one cell, two correct-but-different bodies.",
		a1TenantANS, a1TenantBNS, aliceUID, aliceGen)
}
