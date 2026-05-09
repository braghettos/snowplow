package api

import (
	"context"
	"sync"
	"testing"

	templates "github.com/krateoplatformops/snowplow/apis/templates/v1"
	"github.com/krateoplatformops/snowplow/internal/cache"
)

// Q-5XX-DIAG (0.25.324) — auditUserAccessFilterSkipped MUST co-emit a
// counter on cache.GlobalMetrics.UAFSkipped keyed by reason. The
// existing structured log line is the audit-of-record; the counter is
// the canary scrape surface.

func clearUAFSkipped() {
	cache.GlobalMetrics.UAFSkipped.Range(func(k, _ any) bool {
		cache.GlobalMetrics.UAFSkipped.Delete(k)
		return true
	})
}

func TestAuditUserAccessFilterSkipped_BumpsCounter(t *testing.T) {
	clearUAFSkipped()
	t.Cleanup(clearUAFSkipped)

	apiCall := &templates.API{
		Name: "test",
		UserAccessFilter: &templates.UserAccessFilter{
			Verb:     "get",
			Group:    "core",
			Resource: "configmaps",
		},
	}

	ctx := context.Background()
	auditUserAccessFilterSkipped(ctx, apiCall, "api_error", "503")
	auditUserAccessFilterSkipped(ctx, apiCall, "api_error", "504")
	auditUserAccessFilterSkipped(ctx, apiCall, "", "no reason")

	snap := cache.SnapshotMap(&cache.GlobalMetrics.UAFSkipped)
	if got := snap["api_error"]; got != 2 {
		t.Errorf("UAFSkipped[api_error]: got %d, want 2 (snap=%v)", got, snap)
	}
	if got := snap["unknown"]; got != 1 {
		t.Errorf("UAFSkipped[unknown]: got %d, want 1 (snap=%v)", got, snap)
	}
}

// TestAuditUserAccessFilterSkipped_NoCounterWhenUAFNil verifies the
// no-op contract. apiCall.UserAccessFilter == nil means the api[]
// entry was not UAF-protected, so the audit (and the counter) MUST
// NOT fire — otherwise canary signal would conflate non-UAF api
// failures with UAF skips, defeating the whole H1' attribution.
func TestAuditUserAccessFilterSkipped_NoCounterWhenUAFNil(t *testing.T) {
	clearUAFSkipped()
	t.Cleanup(clearUAFSkipped)

	apiCall := &templates.API{Name: "test"} // UserAccessFilter nil

	auditUserAccessFilterSkipped(context.Background(), apiCall, "api_error", "503")

	snap := cache.SnapshotMap(&cache.GlobalMetrics.UAFSkipped)
	if len(snap) != 0 {
		t.Errorf("UAFSkipped must be empty when UAF==nil, got %v", snap)
	}
}

// TestAuditUserAccessFilterSkipped_ConcurrentSafe drives N goroutines
// against the same reason key to validate the LoadOrStore pattern in
// IncMapKey is race-free and produces an exact total. Without this
// guard a race-detector run could let multiple goroutines each store
// a fresh *atomic.Int64 and lose increments.
func TestAuditUserAccessFilterSkipped_ConcurrentSafe(t *testing.T) {
	clearUAFSkipped()
	t.Cleanup(clearUAFSkipped)

	apiCall := &templates.API{
		Name: "test",
		UserAccessFilter: &templates.UserAccessFilter{
			Verb:     "list",
			Group:    "core",
			Resource: "secrets",
		},
	}

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			auditUserAccessFilterSkipped(context.Background(), apiCall, "api_error", "x")
		}()
	}
	wg.Wait()

	snap := cache.SnapshotMap(&cache.GlobalMetrics.UAFSkipped)
	if got := snap["api_error"]; got != N {
		t.Errorf("UAFSkipped[api_error] under concurrency: got %d, want %d", got, N)
	}
}
