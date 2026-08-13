// refreshes_warmup_test.go — #68: during the post-(re)deploy warmup window,
// /refreshes serves the documented idle stream (keepalives) instead of a hard
// 400, but a WARM pod with a genuinely-empty/all-denied subscription still
// gets the blessed C64-1 honest-400. The divert is STRICTLY warmup-gated
// (refreshWarmupIncomplete = !IsPhase1Done || RBACGen==0) — it keys on
// readiness, never on the armed count or skip reason.
//
// 4 arms (C68-1 real predicate, C68-2 warm+NotFound stays 400):
//   ARM A — warmup-incomplete + a would-derive coord → 200 idle stream
//           (RED 400 today / GREEN after). REAL predicate: ResetPhase1DoneForTest.
//   ARM B — WARM + RBAC-denied coord → armed==0 → 400 (GREEN before AND after).
//           Asserts the WARM predicate explicitly so it can't pass via warmup.
//   ARM C — WARM + valid coord → 200 streaming (armed>=1).
//   ARM D — WARM + genuinely-NotFound coord → armed==0 → 400 (proves the divert
//           is warmup-gated, NOT armed-count-gated — the C64-1 honest fail).
//
// Hermetic: httptest + the seeded cache.Global() (seedAuthTestWidget) + the
// MarkPhase1Done / ResetPhase1DoneForTest / RBAC-publish seams. NO apiserver.
package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/krateo-platformops/plumbing/jwtutil"
	"github.com/krateo-platformops/snowplow/internal/cache"
)

// mintTokenOutsider mints a JWT for a user in a group the seed does NOT grant
// (seedAuthTestWidget binds the "devs" group only). mintToken hardcodes
// Groups=["devs"], so every user it mints IS authorized — useless for the
// RBAC-denied arm. This one puts the user in "outsiders" → genuinely denied.
func mintTokenOutsider(t *testing.T, username string) string {
	t.Helper()
	tok, err := jwtutil.CreateToken(jwtutil.CreateTokenOptions{
		Username:   username,
		Groups:     []string{"outsiders"},
		Duration:   time.Hour,
		KeyID:      refreshTestKeyID,
		PrivateKey: refreshTestPrivateKey,
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return tok
}

// subParamFor builds a ?sub= value for one IDENTITY-BOUND widgets coord on the
// given panel name (page/perPage omitted → 0,0, frontend-shaped). The widgets
// class folds the BindingUID, so an unauthorized user / a NotFound CR derives
// an empty/denied key → skipped → armed==0 (the warm-empty cases ARM B/D need).
func subParamFor(t *testing.T, name string) string {
	t.Helper()
	body := []map[string]any{{
		"class":     "widgets",
		"group":     "widgets.templates.krateo.io",
		"version":   "v1beta1",
		"resource":  "panels",
		"namespace": "krateo-system",
		"name":      name,
	}}
	raw, _ := json.Marshal(body)
	return base64.StdEncoding.EncodeToString(raw)
}

// subParamDenied — a widgets coord on the seeded panel; armed by a user with NO
// binding (userB) → RBAC-denied → empty key → skipped.
func subParamDenied(t *testing.T) string { return subParamFor(t, "dashboard-piechart") }

// subParamNotFound — a widgets coord on a panel name that has NO CR → objects.Get
// NotFound → fail-closed skip (the genuinely-empty warm case, NOT RBAC).
func subParamNotFound(t *testing.T) string { return subParamFor(t, "no-such-panel") }

// warm marks the pod warm: phase1 done AND an RBAC snapshot published
// (seedAuthTestWidget already publishes one via RebuildRBACSnapshotForTest, so
// RBACGen>0; MarkPhase1Done flips the other gate). Asserts the WARM predicate
// holds so the warm arms can't accidentally pass via the warmup path.
func assertWarm(t *testing.T) {
	t.Helper()
	cache.MarkPhase1Done()
	if !cache.IsPhase1Done() || cache.RBACGen() == 0 {
		t.Fatalf("warm setup: IsPhase1Done=%v RBACGen=%d — both gates must be satisfied for the WARM arms",
			cache.IsPhase1Done(), cache.RBACGen())
	}
}

// ARM A — warmup-incomplete → 200 idle stream (RED 400 today / GREEN after).
func TestFalsifier68_ARMA_WarmupServesIdleStream(t *testing.T) {
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t) // seeds the panel CR + RBAC (RBACGen>0)
	// REAL predicate: drive phase1 NOT done → refreshWarmupIncomplete() true.
	cache.ResetPhase1DoneForTest()
	t.Cleanup(cache.ResetPhase1DoneForTest)
	if cache.IsPhase1Done() {
		t.Fatalf("ARM A setup: phase1 must be NOT done to exercise the warmup window")
	}

	base := refreshServer(t) // the real /refreshes chain
	// Use a coord that derives EMPTY (armed==0) — the SAME shape as ARM D's warm
	// case — so the ONLY difference between this (idle 200) and ARM D (400) is
	// the warmup gate. (subParamNotFound: a panel with no CR → skip → armed==0.)
	resp, cancel := openStream(t, base, "?sub="+subParamNotFound(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ARM A RED: warmup-window /refreshes status=%d want 200 idle stream — a browser connecting "+
			"during the ~4-min warmup (armed==0 because nothing's synced yet) must get the documented keepalive "+
			"stream, not a 400.", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("ARM A: warmup idle stream Content-Type=%q want text/event-stream", ct)
	}
}

// ARM B — WARM + RBAC-denied coord → 400 (GREEN before AND after; proves the
// divert did NOT swallow a genuine all-denied subscription once warm).
func TestFalsifier68_ARMB_WarmRBACDeniedStays400(t *testing.T) {
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t)
	assertWarm(t)
	t.Cleanup(cache.ResetPhase1DoneForTest)

	base := refreshServer(t)
	// An "outsiders"-group user that the seed's devs-group binding does NOT
	// authorize → the widgets coord's objects.Get is RBAC-denied → fail-closed
	// skip → armed==0 while WARM → honest 400 (must survive the #68 divert).
	resp, cancel := openStream(t, base, "?sub="+subParamDenied(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintTokenOutsider(t, "outsider1"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ARM B: WARM + all-denied status=%d want 400 — the honest C64-1 fail must survive #68 "+
			"(the warmup divert must NOT swallow a genuinely-empty subscription).", resp.StatusCode)
	}
}

// ARM C — WARM + valid coord → 200 streaming (the normal armed path).
func TestFalsifier68_ARMC_WarmValidStreams(t *testing.T) {
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t)
	assertWarm(t)
	t.Cleanup(cache.ResetPhase1DoneForTest)

	base := refreshServer(t)
	resp, cancel := openStream(t, base, "?sub="+subParam(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ARM C: WARM + valid coord status=%d want 200 streaming", resp.StatusCode)
	}
}

// ARM D — WARM + genuinely-NotFound coord → 400 (the divert is warmup-gated,
// NOT armed-count-gated: a warm pod arming a nonexistent widget still gets the
// honest 400, never a silent idle stream).
func TestFalsifier68_ARMD_WarmNotFoundStays400(t *testing.T) {
	t.Setenv("REFRESH_SSE_ENABLED", "")
	seedAuthTestWidget(t)
	assertWarm(t)
	t.Cleanup(cache.ResetPhase1DoneForTest)

	base := refreshServer(t)
	// A coord for a widget that does NOT exist (NotFound) — userA is authorized
	// on panels, but this name has no CR → objects.Get NotFound → fail-closed
	// skip → armed==0 while WARM.
	resp, cancel := openStream(t, base, "?sub="+subParamNotFound(t), func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+mintToken(t, "userA"))
	})
	defer cancel()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ARM D C68-2: WARM + all-NotFound status=%d want 400 — the divert must be STRICTLY "+
			"warmup-gated; a warm pod arming a nonexistent widget gets the honest 400, never an idle stream.",
			resp.StatusCode)
	}
}
