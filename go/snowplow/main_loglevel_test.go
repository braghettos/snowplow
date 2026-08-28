package main

import (
	"log/slog"
	"testing"
)

// TestParseLogLevel covers the #163 --log-level / LOG_LEVEL contract: the four
// named levels (case- and whitespace-insensitive), and the back-compat default
// where "info", the empty string (unset), and any unrecognised value all
// resolve to Info — so a typo never silences logs and pre-#163 behaviour (no
// LOG_LEVEL set → Info floor) is byte-identical.
func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},         // unset → info (default / back-compat)
		{"nonsense", slog.LevelInfo}, // typo → info, never silences unexpectedly
		{"  WARN  ", slog.LevelWarn}, // trimmed + case-insensitive
		{"Error", slog.LevelError},
		{"DEBUG", slog.LevelDebug},
	}
	for _, tc := range cases {
		if got := parseLogLevel(tc.in); got != tc.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
