"""Behavioural tests for bench/browser.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.4. 13 cases.

NO source-introspection (`inspect.getsource`, `sys.modules[__name__]`).
NO live cluster. NO real Playwright Chromium launch — every test uses the
`fake_page` fixture (see conftest.py).

The ConvergenceTimeout test (acceptance criterion (h)) MUST use
`with pytest.raises(ConvergenceTimeout): browser_measure_stage(...)` —
log-string assertions are forbidden because an upstream `except:` could
swallow the exception while still letting the log message pass.
"""

from __future__ import annotations

import io
import json
import shutil
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path
from unittest import mock

import pytest


import bench.browser as browser_mod
from bench.browser import (
    BROWSER_SCALING_PAGES,
    ConvergenceTimeout,
    browser_measure_navigation,
    browser_measure_stage,
    http_get,
    http_get_json,
    login_all,
    make_browser_context,
    record_video_to_gif,
    verify_composition_count_api,
    verify_composition_count_ui,
    wait_for_compositions,
    _scroll_capture_for_video,
    _validate_widget_terminal_state,
    _count_rendered_comp_cards,
    _errored_call_namespaces,
)


# ─── helpers ────────────────────────────────────────────────────────────────


def _patch_cluster_count(monkeypatch, comp_count=0, ns_count=0,
                         panel_count=None):
    """Wire the deferred cluster.* probes used by browser_measure_stage.

    The browser module's _count_compositions / _count_bench_ns /
    _list_composition_names defer to bench.cluster.* via a lazy import
    inside the helper. We patch the wrapper, not bench.cluster, so the
    test never reaches the real kubectl path.

    Task #250 Block 2b: _validate_widget_terminal_state now also calls
    `_user_visible_composition_count` which in turn imports
    `bench.cluster.count_compositions_with_panels_ready`. Patch the
    cluster module entry so the new admin/cj path stays hermetic.
    `panel_count` defaults to None (the helper returns None → BASE
    expected fallback, matching pre-Block-2b behaviour for tests that
    don't care about the new gate).
    """
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: comp_count)
    monkeypatch.setattr(browser_mod, "_count_bench_ns", lambda: ns_count)
    monkeypatch.setattr(browser_mod, "_list_composition_names", lambda: set())
    # Hermetic isolation: prevent _user_visible_composition_count from
    # escaping to a real apiserver via the new cluster.* probe.
    from bench import cluster as _cluster_mod
    monkeypatch.setattr(_cluster_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: panel_count)


# ─── Case 1: ConvergenceTimeout via pytest.raises (acceptance (h)) ──────────


def test_browser_measure_stage_raises_ConvergenceTimeout(monkeypatch, fake_page,
                                                         tmp_path):
    """VERIFY-poll deadline expiry with matched=False MUST raise
    ConvergenceTimeout (NOT log + silently set convergence_ms=-1).

    This is acceptance criterion (h). The assertion MUST use
    `with pytest.raises(ConvergenceTimeout)` — log-string assertions
    would still pass if an upstream `except:` swallowed the exception.
    """
    # Cluster has 1200 compositions; piechart returns 0 (cache stale).
    _patch_cluster_count(monkeypatch, comp_count=1200, ns_count=20)
    # Both verify probes return 0, never matching cluster=1200.
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 0)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 0)
    # _expected_calls_lookup returns None so no widget-validation failure.
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    with pytest.raises(ConvergenceTimeout) as exc_info:
        browser_measure_stage(
            fake_page, stage_num=6, stage_desc="S6 deploy 1200",
            cache_mode="ON", token="tok", num_navs=1, user="admin",
            verify_against_cluster=True,
            verify_timeout=1,  # 1-second deadline — guaranteed to expire
            verify_interval=0,  # no inter-poll sleep
            screenshots_dir=tmp_path / "ss",
        )

    err = exc_info.value
    assert err.stage == 6
    assert err.user == "admin"
    assert err.cluster == 1200
    assert err.api == 0
    assert err.ui == 0
    assert err.timeout_secs == 1


# ─── Case 2: PASS when api == ui == cluster ─────────────────────────────────


def test_browser_measure_stage_passes_when_api_equals_ui_equals_cluster(
        monkeypatch, fake_page, tmp_path):
    """Happy path: VERIFY poll matches on the first iteration → returns
    the stage dict with convergence_ms >= 0, no exception."""
    _patch_cluster_count(monkeypatch, comp_count=42, ns_count=2)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 42)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 42)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=2, stage_desc="S2 1ns + compdef",
        cache_mode="OFF", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=10, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    assert result["stage"] == 2
    assert result["compositions"] == 42
    # Dashboard nav must carry a non-negative convergence_ms.
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["convergence_ms"] >= 0
    assert dash["verified_api"] == 42
    assert dash["verified_ui"] == 42


# ─── Case 3: cyberjoker uses intra-user consistency (worktree 6454-6462) ────


def test_browser_measure_stage_cyber_uses_intra_user_consistency(
        monkeypatch, fake_page, tmp_path):
    """When `verify_against_cluster=False` (cyberjoker), the gate is
    api == ui, NOT cluster equality. Cluster has 50K but cyberjoker
    sees 10 (RBAC scoped to one namespace); api == ui == 10 must PASS.
    """
    _patch_cluster_count(monkeypatch, comp_count=50_000, ns_count=50)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 10)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 10)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    # NO pytest.raises — must PASS despite cluster=50K vs api/ui=10.
    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="cj S6",
        cache_mode="ON", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False,
        verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash["verified_api"] == 10
    assert dash["verified_ui"] == 10
    assert dash["convergence_ms"] >= 0


# ─── Case 4: record_video_to_gif produces a <=2 MB .gif ─────────────────────


def test_record_video_to_gif_produces_under_2mb_gif(monkeypatch, tmp_path):
    """Stub ffmpeg invocation; produce a 1 KB .gif on disk and assert
    the helper returns True + the file exists + size <= 2 MB."""
    webm = tmp_path / "S1_admin_cold_dashboard.webm"
    webm.write_bytes(b"FAKE_WEBM_HEADER" * 4)
    gif = tmp_path / "S1_admin_cold_dashboard.gif"

    monkeypatch.setattr(shutil, "which", lambda name: "/opt/homebrew/bin/ffmpeg"
                        if name == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        # cmd is the argv list; the last arg is the output gif path.
        out = Path(cmd[-1])
        out.parent.mkdir(parents=True, exist_ok=True)
        out.write_bytes(b"GIF89a" + b"\x00" * 1024)  # ~1 KB fake gif

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    assert record_video_to_gif(webm, gif, fps=4, max_seconds=60, max_mb=2) is True
    assert gif.exists()
    assert gif.stat().st_size < 2 * 1024 * 1024


# ─── Case 5: record_video_to_gif returns False when ffmpeg missing ──────────


def test_record_video_to_gif_returns_false_when_ffmpeg_missing(
        monkeypatch, tmp_path):
    """shutil.which('ffmpeg') is None → returns False, NEVER raises,
    leaves the cluster/run alone (gif not created)."""
    webm = tmp_path / "v.webm"
    webm.write_bytes(b"WEBM")
    gif = tmp_path / "v.gif"

    monkeypatch.setattr(shutil, "which", lambda name: None)

    assert record_video_to_gif(webm, gif) is False
    assert not gif.exists()


# ─── Case 6: record_video_to_gif KEEPS oversize output (no size drop) ───────


def test_record_video_to_gif_keeps_oversize_gif_no_size_drop(
        monkeypatch, tmp_path):
    """Task #267 correction (Diego 2026-06-10): the oversize-delete is
    REMOVED. A large gif (e.g. from a ~25-min S6 install recording) is now
    KEPT — record_video_to_gif returns True and the file remains on disk.
    `max_mb` is accepted but a NO-OP. No video/gif is ever dropped for size.

    Falsifier: pre-correction this returned False and unlinked the gif.
    """
    webm = tmp_path / "big.webm"
    webm.write_bytes(b"WEBM")
    gif = tmp_path / "big.gif"

    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/ffmpeg"
                        if name == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        out = Path(cmd[-1])
        out.write_bytes(b"X" * (3 * 1024 * 1024))  # 3 MB — would've been dropped

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    # max_mb is now inert: the 3 MB gif is KEPT, not deleted.
    assert record_video_to_gif(webm, gif, max_mb=2) is True
    assert gif.exists(), "oversize gif must be RETAINED (no size-based drop)"
    assert gif.stat().st_size == 3 * 1024 * 1024


def test_record_video_to_gif_no_unlink_call_for_large_gif(monkeypatch, tmp_path):
    """Falsifier guard: record_video_to_gif must NOT call Path.unlink on the
    produced gif for any size — proving the size-delete branch is gone."""
    webm = tmp_path / "v.webm"
    webm.write_bytes(b"WEBM")
    gif = tmp_path / "v.gif"

    monkeypatch.setattr(shutil, "which", lambda name: "/usr/bin/ffmpeg"
                        if name == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        Path(cmd[-1]).write_bytes(b"X" * (5 * 1024 * 1024))  # 5 MB

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    unlink_calls = []
    real_unlink = Path.unlink

    def _tracking_unlink(self, *a, **k):
        unlink_calls.append(str(self))
        return real_unlink(self, *a, **k)

    monkeypatch.setattr(Path, "unlink", _tracking_unlink)
    assert record_video_to_gif(webm, gif) is True
    assert str(gif) not in unlink_calls, (
        f"gif was unlinked despite no size cap; unlink_calls={unlink_calls}")


# ─── Case 7: make_browser_context passes record_video_dir to Playwright ─────


def test_make_browser_context_passes_record_video_dir_to_playwright(tmp_path):
    """`record_video_dir=tmp_path/ "videos"` → forwarded to
    browser.new_context(record_video_dir=...). The directory is created
    if missing."""
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    videos = tmp_path / "videos" / "S1_admin"
    ctx = make_browser_context(FakeBrowser(), record_video_dir=videos)

    assert ctx is not None
    assert "record_video_dir" in captured, (
        f"new_context() did not receive record_video_dir; kwargs={captured!r}"
    )
    assert captured["record_video_dir"] == str(videos)
    assert videos.exists() and videos.is_dir(), (
        f"make_browser_context must create the video dir; "
        f"{videos} exists={videos.exists()}"
    )
    assert captured["viewport"] == {"width": 1280, "height": 900}
    assert captured["ignore_https_errors"] is True


def test_make_browser_context_skips_video_when_none(tmp_path):
    """`record_video_dir=None` → new_context() called WITHOUT the kwarg.

    Subsequent samples for the same cell pass None to stay inside R3.1's
    200 MB bundle cap.
    """
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    make_browser_context(FakeBrowser(), record_video_dir=None)
    assert "record_video_dir" not in captured, (
        f"new_context() received record_video_dir despite None; "
        f"kwargs={captured!r}"
    )


# ─── Case 7b: make_browser_context forwards storage_state (Task #307) ────────


def test_make_browser_context_forwards_storage_state():
    """`storage_state=<captured>` → forwarded verbatim to
    browser.new_context(storage_state=...).

    This is what lets a per-page recording context start ALREADY
    authenticated (browser.py:1049-1050): its first `goto` lands on the
    target page with no `/login` drive filmed. The forwarded object must be
    the SAME object passed in (no copy/transform — a Playwright storage_state
    dict is opaque to the bench)."""
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    storage_state = {"cookies": [{"name": "k", "value": "v"}],
                     "origins": [{"origin": "http://x", "localStorage": []}]}
    make_browser_context(FakeBrowser(), storage_state=storage_state)

    assert "storage_state" in captured, (
        f"new_context() did not receive storage_state; kwargs={captured!r}"
    )
    assert captured["storage_state"] is storage_state, (
        f"storage_state must be forwarded verbatim (same object); "
        f"got {captured['storage_state']!r}"
    )


def test_make_browser_context_omits_storage_state_when_none():
    """`storage_state=None` (or omitted) → new_context() called WITHOUT the
    kwarg at all — ABSENT, not present-with-None.

    A present `storage_state=None` would be a no-op for Playwright today, but
    asserting absence locks the `if storage_state is not None` guard
    (browser.py:1049): a future Playwright that rejected None, or a default
    that changed meaning, must not silently regress an unauthenticated
    throwaway-login context into a pre-seeded one."""
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    make_browser_context(FakeBrowser(), storage_state=None)
    assert "storage_state" not in captured, (
        f"new_context() received storage_state despite None — the guard must "
        f"OMIT the kwarg, not pass None; kwargs={captured!r}"
    )


# ─── Case 8: terminal-state PASSes with zero skeletons ──────────────────────


def test_widget_terminal_state_passes_when_no_skeletons(monkeypatch,
                                                        fake_page):
    """`_validate_widget_terminal_state` returns terminal_state='pass'
    when skeleton_count == 0 AND /call count matches expectation."""
    fake_page._skeleton_scoped = 0
    fake_page._skeleton_raw = 0
    fake_page._skeleton_widget = 0
    fake_page._call_count = 16
    fake_page._errored_count = 0

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard",
                                        "label-cold", user="admin")
    assert v["terminal_state"] == "pass"
    assert v["skeleton_count"] == 0
    assert v["actual_calls"] == 16
    assert v["calls_within_tolerance"] is True


# ─── Case 9: terminal-state FAILs when skeletons persist ────────────────────


def test_widget_terminal_state_fails_when_skeletons_persist(monkeypatch,
                                                            fake_page):
    """skeleton_count > 0 → terminal_state='fail' (the stability poll
    exited prematurely; the page is still rendering)."""
    fake_page._skeleton_scoped = 3
    fake_page._skeleton_raw = 5
    fake_page._call_count = 16

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard", "label",
                                        user="admin")
    assert v["terminal_state"] == "fail"
    assert v["skeleton_count"] == 3


# ─── Case 10: scoped selector excludes Drawer/Notification false positives ──


def test_validate_widget_terminal_state_uses_scoped_selector_not_naked_ant_skeleton(
        monkeypatch, fake_page):
    """The widget validator counts SCOPED skeletons (excluding Drawer/
    Notification/Modal/Message/Popover/Dropdown/Tooltip surfaces). A
    page with raw=10 (Notification toast) but scoped=0 must PASS — a
    toast firing during nav must NOT produce a false positive.
    """
    fake_page._skeleton_scoped = 0    # NO widget-tree skeletons
    fake_page._skeleton_raw = 10      # raw includes overlay toasts
    fake_page._skeleton_widget = 0
    fake_page._call_count = 16

    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard", "label",
                                        user="admin")
    assert v["terminal_state"] == "pass", (
        f"scoped=0 + raw=10 (overlay toasts) must PASS — "
        f"got terminal_state={v['terminal_state']!r}, validation={v!r}"
    )
    assert v["skeleton_count"] == 0
    assert v["skeleton_count_raw"] == 10


# ─── Task #284: DOM-derived n_visible for the /compositions call-count gate ─
#
# The EXPECTED_CALLS gate must derive `n_visible` from the cards the datagrid
# actually rendered on page 1 at nav-time, NOT from a cluster-wide apiserver
# LIST that races ahead of the render. These cases exercise the real
# expected_calls() formula (NOT monkeypatched) through the rendered-card
# count so the three Phase 6 regimes pass and a genuine under-call still fails.
#
#   regime              rendered cards   expected (admin, 10 + 4×min(N,5))
#   S4 fresh deploy     0                10  (BASE only)
#   S5 one materialized 1                14
#   S6 50K saturated    5  (or more)     30  (clamped to per_page=5)


def test_count_rendered_comp_cards_reads_dom(fake_page):
    """`_count_rendered_comp_cards` returns the DOM-rendered card count
    the page reports, clamped to >= 0 on failure."""
    fake_page._rendered_cards = 3
    assert _count_rendered_comp_cards(fake_page) == 3
    fake_page._rendered_cards = 0
    assert _count_rendered_comp_cards(fake_page) == 0


def test_count_rendered_comp_cards_returns_zero_on_evaluate_failure(fake_page):
    """An evaluate() exception → 0 (treated as BASE-only expected)."""
    def _boom(js, *args):
        raise RuntimeError("page closed")
    fake_page.evaluate = _boom
    assert _count_rendered_comp_cards(fake_page) == 0


def test_terminal_state_s4_zero_rendered_cards_expects_base(monkeypatch,
                                                            fake_page):
    """S4 regime: 0 cards rendered on page 1 (panels mid-materialization)
    → expected = BASE (10). actual=10 → PASS.

    This is the sole Phase 6 INVALID blocker: previously the cluster LIST
    raced ahead to >=5 panels → expected=30 → false-fail against actual=10.
    """
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 0
    fake_page._call_count = 10
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "S4 ON nav#1 Compositions",
                                        user="admin")
    assert v["n_visible"] == 0
    assert v["expected_calls"] == 10
    assert v["actual_calls"] == 10
    assert v["calls_within_tolerance"] is True
    assert v["terminal_state"] == "pass"


def test_terminal_state_s5_one_rendered_card_expects_14(monkeypatch,
                                                        fake_page):
    """S5 regime: exactly 1 card materialized on page 1 → expected =
    10 + 4×1 = 14. actual=14 → PASS."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 1
    fake_page._call_count = 14
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "S5 ON nav#1 Compositions",
                                        user="admin")
    assert v["n_visible"] == 1
    assert v["expected_calls"] == 14
    assert v["actual_calls"] == 14
    assert v["terminal_state"] == "pass"


def test_terminal_state_s6_five_rendered_cards_expects_30(monkeypatch,
                                                          fake_page):
    """S6 50K regime: page 1 saturated at per_page=5 cards → expected =
    10 + 4×5 = 30. actual=30 → PASS (unchanged from prior behaviour)."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 30
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "S6 ON nav#1 Compositions",
                                        user="admin")
    assert v["n_visible"] == 5
    assert v["expected_calls"] == 30
    assert v["actual_calls"] == 30
    assert v["terminal_state"] == "pass"


def test_terminal_state_more_than_per_page_rendered_clamps_to_30(monkeypatch,
                                                                 fake_page):
    """Rendered cards > per_page (e.g. a scrolled/larger viewport) still
    clamps the fan-out term to min(N,5) → expected=30."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 8
    fake_page._call_count = 30
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="admin")
    assert v["n_visible"] == 8
    assert v["expected_calls"] == 30
    assert v["terminal_state"] == "pass"


def test_terminal_state_genuine_under_call_still_fails(monkeypatch, fake_page):
    """Defect-detection preserved: 5 cards rendered (expected=30) but the
    page issued only 10 /calls (per-card widget RESTActions silently
    missing) → mismatch → FAIL. This is the real under-call the gate must
    catch; the DOM-derived source does NOT mask it because the card count
    is read from render state, independent of the /call timeline."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5     # page painted 5 cards
    fake_page._call_count = 10        # but only fired BASE /calls
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="admin")
    assert v["n_visible"] == 5
    assert v["expected_calls"] == 30
    assert v["actual_calls"] == 10
    assert v["calls_within_tolerance"] is False
    assert v["terminal_state"] == "fail"


# ─── Task #296 — S10 controller-churn ghost demotion ────────────────────────


def _call_status(name, ns, status):
    return {"url": ("http://fake/call?apiVersion=widgets.templates.krateo.io"
                    f"%2Fv1beta1&resource=panels&name={name}&namespace={ns}"),
            "status": status}


def test_errored_call_namespaces_extracts_ns_of_non_200():
    """The helper returns the `namespace` query-param of every non-200
    /call, and only those."""
    statuses = [
        _call_status("p1", "bench-ns-16", 404),
        _call_status("p2", "bench-ns-01", 404),
        _call_status("ok", "bench-ns-09", 200),   # 200 → excluded
    ]
    assert _errored_call_namespaces(statuses) == {"bench-ns-16", "bench-ns-01"}
    assert _errored_call_namespaces([]) == set()
    assert _errored_call_namespaces(None) == set()


def test_s10_churn_ghost_outside_deleted_ns_demotes_to_warn(monkeypatch,
                                                            fake_page):
    """#296: S10 admin Compositions nav with 5 ghost panel 404s whose
    namespaces are all OUTSIDE the bench-deleted ns → the over-call
    call_count_mismatch is DEMOTED to a recorded WARN (terminal_state
    stays 'pass'), mirroring the efaf1a4 skeleton demotion.

    Shape from the trace: 5 rendered cards (expected=30) but actual=35
    because the 5 newest cards' Panel CRs were controller-churned and the
    SPA GETs 404 each → +5 ghost calls. The deleted ns is bench-ns-50;
    the errored panels are in bench-ns-16/01/28/09/12 (all != 50)."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 35          # 30 structural + 5 ghost over-calls
    fake_page._errored_count = 5
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    call_statuses = [
        _call_status("bench-app-16-32-composition-panel", "bench-ns-16", 404),
        _call_status("bench-app-01-34-composition-panel", "bench-ns-01", 404),
        _call_status("bench-app-28-32-composition-panel", "bench-ns-28", 404),
        _call_status("bench-app-09-33-composition-panel", "bench-ns-09", 404),
        _call_status("bench-app-12-31-composition-panel", "bench-ns-12", 404),
    ]
    v = _validate_widget_terminal_state(
        fake_page, "/compositions", "S10 ON nav#1 Compositions",
        user="admin", deleted_ns="bench-ns-50", call_statuses=call_statuses)

    assert v["expected_calls"] == 30
    assert v["actual_calls"] == 35
    assert v["calls_within_tolerance"] is False
    # Demoted: NOT a terminal fail.
    assert v["terminal_state"] == "pass", (
        f"controller-churn ghost outside deleted ns must demote to WARN; "
        f"got {v['terminal_state']!r}")
    assert v["s10_churn_demoted"] is True
    assert v["s10_churn_errors"]["errored_count"] == 5
    assert v["s10_churn_errors"]["deleted_ns"] == "bench-ns-50"
    assert "bench-ns-50" not in v["s10_churn_errors"]["errored_namespaces"]


def test_s10_churn_ghost_IN_deleted_ns_stays_hard_fail(monkeypatch, fake_page):
    """#296 counter-falsifier: if ANY errored panel is IN the bench-deleted
    ns, that is a genuine serve-stale ghost and MUST stay a HARD fail (no
    demotion)."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 35
    fake_page._errored_count = 5
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    call_statuses = [
        _call_status("bench-app-16-32-composition-panel", "bench-ns-16", 404),
        # This one IS in the deleted ns — a real ghost serve.
        _call_status("bench-app-50-01-composition-panel", "bench-ns-50", 404),
    ]
    v = _validate_widget_terminal_state(
        fake_page, "/compositions", "S10 ON nav#1 Compositions",
        user="admin", deleted_ns="bench-ns-50", call_statuses=call_statuses)

    assert v["terminal_state"] == "fail", (
        "an errored panel IN the deleted ns is a genuine serve-stale ghost "
        "and must stay a HARD fail")
    assert v["s10_churn_demoted"] is False


def test_s10_no_demotion_without_deleted_ns(monkeypatch, fake_page):
    """Off-S10 (deleted_ns=None), the demotion path is inert: an over-call
    mismatch fails exactly as before (no behavioural change for the other
    stages)."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 35
    fake_page._errored_count = 5
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    call_statuses = [
        _call_status("p", "bench-ns-16", 404),
    ]
    v = _validate_widget_terminal_state(
        fake_page, "/compositions", "S6 ON nav#1 Compositions",
        user="admin", deleted_ns=None, call_statuses=call_statuses)
    assert v["terminal_state"] == "fail"
    assert v["s10_churn_demoted"] is False


def test_s10_under_call_not_demoted_even_outside_deleted_ns(monkeypatch,
                                                            fake_page):
    """#296 guard: the demotion is for OVER-calls (extra ghost GETs) only.
    A genuine UNDER-call (actual < expected — real missing per-card
    widgets) must NOT be demoted even during S10 with errors outside the
    deleted ns."""
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 12          # under expected=30 → real under-call
    fake_page._errored_count = 5
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    call_statuses = [
        _call_status("p", "bench-ns-16", 404),
    ]
    v = _validate_widget_terminal_state(
        fake_page, "/compositions", "S10 ON nav#1 Compositions",
        user="admin", deleted_ns="bench-ns-50", call_statuses=call_statuses)
    assert v["actual_calls"] == 12
    assert v["terminal_state"] == "fail", (
        "an under-call must stay a hard fail; the churn demotion is "
        "over-call-only")
    assert v["s10_churn_demoted"] is False


def test_terminal_state_cj_two_widgets_per_card(monkeypatch, fake_page):
    """RBAC parity: cyberjoker fires 2 widgets per card (Buttons filtered
    by allowed=false, Task #273), so 5 rendered cards → 10 + 2×5 = 20.
    The DOM-derived n_visible threads through the per-user plate unchanged.
    """
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 5
    fake_page._call_count = 20
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="cyberjoker")
    assert v["n_visible"] == 5
    assert v["expected_calls"] == 20
    assert v["terminal_state"] == "pass"


def test_terminal_state_compositions_does_not_call_cluster_list(monkeypatch,
                                                                fake_page):
    """The gate must NOT reach the cluster-LIST source for call-count
    prediction anymore. If `_user_visible_composition_count` is invoked
    during the /compositions gate, fail loudly — the DOM count is the only
    n_visible source for the gate (cluster LIST is content-only now)."""
    def _must_not_call(*a, **kw):
        raise AssertionError(
            "gate reached cluster-LIST n_visible source; Task #284 requires "
            "the DOM-rendered card count to drive call-count prediction")
    monkeypatch.setattr(browser_mod, "_user_visible_composition_count",
                        _must_not_call)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)
    fake_page._skeleton_scoped = 0
    fake_page._rendered_cards = 1
    fake_page._call_count = 14

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="admin")
    assert v["expected_calls"] == 14
    assert v["terminal_state"] == "pass"


# ─── Task #284 addition: gated skeleton demotion (skeleton_materializing) ───
#
# An .ant-skeleton at nav-time is demoted from HARD FAIL to a recorded WARN
# ONLY when ALL five conditions hold (page=/compositions, page-1 not
# saturated, no errored widget, call-count gate clean, and /calls issued
# exactly account for rendered cards). If ANY condition fails it stays a
# HARD FAIL — real stuck-widget / premature-stability / under-call detection
# is preserved.


def test_skeleton_demoted_to_warn_when_materializing_s4(monkeypatch, fake_page):
    """S4 materializing: rendered=0, skeleton=2, actual=10, expected=10,
    errored=0 → PASS with skeleton_materializing=True (benign race)."""
    fake_page._skeleton_scoped = 2
    fake_page._rendered_cards = 0
    fake_page._call_count = 10
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "S4 ON nav#1 Compositions",
                                        user="admin")
    assert v["terminal_state"] == "pass"
    assert v["skeleton_materializing"] is True
    assert v["skeleton_count"] == 2
    assert v["rendered_cards"] == 0


def test_skeleton_stuck_at_saturation_still_fails(monkeypatch, fake_page):
    """Stuck at saturation: rendered=5 (page-1 full), skeleton=1, actual=30,
    expected=30, errored=0 → still FAIL (condition 2: not <per_page)."""
    fake_page._skeleton_scoped = 1
    fake_page._rendered_cards = 5
    fake_page._call_count = 30
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="admin")
    assert v["terminal_state"] == "fail"
    assert v["skeleton_materializing"] is False


def test_skeleton_rendered_but_didnt_fire_still_fails(monkeypatch, fake_page):
    """Rendered-but-didn't-fire: rendered=2, skeleton=1, actual=10,
    expected=18, errored=0 → FAIL (conditions 4 + 5: /calls do not account
    for the 2 rendered cards). This is the real under-call the gate must
    catch even though page-1 is unsaturated."""
    fake_page._skeleton_scoped = 1
    fake_page._rendered_cards = 2
    fake_page._call_count = 10       # only BASE fired; expected 10+4×2=18
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/compositions",
                                        "Compositions", user="admin")
    assert v["expected_calls"] == 18
    assert v["terminal_state"] == "fail"
    assert v["skeleton_materializing"] is False


def test_skeleton_on_non_compositions_page_still_fails(monkeypatch, fake_page):
    """Non-compositions page with a skeleton → FAIL (condition 1).
    /dashboard stuck-widget detection is unchanged by Task #284."""
    fake_page._skeleton_scoped = 1
    fake_page._call_count = 16
    fake_page._errored_count = 0
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: 16)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    v = _validate_widget_terminal_state(fake_page, "/dashboard",
                                        "S6 ON nav#1 Dashboard", user="admin")
    assert v["terminal_state"] == "fail"
    assert v["skeleton_materializing"] is False
    assert v["rendered_cards"] is None


# ─── Case 11: login_all returns dict keyed by username ──────────────────────


def test_login_all_returns_dict_keyed_by_username(monkeypatch):
    """`login_all()` populates a {username: token} dict from USERS."""
    monkeypatch.setattr(browser_mod, "_ensure_users",
                        lambda: {"admin": "pw1", "cyberjoker": "pw2"})

    def fake_login(user, pw):
        return f"jwt-for-{user}"

    monkeypatch.setattr(browser_mod, "login", fake_login)

    tokens = login_all()
    assert tokens == {"admin": "jwt-for-admin",
                      "cyberjoker": "jwt-for-cyberjoker"}


# ─── Case 12: http_get returns response data via monkeypatched urlopen ──────


def test_http_get_returns_response_data(monkeypatch):
    """`http_get(path, token)` reads body via urllib.request.urlopen
    and returns (ms, code, body)."""
    fake_body = b'{"ok": true, "data": [1, 2, 3]}'

    class FakeResponse:
        status = 200
        headers = {"Content-Type": "application/json"}

        def __init__(self, body):
            self._body = body

        def read(self):
            return self._body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=120):
        return FakeResponse(fake_body)

    monkeypatch.setattr(urllib.request, "urlopen", fake_urlopen)

    ms, code, body = http_get("/call?foo", "fake-token")
    assert code == 200
    assert body == fake_body
    assert ms >= 0


def test_http_get_json_parses_response(monkeypatch):
    """`http_get_json` decodes the body as JSON."""
    fake_body = b'{"items": [{"name": "comp-1"}, {"name": "comp-2"}]}'

    class FakeResponse:
        status = 200
        headers = {}

        def read(self):
            return fake_body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    monkeypatch.setattr(urllib.request, "urlopen",
                        lambda req, timeout=120: FakeResponse())

    ms, code, parsed = http_get_json("/call?foo", "tok")
    assert code == 200
    assert isinstance(parsed, dict)
    assert parsed["items"][0]["name"] == "comp-1"


# ─── Case 13: wait_for_compositions / wait_for_compositions timeout ─────────


def test_wait_for_compositions_returns_true_when_count_at_target(monkeypatch):
    """When `count_compositions()` reaches `expected` immediately,
    `wait_for_compositions` returns True without waiting."""
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: 100)
    assert wait_for_compositions(100, timeout=5, tolerance=0) is True


def test_wait_for_compositions_returns_false_on_timeout(monkeypatch):
    """When `count_compositions()` never reaches `expected`,
    `wait_for_compositions` returns False after the deadline.

    Per `feedback_silent_skip_breaks_convergence_proof.md` (Block 5 memory
    file), helpers returning False on timeout MUST be checked by callers.
    This test pins the return type contract so the storm.run_user_scaling
    deferred-import wrapper continues to receive a bool.
    """
    monkeypatch.setattr(browser_mod, "_count_compositions", lambda: 0)
    # Skip the 5s inter-poll sleep so the test wall-clock stays ≤2s.
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)
    assert wait_for_compositions(100, timeout=1, tolerance=0) is False


# ─── Case 14: smoke video pipeline (per chosen smoke-gate option (b)) ───────


def test_smoke_video_pipeline(monkeypatch, tmp_path):
    """Smoke-gate replacement for `python -m bench phase6 --to-stage S1
    --video representative --tag 0.30.232` (per §G Block 3).

    Per the Block 3 dispatch §6 option (b), the smoke gate runs as an
    in-process unit test against fake Playwright + fake ffmpeg. Exercises:

      1. `make_browser_context(..., record_video_dir=videos/)` creates
         the directory and forwards the kwarg.
      2. `record_video_to_gif(webm, gif)` is invoked with a 0-rc ffmpeg
         stub and produces a small (<2 MB) gif on disk.

    Replacing the live smoke run with this test keeps Block 3 fully
    unit-testable + decoupled from Block 4's phases.py / state.json.
    Block 4 will replace this with a real S1 smoke as its own gate;
    until then, this is the falsifier for the criterion.
    """
    captured: dict = {}

    class FakeBrowser:
        def new_context(self, **kwargs):
            captured.update(kwargs)
            return mock.MagicMock(name="BrowserContext")

    # Step 1: make_browser_context with record_video_dir
    videos_dir = tmp_path / "videos"
    ctx = make_browser_context(FakeBrowser(), record_video_dir=videos_dir)
    assert ctx is not None
    assert captured.get("record_video_dir") == str(videos_dir)
    assert videos_dir.exists()

    # Step 2: simulate a recorded .webm landing on disk
    webm = videos_dir / "S1_admin_cold_dashboard.webm"
    webm.write_bytes(b"FAKE_WEBM" * 32)
    gif = videos_dir / "S1_admin_cold_dashboard.gif"

    # ffmpeg stub
    monkeypatch.setattr(shutil, "which", lambda n: "/opt/homebrew/bin/ffmpeg"
                        if n == "ffmpeg" else None)

    def fake_ffmpeg(cmd, **kwargs):
        out = Path(cmd[-1])
        out.write_bytes(b"GIF89a" + b"\x00" * 1024)

        class _Proc:
            returncode = 0
            stderr = ""
            stdout = ""
        return _Proc()

    monkeypatch.setattr(subprocess, "run", fake_ffmpeg)

    ok = record_video_to_gif(webm, gif, fps=4, max_seconds=60, max_mb=2)
    assert ok is True, "smoke video pipeline must produce a gif"
    assert gif.exists()
    assert gif.stat().st_size < 2 * 1024 * 1024


# ─── verify_composition_count_api error paths ───────────────────────────────


def test_verify_composition_count_api_returns_minus_one_on_non_200(
        monkeypatch):
    """Non-200 response → returns -1 (used to express "indeterminate")."""
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token: (50, 500, None))
    assert verify_composition_count_api("tok") == -1


def test_verify_composition_count_api_returns_count_on_200(monkeypatch):
    """200 with status.widgetData.title='1200' → returns 1200."""
    body = {"status": {"widgetData": {"title": "1200"}}}
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token: (12, 200, body))
    assert verify_composition_count_api("tok") == 1200


# ─── verify_composition_count_ui delegates to page.evaluate ─────────────────


def test_verify_composition_count_ui_reads_via_page_evaluate(fake_page):
    """`verify_composition_count_ui(page)` invokes page.evaluate with a
    JS string that fetches the piechart endpoint, and returns the JS
    result directly. FakePage's `evaluate` dispatches on the JS prefix
    `(async () => {` and returns `_ui_count`.
    """
    fake_page._ui_count = 1234
    assert verify_composition_count_ui(fake_page) == 1234


# ─── Task #181: CONTENT-DRIFT is RBAC-aware ─────────────────────────────────


def test_content_drift_uses_cluster_truth_for_admin(
        monkeypatch, fake_page, tmp_path):
    """When `verify_against_cluster=True` (admin), CONTENT-DRIFT compares
    `list_composition_names_from_cache(token)` against the full cluster
    via `_list_composition_names()` and sets `content_truth_source =
    "cluster"` on the navigation result.

    Admin's RBAC permits the cluster-wide LIST so cluster-truth is the
    authoritative comparison set.
    """
    truth_names = {f"ns{i}/comp{i}" for i in range(5)}
    _patch_cluster_count(monkeypatch, comp_count=len(truth_names), ns_count=2)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: truth_names)
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: set(truth_names))
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: len(truth_names))
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: len(truth_names))
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="admin S6",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash.get("content_match") is True, (
        f"admin CONTENT must PASS when cache == cluster-truth; got "
        f"{dash.get('content_match')!r}")
    assert dash.get("content_truth_source") == "cluster", (
        f"admin must compare against cluster-truth, got "
        f"{dash.get('content_truth_source')!r}")


def test_content_drift_cyber_is_diagnostic_non_load_bearing(
        monkeypatch, fake_page, tmp_path):
    """Task #298: the cj intra-user CONTENT comparison is STRUCTURALLY
    MISMATCHED (cj's cluster-wide compositions-list is legitimately ~0
    because cj has no cluster-scoped LIST perm, vs the RBAC-narrowed
    api_count). Per the trace it is now an explicitly-labeled
    NON-LOAD-BEARING diagnostic: cj MUST NOT set `content_match`
    (true OR false) and MUST emit the labeled diagnostic fields.

    Construct the real-world shape: cached cluster-wide list = 0 names,
    api/ui = 1000 (RBAC-narrowed). Pre-#298 this set content_match=False
    (a permanent false positive). Post-#298 it sets neither pass nor fail.
    """
    cj_cached = set()  # cj sees the cluster-wide compositions-list as empty
    _patch_cluster_count(monkeypatch, comp_count=50_000, ns_count=500)
    monkeypatch.setattr(browser_mod, "_list_composition_names",
                        lambda: {f"ns/c{i}" for i in range(50_000)})
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: cj_cached)
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 1000)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 1000)
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="cj S6",
        cache_mode="ON", token="tok", num_navs=1, user="cyberjoker",
        verify_against_cluster=False, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    # NEITHER pass nor fail — the structurally-mismatched comparison is no
    # longer treated as a CONTENT check (the false-positive is gone).
    assert "content_match" not in dash, (
        f"cj CONTENT comparison must NOT set content_match (it is a "
        f"structurally-mismatched non-load-bearing diagnostic per #298); "
        f"got content_match={dash.get('content_match')!r}")
    assert dash.get("content_truth_source") == "cj_diagnostic_non_load_bearing", (
        f"cj must label the comparison as a non-load-bearing diagnostic, "
        f"got {dash.get('content_truth_source')!r}")
    # The diagnostic fields ARE recorded (post-run inspectable) but are
    # explicitly labeled and do not gate anything.
    assert dash.get("content_cj_cached_count") == 0
    assert dash.get("content_cj_api_count") == 1000
    # Cluster-truth diff fields must NOT appear for cj.
    assert "content_missing" not in dash
    assert "content_extra" not in dash


def test_content_drift_admin_cluster_truth_still_load_bearing(
        monkeypatch, fake_page, tmp_path):
    """Regression guard (#298): the ADMIN cluster-truth CONTENT check
    remains the load-bearing CONTENT gate — only the cj branch was
    demoted. Admin with matching cached==cluster-truth sets
    content_match=True / truth_source='cluster'; a divergence sets
    content_match=False. (feedback_validate_content_not_just_status.)
    """
    truth = {f"ns/c{i}" for i in range(42)}
    _patch_cluster_count(monkeypatch, comp_count=len(truth), ns_count=2)
    monkeypatch.setattr(browser_mod, "_list_composition_names", lambda: truth)
    monkeypatch.setattr(browser_mod, "list_composition_names_from_cache",
                        lambda token: set(truth))  # admin cache matches truth
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: len(truth))
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: len(truth))
    monkeypatch.setattr(browser_mod, "_expected_calls_lookup",
                        lambda u, p, **kw: None)
    monkeypatch.setattr(browser_mod, "_expected_calls_tolerance", lambda: 0)

    result = browser_measure_stage(
        fake_page, stage_num=6, stage_desc="admin S6",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
    )
    dash = result["pages"]["Dashboard"]["navigations"][0]
    assert dash.get("content_match") is True
    assert dash.get("content_truth_source") == "cluster"


# ─── Task #250 Block 2 — Phase 6 S8/S9 probes ──────────────────────────────


def test_read_snowplow_expvar_int_returns_int_on_200(monkeypatch):
    """Happy path: /debug/vars returns 200 + JSON dict with int value
    → integer returned (not float, not None).
    """
    import bench.browser as browser_mod

    captured_urls = []

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def read(self):
            return b'{"snowplow_rbac_publish_seq": 414, "other": "x"}'

    def _fake_urlopen(req, timeout=None):
        captured_urls.append(req.full_url)
        return _FakeResp()

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        _fake_urlopen)
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out == 414
    assert captured_urls == ["http://fake:8081/debug/vars"]


def test_read_snowplow_expvar_int_returns_none_on_transport_failure(
        monkeypatch):
    """Network failure / unreachable host → None (fail closed). Caller
    interprets None as 'cannot read', NOT as 'value is 0'.
    """
    import bench.browser as browser_mod

    def _fake_urlopen(req, timeout=None):
        raise OSError("connection refused")

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        _fake_urlopen)
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out is None


def test_read_snowplow_expvar_int_returns_none_on_missing_key(monkeypatch):
    """200 + JSON dict without the requested key → None (caller fails
    closed; do not confuse 'absent' with '0').
    """
    import bench.browser as browser_mod

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self): return self
        def __exit__(self, *a): return False
        def read(self):
            return b'{"other_key": 1}'

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp())
    out = browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081")
    assert out is None


def test_read_snowplow_expvar_int_rejects_bool_value(monkeypatch):
    """True/False at the key (which int() would coerce to 1/0) returns
    None — a wrong-shape value on the snowplow side surfaces explicitly.
    """
    import bench.browser as browser_mod

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self): return self
        def __exit__(self, *a): return False
        def read(self):
            return b'{"snowplow_rbac_publish_seq": true}'

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp())
    assert browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081") is None


# ─── Task #221 (bench half) — prewarm-boundary expvar probe + fallback ──────
#
# prewarm_complete_via_expvar reads the snowplow_prewarm_complete map expvar
# (internal/cache/prewarm_complete_metric.go) via the established
# read_snowplow_expvar_map transport seam. True (done=1) / False (done=0) /
# None (absent — pre-0.30.264 / cache-off / transport failure → caller must
# fall back to slog-scan). Tests drive the transport via base_url + a mocked
# urlopen, the SAME seam the expvar-int/map transport tests use.


def _stub_debug_vars(monkeypatch, payload_bytes):
    """Install a fake urlopen returning `payload_bytes` from /debug/vars."""
    import bench.browser as browser_mod

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self): return self
        def __exit__(self, *a): return False
        def read(self): return payload_bytes

    monkeypatch.setattr(browser_mod.urllib.request, "urlopen",
                        lambda req, timeout=None: _FakeResp())


def test_prewarm_complete_via_expvar_true_when_done_1(monkeypatch):
    """Key present + done==1 → True (the prewarm boundary has been reached)."""
    import bench.browser as browser_mod
    _stub_debug_vars(
        monkeypatch,
        b'{"snowplow_prewarm_complete": {"done": 1, "elapsed_ms": 1840}}')
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is True


def test_prewarm_complete_via_expvar_false_when_done_0(monkeypatch):
    """Key present + done==0 → False (present but NOT yet reached). The key
    exists, so the caller keeps polling the expvar (authoritative) and does
    NOT fall back to slog-scan."""
    import bench.browser as browser_mod
    _stub_debug_vars(
        monkeypatch,
        b'{"snowplow_prewarm_complete": {"done": 0, "elapsed_ms": -1}}')
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is False


def test_prewarm_complete_via_expvar_none_when_key_absent(monkeypatch):
    """Key ABSENT (image through 0.30.264, or CACHE_ENABLED=false Disabled()
    gate) → None → caller MUST fall back to slog-scan."""
    import bench.browser as browser_mod
    _stub_debug_vars(monkeypatch, b'{"snowplow_refresher_completed_total": 7}')
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is None


def test_prewarm_complete_via_expvar_none_on_transport_failure(monkeypatch):
    """Unreachable /debug/vars → None (fail to fallback, never crash)."""
    import bench.browser as browser_mod

    def _boom(req, timeout=None):
        raise OSError("connection refused")
    monkeypatch.setattr(browser_mod.urllib.request, "urlopen", _boom)
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is None


def test_prewarm_complete_via_expvar_none_on_wrong_done_shape(monkeypatch):
    """`done` present but wrong type (bool / string) → None → fallback. A bool
    must NOT be coerced to 0/1 (int() would), and a missing `done` is absent."""
    import bench.browser as browser_mod
    # done is a bool → reject (would coerce to 1 via int()).
    _stub_debug_vars(
        monkeypatch,
        b'{"snowplow_prewarm_complete": {"done": true, "elapsed_ms": 5}}')
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is None
    # done missing entirely from an otherwise-present map → None.
    _stub_debug_vars(
        monkeypatch,
        b'{"snowplow_prewarm_complete": {"elapsed_ms": 5}}')
    assert browser_mod.prewarm_complete_via_expvar(
        base_url="http://fake:8081") is None


def test_wait_for_l1_warmup_prefers_expvar_and_skips_slog_scan(monkeypatch):
    """When the expvar reports done=1, wait_for_l1_warmup returns True via the
    expvar path and NEVER shells kubectl for the slog-scan (the expvar is
    authoritative when present)."""
    import bench.browser as browser_mod
    import bench.cluster as cluster_mod

    monkeypatch.setattr(browser_mod, "prewarm_complete_via_expvar",
                        lambda *a, **k: True)

    def _kubectl_tripwire(*a, **k):
        raise AssertionError(
            "wait_for_l1_warmup shelled kubectl slog-scan despite expvar done=1")
    monkeypatch.setattr(cluster_mod, "kubectl", _kubectl_tripwire)
    # get_runtime_metrics must also not be consulted on the expvar path.
    monkeypatch.setattr(browser_mod, "get_runtime_metrics",
                        lambda: (_ for _ in ()).throw(
                            AssertionError("runtime metrics consulted on "
                                           "expvar path")))
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)

    assert browser_mod.wait_for_l1_warmup(timeout=30) is True


def test_wait_for_l1_warmup_expvar_present_not_done_keeps_polling_expvar(
        monkeypatch):
    """Expvar present but done=0 for a few polls, then done=1 → returns True
    via expvar, still WITHOUT any slog-scan (key present = authoritative)."""
    import bench.browser as browser_mod
    import bench.cluster as cluster_mod

    seq = [False, False, True]  # done=0, done=0, done=1
    calls = {"n": 0}

    def _expvar(*a, **k):
        i = min(calls["n"], len(seq) - 1)
        calls["n"] += 1
        return seq[i]
    monkeypatch.setattr(browser_mod, "prewarm_complete_via_expvar", _expvar)
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("slog-scan ran while expvar present")))
    monkeypatch.setattr(browser_mod, "get_runtime_metrics", lambda: None)
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)

    assert browser_mod.wait_for_l1_warmup(timeout=30) is True
    assert calls["n"] >= 3, "expvar should have been polled until done=1"


def test_wait_for_l1_warmup_falls_back_to_slog_scan_when_key_absent(monkeypatch):
    """Mandatory fallback: when the expvar key is ABSENT (probe returns None —
    images through 0.30.264), wait_for_l1_warmup uses the legacy slog-scan and
    detects the boundary from the pod-log 'warmup: completed' sentinel."""
    import bench.browser as browser_mod
    import bench.cluster as cluster_mod

    # Expvar absent on this (old) deployed image.
    monkeypatch.setattr(browser_mod, "prewarm_complete_via_expvar",
                        lambda *a, **k: None)
    # /metrics/runtime shows no cache keys yet → forces the slog-scan branch.
    monkeypatch.setattr(browser_mod, "get_runtime_metrics",
                        lambda: {"cache_key_count": 0})

    kubectl_calls = {"n": 0}

    def _fake_kubectl(*a, **k):
        kubectl_calls["n"] += 1
        return (0, "... warmup: completed ...", "")
    monkeypatch.setattr(cluster_mod, "kubectl", _fake_kubectl)
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)

    assert browser_mod.wait_for_l1_warmup(timeout=30) is True
    assert kubectl_calls["n"] >= 1, (
        "fallback slog-scan must shell kubectl when the expvar key is absent")


def test_wait_for_l1_warmup_fallback_detects_via_runtime_cache_keys(monkeypatch):
    """Fallback path, runtime signal: expvar absent + /metrics/runtime reports
    cache_key_count>0 → boundary detected via runtime WITHOUT needing the
    pod-log scan (the legacy primary fallback signal, preserved)."""
    import bench.browser as browser_mod
    import bench.cluster as cluster_mod

    monkeypatch.setattr(browser_mod, "prewarm_complete_via_expvar",
                        lambda *a, **k: None)
    monkeypatch.setattr(browser_mod, "get_runtime_metrics",
                        lambda: {"cache_key_count": 4231})
    monkeypatch.setattr(cluster_mod, "kubectl",
                        lambda *a, **k: (_ for _ in ()).throw(
                            AssertionError("kubectl scan ran though runtime "
                                           "cache_key_count>0 already detected")))
    monkeypatch.setattr(browser_mod.time, "sleep", lambda s: None)

    assert browser_mod.wait_for_l1_warmup(timeout=30) is True


def test_count_user_compositions_in_ns_returns_minus1_when_no_token():
    """Missing token → -1 sentinel (re-gate v3: ns argument no longer
    in the URL, so empty ns is no longer rejected — caller may pass
    "" as a label-only field).
    """
    import bench.browser as browser_mod
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", None, "bench-ns-01") == -1


def test_count_user_compositions_in_ns_uses_piechart_widget_url(monkeypatch):
    """Re-gate v3 (2026-06-05): Probe B calls the piechart-widget
    endpoint `resource=piecharts&name=dashboard-compositions-panel-
    row-piechart` (NOT the raw composition GVR list which lacks
    `name=` and returns 400). The shape mirrors
    verify_composition_count_api so both probes share the same
    /call path the dashboard piechart already uses.

    Asserts:
      1. URL includes `name=dashboard-compositions-panel-row-piechart`.
      2. URL includes `resource=piecharts`.
      3. URL does NOT include the raw composition GVR (no
         resource=githubscaffoldingwithcompositionpages).
      4. Returns int(title) parsed from
         body.status.widgetData.title (piechart contract).
    """
    import bench.browser as browser_mod

    captured = {}

    def _fake_http_get_json(path, token, **kw):
        captured["path"] = path
        captured["token"] = token
        return (0, 200, {
            "status": {"widgetData": {"title": "999"}},
        })

    monkeypatch.setattr(browser_mod, "http_get_json", _fake_http_get_json)
    out = browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01")
    assert out == 999
    assert captured["token"] == "tok"
    # Falsifier #1: the piechart widget name MUST be in the URL.
    assert "name=dashboard-compositions-panel-row-piechart" \
        in captured["path"], \
        f"Probe B URL must hit the piechart RESTAction by name; " \
        f"got: {captured['path']!r}"
    # Falsifier #2: resource is piecharts (widget endpoint), NOT the
    # raw composition GVR which would 400.
    assert "resource=piecharts" in captured["path"]
    assert "githubscaffoldingwithcompositionpages" not in captured["path"]


def test_count_user_compositions_in_ns_returns_minus1_on_non200(monkeypatch):
    """Non-200 response → -1 sentinel (poll continues)."""
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token, **kw: (0, 401, {}))
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01") == -1


def test_count_user_compositions_in_ns_returns_minus1_on_missing_title(
        monkeypatch):
    """200 but body lacks status.widgetData.title → -1 (caller falls
    back / poll continues). Guards against snowplow shape changes.
    """
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "http_get_json",
                        lambda path, token, **kw: (0, 200, {"status": {}}))
    assert browser_mod.count_user_compositions_in_ns(
        "cyberjoker", "tok", "bench-ns-01") == -1


def test_user_visible_composition_count_returns_none_for_non_compositions(
        monkeypatch):
    """The N-aware formula only applies to /compositions; other paths
    return None so the lookup falls back to BASE.
    """
    import bench.browser as browser_mod
    out = browser_mod._user_visible_composition_count(
        "admin", "/dashboard", token=None)
    assert out is None


def test_user_visible_composition_count_admin_uses_panel_count(monkeypatch):
    """admin path goes through cluster.count_compositions_with_panels_ready
    (Task #250 Block 2b re-gate fix 2026-06-05). The empirical source
    for n_visible on /compositions is the COMP-PANEL count, not the
    raw composition CR count — comp-panels are what the /compositions
    datagrid iterates over (TRACED via the compositions-panels
    RESTAction at krateo-system). Hermetic isolation: monkeypatch the
    cluster.* helper so this test NEVER hits a real apiserver.
    """
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod
    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: 4423)
    out = browser_mod._user_visible_composition_count(
        "admin", "/compositions", token=None)
    assert out == 4423


def test_user_visible_composition_count_admin_returns_none_on_transport_fail(
        monkeypatch):
    """Transport failure inside count_compositions_with_panels_ready →
    None at this layer → expected_calls falls back to BASE."""
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod

    def _raise(target_ns=None):
        raise RuntimeError("apiserver unreachable")

    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        _raise)
    out = browser_mod._user_visible_composition_count(
        "admin", "/compositions", token=None)
    assert out is None


def test_user_visible_composition_count_non_admin_with_target_ns(monkeypatch):
    """Non-admin path WITH target_ns goes through
    count_compositions_with_panels_ready(target_ns=...) — admin and
    narrow-RBAC users use the SAME mechanism for /compositions.
    """
    import bench.browser as browser_mod
    from bench import cluster as cluster_mod

    captured = {}

    def _fake_count(target_ns=None):
        captured["target_ns"] = target_ns
        return 143  # bench-ns-01 comp-panel count from 2026-06-05 probe

    monkeypatch.setattr(cluster_mod, "count_compositions_with_panels_ready",
                        _fake_count)
    out = browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok",
        target_ns="bench-ns-01")
    assert out == 143
    assert captured["target_ns"] == "bench-ns-01"


def test_user_visible_composition_count_non_admin_falls_back_to_piechart(
        monkeypatch):
    """Non-admin WITHOUT target_ns falls back to
    verify_composition_count_api (pre-Block-2b shape; still correct
    for cj at S1-S7 where no grant means piechart=0 → BASE).
    """
    import bench.browser as browser_mod
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 999)
    out = browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok")
    assert out == 999

    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: -1)
    assert browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token="tok") is None


def test_user_visible_composition_count_non_admin_without_token(monkeypatch):
    """Non-admin without token AND without target_ns → None (legacy
    BASE behaviour preserved)."""
    import bench.browser as browser_mod
    assert browser_mod._user_visible_composition_count(
        "cyberjoker", "/compositions", token=None) is None


# ─── Task #267 FIX 1 — filmed nav scrolls below the fold ────────────────────
#
# browser_measure_navigation must run a below-the-fold scroll pass at the END
# of the filmed nav (after the /call waterfall + nav-timing reads, before
# return) so the per-cell .webm captures the north-star views. These cases use
# the FakePage's evaluate_log (every page.evaluate JS string is recorded) to
# prove the scroll pass ran AND that it ran in the correct order so it cannot
# perturb the /call waterfall timing capture.


def _scroll_js_indices(evaluate_log):
    """Indices in evaluate_log whose JS performs a page scroll."""
    return [i for i, js in enumerate(evaluate_log)
            if ("scrollTo" in js or "scrollIntoView" in js)]


def _stub_nav_validation(monkeypatch):
    """Neutralise widget validation so browser_measure_navigation runs to
    its scroll pass without a real terminal-state check."""
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")
    monkeypatch.setattr(
        browser_mod, "_validate_widget_terminal_state",
        lambda page, path, label, **kw: {"terminal_state": "pass"})


def test_filmed_nav_scrolls_dashboard_below_the_fold(monkeypatch, fake_page):
    """FIX 1: the dashboard filmed nav invokes the scroll-capture pass —
    evaluate_log contains scroll JS that did NOT exist before the fix.

    Falsifier: with the scroll pass removed, _scroll_js_indices is empty.
    """
    _stub_nav_validation(monkeypatch)
    fake_page._call_count = 16

    r = browser_measure_navigation(fake_page, "/dashboard", "S6 ON nav#1",
                                   min_calls=0, user="admin")
    assert r["incomplete"] is False
    scroll_idx = _scroll_js_indices(fake_page.evaluate_log)
    assert scroll_idx, (
        "filmed dashboard nav did NOT scroll — evaluate_log has no scrollTo/"
        f"scrollIntoView JS; log={fake_page.evaluate_log!r}")


def test_filmed_nav_scrolls_compositions_datagrid(monkeypatch, fake_page):
    """FIX 1: the /compositions filmed nav does a stepped datagrid scroll
    (>=2 scroll steps so multiple panel cards render on camera)."""
    _stub_nav_validation(monkeypatch)
    fake_page._call_count = 30

    browser_measure_navigation(fake_page, "/compositions", "S6 ON nav#1",
                               min_calls=0, user="admin")
    scroll_idx = _scroll_js_indices(fake_page.evaluate_log)
    assert len(scroll_idx) >= 2, (
        "filmed /compositions nav must step-scroll the datagrid (>=2 steps); "
        f"got {len(scroll_idx)} scroll JS calls in {fake_page.evaluate_log!r}")


def test_filmed_nav_scroll_runs_after_waterfall_and_nav_timing_reads(
        monkeypatch, fake_page):
    """FIX 1 ordering contract: the scroll pass runs AFTER the /call
    waterfall read (the `GAP_MS` levels JS) AND after the navigation-timing
    read (`loadEventEnd`), so it cannot perturb the captured /call timings.

    Falsifier: if the scroll were placed before either read, the first
    scroll index would precede that read's index.
    """
    _stub_nav_validation(monkeypatch)
    fake_page._call_count = 16

    browser_measure_navigation(fake_page, "/dashboard", "S6 ON nav#1",
                               min_calls=0, user="admin")
    log = fake_page.evaluate_log
    waterfall_idx = next(i for i, js in enumerate(log)
                         if "GAP_MS" in js and "calls.length" in js)
    navtiming_idx = next(i for i, js in enumerate(log)
                         if "navigation" in js and "loadEventEnd" in js)
    first_scroll_idx = _scroll_js_indices(log)[0]
    assert first_scroll_idx > waterfall_idx, (
        f"scroll (idx {first_scroll_idx}) ran BEFORE the /call waterfall read "
        f"(idx {waterfall_idx}) — would perturb the timing capture")
    assert first_scroll_idx > navtiming_idx, (
        f"scroll (idx {first_scroll_idx}) ran BEFORE the navigation-timing "
        f"read (idx {navtiming_idx})")


def test_scroll_capture_gated_off_by_module_flag(monkeypatch, fake_page):
    """`SCROLL_CAPTURE_FOR_VIDEO=False` → the helper is a no-op (returns 0,
    records no evaluate calls). Confirms the gate flag works."""
    monkeypatch.setattr(browser_mod, "SCROLL_CAPTURE_FOR_VIDEO", False)
    n = _scroll_capture_for_video(fake_page, "/dashboard")
    assert n == 0
    assert _scroll_js_indices(fake_page.evaluate_log) == []


@pytest.mark.parametrize("page_path", ["/dashboard", "/compositions", "/other"])
def test_scroll_capture_never_raises_when_evaluate_throws(fake_page, page_path):
    """The scroll pass is best-effort: a page.evaluate that raises must be
    swallowed on EVERY page path (the filmed nav must never fail because of
    the scroll). Covers the dashboard widget-locate evaluate too — that call
    must be wrapped, not direct."""
    def _boom(js, *args):
        fake_page.evaluate_log.append(js)
        raise RuntimeError("page closed mid-scroll")
    fake_page.evaluate = _boom
    # Must not raise on any page path.
    n = _scroll_capture_for_video(fake_page, page_path)
    assert isinstance(n, int)


# ─── #310b: slot-map helpers for browser_measure_stage's page_slots param ───
#
# #310b collapsed the prior parallel `pages_by_name` + `page_factories` dicts
# into ONE slot map {page_name: {"page": <live|None>, "make": <factory|None>}}.
# These builders construct that shape so the tests below exercise the SAME
# routing + lazy-materialise intents through the collapsed structure.


def _live_slot(page):
    """A slot with a live page (the routing sentinel resolves directly to it —
    no factory). Mirrors the legacy `pages_by_name[name] = <live page>`."""
    return {"page": page, "make": None}


def _lazy_slot(make):
    """A LAZY slot: page=None (routing sentinel → route into the factory) +
    a zero-arg `make` factory. Mirrors the legacy `pages_by_name[name] = None`
    paired with `page_factories[name] = make`."""
    return {"page": None, "make": make}


def test_both_pages_scrolled_per_stage_via_page_slots(monkeypatch, tmp_path):
    """FIX 1 + FIX 2 under the per-page (representative AND all) recording
    path: browser_measure_stage with a `page_slots` map measures each page on
    its OWN page object, and the filmed-nav scroll fires for BOTH — the
    dashboard page gets the dashboard scroll idiom, the compositions page
    gets the stepped datagrid scroll. This is the per-stage guarantee the
    `--video all` clean re-run depends on (every stage's navs, both pages,
    scrolled), since the scroll is mode-independent and page_slots is set
    for both representative and all.

    #310b: each slot carries the live page directly (slot["page"]). Falsifier:
    if browser_measure_stage ignored page_slots (measured both on the single
    `page`), the compositions FakePage's evaluate_log would be empty — no
    scroll, no nav.
    """
    from tests.conftest import FakePage

    _patch_cluster_count(monkeypatch, comp_count=42, ns_count=2)
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: 42)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        lambda page: 42)
    monkeypatch.setattr(browser_mod, "_validate_widget_terminal_state",
                        lambda page, path, label, **kw: {"terminal_state": "pass"})

    dash_page = FakePage(call_count=16, ui_count=42)
    comp_page = FakePage(call_count=30, ui_count=42)
    page_slots = {"Dashboard": _live_slot(dash_page),
                  "Compositions": _live_slot(comp_page)}

    browser_measure_stage(
        dash_page, stage_num=6, stage_desc="S6 all-mode",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
        page_slots=page_slots)

    # Dashboard page filmed-nav scrolled (its own object, not comp_page).
    dash_scrolls = _scroll_js_indices(dash_page.evaluate_log)
    assert dash_scrolls, (
        "dashboard page was not scrolled during the per-page stage measure; "
        f"log={dash_page.evaluate_log!r}")
    # Compositions page measured on its OWN object AND step-scrolled.
    comp_scrolls = _scroll_js_indices(comp_page.evaluate_log)
    assert len(comp_scrolls) >= 2, (
        "compositions page (its own recording context) was not step-scrolled "
        f"— page_slots not honoured?; log={comp_page.evaluate_log!r}")
    # The compositions page issued a /call waterfall read of its OWN (proves it
    # was actually navigated/measured, not skipped).
    assert any("GAP_MS" in js for js in comp_page.evaluate_log), (
        "compositions page was never measured on its own context")


def test_scroll_capture_excludes_chrome_nav_idiom(fake_page):
    """The dashboard scroll JS must apply the shared chrome-exclusion idiom
    (an `inChrome`/`closest(chromeExclude)` guard) so it never scrolls to a
    left-nav menu item that shares the 'Compositions' label text. The actual
    selector string is passed as the JS arg `_VIDEO_SCROLL_CHROME_EXCLUDE`,
    which carries the proven nav/sidebar/menu/sider exclusion."""
    _scroll_capture_for_video(fake_page, "/dashboard")
    joined = "\n".join(fake_page.evaluate_log)
    assert "inChrome" in joined and "closest(chromeExclude)" in joined, (
        "dashboard scroll JS missing the chrome-exclusion guard; "
        f"log={fake_page.evaluate_log!r}")
    # And the selector arg itself carries the proven exclusion classes.
    sel = browser_mod._VIDEO_SCROLL_CHROME_EXCLUDE
    assert all(k in sel for k in ("nav", "sidebar", "menu", "sider")), (
        f"_VIDEO_SCROLL_CHROME_EXCLUDE missing exclusion classes: {sel!r}")


# ─── Task #307 / A1-full falsifiers (F-A, F-B) + Defect 2 anchor (F-C) ──────


def _stub_stage_verify(monkeypatch, count=42, ui=None):
    """Make browser_measure_stage's VERIFY poll match on the first iteration
    and neutralise widget validation, so the stage runs cleanly through both
    page iterations (Dashboard then Compositions). `ui` overrides the
    verify_composition_count_ui stub (e.g. a recorder noting which page object
    the VERIFY read ran on)."""
    _patch_cluster_count(monkeypatch, comp_count=count, ns_count=2)
    monkeypatch.setattr(browser_mod, "FRONTEND", "http://fake")
    monkeypatch.setattr(browser_mod, "verify_composition_count_api",
                        lambda token: count)
    monkeypatch.setattr(browser_mod, "verify_composition_count_ui",
                        ui if ui is not None else (lambda page: count))
    monkeypatch.setattr(browser_mod, "_validate_widget_terminal_state",
                        lambda page, path, label, **kw: {"terminal_state": "pass"})


def test_A1_compositions_page_goto_log_has_no_login_no_dashboard(
        monkeypatch, tmp_path):
    """Falsifier F-A (Task #307): with storage-state reuse + lazy creation,
    the Compositions recording page's `goto_log` ends on `/compositions` and
    contains NO `/login` and NO `/dashboard` entry — i.e. the login/dashboard
    head is gone from the page that films the /compositions nav.

    Pre-fix, that page was driven through `browser_login` (goto `/login` →
    SPA redirect to dashboard) BEFORE its /compositions goto, so its goto_log
    led with `/login`. Post-fix the recording context starts authenticated
    (storage_state) and is created lazily, so its FIRST and only goto is
    `/compositions`. This is the discriminating assertion (a plain FakePage
    has no video timeline, so the duration property can't be unit-tested —
    see the §6.2 artifact spot-check; this locks the structural invariant).
    """
    from tests.conftest import FakePage

    _stub_stage_verify(monkeypatch)

    dash_page = FakePage(call_count=16, ui_count=42)
    comp_page = FakePage(call_count=30, ui_count=42)

    # Lazy wiring (#310b slot map): Dashboard has a live page; Compositions is a
    # LAZY slot — page=None + a factory returning comp_page ONLY when invoked
    # (mirrors _open_stage_recording_pages' deferred slot).
    page_slots = {"Dashboard": _live_slot(dash_page),
                  "Compositions": _lazy_slot(lambda: comp_page)}

    browser_measure_stage(
        dash_page, stage_num=6, stage_desc="S6 A1",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
        page_slots=page_slots)

    comp_gotos = [u for (u, _kw) in comp_page.goto_log]
    assert comp_gotos, "Compositions page was never navigated (factory not run?)"
    assert comp_page.url.endswith("/compositions"), (
        f"Compositions page did not end on /compositions: {comp_page.url!r}")
    assert comp_gotos[-1].endswith("/compositions"), (
        f"Compositions page's last goto was not /compositions: {comp_gotos!r}")
    assert not any("/login" in u for u in comp_gotos), (
        f"Compositions recording page filmed a /login goto: {comp_gotos!r}")
    assert not any(u.rstrip("/").endswith("/dashboard") for u in comp_gotos), (
        f"Compositions recording page filmed a /dashboard goto: {comp_gotos!r}")


def test_A1_compositions_context_created_after_dashboard_verify(
        monkeypatch, tmp_path):
    """Falsifier F-B (Task #307, A1-full): the Compositions recording context's
    FIRST goto occurs AFTER the Dashboard VERIFY/convergence block completes.

    We stamp a shared monotonic counter on every goto across both pages. The
    Dashboard page's final goto (its end-of-VERIFY `/dashboard` reload) must be
    stamped BEFORE the Compositions page's first goto. Equivalently, the factory
    that creates the Compositions context must not have been invoked until the
    Compositions loop iteration (after VERIFY). Pre-fix the Compositions context
    existed from login time — its video clock ran throughout the VERIFY poll.
    """
    from tests.conftest import FakePage

    _stub_stage_verify(monkeypatch)

    order: list[tuple[str, str]] = []  # (page_tag, url) in invocation order

    class StampedPage(FakePage):
        def __init__(self, tag, **kw):
            super().__init__(**kw)
            self._tag = tag

        def goto(self, url, **kwargs):
            order.append((self._tag, url))
            super().goto(url, **kwargs)

    dash_page = StampedPage("dash", call_count=16, ui_count=42)
    comp_page = StampedPage("comp", call_count=30, ui_count=42)

    factory_calls = {"n": 0}

    def _make_comp():
        factory_calls["n"] += 1
        return comp_page

    browser_measure_stage(
        dash_page, stage_num=6, stage_desc="S6 A1 order",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
        page_slots={"Dashboard": _live_slot(dash_page),
                    "Compositions": _lazy_slot(_make_comp)})

    # The factory ran exactly once (lazy, not eager).
    assert factory_calls["n"] == 1, (
        f"Compositions factory invoked {factory_calls['n']}x (expected 1, lazy)")
    # Every dashboard goto precedes every compositions goto in invocation order.
    last_dash_idx = max(i for i, (tag, _u) in enumerate(order) if tag == "dash")
    first_comp_idx = min(i for i, (tag, _u) in enumerate(order) if tag == "comp")
    assert last_dash_idx < first_comp_idx, (
        "Compositions context's first goto did NOT occur strictly after the "
        f"Dashboard VERIFY block; invocation order={order!r}")


def test_browser_measure_stage_materializes_lazy_dashboard_and_verifies_on_it(
        monkeypatch, tmp_path):
    """B1 invariant lock (Task #307 / ArchLazyDash; #310b slot-map form): when
    the DASHBOARD arrives LAZY (its slot has page=None + a callable make),
    browser_measure_stage materialises it as the FIRST statement of the
    Dashboard iteration and the VERIFY read (`verify_composition_count_ui`) runs
    on the lazily-materialised dashboard page — NOT on the shared/fallback page.

    NOTE — invariant lock, NOT a prod-revert discriminator: browser.py's
    materialise block (B1) already generalises to ANY lazy page; the ONLY prod
    change in browser.py for this ship is the comment generalisation. The
    behavioural change that makes Dashboard lazy lives in
    phases._open_stage_recording_pages (P1), discriminated by the test_phases.py
    F-B′ end-to-end test + F-open + F-measure. This test guards that the
    consumer contract browser.py relies on — materialise-before-VERIFY for a
    lazy Dashboard — stays true after the change, so it must remain GREEN both
    pre- and post-fix when a lazy Dashboard slot is supplied directly.

    Asserts: (a) the Dashboard factory is invoked exactly once, at/after the
    Dashboard iteration start (its first goto is `/dashboard`); (b)
    verify_composition_count_ui was called with the factory's page object, not
    the shared/fallback page.
    """
    from tests.conftest import FakePage

    # Record which page object each verify_composition_count_ui call inspected.
    ui_called_on: list = []
    _stub_stage_verify(
        monkeypatch, ui=lambda page: (ui_called_on.append(page), 42)[1])

    # Distinct live pages: a shared/fallback page (must NOT drive VERIFY) and the
    # lazily-materialised dashboard + compositions pages.
    shared_page = FakePage(call_count=16, ui_count=42)
    dash_page = FakePage(call_count=16, ui_count=42)
    comp_page = FakePage(call_count=30, ui_count=42)

    dash_factory_calls = {"n": 0}

    def _make_dash():
        dash_factory_calls["n"] += 1
        return dash_page

    # ALL slots lazy (#310b): each slot has page=None; factories materialise.
    browser_measure_stage(
        shared_page, stage_num=6, stage_desc="S6 F-B'",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
        page_slots={"Dashboard": _lazy_slot(_make_dash),
                    "Compositions": _lazy_slot(lambda: comp_page)})

    # (a) The Dashboard factory ran exactly once (lazy, not eager).
    assert dash_factory_calls["n"] == 1, (
        f"Dashboard factory invoked {dash_factory_calls['n']}x (expected 1, "
        f"lazy — pre-fix the Dashboard slot was eager with no factory)")
    # The materialised Dashboard page's first/only nav is /dashboard (the
    # factory was NOT invoked before the Dashboard iteration started).
    dash_gotos = [u for (u, _kw) in dash_page.goto_log]
    assert dash_gotos and dash_gotos[0].endswith("/dashboard"), (
        f"materialised Dashboard page's first goto must be /dashboard; "
        f"got {dash_gotos!r}")
    # (b) The VERIFY read ran on the lazily-materialised dashboard page object,
    # NOT on the shared/fallback page and NOT on the compositions page.
    assert ui_called_on, "verify_composition_count_ui was never called"
    assert all(p is dash_page for p in ui_called_on), (
        "VERIFY poll's verify_composition_count_ui did NOT run on the lazily-"
        f"materialised dashboard page; ran on {ui_called_on!r}")
    assert shared_page not in ui_called_on, (
        "VERIFY ran on the shared/fallback page — the materialise block did not "
        "replace nav_page before the VERIFY poll")


def test_lazy_factory_failure_falls_back_to_shared_page(monkeypatch, tmp_path):
    """Safety invariant (ArchLazyDash): if a lazy page factory raises, the stage
    falls back to the persistent non-recording `page` and still measures +
    runs VERIFY on it (parity with today's no-video path) — it does NOT abort.

    Now that the Dashboard is lazy too, a Dashboard-factory failure must degrade
    gracefully: the VERIFY poll runs on the shared fallback page. This replaces
    the open-time orphan-leak guard (there is no eager new_page() at open time
    anymore; the only failure surface is the lazy factory, handled by the B1
    fallback `nav_page = page` in browser_measure_stage).
    """
    from tests.conftest import FakePage

    ui_called_on: list = []
    _stub_stage_verify(
        monkeypatch, ui=lambda page: (ui_called_on.append(page), 42)[1])

    shared_page = FakePage(call_count=16, ui_count=42)

    def _boom():
        raise RuntimeError("lazy dashboard context create failed")

    # Both slot factories raise; nav_page must fall back to shared_page each
    # time (#310b: the make lives in the slot).
    result = browser_measure_stage(
        shared_page, stage_num=6, stage_desc="S6 fallback",
        cache_mode="ON", token="tok", num_navs=1, user="admin",
        verify_against_cluster=True, verify_timeout=5, verify_interval=0,
        screenshots_dir=tmp_path / "ss",
        page_slots={"Dashboard": _lazy_slot(_boom),
                    "Compositions": _lazy_slot(_boom)})

    # Stage still produced results (did not abort on the factory failure).
    assert result and "pages" in result, result
    # The Dashboard nav + VERIFY ran on the shared fallback page.
    assert any(u.endswith("/dashboard") for (u, _kw) in shared_page.goto_log), (
        f"fallback shared page never navigated /dashboard; "
        f"goto_log={shared_page.goto_log!r}")
    assert ui_called_on and all(p is shared_page for p in ui_called_on), (
        f"VERIFY must run on the shared fallback page when the factory fails; "
        f"ran on {ui_called_on!r}")


def test_D2_dashboard_final_hold_targets_compositions_not_first_chart(
        monkeypatch, fake_page):
    """Defect 2 anchor (Task #307): the dashboard scroll's FINAL hold must
    anchor on the COMPOSITIONS section / page bottom — NOT re-centre the
    first chart on the page (which is the Blueprints donut, leaving the
    Compositions donut ~150px below frame).

    Falsifier (structural): the dashboard choreography's LAST evaluate must BE
    the module constant `_DASHBOARD_FINAL_HOLD_JS` (the Compositions-anchored
    hold), and that constant must carry the 'Compositions' label anchor + the
    scrollHeight bottom fallback and must NOT reset to top:0 (the pre-fix
    miss-fallback that framed the Blueprints donut). Asserting on the constant
    — not a whole-log token grep — keeps the test discriminating even if other
    scroll steps legitimately use these tokens.
    """
    _scroll_capture_for_video(fake_page, "/dashboard")
    assert fake_page.evaluate_log, "no scroll JS ran on /dashboard"
    final = browser_mod._DASHBOARD_FINAL_HOLD_JS
    assert fake_page.evaluate_log[-1] == final, (
        "the dashboard scroll's final hold is not the Compositions-anchored "
        f"constant; last evaluate was: {fake_page.evaluate_log[-1][:120]!r}")
    # The constant itself carries the anchor strategy markers.
    assert "'Compositions'" in final, (
        "_DASHBOARD_FINAL_HOLD_JS has no 'Compositions' label anchor — it "
        "would centre the first (Blueprints) chart")
    assert "scrollHeight" in final, (
        "_DASHBOARD_FINAL_HOLD_JS lacks a scrollHeight (page-bottom) fallback")
    assert "top: 0" not in final and "top:0" not in final, (
        "_DASHBOARD_FINAL_HOLD_JS still resets to top:0 (the pre-fix "
        "miss-fallback that framed the Blueprints donut)")


# ─── O-D3 (snowplow 1.12.3) — /debug/* bearer plumbing ──────────────────────
#
# snowplow 1.12.3 put the whole /debug/* surface behind a JWT. The harness has
# to present one, but it must ALSO stay usable against a pre-1.12.3 pod, which
# serves /debug/vars anonymously. So the contract on _debug_auth_headers is:
# add Authorization when a token is parked, and otherwise be a no-op — never an
# empty-bearer header, never a raise, never a hidden login on the probe path.


def test_debug_auth_headers_is_a_noop_without_a_token(monkeypatch):
    """No parked token → headers pass through unchanged, no Authorization.

    This is the pre-1.12.3-pod arm: a bench run against an older snowplow must
    still issue the SAME anonymous GET it always did. An empty or malformed
    Authorization header would be worse than none — a 1.12.3 pod would 401 it
    and an older pod might reject it outright.
    """
    import bench.browser as browser_mod

    monkeypatch.setattr(browser_mod, "DEBUG_TOKEN", None)

    assert browser_mod._debug_auth_headers() == {}
    assert browser_mod._debug_auth_headers({"Accept": "application/json"}) == {
        "Accept": "application/json"}


def test_debug_auth_headers_adds_bearer_without_mutating_the_caller(
        monkeypatch):
    """Parked token → Authorization added, and `extra` is left alone.

    The readers pass a fresh literal each call today, but mutating the caller's
    dict would leak the bearer into unrelated request headers the moment one of
    them hoists that literal.
    """
    import bench.browser as browser_mod

    monkeypatch.setattr(browser_mod, "DEBUG_TOKEN", "tok-abc")

    extra = {"Accept": "application/json"}
    out = browser_mod._debug_auth_headers(extra)

    assert out["Authorization"] == "Bearer tok-abc"
    assert out["Accept"] == "application/json"
    assert extra == {"Accept": "application/json"}, (
        "_debug_auth_headers mutated the caller's dict")


def test_set_debug_token_ignores_empty_values(monkeypatch):
    """set_debug_token(None) / ("") must NOT clobber a good token.

    login_all() forwards whatever it got; a failed login round that yields no
    token must not disarm the bearer a previous round parked.
    """
    import bench.browser as browser_mod

    monkeypatch.setattr(browser_mod, "DEBUG_TOKEN", "tok-good")
    browser_mod.set_debug_token(None)
    browser_mod.set_debug_token("")
    assert browser_mod.DEBUG_TOKEN == "tok-good"

    browser_mod.set_debug_token("tok-new")
    assert browser_mod.DEBUG_TOKEN == "tok-new"


def test_expvar_reader_sends_the_parked_bearer(monkeypatch):
    """End of the plumbing: the parked token reaches the actual GET.

    Drives read_snowplow_expvar_int with urlopen faked and asserts the
    Authorization header on the Request object — not merely that the helper
    returns one. Fails if a future reader builds its own headers and bypasses
    _debug_auth_headers.
    """
    import bench.browser as browser_mod

    captured = {}

    class _FakeResp:
        status = 200
        headers = {}

        def __enter__(self):
            return self

        def __exit__(self, *a):
            return False

        def read(self):
            return b'{"snowplow_rbac_publish_seq": 7}'

    def _fake_urlopen(req, timeout=None):
        captured["headers"] = dict(req.headers)
        return _FakeResp()

    monkeypatch.setattr(browser_mod, "DEBUG_TOKEN", "tok-xyz")
    monkeypatch.setattr(browser_mod.urllib.request, "urlopen", _fake_urlopen)

    assert browser_mod.read_snowplow_expvar_int(
        "snowplow_rbac_publish_seq", base_url="http://fake:8081") == 7
    # urllib title-cases header names on the Request object.
    assert captured["headers"].get("Authorization") == "Bearer tok-xyz"
