package cache

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	xcontext "github.com/krateoplatformops/plumbing/context"
)

// TestBindingIdentityTransition — Q-RBAC-DECOUPLE C(d) v5 — D3b
// (audit 2026-05-05).
//
// Asserts CacheIdentity emits audit=binding_identity_transition exactly
// once per (username, identity-change) pair, never on first observation
// or repeat observations of the same identity.
func TestBindingIdentityTransition(t *testing.T) {
	mkCtxWithBuf := func(bid string) (context.Context, *bytes.Buffer) {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewJSONHandler(buf, nil))
		ctx := xcontext.BuildContext(context.Background(),
			xcontext.WithLogger(log),
		)
		ctx = WithBindingIdentity(ctx, bid)
		return ctx, buf
	}

	t.Run("FirstObservation_NoEmit", func(t *testing.T) {
		resetLastBindingIdentityForTest()
		ctx, buf := mkCtxWithBuf("abc")
		got := CacheIdentity(ctx, "alice")
		if got != "abc" {
			t.Fatalf("CacheIdentity returned %q want %q", got, "abc")
		}
		if buf.Len() != 0 {
			t.Fatalf("expected zero log output on first observation; got: %s", buf.String())
		}
	})

	t.Run("IdentityChange_EmitsTransitionAudit", func(t *testing.T) {
		resetLastBindingIdentityForTest()

		// Seed: first call records "abc" silently.
		ctx1, _ := mkCtxWithBuf("abc")
		_ = CacheIdentity(ctx1, "alice")

		// Transition: second call sees "xyz" → must emit.
		ctx2, buf := mkCtxWithBuf("xyz")
		got := CacheIdentity(ctx2, "alice")
		if got != "xyz" {
			t.Fatalf("CacheIdentity returned %q want %q", got, "xyz")
		}
		out := buf.String()
		if out == "" {
			t.Fatalf("expected emit on identity transition; got nothing")
		}
		// Expect exactly ONE log line.
		lines := nonEmptyLines(out)
		if len(lines) != 1 {
			t.Fatalf("expected exactly 1 audit line; got %d:\n%s", len(lines), out)
		}
		mustContainAll(t, out, []string{
			`"audit":"binding_identity_transition"`,
			`"user":"alice"`,
			`"old_id":"abc"`,
			`"new_id":"xyz"`,
		})
	})

	t.Run("RepeatObservation_NoEmit", func(t *testing.T) {
		resetLastBindingIdentityForTest()

		// Seed.
		ctx1, _ := mkCtxWithBuf("abc")
		_ = CacheIdentity(ctx1, "alice")

		// Repeat with the SAME identity — must be silent.
		ctx2, buf := mkCtxWithBuf("abc")
		_ = CacheIdentity(ctx2, "alice")
		if buf.Len() != 0 {
			t.Fatalf("expected zero log output on repeat observation; got: %s", buf.String())
		}
	})

	t.Run("EmptyUsername_NoEmit_NoMapPollution", func(t *testing.T) {
		resetLastBindingIdentityForTest()

		ctx, buf := mkCtxWithBuf("abc")
		got := CacheIdentity(ctx, "")
		if got != "abc" {
			t.Fatalf("CacheIdentity returned %q want %q", got, "abc")
		}
		if buf.Len() != 0 {
			t.Fatalf("expected zero log output for empty username; got: %s", buf.String())
		}

		// Followup: an empty-string entry must NOT have been recorded
		// (otherwise a transition could fire spuriously).
		if _, ok := lastBindingIdentityForUser.Load(""); ok {
			t.Fatalf("empty-username key was inserted into map (must not happen)")
		}
	})

	t.Run("MultipleUsers_IndependentTransitions", func(t *testing.T) {
		resetLastBindingIdentityForTest()

		// alice: abc → def (transition expected).
		ctx, _ := mkCtxWithBuf("abc")
		_ = CacheIdentity(ctx, "alice")
		ctx2, bufAlice := mkCtxWithBuf("def")
		_ = CacheIdentity(ctx2, "alice")
		if bufAlice.Len() == 0 {
			t.Fatalf("expected alice transition emit")
		}

		// bob: xyz only (first observation; no emit).
		ctx3, bufBob := mkCtxWithBuf("xyz")
		_ = CacheIdentity(ctx3, "bob")
		if bufBob.Len() != 0 {
			t.Fatalf("expected zero emit for bob first observation; got: %s", bufBob.String())
		}
	})
}

func mustContainAll(t *testing.T, hay string, needles []string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(hay, n) {
			t.Errorf("output missing fragment %q\nFULL OUTPUT:\n%s", n, hay)
		}
	}
}

func nonEmptyLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
