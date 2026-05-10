package cache_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/krateoplatformops/snowplow/internal/cache"

	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
)

// TestNewResourceWatcher_DormantWhenCacheDisabled covers PM amendment 1
// (factory dormancy unit test). When CACHE_ENABLED is unset or false:
//
//   - NewResourceWatcher MUST return (nil, nil)
//   - The factory MUST NOT be instantiated
//   - Goroutine count MUST NOT increase (delta = 0; PM amendment 3 cap < 3)
func TestNewResourceWatcher_DormantWhenCacheDisabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "")

	if !cache.Disabled() {
		t.Fatalf("Disabled() should be true with empty CACHE_ENABLED")
	}

	before := runtime.NumGoroutine()

	rw, err := cache.NewResourceWatcher(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewResourceWatcher: unexpected error: %v", err)
	}
	if rw != nil {
		t.Fatalf("NewResourceWatcher: expected nil watcher when Disabled(), got %#v", rw)
	}

	// Settle scheduler so any spawned goroutines reach steady state.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()

	after := runtime.NumGoroutine()
	delta := after - before
	if delta != 0 {
		t.Fatalf("goroutine delta = %d (want 0); before=%d after=%d", delta, before, after)
	}
}

// TestNewResourceWatcher_DormantValuesEnumerated covers every "off"
// value for CACHE_ENABLED — explicit false, 0, no, empty.
func TestNewResourceWatcher_DormantValuesEnumerated(t *testing.T) {
	for _, v := range []string{"", "false", "0", "no", "FALSE"} {
		v := v
		t.Run("CACHE_ENABLED="+v, func(t *testing.T) {
			t.Setenv("CACHE_ENABLED", v)
			if !cache.Disabled() {
				t.Fatalf("Disabled() should be true for CACHE_ENABLED=%q", v)
			}
			rw, err := cache.NewResourceWatcher(context.Background(), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rw != nil {
				t.Fatalf("expected nil watcher for CACHE_ENABLED=%q", v)
			}
		})
	}
}

// TestNewResourceWatcher_FactoryConstructedWhenCacheEnabled covers PM
// amendment 1 (other half). When CACHE_ENABLED=true:
//
//   - NewResourceWatcher MUST return a non-nil watcher
//   - factory.Start MUST NOT be called yet (deferred to 0.30.2)
//
// The "Start not called" invariant is verified indirectly: with no
// informers registered AND no Start invoked, NumGoroutine MUST NOT
// climb past the constructor itself.
func TestNewResourceWatcher_FactoryConstructedWhenCacheEnabled(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")

	if cache.Disabled() {
		t.Fatalf("Disabled() should be false when CACHE_ENABLED=true")
	}

	dyn := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme())

	before := runtime.NumGoroutine()
	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: unexpected error: %v", err)
	}
	if rw == nil {
		t.Fatalf("NewResourceWatcher: expected non-nil watcher when CACHE_ENABLED=true")
	}
	defer rw.Stop()

	// Allow any spurious scheduler activity to settle.
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()

	after := runtime.NumGoroutine()
	delta := after - before
	// PM amendment 3: < 3 goroutine delta. Construction without Start
	// should ideally be 0 — we leave headroom for one bookkeeping
	// goroutine if the factory ever adds one in future client-go versions.
	if delta >= 3 {
		t.Fatalf("goroutine delta = %d (want < 3); before=%d after=%d", delta, before, after)
	}
}

// TestNewResourceWatcher_AddResourceTypeWithoutStart documents that
// AddResourceType BEFORE Start does not spawn goroutines — only
// Start() activates the informer Run loops. This is the contract the
// 0.30.2 routing relies on.
func TestNewResourceWatcher_AddResourceTypeWithoutStart(t *testing.T) {
	t.Setenv("CACHE_ENABLED", "true")
	dyn := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme())

	rw, err := cache.NewResourceWatcher(context.Background(), dyn)
	if err != nil {
		t.Fatalf("NewResourceWatcher: %v", err)
	}
	if rw == nil {
		t.Fatalf("expected non-nil watcher")
	}
	defer rw.Stop()

	before := runtime.NumGoroutine()
	rw.AddResourceType(schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "configmaps",
	})
	runtime.Gosched()
	time.Sleep(20 * time.Millisecond)
	runtime.Gosched()
	after := runtime.NumGoroutine()

	if after-before >= 3 {
		t.Fatalf("AddResourceType (no Start) goroutine delta = %d (want < 3)", after-before)
	}
}

// TestDisabled_TruthyValues confirms the truthy whitelist.
func TestDisabled_TruthyValues(t *testing.T) {
	for _, v := range []string{"true", "1", "yes"} {
		t.Setenv("CACHE_ENABLED", v)
		if cache.Disabled() {
			t.Fatalf("Disabled() should be false for CACHE_ENABLED=%q", v)
		}
	}
}
