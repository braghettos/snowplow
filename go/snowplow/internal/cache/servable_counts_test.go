// servable_counts_test.go — 1.12.4 §7c acceptance for ServableCounts.
//
// The risk with an aggregate derived from a per-item snapshot is that
// the two drift: someone adds a servability conjunct, updates
// servableLocked and ServableSnapshot, and the gauge quietly keeps
// reporting the old definition. So the arm below does not assert
// hard-coded numbers — it drives a REAL ResourceWatcher over a fake
// dynamic client and requires the five counts to agree, GVR for GVR,
// with what ServableSnapshot reports for the same watcher at the same
// moment. A conjunct change that misses one of the two functions reds.
package cache

import (
	"context"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// TestServableCounts_AgreesWithSnapshot builds a live watcher, syncs it,
// and cross-checks the aggregate against the per-GVR rows.
func TestServableCounts_AgreesWithSnapshot(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	t.Setenv("RESOLVED_CACHE_ENABLED", "true")

	aGVR := schema.GroupVersionResource{Group: "a.example.io", Version: "v1", Resource: "aardvarks"}
	zGVR := schema.GroupVersionResource{Group: "z.example.io", Version: "v1", Resource: "zebras"}

	scheme := runtime.NewScheme()
	_ = rbacv1.AddToScheme(scheme)
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}: "ClusterRoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}:        "ClusterRoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}:        "RoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:               "RoleList",
		aGVR: "AardvarkList",
		zGVR: "ZebraList",
	}

	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	rw, err := NewResourceWatcher(wctx, dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	defer rw.Stop()

	syncCtx, syncCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer syncCancel()
	if err := rw.WaitForCacheSync(syncCtx, 5*time.Second); err != nil {
		t.Fatalf("WaitForCacheSync: %v", err)
	}
	_, _ = rw.EnsureResourceType(aGVR)
	_, _ = rw.EnsureResourceType(zGVR)
	_ = rw.WaitForCacheSync(syncCtx, 5*time.Second)

	rows := rw.ServableSnapshot()
	if len(rows) < 2 {
		t.Fatalf("setup: only %d GVRs registered; the aggregate must be checked over several", len(rows))
	}

	// Derive the expected aggregate from the per-GVR rows — the same data
	// the JWT-gated /debug/servable diagnostic shows an operator.
	var wantRegistered, wantSynced, wantServable, wantBroken, wantConfirmed int
	for _, r := range rows {
		wantRegistered++
		if r.HasSynced {
			wantSynced++
		}
		if r.Servable {
			wantServable++
		}
		if r.WatchBroken {
			wantBroken++
		}
		if r.Confirmed {
			wantConfirmed++
		}
	}

	registered, synced, servable, broken, confirmed := rw.ServableCounts()
	if registered != wantRegistered || synced != wantSynced || servable != wantServable ||
		broken != wantBroken || confirmed != wantConfirmed {
		t.Errorf("ServableCounts disagrees with ServableSnapshot:\n"+
			"  got  registered=%d synced=%d servable=%d watchBroken=%d confirmed=%d\n"+
			"  want registered=%d synced=%d servable=%d watchBroken=%d confirmed=%d\n"+
			"  a servability conjunct changed in one function and not the other",
			registered, synced, servable, broken, confirmed,
			wantRegistered, wantSynced, wantServable, wantBroken, wantConfirmed)
	}

	// Non-degeneracy: an all-zero agreement would pass the check above
	// while proving nothing.
	if registered == 0 {
		t.Error("registered == 0 — the arm agreed trivially; no informer was actually observed")
	}

	// The package-level accessor the telemetry surfaces use must report
	// the same thing once the watcher is global.
	prev := Global()
	SetGlobal(rw)
	t.Cleanup(func() { SetGlobal(prev) })
	gr, gs, gv, gb, gc := ServableCountsSnapshot()
	if gr != registered || gs != synced || gv != servable || gb != broken || gc != confirmed {
		t.Errorf("ServableCountsSnapshot = %d/%d/%d/%d/%d; want %d/%d/%d/%d/%d",
			gr, gs, gv, gb, gc, registered, synced, servable, broken, confirmed)
	}
}

// TestServableCounts_NilAndCacheOffAreZero pins the two degenerate
// paths. A nil watcher must not panic on the OTLP collection interval,
// and under CACHE_ENABLED=false there are no informers to report — the
// gauge reads 0 rather than reaching into a watcher that should not
// exist (project_caching_is_provisional).
func TestServableCounts_NilAndCacheOffAreZero(t *testing.T) {
	var rw *ResourceWatcher
	if r, s, v, b, c := rw.ServableCounts(); r|s|v|b|c != 0 {
		t.Errorf("nil-receiver ServableCounts = %d/%d/%d/%d/%d; want all zero", r, s, v, b, c)
	}

	t.Setenv("CACHE_ENABLED", "false")
	if r, s, v, b, c := ServableCountsSnapshot(); r|s|v|b|c != 0 {
		t.Errorf("ServableCountsSnapshot under cache-off = %d/%d/%d/%d/%d; want all zero", r, s, v, b, c)
	}
}
