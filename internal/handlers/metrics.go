package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/krateoplatformops/snowplow/internal/cache"
)

func CacheMetrics() http.Handler {
	return http.HandlerFunc(func(wri http.ResponseWriter, _ *http.Request) {
		snap := cache.GlobalMetrics.Snapshot()
		wri.Header().Set("Content-Type", "application/json")
		wri.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(wri)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	})
}
