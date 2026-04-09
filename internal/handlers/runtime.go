package handlers

import (
	"encoding/json"
	"net/http"
	"runtime"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

// RuntimeMetrics holds the JSON structure returned by /metrics/runtime.
type RuntimeMetrics struct {
	HeapAllocMB    float64 `json:"heap_alloc_mb"`
	HeapSysMB      float64 `json:"heap_sys_mb"`
	GoroutineCount int     `json:"goroutine_count"`
	NumGC          uint32  `json:"num_gc"`
	ActiveUsers    int     `json:"active_users"`
	L1DiskFiles    int64   `json:"l1_disk_files"`
	RedisKeyCount  int64   `json:"redis_key_count"`
}

// RuntimeMetricsHandler returns an http.Handler that serves /metrics/runtime.
// It collects Go runtime stats, active user count from Redis, disk L1 file
// count, and total Redis key count. All operations are lightweight.
func RuntimeMetricsHandler(c *cache.RedisCache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)

		ctx := r.Context()

		activeUsers := 0
		if c != nil {
			members, err := c.SMembers(ctx, cache.ActiveUsersKey)
			if err == nil {
				activeUsers = len(members)
			}
		}

		m := RuntimeMetrics{
			HeapAllocMB:    float64(ms.HeapAlloc) / (1024 * 1024),
			HeapSysMB:      float64(ms.HeapSys) / (1024 * 1024),
			GoroutineCount: runtime.NumGoroutine(),
			NumGC:          ms.NumGC,
			ActiveUsers:    activeUsers,
			L1DiskFiles:    c.DiskFileCount(),
			RedisKeyCount:  c.DBSize(ctx),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(m)
	})
}
