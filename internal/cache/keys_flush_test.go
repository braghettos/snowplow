// Q-RBAC-DECOUPLE C(d) v3 — FlushResolvedPrefix is the startup hook used
// by main.go to evict stale snowplow:resolved:* entries written under an
// incompatible cache schema. On the production in-process MemCache this
// is a no-op (each pod restart starts with an empty cache), but we still
// gate the contract so a future Redis backend would automatically flush
// pre-v3 entries without manual intervention.
package cache

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestFlushResolvedPrefix_NilCache(t *testing.T) {
	n, err := FlushResolvedPrefix(context.Background(), nil)
	if err != nil {
		t.Errorf("nil-cache flush should be no-op error-free, got %v", err)
	}
	if n != 0 {
		t.Errorf("nil-cache flush should report 0 deletions, got %d", n)
	}
}

func TestFlushResolvedPrefix_EmptyCache(t *testing.T) {
	c := NewMem(0)
	n, err := FlushResolvedPrefix(context.Background(), c)
	if err != nil {
		t.Errorf("empty-cache flush err: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 deletions on empty cache, got %d", n)
	}
}

func TestFlushResolvedPrefix_RemovesResolvedKeysOnly(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

	// Seed a mix of keys: some snowplow:resolved:*, some snowplow:api-result:*,
	// and some unrelated. Only the resolved keys should be flushed.
	resolvedKeys := []string{
		ResolvedKey("user1", gvr, "ns", "name1", 0, 0),
		ResolvedKey("user1", gvr, "ns", "name2", 0, 0),
		ResolvedKey("user2", gvr, "ns", "name1", 1, 10),
	}
	apiKeys := []string{
		APIResultKey("user1", gvr, "ns", "name1"),
		APIResultKey("user2", gvr, "ns", ""),
	}
	unrelated := []string{
		"snowplow:get:templates.krateo.io/v1/restactions:ns:name1",
		"snowplow:list-idx:core/v1/namespaces:",
	}
	for _, k := range resolvedKeys {
		if err := c.SetResolvedRaw(ctx, k, []byte("dummy")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, k := range apiKeys {
		if err := c.SetAPIResultRaw(ctx, k, []byte("dummy")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, k := range unrelated {
		if err := c.SetRaw(ctx, k, []byte("dummy")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n, err := FlushResolvedPrefix(ctx, c)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if n != len(resolvedKeys) {
		t.Errorf("expected %d resolved keys flushed, got %d", len(resolvedKeys), n)
	}
	// Resolved keys should be gone.
	for _, k := range resolvedKeys {
		if _, hit, _ := c.GetRaw(ctx, k); hit {
			t.Errorf("resolved key %q should be flushed but is still present", k)
		}
	}
	// api-result keys must NOT be flushed (they hold UNFILTERED bytes that
	// remain shape-compatible across v0/v2/v3).
	for _, k := range apiKeys {
		if _, hit, _ := c.GetRaw(ctx, k); !hit {
			t.Errorf("api-result key %q should be preserved but was flushed", k)
		}
	}
	// Unrelated keys must also be preserved.
	for _, k := range unrelated {
		if _, hit, _ := c.GetRaw(ctx, k); !hit {
			t.Errorf("unrelated key %q should be preserved but was flushed", k)
		}
	}
}

func TestFlushResolvedPrefix_Idempotent(t *testing.T) {
	c := NewMem(0)
	ctx := context.Background()
	gvr := schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}
	if err := c.SetResolvedRaw(ctx, ResolvedKey("u", gvr, "ns", "n", 0, 0), []byte("x")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n, err := FlushResolvedPrefix(ctx, c); err != nil || n != 1 {
		t.Errorf("first flush: n=%d err=%v (want n=1, err=nil)", n, err)
	}
	if n, err := FlushResolvedPrefix(ctx, c); err != nil || n != 0 {
		t.Errorf("second flush should be no-op: n=%d err=%v", n, err)
	}
}
