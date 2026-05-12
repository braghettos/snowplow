// eager.go — Step 3 / Tag 0.30.6 binding: bulk-register the RestAction
// inventory's resource-type set into the ResourceWatcher, start the
// factory (idempotent), and WaitForCacheSync up to STARTUP_TIMEOUT.
//
// Per implementation plan §"Tag 0.30.6 — What's implemented" bullet 2:
//   EagerRegisterAll(ctx, resourceTypes): for each resource type,
//   AddResourceType; then factory.Start(ctx.Done()) (idempotent);
//   WaitForCacheSync for all resource types. Bound by
//   STARTUP_INFORMER_FANIN (default 8).
//
// The fanin only controls how many AddResourceType calls run in
// parallel; AddResourceType itself is cheap (just records a GVR and
// installs the transform), but the dynamicinformer factory amortises
// some setup work and we want bounded concurrency. Once Start() has
// been called the informers' LISTs run in parallel anyway — the
// factory handles concurrency internally.

package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// defaultStartupInformerFanin is the default upper bound on parallel
// AddResourceType calls during eager registration. Matches plan
// §"Chart values" — `env.STARTUP_INFORMER_FANIN: "8"`.
const defaultStartupInformerFanin = 8

// EagerRegisterAll registers every gvr in resourceTypes with rw, starts
// the factory (idempotent — AddResourceType inside the constructor
// already started it for the RBAC GVRs), then blocks on WaitForCacheSync
// up to timeout.
//
// Returns the number of GVRs registered and any sync error. The error
// is soft: callers MAY log + continue (the lazy fallback in
// AddResourceType handles late dispatchers).
//
// fanin <= 0 falls back to defaultStartupInformerFanin.
//
// Per plan §"Code-path falsifier": an INFO line of shape
//   eager-register: resource_types=N startup_fanin=F batches=B total_sync_ms=Y
// is emitted at the end. N==0 means the inventory walker is broken
// (audit 0.25.329 walk-back).
func EagerRegisterAll(ctx context.Context, rw *ResourceWatcher, resourceTypes []schema.GroupVersionResource, fanin int, timeout time.Duration) (int, error) {
	if rw == nil {
		return 0, fmt.Errorf("cache.eager: ResourceWatcher must be non-nil")
	}
	if fanin <= 0 {
		fanin = defaultStartupInformerFanin
	}

	start := time.Now()

	// Bounded fan-in over AddResourceType. The work is short (just
	// records a GVR and installs the transform); we still cap it so a
	// large inventory doesn't spike scheduler pressure.
	sem := make(chan struct{}, fanin)
	var wg sync.WaitGroup
	batches := 0
	for _, gvr := range resourceTypes {
		if ctx.Err() != nil {
			break
		}
		batches++
		wg.Add(1)
		sem <- struct{}{}
		go func(g schema.GroupVersionResource) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					slog.Error("cache.eager.add_resource_type_panic",
						slog.String("subsystem", "cache"),
						slog.String("gvr", g.String()),
						slog.Any("recovered", r),
					)
				}
			}()
			rw.AddResourceType(g)
		}(gvr)
	}
	wg.Wait()

	// factory.Start is idempotent (rw.Start guards with rw.started).
	// We call it after AddResourceType for the inventory GVRs so any
	// newly-registered informer's run-loop is kicked off via the
	// "post-Start late-registration" branch in addResourceTypeLocked
	// — that branch fires goroutines directly. Calling Start again
	// here is defensive and a no-op when rw was already started by
	// the constructor.
	rw.Start()

	// Block on WaitForCacheSync up to timeout. Soft failure: log and
	// return the error so the caller can decide whether to bail.
	syncErr := rw.WaitForCacheSync(ctx, timeout)
	totalMs := time.Since(start).Milliseconds()

	slog.Info("eager-register",
		slog.String("subsystem", "cache"),
		slog.Int("resource_types", len(resourceTypes)),
		slog.Int("startup_fanin", fanin),
		slog.Int("batches", batches),
		slog.Int64("total_sync_ms", totalMs),
		slog.Bool("sync_ok", syncErr == nil),
	)
	if len(resourceTypes) == 0 {
		// Falsifier per plan: 0 inventory size means the walker is
		// broken (mirror of audit 0.25.329 walk-back when
		// registerFromRedis was empty). We log loud so it's visible
		// at every restart and the dev gate catches it pre-bench.
		slog.Warn("cache.eager.inventory_empty",
			slog.String("subsystem", "cache"),
			slog.String("walker", "CollectResourceTypesFromRestActions"),
			slog.String("hint", "no RestActions in cluster OR walker is broken — audit 0.25.329"),
		)
	}

	return len(resourceTypes), syncErr
}
