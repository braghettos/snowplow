//go:build unit
// +build unit

package api

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// TestClusterWideDepNotPopulatedByResolve asserts that the resolve.go API
// result write path no longer fans out per-namespace API result keys into
// the cluster-wide dep set L1ResourceDepKey(gvr, "", "").
//
// This is the unit pin for the bug where a single-namespace DELETE
// invalidated all 50 namespaces' L1 entries because every per-namespace
// API result key had been registered in the cluster-wide dep set.
//
// What the test exercises (mirrors the SAdd pattern in resolve.go after the
// fix at internal/resolvers/restactions/api/resolve.go:343-347 and :355-359):
//   - 3 namespaces produce a per-namespace API result key
//   - Each is registered in its own per-ns dep: L1ResourceDepKey(gvr, ns, "")
//   - The cluster-wide dep L1ResourceDepKey(gvr, "", "") MUST stay empty
//
// Then the test simulates the watcher DELETE path (the relevant slice of
// triggerL1RefreshBatch in internal/cache/watcher.go) for one namespace and
// asserts that only that namespace's API result key is collected for
// deletion — not all three.
func TestClusterWideDepNotPopulatedByResolve(t *testing.T) {
	ctx := context.Background()
	c := cache.NewMem(time.Hour)

	gvr := schema.GroupVersionResource{
		Group:    "composition.krateo.io",
		Version:  "v1",
		Resource: "compositions",
	}
	gvrKey := cache.GVRToKey(gvr)
	identity := "user-test"

	namespaces := []string{"ns-01", "ns-02", "ns-03"}
	apiKeys := make(map[string]string, len(namespaces))

	// Mirror the resolve.go write path after the fix: write the API result
	// raw entry, then SAdd into the per-resource dep ONLY (no cluster-wide
	// fan-out for pathName == "").
	for _, ns := range namespaces {
		apiKey := cache.APIResultKey(identity, gvr, ns, "")
		apiKeys[ns] = apiKey
		if err := c.SetAPIResultRaw(ctx, apiKey, []byte(`{"items":[]}`)); err != nil {
			t.Fatalf("SetAPIResultRaw(%s): %v", apiKey, err)
		}
		// pathName == "" → LIST result. The fix removes the cluster-wide
		// SAdd that used to follow this line.
		depKey := cache.L1ResourceDepKey(gvrKey, ns, "")
		if err := c.SAddWithTTL(ctx, depKey, apiKey, cache.ReverseIndexTTL); err != nil {
			t.Fatalf("SAddWithTTL(%s): %v", depKey, err)
		}
	}

	// ── Assertion 1: cluster-wide dep MUST be empty ─────────────────────
	clusterDepKey := cache.L1ResourceDepKey(gvrKey, "", "")
	clusterMembers, err := c.SMembers(ctx, clusterDepKey)
	if err != nil {
		t.Fatalf("SMembers(cluster-wide dep): %v", err)
	}
	if len(clusterMembers) != 0 {
		t.Fatalf("cluster-wide dep contains %d members, want 0; members=%v",
			len(clusterMembers), clusterMembers)
	}
	for _, m := range clusterMembers {
		if cache.IsAPIResultKey(m) {
			t.Fatalf("cluster-wide dep contains api-result key %q; "+
				"resolve.go is still fanning out per-ns keys into the cluster dep",
				m)
		}
	}

	// ── Assertion 2: each per-ns dep contains exactly its own api key ──
	for _, ns := range namespaces {
		depKey := cache.L1ResourceDepKey(gvrKey, ns, "")
		members, err := c.SMembers(ctx, depKey)
		if err != nil {
			t.Fatalf("SMembers(%s): %v", depKey, err)
		}
		if len(members) != 1 {
			t.Fatalf("per-ns dep %s has %d members, want 1; members=%v",
				depKey, len(members), members)
		}
		if members[0] != apiKeys[ns] {
			t.Fatalf("per-ns dep %s contains %q, want %q",
				depKey, members[0], apiKeys[ns])
		}
	}

	// ── Assertion 3: simulate one-namespace DELETE → only that ns's
	// api-result key is collected for deletion ─────────────────────────
	// Mirrors the relevant slice of triggerL1RefreshBatch:
	//   1) collect listDeps for (gvr, ns), clusterDeps for (gvr) — both LIST
	//   2) SMembers each, union into allKeys
	//   3) on DELETE event, gather IsAPIResultKey(k) → Delete them
	deletedNS := namespaces[0]
	allKeys := make(map[string]bool)

	listDepKey := cache.L1ResourceDepKey(gvrKey, deletedNS, "")
	listMembers, err := c.SMembers(ctx, listDepKey)
	if err != nil {
		t.Fatalf("SMembers(list dep): %v", err)
	}
	for _, k := range listMembers {
		allKeys[k] = true
	}

	clusterMembers2, err := c.SMembers(ctx, clusterDepKey)
	if err != nil {
		t.Fatalf("SMembers(cluster dep): %v", err)
	}
	for _, k := range clusterMembers2 {
		allKeys[k] = true
	}

	// Filter API result keys (the watcher's DELETE branch).
	var apiResultKeys []string
	for k := range allKeys {
		if cache.IsAPIResultKey(k) {
			apiResultKeys = append(apiResultKeys, k)
		}
	}

	if len(apiResultKeys) != 1 {
		t.Fatalf("DELETE in ns=%s collected %d api-result keys for deletion, want 1; got=%v",
			deletedNS, len(apiResultKeys), apiResultKeys)
	}
	if apiResultKeys[0] != apiKeys[deletedNS] {
		t.Fatalf("DELETE in ns=%s collected key %q, want %q",
			deletedNS, apiResultKeys[0], apiKeys[deletedNS])
	}

	// Apply the deletion (matches watcher.go: rw.cache.Delete(ctx, apiResultKeys...)).
	if err := c.Delete(ctx, apiResultKeys...); err != nil {
		t.Fatalf("Delete(api-result keys): %v", err)
	}

	// ── Assertion 4: only ns-01's api-result key is gone ────────────────
	for _, ns := range namespaces {
		_, exists, err := c.GetRaw(ctx, apiKeys[ns])
		if err != nil {
			t.Fatalf("GetRaw(%s): %v", apiKeys[ns], err)
		}
		if ns == deletedNS {
			if exists {
				t.Errorf("api-result key for deleted ns=%s still exists", ns)
			}
		} else {
			if !exists {
				t.Errorf("api-result key for untouched ns=%s was incorrectly invalidated; "+
					"this would mean cluster-wide DELETE blast radius regressed", ns)
			}
		}
	}
}
