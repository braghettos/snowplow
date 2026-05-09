package main

import "testing"

// TestCORSAllowedHeaders_IncludesXReloadIdx pins the Q-5XX-DIAG harness
// contract: the bench measure.py emits X-Reload-Idx on every widget
// request so the dispatcher can label per-CR 5xx counters by reload
// index. Browser preflight strips any header not on the allowlist —
// pre-0.25.325 the header was missing here while the dispatcher read
// it (widgets.go:386), producing all `reload_idx=-1` rows in the
// /metrics/runtime widgets block under load.
//
// This test fails noisily if a future refactor drops X-Reload-Idx (or
// renames it) so the canary scrape contract stays intact.
func TestCORSAllowedHeaders_IncludesXReloadIdx(t *testing.T) {
	want := []string{
		"Accept",
		"Authorization",
		"Content-Type",
		"X-Auth-Code",
		"X-Krateo-TraceId",
		"X-Reload-Idx",
	}
	if len(corsAllowedHeaders) != len(want) {
		t.Fatalf("corsAllowedHeaders: got %d entries (%v), want %d (%v)",
			len(corsAllowedHeaders), corsAllowedHeaders, len(want), want)
	}
	idx := map[string]bool{}
	for _, h := range corsAllowedHeaders {
		idx[h] = true
	}
	for _, h := range want {
		if !idx[h] {
			t.Errorf("corsAllowedHeaders missing %q (got %v)", h, corsAllowedHeaders)
		}
	}
}
