"""Playwright driver — login, navigation, validation, convergence poll,
video recording.

The harness uses Playwright exclusively (no Chrome MCP); video → GIF
post-processing via ffmpeg. NEVER uses kubectl-mediated paths in scored
measurement (see feedback_no_kubectl_in_measurement.md).

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.5 (Block 3).

Surface
-------
HTTP helpers (no Playwright dependency):
    login, login_all, http_get, http_get_json, http_get_with_headers,
    cache_metrics

Playwright-based helpers (require playwright.sync_api):
    browser_login (alias: _browser_login)
    browser_measure_navigation (alias: _browser_measure_navigation)
    browser_measure_stage (alias: _browser_measure_stage)
    verify_composition_count_api / verify_composition_count_ui
    make_browser_context, record_video_to_gif

Convergence:
    ConvergenceTimeout  (raised by browser_measure_stage on VERIFY poll
                         deadline — replaces the source-script's silent
                         TIMEOUT/MISMATCH log at worktree line 6465-6475)

Constants:
    WIDGET_ENDPOINTS, RESTACTION_ENDPOINTS, ALL_ENDPOINTS, BROWSER_PAGES,
    BROWSER_NAV_PAGES, BROWSER_SCALING_PAGES

Dependencies: playwright.sync_api (optional — only loaded when caller hits
a Playwright-based helper), urllib, gzip, json, base64, subprocess,
bench.cluster (count_compositions, list_composition_names + kubectl for
user-secret reads), bench.expected (_expected_calls + EXPECTED_CALLS_TOLERANCE
via deferred import to avoid circular import during partial migration).
Per plan §G.0: deferred-import shims absorb ~60 LOC.
"""

from __future__ import annotations

import base64
import gzip
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


__all__ = [
    # HTTP helpers
    "login",
    "login_all",
    "http_get",
    "http_get_json",
    "http_get_with_headers",
    "cache_metrics",
    "get_runtime_metrics",
    # Constants
    "WIDGET_ENDPOINTS",
    "RESTACTION_ENDPOINTS",
    "ALL_ENDPOINTS",
    "BROWSER_PAGES",
    "BROWSER_NAV_PAGES",
    "BROWSER_SCALING_PAGES",
    # User credentials
    "USERS",
    # Convergence-timeout
    "ConvergenceTimeout",
    # Playwright-based helpers
    "browser_login",
    "browser_measure_navigation",
    "browser_measure_stage",
    "verify_composition_count_api",
    "verify_composition_count_ui",
    "make_browser_context",
    "record_video_to_gif",
    # L1 / compositions waiters (browser-context callers)
    "wait_for_compositions",
    "wait_for_l1_ready",
    "wait_for_l1_warmup",
    # Task #221 (bench half) — prewarm-boundary expvar probe (prefer/fallback)
    "prewarm_complete_via_expvar",
    # Task #250 Block 2 — Phase 6 S8/S9 probes
    "read_snowplow_expvar_int",
    "count_user_compositions_in_ns",
    # Task #334a — convergence-cell discrimination tuple (report-only)
    "capture_conv_discrimination",
    # Task #217 — per-stage L1 hit/miss delta (report-only)
    "read_snowplow_expvar_map",
    "compute_l1_lookup_delta",
    # O-D3 (snowplow 1.12.3) — /debug/* is JWT-gated; park the bearer.
    "set_debug_token",
]


# ─── Environment / endpoints ────────────────────────────────────────────────

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
FRONTEND = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080") or None
SCREENSHOTS = os.environ.get("SCREENSHOTS", "0") == "1"

# Iteration counts — kept at module level so callers / tests can monkeypatch.
ITERS = int(os.environ.get("ITERS", "10"))


# ─── Deferred-import shims (per plan §G.0 cost #1) ──────────────────────────


def _log(msg):
    """Local log shim — replaced by cli.py logger when present."""
    try:
        from bench.cli import log
        log(msg)
    except ImportError:
        ts = time.strftime("%H:%M:%S")
        print(f"  [{ts}] {msg}", flush=True)


def _section(title):
    """Local section shim — replaced by cli.py logger when present."""
    try:
        from bench.cli import section
        section(title)
    except ImportError:
        print(f"\n  === {title} ===", flush=True)


def _count_compositions():
    """Defer to bench.cluster (sole cluster-state probe used by VERIFY poll)."""
    from bench.cluster import count_compositions  # type: ignore
    return count_compositions()


def _count_bench_ns():
    """Defer to bench.cluster (used by browser_measure_stage header log)."""
    from bench.cluster import count_bench_ns  # type: ignore
    return count_bench_ns()


def _list_composition_names():
    """Defer to bench.cluster (CONTENT-correctness check post-VERIFY)."""
    from bench.cluster import list_composition_names  # type: ignore
    return list_composition_names()


def _pct(data, p):
    """Defer to bench.cluster (percentile helper used in waterfall stats)."""
    from bench.cluster import pct  # type: ignore
    return pct(data, p)


def _expected_calls_lookup(user, page_path, *, n_visible=None):
    """Defer to bench.expected — lazy import to avoid pulling expected.py
    at module load (keeps the import surface lean for tests that only
    exercise the HTTP helpers).

    Block 2 (Task #250): forwards `n_visible` through the N-aware formula.
    `None` (the default) reproduces the legacy BASE-only behaviour, so
    callers that have not migrated keep the prior shape.
    """
    from bench.expected import expected_calls  # type: ignore
    return expected_calls(user, page_path, n_visible=n_visible)


def _user_visible_composition_count(user, page_path, token=None,
                                    target_ns=None):
    """Return the per-user count of compositions VISIBLE on `page_path`.

    CONTENT-correctness source (cluster-side panel materialization count).
    As of Task #284 (2026-06-10) this is NO LONGER the EXPECTED_CALLS gate's
    `n_visible` source — the gate now derives `n_visible` from the rendered
    DOM cards (`_count_rendered_comp_cards`) per the strategic rule "rendered
    DOM predicts call-counts; cluster LIST validates content." This helper
    is retained for content-correctness callers (the S8/S9 card-presence
    checks and ad-hoc /compositions storm navs) which legitimately want the
    cluster-truth panel count, not the render-state count.

    Empirical source (Task #250 Block 2b re-gate fix 2026-06-05):
    the right shape for `n_visible` is the count of
    `panels.widgets.templates.krateo.io` filtered by
    `krateo.io/portal-page=compositions` — these are the comp-panels
    that DRIVE the /compositions datagrid (TRACED via the
    `compositions-panels` RESTAction at krateo-system, which filters
    panels by that label and yields the datagrid's iterator). NOT the
    raw composition CRs:
      - At small N (S4 with 20 comps just deployed) only a handful of
        comp-panels have materialized (controller-dynamic race),
        explaining empirical actual=14 = 10+4×1.
      - At large N (S6 with 50K comps) > per_page=5 comp-panels exist
        → datagrid caps at 5 → actual=30 = 10+4×5.

    Per-user RBAC narrowing:
      - admin: target_ns=None → cluster-wide comp-panel count.
      - cyberjoker (and any future Group-scoped user): target_ns is
        the granted namespace → only comp-panels in that ns appear
        (UAF on the panels resource).

    Returns None for pages that are not the Compositions page or when
    no per-user count source applies; callers treat None as 'no
    narrowing — use BASE expected'.

    Args:
        user:      subject identity ("admin", "cyberjoker", ...).
        page_path: page URL path; only "/compositions" is currently
                   formula-relevant.
        token:     unused at this layer for the comp-panel mechanism
                   (the k8s client uses the bench's own kubeconfig
                   identity, not the per-subject JWT). Kept for the
                   piechart fallback path and future symmetry.
        target_ns: for narrow-RBAC users, the namespace the user is
                   granted access to. When None for admin → cluster-
                   wide; when set for non-admin → ns-scoped count.
    """
    if page_path != "/compositions":
        return None
    if user == "admin":
        # Cluster-wide comp-panel count. Returns None on transport
        # failure → falls back to BASE expected.
        try:
            from bench.cluster import (  # type: ignore
                count_compositions_with_panels_ready)
            return count_compositions_with_panels_ready(target_ns=None)
        except Exception:
            return None
    # Non-admin path:
    #   - If caller passed target_ns (Phase 6 S8/S9 context), count
    #     comp-panels in that ns directly — this is the post-Block-2b
    #     shape that aligns with the cluster-wide path for admin.
    #   - Otherwise fall back to the piechart-widget API (composition
    #     CR count) — pre-Block-2b shape; still useful for callers
    #     outside the S8/S9 stage runners (e.g. ad-hoc /compositions
    #     navs in storm scenarios).
    if target_ns:
        try:
            from bench.cluster import (  # type: ignore
                count_compositions_with_panels_ready)
            return count_compositions_with_panels_ready(
                target_ns=target_ns)
        except Exception:
            return None
    if not token:
        return None
    try:
        n = verify_composition_count_api(token)
    except Exception:
        return None
    if n is None or n < 0:
        return None
    return n


def read_snowplow_expvar_int(key, *, base_url=None, timeout=10):
    """Fetch a single integer value from snowplow's /debug/vars.

    Mechanism-independent probe used by the Phase 6 S8/S9 inner gate
    (Probe A — RBAC publish-seq delta). Mirrors the pattern of
    http_get_json (single GET, gzip-decompress).

    As of snowplow 1.12.3 (O-D3) /debug/vars is JWT-gated like every other
    /debug/* route, so the GET carries the bearer _debug_auth_headers
    resolves. An anonymous read of a 1.12.3 pod returns 401 → None.

    Args:
        key:      JSON top-level key in /debug/vars (e.g.
                  "snowplow_rbac_publish_seq").
        base_url: override SNOWPLOW; useful for tests.
        timeout:  HTTP timeout seconds.

    Returns:
        int when /debug/vars returns 200 + key is an int.
        None on any failure path (unreachable, malformed JSON, missing
        key, non-int value). Caller FAILS CLOSED on None — never assume
        a missing read is a propagation signal.
    """
    base = base_url if base_url is not None else SNOWPLOW
    url = f"{base.rstrip('/')}/debug/vars"
    try:
        req = urllib.request.Request(
            url,
            headers=_debug_auth_headers({"Accept": "application/json"}),
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if resp.status != 200:
                return None
            body = resp.read()
            body = _decompress(body, headers=dict(resp.headers))
            data = json.loads(body)
    except Exception:
        return None
    if not isinstance(data, dict):
        return None
    val = data.get(key)
    if isinstance(val, bool):
        # `True/False` would coerce to 1/0 via int(); reject explicitly so
        # a misshape on the snowplow side surfaces as None.
        return None
    if isinstance(val, int):
        return val
    if isinstance(val, float):
        # expvar serializes uint64 as a JSON number; some JSON libraries
        # may upcast to float for very large values. Round-trip safely.
        try:
            return int(val)
        except (ValueError, OverflowError):
            return None
    return None


def read_snowplow_expvar_map(key, *, base_url=None, timeout=10):
    """Fetch a single map-valued expvar from snowplow's /debug/vars.

    Task #217 — REPORT-ONLY per-stage L1 hit/miss delta. Used to snapshot
    `snowplow_dispatch_l1_lookups` (map[str] -> {hit_total, miss_total}, per
    dispatchers/l1_lookup_metrics.go) before/after a stage so the stage proof
    can carry the per-GVR L1 lookup delta.

    Reuses the SAME read path as read_snowplow_expvar_int (single GET,
    gzip-decompress, json.loads of /debug/vars — the established #334-A expvar
    machinery); the ONLY difference is the value type asserted (dict, not int).

    Args:
        key:      JSON top-level key in /debug/vars (e.g.
                  "snowplow_dispatch_l1_lookups").
        base_url: override SNOWPLOW; useful for tests.
        timeout:  HTTP timeout seconds.

    Returns:
        dict when /debug/vars returns 200 + key is a JSON object.
        None on any failure path (unreachable, malformed JSON, missing key,
        non-dict value). A cache-OFF pod publishes no such expvar, so the
        caller records the L1 field as None — NEVER crashes a stage.
    """
    base = base_url if base_url is not None else SNOWPLOW
    url = f"{base.rstrip('/')}/debug/vars"
    try:
        req = urllib.request.Request(
            url,
            headers=_debug_auth_headers({"Accept": "application/json"}),
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if resp.status != 200:
                return None
            body = resp.read()
            body = _decompress(body, headers=dict(resp.headers))
            data = json.loads(body)
    except Exception:
        return None
    if not isinstance(data, dict):
        return None
    val = data.get(key)
    if isinstance(val, dict):
        return val
    return None


def compute_l1_lookup_delta(before_map, after_map):
    """Diff two snowplow_dispatch_l1_lookups snapshots into a report-only
    per-GVR L1 hit/miss delta.

    Task #217 — REPORT-ONLY. Pure function (no I/O); fully None-safe so a
    cache-OFF / unreachable snapshot records the field as None rather than
    crashing the stage.

    The expvar value is map["<handlerKind>|<gvrString>"] ->
    {hit_total, miss_total} (dispatchers/l1_lookup_metrics.go). We diff
    per cell and aggregate, returning:

        {
          "per_gvr": {"<handlerKind>|<gvr>": {"hits": N, "misses": M}, ...},
          "total":   {"hits": <sum>, "misses": <sum>},
        }

    Only cells with a non-zero hit OR miss delta appear under per_gvr, to keep
    the proof compact. A cell present only in `after` counts its full value as
    the delta (new (handlerKind, gvr) observed during the stage).

    Returns:
        dict as above when BOTH snapshots are dicts.
        None if either snapshot is None (cache-off / unreachable — the caller
        records null).
    """
    if not isinstance(before_map, dict) or not isinstance(after_map, dict):
        return None

    def _cell_int(cell, field):
        if not isinstance(cell, dict):
            return 0
        v = cell.get(field, 0)
        try:
            return int(v or 0)
        except (TypeError, ValueError):
            return 0

    per_gvr = {}
    total_hits = 0
    total_misses = 0
    for cell_key in set(before_map) | set(after_map):
        before_cell = before_map.get(cell_key)
        after_cell = after_map.get(cell_key)
        d_hits = _cell_int(after_cell, "hit_total") - _cell_int(before_cell, "hit_total")
        d_misses = _cell_int(after_cell, "miss_total") - _cell_int(before_cell, "miss_total")
        if d_hits != 0 or d_misses != 0:
            per_gvr[cell_key] = {"hits": d_hits, "misses": d_misses}
        total_hits += d_hits
        total_misses += d_misses

    return {
        "per_gvr": per_gvr,
        "total": {"hits": total_hits, "misses": total_misses},
    }


# ─── Task #334a: convergence-cell discrimination tuple ──────────────────────
#
# Telemetry-ONLY (report-only). Attached alongside `convergence_ms` at every
# S6/S7/S8 convergence recording site so post-run analysis can discriminate
# "flat slope + high churn = cluster-state" (a slow-but-healthy convergence
# driven by controller-install churn) from "dropped slope = code regression"
# (the refresher stopped draining). This NEVER feeds compute_verdict — see the
# proof in the dev report: only conv_s8_p99 (convergence_p99_for_stage, which
# reads nav["convergence_ms"]) gates; this is an additive sibling key.
#
# Field sourcing (each None-safe; cache-OFF runs publish no expvars):
#   (a) dep_dirty_mark_total — the ABSOLUTE dirty-mark counter snapshot near
#       window-open. NOT a /debug/vars expvar: it is emitted ONLY in the
#       periodic `resolved_cache.summary` slog line (resolved.go:1303,
#       Deps().Stats().DirtyMarkTotal, default cadence 300s). Sourced by
#       grepping the streamed pod-log window for the LAST `dep_dirty_mark_total=N`
#       token. CAVEAT (flagged for architect): the 300s emission cadence means
#       this snapshot can lag the true mutation-land instant by up to one
#       summary interval; it is the most-recent value visible in the window.
#   (b) refresh_completed slope — the WIRED expvar
#       `snowplow_refresher_completed_total` (refresher_metrics.go:70). Two
#       snapshots (window-open + window-close) -> (delta / window_seconds).
#       Same prior-art mechanism as the S8b/S8c Probe-A (phases.py:2444-2473).
#   (c) addupdate_events_in_window — count of `cache_event.consumed` dirty-mark
#       (non-delete) lines with type in {ADD, UPDATE, CRD_ADD} (deps.go:549/745,
#       onChange + dirtyMarkResourceType) inside the window, from the same
#       streamed pod-log fetch as (a).


# cache_event.consumed `type` values that represent an ADD/UPDATE dirty-mark
# (NOT a delete): onChange emits ADD/UPDATE (deps.go:524/551); CRD_ADD comes
# from dirtyMarkResourceType (deps.go:697/745). DELETE / CRD_DELETE are
# excluded — they are eviction/delete events, not the churn signal field (c)
# measures.
_CONV_ADDUPDATE_EVENT_TYPES = ("ADD", "UPDATE", "CRD_ADD")


def _snowplow_pod_log_window(since_iso=None, *, tail=None, timeout=15):
    """Fetch snowplow pod-log text for a bounded window (NOT -f / streaming).

    A point-in-time `kubectl logs` snapshot used by the convergence
    discrimination tuple to read counters that are slog-only (not on
    /debug/vars). Mirrors the existing `kubectl logs --tail` call at
    wait_for_l1_warmup (line ~701) and storm.pod_logs_since.

    `since_iso` bounds the window via `--since-time` (RFC3339); `tail`
    bounds by line count. At least one SHOULD be set; if both are None we
    fall back to a 2000-line tail so the call is still bounded.

    CAVEAT (#334-A review): the 2000-line tail cap applies even with
    since_iso set, so a verify window exceeding 2000 lines truncates and
    undercounts field (c). Acceptable because (c) is a RELATIVE signal
    with the same ceiling on both compared runs; if a window legitimately
    exceeds the cap, raise/drop `tail` — `--since-time` is the real
    window delimiter.

    Returns the decoded log text, or None on any transport failure
    (kubectl missing, non-zero rc) — callers FAIL SOFT to None fields.
    """
    from bench.cluster import kubectl  # deferred — matches line ~689 style
    args = ["logs", "deployment/snowplow", "-n", NS, "-c", "snowplow"]
    if since_iso:
        args.append(f"--since-time={since_iso}")
    args.append(f"--tail={tail if tail is not None else 2000}")
    # cluster.kubectl uses `timeout_secs` (not `timeout`); pass it so the
    # window fetch is bounded. Fall back to a bare call if a test seam stubs
    # kubectl with a narrower signature.
    try:
        rc, out, _ = kubectl(*args, timeout_secs=timeout)
    except TypeError:
        try:
            rc, out, _ = kubectl(*args)
        except Exception:
            return None
    except Exception:
        return None
    if rc != 0 or not out:
        return None
    return out


def _parse_last_dep_dirty_mark_total(log_text):
    """Return the LAST `dep_dirty_mark_total=N` integer in `log_text`, or None.

    The token appears in the periodic `resolved_cache.summary` slog line
    (resolved.go:1303). slog text-handler renders it as
    `dep_dirty_mark_total=12345`. We take the LAST occurrence (closest to
    window-close). Returns None when the token is absent (no summary line
    emitted in the window — short window or cache-off).
    """
    if not log_text:
        return None
    last = None
    for m in re.finditer(r"dep_dirty_mark_total=(\d+)", log_text):
        last = m.group(1)
    if last is None:
        return None
    try:
        return int(last)
    except (ValueError, OverflowError):
        return None


def _count_addupdate_events(log_text):
    """Count `cache_event.consumed` ADD/UPDATE/CRD_ADD lines in `log_text`.

    Cheap line scan: a line counts iff it carries the `cache_event.consumed`
    message AND a `type=<X>` token whose X is in _CONV_ADDUPDATE_EVENT_TYPES.
    Returns None when `log_text` is None (transport failure), else the int
    count (0 when no matching lines — a real, distinguishable value).
    """
    if log_text is None:
        return None
    count = 0
    for line in log_text.splitlines():
        if "cache_event.consumed" not in line:
            continue
        m = re.search(r"\btype=(\w+)", line)
        if m and m.group(1) in _CONV_ADDUPDATE_EVENT_TYPES:
            count += 1
    return count


def capture_conv_discrimination(window_start_iso, window_seconds,
                                refresher_completed_before,
                                refresher_completed_after,
                                *, log_text=None):
    """Build the Task #334a convergence-cell discrimination tuple (report-only).

    All fields are None-safe. Returns a dict with:
      dep_dirty_mark_total          — (a) last in-window summary-line value, or None.
      refresh_completed_before/after — (b) raw expvar snapshots (or None).
      refresh_completed_slope_per_s  — (b) (after-before)/window_seconds, or None
                                       when either snapshot is None or window<=0.
      addupdate_events_in_window     — (c) count, or None when the log is
                                       unavailable.
      window_seconds                 — the verify-window width used for the slope.

    `log_text` is the pre-fetched pod-log window (so the caller fetches once and
    reuses it for (a) and (c); tests inject it directly). When None and a
    `window_start_iso` is given, the log is fetched here via
    `_snowplow_pod_log_window`.
    """
    if log_text is None and window_start_iso:
        log_text = _snowplow_pod_log_window(since_iso=window_start_iso)

    slope = None
    if (refresher_completed_before is not None
            and refresher_completed_after is not None
            and window_seconds is not None and window_seconds > 0):
        slope = (refresher_completed_after - refresher_completed_before) / window_seconds

    return {
        "dep_dirty_mark_total": _parse_last_dep_dirty_mark_total(log_text),
        "refresh_completed_before": refresher_completed_before,
        "refresh_completed_after": refresher_completed_after,
        "refresh_completed_slope_per_s": slope,
        "addupdate_events_in_window": _count_addupdate_events(log_text),
        "window_seconds": window_seconds,
    }


def count_user_compositions_in_ns(user, token, ns):
    """Count compositions VISIBLE to `user` via the piechart widget API.

    Probe B in the Phase 6 S8/S9 two-probe inner gate. Hits the same
    `/call?resource=piecharts&name=dashboard-compositions-panel-row-piechart`
    path the dashboard piechart uses; isolates 'snowplow's evaluator
    serves the right narrowed view' from 'snowplow's RBAC snapshot was
    rebuilt' (Probe A reads the expvar).

    Re-gate v3 (2026-06-05): renamed mechanism. The pre-v3 implementation
    hit `cluster.call_url(ns)` which constructs a /call URL WITHOUT a
    `name=` parameter. Snowplow's /call requires `name=` for composition
    GVR reads (TRACED: HTTP 400 "missing 'name' query parameter" for
    both admin AND cj at the diag capture
    `/tmp/diag-cj-rbac-probe-2026-06-05.log`). The -1 sentinel masked
    the URL bug, NOT RBAC.

    The piechart-widget endpoint returns `status.widgetData.title` as
    the user-narrowed composition count (UAF per JWT). Symmetric
    admin/cj: admin gets cluster-wide total; cj gets the sum across
    granted namespaces. For the bench's single-ns-grant pattern this
    equals comps_in_ns; for multi-ns grants it sums.

    The `ns` argument is retained for API stability + log correlation
    but is NOT threaded into the URL — the piechart RESTAction does
    its own RBAC narrowing per token. Callers may still pass it for
    diagnostic record-keeping.

    Args:
        user:  subject string (metadata only — token carries identity).
        token: bearer JWT for the user.
        ns:    namespace label (NOT used in URL — see above).

    Returns:
        int count on success, -1 on any failure path. Mirrors the
        verify_composition_count_api convention so the caller can poll
        with the same sentinel semantics.
    """
    if not token:
        return -1
    try:
        # Re-gate v3: the piechart-widget URL — empirically verified
        # 2026-06-05 against gke_neon-481711:
        #   admin token → HTTP 200, title='48999' (cluster-wide).
        #   cj token (no grant) → HTTP 200, title='0' (RBAC-narrowed).
        # Identical shape to verify_composition_count_api(token) below;
        # the two paths share the same RESTAction.
        _ms, code, body = http_get_json(
            '/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1'
            '&resource=piecharts'
            '&name=dashboard-compositions-panel-row-piechart'
            '&namespace=krateo-system',
            token,
        )
        if code != 200 or not isinstance(body, dict):
            return -1
        title = (body.get("status") or {}).get("widgetData", {}).get("title")
        if title is None:
            return -1
        return int(title)
    except Exception:
        return -1


def _expected_calls_tolerance():
    """Defer to bench.expected (tolerance is a constant but threading
    through a getter keeps tests that monkeypatch the constant honest)."""
    from bench.expected import EXPECTED_CALLS_TOLERANCE  # type: ignore
    return EXPECTED_CALLS_TOLERANCE


def _comp_card_params(user):
    """Return (base, per_card, per_page) for the /compositions fan-out
    formula, sourced from bench.expected so the skeleton-materializing
    consistency check (Task #284 addition) uses the SAME constants the
    expected_calls() formula uses — no drift between the two.

    `per_card` is the per-user widget plate (admin=4, cj=2), defaulting to
    the EXPECTED_CALLS_DEFAULT_USER plate for unknown subjects, exactly as
    expected_calls() resolves it.
    """
    from bench.expected import (  # type: ignore
        COMP_BASE_CALLS_STRUCTURAL,
        COMP_DATAGRID_PER_PAGE,
        COMP_PER_CARD_WIDGETS_BY_USER,
        COMP_PER_CARD_WIDGETS,
        EXPECTED_CALLS_DEFAULT_USER,
    )
    per_card = COMP_PER_CARD_WIDGETS_BY_USER.get(
        user, COMP_PER_CARD_WIDGETS_BY_USER.get(
            EXPECTED_CALLS_DEFAULT_USER, COMP_PER_CARD_WIDGETS))
    return (COMP_BASE_CALLS_STRUCTURAL, per_card, COMP_DATAGRID_PER_PAGE)


# ─── User credentials — read from in-cluster secrets at first use ───────────


NS = os.environ.get("BENCH_NS", "krateo-system")


def _read_password_from_secret(secret_name, ns=NS):
    """Read a password from a Kubernetes Secret's `password` data key.

    Replaces hardcoded constants. Hardcoded values drift the moment chart
    helpers regenerate the secrets at install time, then every login()
    returns 401 and the bench produces structurally identical numbers
    that have nothing to do with cache state.

    Returns the decoded password string. Raises RuntimeError if the
    secret is missing or malformed -- the bench MUST not silently fall
    back to a stale literal.
    """
    from bench import cluster  # lazy, matches this module's import style
    proc = subprocess.run(
        ["kubectl", *cluster.kubectl_context_args(),
         "get", "secret", secret_name, "-n", ns,
         "-o", "jsonpath={.data.password}"],
        capture_output=True, text=True, timeout=30,
    )
    if proc.returncode != 0:
        raise RuntimeError(
            f"could not read secret {secret_name} in namespace {ns}: "
            f"{proc.stderr.strip()}")
    encoded = (proc.stdout or "").strip()
    if not encoded:
        raise RuntimeError(
            f"secret {secret_name} in namespace {ns} has empty data.password")
    try:
        return base64.b64decode(encoded).decode("utf-8").rstrip("\n")
    except Exception as e:
        raise RuntimeError(
            f"could not base64-decode data.password from secret "
            f"{secret_name}: {type(e).__name__}: {e}")


def _build_users_dict():
    """Build USERS from in-cluster secrets at first credential read.

    Lazy-evaluated so unit tests and `python -m bench --help` do not
    require kubectl access.
    """
    return {
        "admin": _read_password_from_secret("admin-password"),
        "cyberjoker": _read_password_from_secret("cyberjoker-password"),
    }


# USERS is populated lazily by _ensure_users() on first credential read.
USERS: dict[str, str] = {}


def _ensure_users():
    """Populate USERS on first credential read."""
    global USERS
    if not USERS:
        USERS = _build_users_dict()
    return USERS


# ─── HTTP helpers ───────────────────────────────────────────────────────────


def _decompress(body, headers=None):
    """Decompress gzip response body if needed."""
    if headers and headers.get("Content-Encoding", "").lower() == "gzip":
        try:
            return gzip.decompress(body)
        except Exception:
            pass
    # Also try if it looks like gzip magic bytes
    if body[:2] == b"\x1f\x8b":
        try:
            return gzip.decompress(body)
        except Exception:
            pass
    return body


def login(username, password):
    """POST basic-auth credentials to AUTHN, return the JWT accessToken.

    Raises urllib.error.URLError / RuntimeError on failure. Callers
    (login_all + bench calibrate) wrap with retries.
    """
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def login_all():
    """Log in every USER and return {username: token}.

    Failed logins skip the user (with a logged WARNING). Returns the
    populated dict; if every login fails the dict is empty (caller must
    decide what to do).
    """
    tokens = {}
    for username, password in _ensure_users().items():
        for attempt in range(5):
            try:
                tokens[username] = login(username, password)
                _log(f"{username}: JWT acquired")
                break
            except Exception as e:
                if attempt < 4:
                    time.sleep(3)
                else:
                    _log(f"{username}: login FAILED — {e}")
    # O-D3 (snowplow 1.12.3): /debug/vars is no longer anonymous. Park the
    # first token we obtained so the expvar readers below have credentials
    # without every caller having to thread one through (see
    # _debug_auth_headers).
    if tokens:
        set_debug_token(next(iter(tokens.values())))
    return tokens


# ─── /debug/* credentials (snowplow O-D3, 1.12.3) ───────────────────────────
#
# As of snowplow 1.12.3 the whole /debug/* surface (pprof, vars, servable,
# apistage, refreshes) is behind the same JWT gate /debug/refreshes has had
# since #69 — an anonymous GET now returns 401, so the readers below MUST send
# a bearer. Resolution order:
#
#   1. SNOWPLOW_DEBUG_TOKEN in the environment (an operator-supplied JWT, or a
#      CI-injected one) — wins, so a run can probe with a specific identity;
#   2. whatever login_all() parked — the normal bench path, since the harness
#      logs every USER in before any stage runs.
#
# There is deliberately NO lazy login here: _debug_auth_headers sits on the
# probe path and must stay I/O-free and side-effect-free (the unit tests drive
# it with urlopen mocked and no cluster). A probe-only invocation that never
# called login_all sets SNOWPLOW_DEBUG_TOKEN, or calls set_debug_token().
#
# Any authenticated identity works: the gate validates the JWT signature and
# does not authorize per-key, so the counters an operator sees are the ones the
# bench reads. With no token the readers issue an anonymous GET — against a
# pre-1.12.3 pod that still succeeds, and against a 1.12.3 pod it 401s and the
# reader returns None, which every caller already fails closed on.

DEBUG_TOKEN: str | None = os.environ.get("SNOWPLOW_DEBUG_TOKEN") or None


def set_debug_token(token):
    """Park the JWT the /debug/* readers should present."""
    global DEBUG_TOKEN
    if token:
        DEBUG_TOKEN = token


def _debug_auth_headers(extra=None):
    """Return request headers for a /debug/* GET, incl. Authorization.

    Pure + I/O-free: merges the parked bearer (if any) on top of `extra`.
    """
    headers = dict(extra or {})
    if DEBUG_TOKEN:
        headers["Authorization"] = "Bearer " + DEBUG_TOKEN
    return headers


def http_get(path, token, base_url=None, timeout=120, retries=3,
             headers=None):
    """GET `path` against snowplow with Bearer auth + gzip decode.

    Returns (elapsed_ms, http_code, body_bytes). Retries up to `retries`
    times on connection failure (code==0); HTTP errors return their
    status code without retry.

    `headers` (optional dict) is merged on top of the always-present
    Authorization + Accept-Encoding pair — callers that need a request
    header (e.g. verify_serve_stale's `X-Krateo-TraceId` for the
    SECONDARY pod-log correlation source — plumbing v0.9.3
    server/use/logger.go:17 binds that header into the request's slog
    logger, so it lands on every `resolved_cache.lookup` line) supply it
    here. Defaults to None → existing callers are byte-for-byte
    unaffected.
    """
    url = (base_url or SNOWPLOW) + path
    req_headers = {"Authorization": "Bearer " + token,
                   "Accept-Encoding": "gzip"}
    if headers:
        req_headers.update(headers)
    req = urllib.request.Request(url, headers=req_headers)
    elapsed_ms = 0
    code = 0
    body = b""
    for attempt in range(retries):
        t0 = time.perf_counter()
        code, body = 0, b""
        try:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                raw = r.read()
                code = r.status
                body = _decompress(raw, dict(r.headers))
        except urllib.error.HTTPError as e:
            code = e.code
            try:
                raw = e.read()
                body = _decompress(raw, dict(e.headers) if hasattr(e, "headers") else None)
            except Exception:
                pass
        except Exception:
            code = 0
        elapsed_ms = int((time.perf_counter() - t0) * 1000)
        if code != 0:
            return elapsed_ms, code, body
        if attempt < retries - 1:
            _log(f"    HTTP 0, retry {attempt + 2}/{retries} in 3s ...")
            time.sleep(3)
    return elapsed_ms, 0, body


def http_get_json(path, token, **kw):
    """Like http_get but JSON-decodes the body."""
    ms, code, body = http_get(path, token, **kw)
    try:
        return ms, code, json.loads(body)
    except Exception:
        return ms, code, None


def http_get_with_headers(path, token, base_url=None, timeout=120):
    """Like http_get but also returns response headers as a dict."""
    url = (base_url or SNOWPLOW) + path
    req = urllib.request.Request(
        url,
        headers={"Authorization": "Bearer " + token, "Accept-Encoding": "gzip"},
    )
    t0 = time.perf_counter()
    code, body, hdrs = 0, b"", {}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            code = r.status
            hdrs = dict(r.headers)
            body = _decompress(raw, hdrs)
    except urllib.error.HTTPError as e:
        code = e.code
        hdrs = dict(e.headers) if hasattr(e, "headers") else {}
    except Exception:
        pass
    elapsed_ms = int((time.perf_counter() - t0) * 1000)
    return elapsed_ms, code, body, hdrs


def cache_metrics(token):
    """Fetch /metrics/cache as JSON (used to estimate L1-ready sentinel)."""
    _, _, body = http_get_json("/metrics/cache", token)
    return body or {}


def get_runtime_metrics():
    """Fetch /metrics/runtime from snowplow. Returns dict or None on error.

    Unauthenticated probe (legacy snowplow_test.py behaviour).
    """
    try:
        req = urllib.request.Request(SNOWPLOW + "/metrics/runtime")
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read())
    except Exception:
        return None


def _read_l1_ready_ts():
    """Read the L1-ready timestamp proxy. Uses /metrics/cache l1_hits as a
    monotonic freshness signal.
    """
    m = cache_metrics("")
    return int(time.time()) if m.get("l1_hits", 0) > 0 else 0


# ─── Endpoint catalogues ────────────────────────────────────────────────────


WIDGET_ENDPOINTS = [
    ("page/dashboard",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=dashboard-page&namespace=krateo-system"),
    ("page/blueprints",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=blueprints-page&namespace=krateo-system"),
    ("page/compositions",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=compositions-page&namespace=krateo-system"),
    ("navmenu/sidebar",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system"),
    ("routes/loader",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=routesloaders&name=routes-loader&namespace=krateo-system"),
]


RESTACTION_ENDPOINTS = [
    ("restaction/all-routes",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=all-routes&namespace=krateo-system"),
    ("restaction/bp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=blueprints-list&namespace=krateo-system"),
    ("restaction/comp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"),
]


ALL_ENDPOINTS = WIDGET_ENDPOINTS + RESTACTION_ENDPOINTS


BROWSER_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


BROWSER_NAV_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


BROWSER_SCALING_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


# ─── L1 + compositions waiters (used by browser-side callers) ───────────────


# Task #221 (bench half): the prewarm-completion boundary expvar published by
# the snowplow half (internal/cache/prewarm_complete_metric.go). Value shape is
# a map {"done": 0|1, "elapsed_ms": int}; done==1 is the boundary (the same
# atomic /readyz consults — funnels EVERY Phase1Done flip path: dispatcher
# happy path, cache-off else branch, readiness safety-net). The key is ABSENT
# on images through 0.30.264 (and under CACHE_ENABLED=false — Disabled() gate),
# so callers MUST fall back to the slog-scan boundary when this returns None.
PREWARM_COMPLETE_EXPVAR = "snowplow_prewarm_complete"


def prewarm_complete_via_expvar(base_url=None, timeout=10):
    """Probe the snowplow_prewarm_complete expvar for the prewarm boundary.

    Returns:
        True  — the expvar is present AND done==1 (boundary reached).
        False — the expvar is present but done==0 (boundary NOT yet reached);
                the key exists, so the caller should keep polling the expvar
                (it is authoritative — do NOT fall back to slog-scan).
        None  — the key is ABSENT or unreadable (pre-0.30.264 image, cache-off
                Disabled() gate, or transport failure). The caller MUST fall
                back to the existing slog-scan boundary.

    Reuses the established read_snowplow_expvar_map seam (single GET of
    /debug/vars, gzip-decompress, json.loads); base_url overrides SNOWPLOW for
    tests. None-safe throughout.
    """
    m = read_snowplow_expvar_map(PREWARM_COMPLETE_EXPVAR,
                                 base_url=base_url, timeout=timeout)
    if not isinstance(m, dict):
        return None
    done = m.get("done")
    # `done` is published as an int (0/1). Anything else (missing / wrong
    # shape) → treat the key as unusable and fall back.
    if not isinstance(done, int) or isinstance(done, bool):
        return None
    return done == 1


def wait_for_l1_warmup(timeout=300):
    """Wait for the prewarm / L1-warmup boundary.

    Task #221 (bench half): PREFER the `snowplow_prewarm_complete` expvar
    (published by the snowplow half — the single Phase1Done flip boundary);
    fall back to the legacy /metrics/runtime + pod-log slog-scan when the key
    is ABSENT (images through 0.30.264 do NOT publish it, and CACHE_ENABLED=
    false gates it off). The bench must work against BOTH, so this is a
    prefer-expvar / fallback-to-scan probe that logs which path it used.

    Returns True when the boundary is detected, False on timeout.

    Legacy fallback behaviour (unchanged) matches the source script's
    `wait_for_l1_warmup` (worktree line 1000): primary signal is
    `cache_key_count > 0` on /metrics/runtime; secondary scans pod logs for
    the warmup-completed sentinel.
    """
    from bench.cluster import kubectl  # deferred — cluster is lifecycle-side

    _log("Waiting for L1 warmup ...")
    deadline = time.time() + timeout
    expvar_path_logged = False
    scan_path_logged = False
    while time.time() < deadline:
        # ── Prefer the expvar boundary (Task #221). ──
        expvar_done = prewarm_complete_via_expvar()
        if expvar_done is not None:
            # Key present → it is authoritative. Use it EXCLUSIVELY (do NOT
            # consult the slog-scan, which could fire on a stale log line).
            if not expvar_path_logged:
                _log(f"L1 warmup boundary: using expvar "
                     f"{PREWARM_COMPLETE_EXPVAR} (deployed image publishes it)")
                expvar_path_logged = True
            if expvar_done:
                _log(f"L1 warmup detected (expvar "
                     f"{PREWARM_COMPLETE_EXPVAR} done=1)")
                return True
            time.sleep(5)
            continue

        # ── Mandatory fallback: expvar key ABSENT (pre-0.30.264 / cache-off).
        if not scan_path_logged:
            _log(f"L1 warmup boundary: expvar {PREWARM_COMPLETE_EXPVAR} absent "
                 f"— falling back to /metrics/runtime + pod-log slog-scan")
            scan_path_logged = True

        rt = get_runtime_metrics()
        if rt:
            keys = rt.get("cache_key_count", 0)
            if keys > 0:
                _log(f"L1 warmup detected ({keys} cache keys)")
                return True

        rc, out, _ = kubectl("logs", "deployment/snowplow", "-n", NS,
                             "-c", "snowplow", "--tail=500")
        if rc == 0:
            if "warmup: completed" in out or "warmup: pre-warm disabled" in out:
                _log("L1 warmup completed (log)")
                return True
            if "warmup: skipped" in out or "warmup: no users found" in out:
                _log("L1 warmup skipped (log)")
                return True

        time.sleep(5)
    _log("WARNING: L1 warmup not detected within timeout")
    return False


def wait_for_l1_ready(since_epoch=None, timeout=120):
    """Wait until the L1-ready sentinel is strictly > since_epoch.

    The sentinel is derived from /metrics/cache l1_hits (the legacy proxy
    used by the source script). When `since_epoch` is None, the current
    sentinel is snapshotted and we wait for it to change.
    """
    if since_epoch is None:
        since_epoch = _read_l1_ready_ts()
        _log(f"Waiting for L1 ready (current sentinel={since_epoch}) ...")
    else:
        _log(f"Waiting for L1 ready (since={since_epoch}) ...")

    deadline = time.time() + timeout
    while time.time() < deadline:
        ts = _read_l1_ready_ts()
        if ts > since_epoch:
            _log(f"L1 ready (sentinel={ts})")
            return True
        time.sleep(2)

    _log(f"WARNING: L1 not ready within {timeout}s "
         f"(sentinel={_read_l1_ready_ts()}, need >{since_epoch})")
    return False


def wait_for_compositions(expected, timeout=300, tolerance=5):
    """Wait until at least `expected - tolerance` compositions exist in
    the cluster.

    Returns True on success, False on timeout. NEVER raises — Block 3
    callers (Block 4's phases.py) check the return code.

    NOTE: this is the LEGACY helper retained for the run_user_scaling
    flow that lives in storm.py. The Block 4 stage runner's verify-step
    against partial state is the new ConvergenceTimeout-emitting helper
    inside `browser_measure_stage`.
    """
    _log(f"Waiting for {expected} compositions to exist (tolerance={tolerance}) ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        actual = _count_compositions()
        if actual >= expected:
            _log(f"All {actual} compositions exist")
            return True
        if actual >= expected - tolerance:
            _log(f"All {actual}/{expected} compositions exist (within tolerance)")
            return True
        elapsed = int(time.time() - (deadline - timeout))
        if elapsed % 30 < 5:
            _log(f"  {actual}/{expected} compositions exist ({elapsed}s elapsed)")
        time.sleep(5)
    actual = _count_compositions()
    _log(f"WARNING: only {actual}/{expected} compositions after {timeout}s")
    return False


def list_composition_names_from_cache(token):
    """Return the set of `ns/name` strings reported by compositions-list.

    Used by browser_measure_stage for the post-VERIFY CONTENT match. None
    on transport/parse failure (caller silently skips CONTENT check).
    """
    try:
        _ms, code, body = http_get_json(
            '/call?apiVersion=templates.krateo.io%2Fv1'
            '&resource=restactions'
            '&name=compositions-list'
            '&namespace=krateo-system',
            token,
        )
        if code != 200 or not isinstance(body, dict):
            return None
        items = (body.get("status") or {}).get("list") or []
        names = set()
        for item in items:
            if not isinstance(item, dict):
                continue
            ns = item.get("ns", "")
            name = item.get("name", "")
            if not ns or not name:
                md = item.get("metadata") or {}
                ns = ns or md.get("namespace", "")
                name = name or md.get("name", "")
            if ns and name:
                names.add(f"{ns}/{name}")
        return names
    except Exception:
        return None


# ─── ConvergenceTimeout exception (new — Block 3) ───────────────────────────


class ConvergenceTimeout(Exception):
    """Raised when VERIFY poll's deadline expires with matched=False.

    Replaces today's silent skip at source 6465-6475 (worktree
    snowplow_test.py). Caller MUST NOT silently swallow. Block 4's stage
    runner catches this, writes a stage proof with `passed=False` AND
    `convergence_timeout=true`, persists state.json, and re-raises to
    abort the run with exit 4.

    Fields enable post-mortem diagnostics:
      stage    — stage number that failed convergence (1..8)
      user     — which user's navigation timed out (admin/cyberjoker)
      api      — what /call piechart API saw last
      ui       — what the UI fetch saw last
      cluster  — what `kubectl count_compositions` reports as ground truth
      timeout_secs — the deadline that was exceeded (default 300s)
    """

    def __init__(self, *, stage, user, api, ui, cluster, timeout_secs=None):
        self.stage = stage
        self.user = user
        self.api = api
        self.ui = ui
        self.cluster = cluster
        self.timeout_secs = timeout_secs
        api_str = str(api) if api is not None and api >= 0 else "?"
        ui_str = str(ui) if ui is not None and ui >= 0 else "?"
        timeout_str = f"{timeout_secs}s" if timeout_secs is not None else "deadline"
        super().__init__(
            f"convergence timeout: stage=S{stage} user={user!r} "
            f"api={api_str} ui={ui_str} cluster={cluster} "
            f"(deadline {timeout_str} expired with matched=False)"
        )


# ─── Browser context + video → GIF (new — Block 3) ──────────────────────────


def make_browser_context(playwright_browser, *,
                         record_video_dir: Path | None = None,
                         viewport=(1280, 900),
                         ignore_https_errors: bool = True,
                         storage_state=None):
    """Construct a Playwright BrowserContext with optional video recording.

    `record_video_dir` is per cell representative only (n=1 per stage,
    user, cache_mode, page) — see plan §I R3.1. When set, the directory
    is created if missing and passed to `browser.new_context(...)`.
    Subsequent samples for the same cell should pass `record_video_dir=None`
    to keep the run bundle inside the 200 MB cap.

    `viewport` is a (width, height) tuple; converted to Playwright's
    {"width", "height"} dict at the call site so callers don't depend
    on Playwright's exact dict shape.

    `storage_state` (Task #307 / A1-full): an optional Playwright
    storage-state (the dict/JSON captured from a prior
    `context.storage_state()`). When provided it is forwarded to
    `new_context(storage_state=...)` so the new context starts ALREADY
    authenticated — its first `goto` lands on the target page directly,
    with NO `/login` form drive and NO dashboard-landing redirect filmed.
    This is what lets a per-page recording context's `.webm` begin on its
    own page instead of the login/dashboard head.

    Returns the new BrowserContext object.
    """
    kwargs = {
        "viewport": {"width": int(viewport[0]), "height": int(viewport[1])},
        "ignore_https_errors": ignore_https_errors,
    }
    if record_video_dir is not None:
        rvd = Path(record_video_dir)
        rvd.mkdir(parents=True, exist_ok=True)
        kwargs["record_video_dir"] = str(rvd)
    if storage_state is not None:
        kwargs["storage_state"] = storage_state

    return playwright_browser.new_context(**kwargs)


def record_video_to_gif(webm_path: Path, gif_path: Path,
                        *, fps: int = 4, max_seconds: int = 60,
                        max_mb: int | None = None) -> bool:
    """Convert a Playwright .webm video to .gif via ffmpeg.

    Returns True on success, False when:
      - ffmpeg is not on PATH (operator missing toolchain)
      - the source .webm does not exist
      - ffmpeg returns non-zero exit (or produces no output)

    NO size-based drop (Task #267 correction — Diego 2026-06-10): the gif is
    KEPT regardless of size, and the source .webm is NEVER deleted for size by
    this helper. The prior `max_mb` oversize-delete is REMOVED so no stage
    video/gif is ever dropped for size (a ~25-min S6 install produces a large
    .webm that MUST be retained). The `max_mb` parameter is kept only for
    call-site backward-compat and is now a NO-OP (ignored).

    `max_seconds` is retained: it bounds the GIF's *duration* via ffmpeg `-t`
    (gif quality/length, NOT a drop of any artifact) so a long recording
    yields a watchable-length gif rather than a multi-minute one. The full
    .webm is always retained at full length.

    This helper NEVER raises; on any ffmpeg failure the run continues with the
    .webm only.

    Filter graph: `fps={fps},scale=720:-1:flags=lanczos`.
    """
    if max_mb is not None:
        # Deprecated/no-op (kept for call-site compatibility). Surfaced once
        # at debug volume so a stale caller is visible without changing
        # behaviour — nothing is dropped for size anymore.
        _log(f"  record_video_to_gif: max_mb={max_mb} is a no-op "
             f"(size-based drop removed; gif kept regardless of size)")
    webm = Path(webm_path)
    gif = Path(gif_path)

    if shutil.which("ffmpeg") is None:
        _log(f"  record_video_to_gif: ffmpeg not on PATH — skipping {webm.name}")
        return False

    if not webm.exists():
        _log(f"  record_video_to_gif: source missing {webm} — skipping")
        return False

    gif.parent.mkdir(parents=True, exist_ok=True)
    # Bound the GIF *duration* at max_seconds via `-t` (gif length/quality —
    # NOT a drop). The .webm itself is recorded + retained at full length.
    cmd = [
        "ffmpeg",
        "-y",                       # overwrite without prompt
        "-i", str(webm),
        "-t", str(int(max_seconds)),
        "-vf", f"fps={int(fps)},scale=720:-1:flags=lanczos",
        "-loop", "0",
        str(gif),
    ]
    try:
        proc = subprocess.run(
            cmd, capture_output=True, text=True, timeout=120,
        )
    except subprocess.TimeoutExpired:
        _log(f"  record_video_to_gif: ffmpeg timeout on {webm.name}")
        return False
    except Exception as e:
        _log(f"  record_video_to_gif: ffmpeg invocation failed: "
             f"{type(e).__name__}: {e}")
        return False

    if proc.returncode != 0:
        _log(f"  record_video_to_gif: ffmpeg rc={proc.returncode} "
             f"on {webm.name}; stderr={proc.stderr[:200]!r}")
        return False

    if not gif.exists():
        _log(f"  record_video_to_gif: ffmpeg returned 0 but {gif} missing")
        return False

    # NO size cap: the gif is kept regardless of size (no artifact dropped).
    return True


# ─── Widget skeleton + terminal-state validation ────────────────────────────


# JS that counts widget-scoped .ant-skeleton elements. We intentionally
# scope the count rather than match `.ant-skeleton` cluster-wide because
# the bare selector catches transient overlay surfaces (Drawer/
# Notification/Modal/Message/Popover/Dropdown/Tooltip) that have nothing
# to do with the main widget tree. Phase A 0.30.6 v2 attempt 4 surfaced
# this as false positives (e.g. a Notification toast firing during nav
# got logged as a "stuck widget skeleton").
#
# Two layers of scoping:
#
#   (preferred) [data-widget-renderer] .ant-skeleton — exact match
#     against the widget mount point. Becomes live the moment frontend
#     lands the `data-widget-renderer` wrapper on WidgetRenderer.tsx.
#     Today the wrapper does NOT exist, so this branch matches zero.
#   (fallback) all .ant-skeleton EXCLUDING those that have an ancestor
#     in the overlay-selector list.
#
# The JS returns both counts so logs can show whether the preferred
# wrapper has rolled out; once `widget_scoped` matches the fallback
# consistently, the fallback can be deleted.
_WIDGET_SKELETON_COUNT_JS = """
() => {
    const EXCLUDED_ANCESTORS = [
        '.ant-drawer',
        '.ant-notification',
        '.ant-notification-notice',
        '.ant-modal',
        '.ant-modal-root',
        '.ant-message',
        '.ant-popover',
        '.ant-dropdown',
        '.ant-tooltip',
        '.ant-select-dropdown'
    ];
    const all = Array.from(document.querySelectorAll('.ant-skeleton'));
    const widgetScoped = document.querySelectorAll(
        '[data-widget-renderer] .ant-skeleton').length;
    let scoped = 0;
    for (const el of all) {
        let excluded = false;
        for (const sel of EXCLUDED_ANCESTORS) {
            if (el.closest(sel)) { excluded = true; break; }
        }
        if (!excluded) scoped++;
    }
    return {raw: all.length, scoped: scoped, widget_scoped: widgetScoped};
}
"""


def _count_widget_skeletons(page):
    """Return (scoped, raw, widget_scoped) skeleton counts on the page.

    `scoped` is the gate input: skeletons OUTSIDE Drawer/Notification/
    Modal/Message/Popover/Dropdown/Tooltip surfaces. `raw` is the legacy
    unscoped `.ant-skeleton` count, kept for diagnostic visibility.
    `widget_scoped` matches `[data-widget-renderer] .ant-skeleton`; it
    is zero today (the frontend does not yet emit `data-widget-renderer`)
    and becomes the canonical metric once that wrapper rolls out.
    """
    result = page.evaluate(_WIDGET_SKELETON_COUNT_JS)
    return (int(result.get("scoped", 0)),
            int(result.get("raw", 0)),
            int(result.get("widget_scoped", 0)))


# JS that counts the composition CARDS the datagrid has actually rendered
# on page 1 at nav-time. This is the DOM-side source of truth for the
# /compositions per-card fan-out term (Task #284): each rendered card
# fires COMP_PER_CARD_WIDGETS_BY_USER[user] /call requests, so the gate's
# expected count must be derived from the cards the page actually painted,
# NOT from a cluster-wide apiserver LIST that races ahead of the render.
#
# A "rendered card" is identified by its composition-name title. Every
# /compositions card renders its composition name as text (the
# `compositions-panels` datagrid's per-row Panel title); in Phase 6 those
# names all carry the `bench-app-` prefix (the bench's own deploy naming,
# `lifecycle.deploy_compositions_parallel`). We count the INNERMOST element
# whose trimmed text is exactly a composition name, scoped OUT of the
# nav/sidebar/menu chrome — the same exclusion idiom the VERIFY scroll-
# capture uses (browser.py scroll-capture: `nav, [class*="sidebar"],
# [class*="menu"], [class*="sider"]`). Counting innermost-only de-dups the
# card-container ancestors that also contain the same text.
#
# This deliberately does NOT read the /call performance timeline: deriving
# `expected` from the page's own /call count would be circular and would
# mask a genuine under-call (cards rendered but per-card /calls missing).
# The card count here is a pure render-state signal, independent of the
# `actual_calls` it is compared against.
_RENDERED_COMP_CARDS_JS = r"""
(namePrefix) => {
    const EXCLUDED_ANCESTORS = 'nav, [class*="sidebar"], [class*="menu"], [class*="sider"]';
    // A composition name is `<prefix><digits/dashes>`, e.g. bench-app-01-39.
    const re = new RegExp('^' + namePrefix.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '[a-z0-9-]*$', 'i');
    const matched = new Set();
    const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT, null);
    while (walker.nextNode()) {
        const node = walker.currentNode;
        const txt = (node.textContent || '').trim();
        if (!re.test(txt)) continue;
        const el = node.parentElement;
        if (!el) continue;
        if (el.closest(EXCLUDED_ANCESTORS)) continue;  // skip nav/sidebar chrome
        matched.add(txt);  // one entry per distinct composition name = one card
    }
    return matched.size;
}
"""


def _count_rendered_comp_cards(page, name_prefix="bench-app-"):
    """Return the number of composition cards rendered on /compositions
    page 1 at nav-time, read from the live DOM.

    DOM-side source of truth for Task #284: the per-card /call fan-out is
    physically realized only by cards the datagrid has painted, so the
    EXPECTED_CALLS gate's `n_visible` must come from here, not a cluster
    LIST. Returns 0 on any evaluate failure (caller treats 0 as BASE-only
    expected, matching a page that rendered no cards yet).
    """
    try:
        n = page.evaluate(_RENDERED_COMP_CARDS_JS, name_prefix)
        return max(0, int(n))
    except Exception:
        return 0


def _errored_call_namespaces(call_statuses):
    """Return the set of `namespace` query-params of non-200 /call URLs.

    Task #296: the S10 churn-ghost demotion needs to know WHICH namespaces
    the errored per-row panel GETs targeted. Each errored panel GET is a
    `/call?...&name=<panel>&namespace=<ns>` that returned non-200 (snowplow
    correctly 404s a controller-churned/deleted Panel). The set of those
    `namespace` params is the evidence the demotion predicate keys on:
    if EVERY errored ns is OUTSIDE the bench-deleted ns, the errors are
    controller-churn ghosts (a recorded WARN); if ANY errored ns IS the
    deleted ns, that is a genuine serve-stale ghost and stays a HARD fail.

    call_statuses is the list of {"url","status"} dicts the response
    listener captured. Returns a set[str]; empty when nothing errored.
    """
    from urllib.parse import urlparse, parse_qs
    out = set()
    for s in (call_statuses or []):
        if s.get("status") == 200:
            continue
        try:
            qs = parse_qs(urlparse(s.get("url", "")).query)
        except Exception:
            continue
        ns = (qs.get("namespace") or [None])[0]
        if ns:
            out.add(ns)
    return out


def _validate_widget_terminal_state(page, page_path, label, user="admin",
                                    token=None, deleted_ns=None,
                                    call_statuses=None):
    """Inspect the rendered page after the /call stability poll returns.

    The waterfall measurement only tells us when network activity stopped.
    It does not tell us whether the dashboard actually rendered piechart
    + table or whether every widget is still showing a Skeleton because
    the stability poll exited prematurely. This helper applies three
    gates and returns a structured result dict; callers decide whether
    to invalidate `waterfallMs` based on `terminal_state`.

    Gates:
      1. widget-scoped .ant-skeleton count == 0 — HARD FAIL.
      2. .ant-result-error count is recorded but NOT a failure: errored
         widgets are a valid terminal state. The count IS still
         load-bearing in S8/S9 `__passed__` per Task #250 R4 mitigation —
         the stage runner reads `errored_count` from the proof and
         requires `== 0` for the cj/Compositions cell.
      3. /call count must be within EXPECTED_CALLS_TOLERANCE of
         expected_calls(user, page_path, n_visible=N) — HARD FAIL on
         deviation. The N-aware formula (Task #250 Block 2) adds the
         per-card-widget fan-out term for /compositions when the user
         has visible cards; admin reads cluster-truth, cj reads the
         RBAC-narrowed piechart count (parametric per
         feedback_no_special_cases).

    Args:
        page:      Playwright Page object.
        page_path: page URL path used for EXPECTED_CALLS lookup.
        label:     diagnostic label for log lines.
        user:      subject the page is logged in as (drives n_visible
                   sourcing).
        token:     optional bearer JWT for the same user; used to query
                   the per-user composition count when narrowing applies.
                   When None and the user is non-admin, falls back to
                   BASE expected (the legacy pre-Block-2 behaviour).

    Returns a dict with keys:
        skeleton_count, skeleton_count_raw, skeleton_count_widget_scoped,
        errored_count, expected_calls, actual_calls,
        calls_within_tolerance, terminal_state ("pass" or "fail"), user,
        n_visible (the value threaded into the formula; None when not
        applicable).
    """
    try:
        skeleton_count, skeleton_raw, skeleton_widget = _count_widget_skeletons(page)
    except Exception as e:
        _log(f"    [WARN] could not count widget-scoped .ant-skeleton at "
             f"{label}: {e}")
        skeleton_count = skeleton_raw = skeleton_widget = 0
    try:
        errored_count = page.locator(".ant-result-error").count()
    except Exception as e:
        _log(f"    [WARN] could not count .ant-result-error at {label}: {e}")
        errored_count = 0
    try:
        actual_calls = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
    except Exception as e:
        _log(f"    [WARN] could not read /call count at {label}: {e}")
        actual_calls = 0

    # Source the per-user visible count for the N-aware EXPECTED_CALLS
    # formula.
    #
    # STRATEGIC RULE (Task #284, Diego-ratified 2026-06-10): the RENDERED
    # DOM predicts /call-counts; the cluster LIST validates CONTENT. The
    # per-card fan-out term is physically realized only by cards the
    # datagrid has painted on page 1 at nav-time, so `n_visible` for the
    # EXPECTED_CALLS gate MUST be derived from the rendered cards on the
    # page under validation — NOT from `count_compositions_with_panels_ready`
    # (a cluster-wide apiserver LIST on a later/faster path that races ahead
    # of the render at small-N right after a fresh deploy, producing
    # expected=30 against a page that legitimately issued 10/14). The
    # cluster-LIST source stays available (and is still used by the S8/S9
    # content-correctness card-presence checks in phases.py) — it is just
    # the wrong proxy for call-count prediction. See
    # docs/task-284-s4s5-admin-callcount-trace-2026-06-10.md §7 Option A.
    #
    # Returns None for pages where the formula does not apply (anything
    # other than /compositions today), which makes the lookup transparently
    # fall back to BASE.
    if page_path == "/compositions":
        # rendered_card_count drives the fan-out term; expected_calls()
        # clamps to COMP_DATAGRID_PER_PAGE internally, so:
        #   0 rendered  -> expected = BASE (10)         [S4 fresh deploy]
        #   1 rendered  -> expected = 10 + per_card×1    [S5 one materialized]
        #   >=5 rendered -> expected = 10 + per_card×5    [S6 50K saturated]
        # A page that renders cards but omits their per-card /calls still
        # mismatches (real under-call detection is preserved).
        n_visible = _count_rendered_comp_cards(page)
    else:
        n_visible = None

    expected = _expected_calls_lookup(user, page_path, n_visible=n_visible)
    tolerance = _expected_calls_tolerance()
    if expected is None:
        calls_within_tolerance = True
    else:
        calls_within_tolerance = abs(actual_calls - expected) <= tolerance

    # For /compositions the rendered-card count IS n_visible (set above).
    # rendered_cards is persisted into the result dict (Task #284) so the
    # materialization-vs-under-call distinction is post-run checkable.
    rendered_cards = n_visible if page_path == "/compositions" else None

    terminal_state = "pass"

    # Skeleton gate (Task #284 addition, architect-specified gated demotion):
    # an unconditional `skeleton>0 → fail` mis-classifies the small-N
    # materialization race — at S4 a page-1 card slot is still painting an
    # .ant-skeleton because its Panel CR hasn't materialized, so the card
    # has not yet rendered and has not yet issued its per-card /calls. That
    # is a benign mid-materialization state, NOT a stuck widget. Demote the
    # skeleton to a recorded WARN (`skeleton_materializing`) ONLY when EVERY
    # condition holds; if ANY fails it stays a HARD FAIL (real stuck-widget /
    # premature-stability / under-call detection is preserved):
    #   1. page_path == "/compositions"            (only the datagrid races)
    #   2. rendered_cards < per_page                (page-1 not saturated)
    #   3. errored_count == 0                       (no errored widget)
    #   4. actual_calls == expected_calls           (call-count gate clean)
    #   5. (actual_calls - BASE) == per_card×rendered  (/calls issued exactly
    #      account for the cards that DID render — the consistency check that
    #      separates "still materializing" from "rendered but didn't fire")
    skeleton_materializing = False
    if skeleton_count > 0:
        base, per_card, per_page = _comp_card_params(user)
        materializing = (
            page_path == "/compositions"
            and rendered_cards is not None
            and rendered_cards < per_page
            and errored_count == 0
            and expected is not None
            and actual_calls == expected
            and (actual_calls - base) == per_card * rendered_cards
        )
        if materializing:
            skeleton_materializing = True
            _log(f"    [WARN] skeleton_materializing: {skeleton_count} "
                 f"skeleton(s) at {label} while datagrid page-1 is still "
                 f"materializing (rendered_cards={rendered_cards}<{per_page}, "
                 f"actual={actual_calls}==expected, errored=0) — benign race")
        else:
            _log(f"    [FAIL] stability_premature: {skeleton_count} skeletons "
                 f"still visible at {label} (raw={skeleton_raw}, "
                 f"widget_scoped={skeleton_widget})")
            terminal_state = "fail"
    if errored_count > 0:
        _log(f"    [WARN] errored_widgets={errored_count} at {label}")

    # Task #296 — S10 controller-churn ghost demotion (mirrors the efaf1a4
    # skeleton-demotion pattern: narrow predicate, hard-fail preserved
    # otherwise). During S10's bulk delete, composition-controller reconcile
    # churn deletes the Panel CRs backing the newest-5 cluster-wide
    # compositions; the SPA reads those (still-fresh) composition names from
    # the datagrid's newest-first page 1 and GETs each per-row Panel, which
    # snowplow correctly 404s (serving a ghost panel body would be the real
    # bug). Those ghost 404s inflate actual_calls past expected → a
    # call_count_mismatch. That is an expected controller-churn input, NOT a
    # snowplow defect — UNLESS an errored panel is in the bench-DELETED ns,
    # which WOULD be a genuine serve-stale ghost and stays a HARD fail.
    #
    # Demote ONLY when EVERY condition holds:
    #   1. deleted_ns is set                     (S10 only — None off-S10)
    #   2. errored_count > 0                     (there are ghost cards)
    #   3. the ONLY reason this nav would fail is the call_count_mismatch
    #      (no skeleton fail) AND the mismatch is an OVER-call (actual >
    #      expected — extra ghost GETs), never an under-call
    #   4. EVERY errored /call namespace is OUTSIDE deleted_ns
    # If any errored ns IS the deleted ns, or the mismatch is an under-call,
    # or a skeleton already failed, the normal hard-fail path runs.
    s10_churn_errors = None
    errored_namespaces = sorted(_errored_call_namespaces(call_statuses))
    churn_demoted = False
    if (deleted_ns is not None
            and errored_count > 0
            and expected is not None
            and not calls_within_tolerance
            and actual_calls > expected          # over-call from ghost GETs
            and terminal_state == "pass"          # no prior (skeleton) fail
            and errored_namespaces
            and deleted_ns not in errored_namespaces):
        churn_demoted = True
        s10_churn_errors = {
            "errored_count": errored_count,
            "errored_namespaces": errored_namespaces,
            "deleted_ns": deleted_ns,
            "expected_calls": expected,
            "actual_calls": actual_calls,
        }
        _log(f"    [WARN] s10_churn_errors: {errored_count} errored "
             f"controller-churn ghost panel(s) at {label} in ns "
             f"{errored_namespaces} (all OUTSIDE deleted ns {deleted_ns}); "
             f"actual={actual_calls}>expected={expected} demoted to WARN "
             f"(transient newest-card ghost, not a serve-stale defect)")

    if expected is not None and not calls_within_tolerance and not churn_demoted:
        nvis_str = f"n_visible={n_visible}" if n_visible is not None else \
                   "n_visible=base"
        _log(f"    [FAIL] call_count_mismatch[{user}]: expected={expected}"
             f"±{tolerance} actual={actual_calls} ({nvis_str}) at {label}")
        terminal_state = "fail"

    return {
        "skeleton_count": skeleton_count,
        "skeleton_count_raw": skeleton_raw,
        "skeleton_count_widget_scoped": skeleton_widget,
        "skeleton_materializing": skeleton_materializing,
        "rendered_cards": rendered_cards,
        "errored_count": errored_count,
        "expected_calls": expected,
        "actual_calls": actual_calls,
        "calls_within_tolerance": calls_within_tolerance,
        "terminal_state": terminal_state,
        "s10_churn_demoted": churn_demoted,
        "s10_churn_errors": s10_churn_errors,
        "user": user,
        "n_visible": n_visible,
    }


# ─── Browser login + navigation measurement ─────────────────────────────────


def browser_login(page, username, password, retries=3):
    """Login via the frontend UI; return True on success.

    Drives the form via Playwright. Verifies success by reading the
    `K_user` localStorage entry — pure URL redirects can lie.
    """
    if FRONTEND is None:
        _log("    browser: FRONTEND_URL not set — skipping login")
        return False
    for attempt in range(retries):
        try:
            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=300000)
            _log(f"    browser: login page loaded, URL={page.url}")
            page.click('#basic_username', timeout=10000)
            page.keyboard.type(username, delay=30)
            page.click('#basic_password', timeout=10000)
            page.keyboard.type(password, delay=30)
            page.click('button[type="submit"]', timeout=10000)
            page.wait_for_load_state("networkidle", timeout=30000)
            page.wait_for_timeout(5000)
            has_token = page.evaluate("""() => {
                try {
                    const u = JSON.parse(localStorage.getItem('K_user') || '{}');
                    return !!u.accessToken;
                } catch { return false; }
            }""")
            if has_token:
                _log(f"    browser: login OK — auth token in localStorage")
                return True
            _log(f"    browser: login attempt {attempt + 1}: no auth token, retrying")
        except Exception as e:
            current = getattr(page, "url", "?")
            _log(f"    browser: login attempt {attempt + 1} failed "
                 f"(URL={current}): {e}")
        if attempt < retries - 1:
            time.sleep(5)
    return False


# Backward-compat alias (callers that haven't migrated yet).
_browser_login = browser_login
_validate_widget_terminal_state_public = _validate_widget_terminal_state


# When True (the default), browser_measure_navigation runs a below-the-fold
# scroll pass at the END of the filmed nav so the per-cell .webm captures the
# north-star views (dashboard piechart+table, /compositions panel cards) and
# not just the above-the-fold header. The pass runs AFTER the /call waterfall +
# navigation-timing reads, so it never perturbs the latency capture. Set to
# False to skip (e.g. a future pure-latency mode); recording cells still work
# either way because the scroll is best-effort and never raises.
SCROLL_CAPTURE_FOR_VIDEO = True


# Chrome-exclusion ancestor selector shared with browser_measure_stage's VERIFY
# scroll (browser.py ~1774 / ~1940). A text node matching a widget title that
# lives inside the left nav / sidebar / menu is a MENU ITEM, not the on-page
# widget, so we skip it and keep walking. Single source of truth for the idiom.
_VIDEO_SCROLL_CHROME_EXCLUDE = (
    'nav, [class*="sidebar"], [class*="menu"], [class*="sider"]')


# Dashboard final-hold script (Task #307): after the stepped descent, anchor
# the camera on the COMPOSITIONS donut (the north-star widget), NOT the first
# chart in document order (the Blueprints donut — the original bug). Strategy
# is structural — no magic pixel offsets:
#   (a) the on-page (non-chrome) "Compositions"/"Composition" label's nearest
#       chart/canvas — that is the Compositions donut;
#   (b) else the LAST chart in document order (Compositions renders below
#       Blueprints, so the last donut is the Compositions one);
#   (c) else the page bottom (scrollHeight) so the lowest widget is framed —
#       never re-centre the first/Blueprints chart, which was the bug.
# Module constant so tests assert the final hold structurally (the last
# evaluate IS this script) instead of grepping source tokens.
_DASHBOARD_FINAL_HOLD_JS = """(chromeExclude) => {
    const inChrome = (el) =>
        !!(el && el.closest && el.closest(chromeExclude));
    const chartSel =
        'canvas, [class*="chart"], [class*="pie"], [class*="g2"]';
    // (a) label-anchored Compositions donut.
    let target = null;
    const walker = document.createTreeWalker(
        document.body, NodeFilter.SHOW_TEXT, null);
    while (walker.nextNode()) {
        const t = walker.currentNode.textContent.trim();
        if (t !== 'Compositions' && t !== 'Composition') continue;
        let el = walker.currentNode.parentElement;
        if (!el || inChrome(el)) continue;
        // Climb to the widget container, then find its chart/canvas.
        let scope = el;
        for (let i = 0; i < 6 && scope.parentElement; i++) {
            const c = scope.querySelector(chartSel);
            if (c && !inChrome(c)) { target = c; break; }
            scope = scope.parentElement;
        }
        if (target) break;
    }
    // (b) else the LAST chart/canvas in document order.
    if (!target) {
        const charts = [...document.querySelectorAll(chartSel)]
            .filter((el) => !inChrome(el));
        if (charts.length) target = charts[charts.length - 1];
    }
    if (target) {
        target.scrollIntoView({ block: 'center', behavior: 'instant' });
    } else {
        // (c) page bottom — frames the lowest (Compositions) widget.
        window.scrollTo(
            { top: document.body.scrollHeight, behavior: 'instant' });
    }
}"""


def _scroll_capture_for_video(page, page_path):
    """Scroll the page through its content so Playwright films below-the-fold.

    The filmed nav (browser_measure_navigation) otherwise leaves the page at
    scroll-top, so the per-cell .webm shows only the header — never the
    compositions piechart + table (dashboard) or the panel cards
    (/compositions). This pass reveals those north-star views on camera.

    Ordering contract: the caller invokes this AFTER the /call waterfall read
    and the navigation-timing read, and BEFORE `return`. Scrolling mutates
    layout/scroll position but does NOT add or alter `resource`/`navigation`
    PerformanceEntry timings already captured, so the /call latency numbers
    are unaffected (verified against the read at browser.py ~1451/~1506).

    Behaviour by page:
      - "/" or "/dashboard": reveal the compositions piechart widget AND the
        compositions table, then hold on that region. Falls back to a stepped
        window.scrollTo descent with pauses, ending at scrollHeight, when the
        widget mounts can't be located.
      - "/compositions": stepped scroll down the datagrid so multiple panel
        cards render on camera.
      - anything else: a generic stepped descent (still useful footage).

    Robustness: does NOT depend on `data-widget-renderer` (the frontend does
    not emit it — confirmed). Uses the shared chrome-exclusion idiom to avoid
    scrolling to a left-nav menu item that happens to share a widget's title
    text. Entirely best-effort: every step is wrapped so it never raises.

    Returns the number of scroll "steps" actually performed (0 when disabled
    or when the page object can't be scrolled), purely for test assertions.
    """
    if not SCROLL_CAPTURE_FOR_VIDEO:
        return 0

    # Normalise: treat the SPA root and explicit /dashboard the same.
    pp = (page_path or "").rstrip("/") or "/"
    is_dashboard = pp in ("/", "/dashboard")
    is_compositions = pp.startswith("/compositions")

    steps = 0

    def _eval(js, *args):
        nonlocal steps
        try:
            page.evaluate(js, *args)
            steps += 1
        except Exception:
            pass

    def _pause(ms):
        try:
            page.wait_for_timeout(ms)
        except Exception:
            pass

    if is_dashboard:
        # 1) Try to scroll the compositions piechart + table into view. We
        #    locate widget mounts by visible label text ("Compositions",
        #    "Composition") and by Ant's chart/table container classes,
        #    EXCLUDING any candidate that sits inside the left nav/sidebar/menu
        #    (those are navigation entries, not the on-page widgets).
        #    Wrapped via _eval so a mid-scroll page error never escapes.
        _eval(
            """(chromeExclude) => {
                const inChrome = (el) =>
                    !!(el && el.closest && el.closest(chromeExclude));
                // Candidate widget mounts: pie/chart canvases + Ant tables.
                const widgetSel = [
                    'canvas',
                    '[class*="g2"]', '[class*="chart"]', '[class*="Chart"]',
                    '[class*="pie"]', '[class*="Pie"]',
                    '.ant-table', '[class*="table"]', '[class*="Table"]',
                ].join(',');
                let pieEl = null, tableEl = null;
                for (const el of document.querySelectorAll(widgetSel)) {
                    if (inChrome(el)) continue;
                    const cls = (el.className || '').toString().toLowerCase();
                    const tag = el.tagName.toLowerCase();
                    if (!tableEl && (cls.includes('table') ||
                                     el.matches('.ant-table'))) {
                        tableEl = el;
                    } else if (!pieEl && (tag === 'canvas' ||
                               cls.includes('chart') || cls.includes('pie') ||
                               cls.includes('g2'))) {
                        pieEl = el;
                    }
                }
                // Also try a label-text anchor for the compositions widget,
                // skipping left-nav menu items via the chrome exclusion.
                if (!pieEl && !tableEl) {
                    const walker = document.createTreeWalker(
                        document.body, NodeFilter.SHOW_TEXT, null);
                    while (walker.nextNode()) {
                        const node = walker.currentNode;
                        const t = node.textContent.trim();
                        if (t === 'Compositions' || t === 'Composition') {
                            const el = node.parentElement;
                            if (el && !inChrome(el)) { pieEl = el; break; }
                        }
                    }
                }
                const target = pieEl || tableEl;
                if (target) {
                    target.scrollIntoView(
                        { block: 'center', behavior: 'instant' });
                    return true;
                }
                return false;
            }""",
            _VIDEO_SCROLL_CHROME_EXCLUDE,
        )
        _pause(700)
        # 2) Whether or not we found the mounts, do a stepped descent so the
        #    camera pans across the whole below-the-fold region (covers the
        #    piechart AND the table even if only one mount was located).
        for frac in (0.33, 0.66, 1.0):
            _eval(
                "(f) => window.scrollTo({ top: "
                "document.body.scrollHeight * f, behavior: 'instant' })",
                frac)
            _pause(650)
        # 3) End by holding on the COMPOSITIONS donut (Task #307) — see the
        #    _DASHBOARD_FINAL_HOLD_JS constant for the anchor strategy.
        _eval(_DASHBOARD_FINAL_HOLD_JS, _VIDEO_SCROLL_CHROME_EXCLUDE)
        _pause(800)

    elif is_compositions:
        # Stepped descent through the datagrid so multiple panel cards render
        # on camera (the cards lazy-mount as they enter the viewport).
        for frac in (0.25, 0.5, 0.75, 1.0):
            _eval(
                "(f) => window.scrollTo({ top: "
                "document.body.scrollHeight * f, behavior: 'instant' })",
                frac)
            _pause(700)

    else:
        # Generic page: a short stepped descent still yields useful footage.
        for frac in (0.5, 1.0):
            _eval(
                "(f) => window.scrollTo({ top: "
                "document.body.scrollHeight * f, behavior: 'instant' })",
                frac)
            _pause(600)

    return steps


def browser_measure_navigation(page, page_path, label, min_calls=0,
                               user="admin", token=None, deleted_ns=None):
    """Navigate to a page; measure the /call API waterfall + widget gates.

    Args:
        min_calls: Minimum number of /call requests to wait for before
            declaring stability. Set from the COLD navigation's call count
            so WARM navigations don't exit early.
        user: Subject the page is logged in as. Forwarded to
            _validate_widget_terminal_state for per-user EXPECTED_CALLS.
        token: Bearer JWT for the same user. Forwarded to
            _validate_widget_terminal_state so the N-aware EXPECTED_CALLS
            formula (Task #250 Block 2) can source the per-user visible
            composition count for /compositions navs. None reproduces the
            pre-Block-2 BASE-only lookup (backward-compatible).

    Returns a dict with `waterfallMs`, `callCount`, `loadComplete`,
    `levels`, `httpOk/httpErr`, `validation`, and `incomplete` (True when
    the sample is unusable per widget-terminal-state or min_calls floor).
    """
    if FRONTEND is None:
        _log(f"    browser: FRONTEND_URL not set — skipping {label}")
        return {"label": label, "incomplete": True, "callCount": 0,
                "waterfallMs": 0, "loadComplete": 0, "domContentLoaded": 0,
                "calls": [], "levels": [], "httpOk": 0, "httpErr": 0,
                "validation": {"terminal_state": "fail"}}

    # Track /call HTTP response statuses via Playwright response listener.
    # Resource Timing API doesn't expose status codes, so we capture them here.
    _call_statuses = []

    def _on_response(response):
        if "/call" in response.url:
            _call_statuses.append({"url": response.url, "status": response.status})

    page.on("response", _on_response)

    # Clear previous performance entries and expand the resource timing buffer.
    # The default buffer (250 entries) includes ALL resources; at high
    # cardinality late /call entries are silently dropped.
    page.evaluate("""() => {
        performance.clearResourceTimings();
        performance.setResourceTimingBufferSize(2000);
    }""")

    # Navigate — use domcontentloaded instead of networkidle to avoid hanging
    # when the page has persistent connections (SSE, websockets, long-poll).
    try:
        page.goto(f"{FRONTEND}{page_path}", wait_until="domcontentloaded",
                  timeout=60_000)
    except Exception as e:
        _log(f"    WARNING: page.goto timeout ({e}), continuing with "
             f"stability poll")

    if "/login" in page.url:
        _log(f"    WARNING: redirected to login at {page.url}")

    # Wait for /call requests to stabilize.
    _stable_streak = 0
    _prev_count = -1
    for _ in range(120):
        _cur_count = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
        if _cur_count == _prev_count and _cur_count > 0 and _cur_count >= min_calls:
            _stable_streak += 1
            if _stable_streak >= 2:
                break
        else:
            _stable_streak = 0
        _prev_count = _cur_count
        page.wait_for_timeout(1000)

    validation = _validate_widget_terminal_state(page, page_path, label,
                                                 user=user, token=token,
                                                 deleted_ns=deleted_ns,
                                                 call_statuses=_call_statuses)

    # Measure /call waterfall + cluster calls into progressive-rendering levels.
    result = page.evaluate("""() => {
        const GAP_MS = 150;
        const entries = performance.getEntriesByType('resource');
        const calls = entries
            .filter(e => e.name.includes('/call'))
            .sort((a, b) => a.startTime - b.startTime);
        if (calls.length === 0) return { callCount: 0, waterfallMs: 0, calls: [], levels: [] };
        const first = calls[0].startTime;
        const last = Math.max(...calls.map(c => c.startTime + c.duration));
        const callsRel = calls.map(c => ({
            name: new URL(c.name).searchParams.get('name') || c.name.split('/').pop(),
            duration: Math.round(c.duration),
            startTime: Math.round(c.startTime - first),
            endTime: Math.round(c.startTime + c.duration - first),
        }));
        const levels = [];
        let cur = null;
        let curMaxEnd = -Infinity;
        for (const c of callsRel) {
            if (cur === null || c.startTime > curMaxEnd + GAP_MS) {
                if (cur !== null) levels.push(cur);
                cur = { level: levels.length + 1, count: 0, startMs: c.startTime,
                        endMs: c.endTime, names: {} };
                curMaxEnd = c.endTime;
            }
            cur.count += 1;
            if (c.endTime > cur.endMs) cur.endMs = c.endTime;
            if (c.endTime > curMaxEnd) curMaxEnd = c.endTime;
            cur.names[c.name] = (cur.names[c.name] || 0) + 1;
        }
        if (cur !== null) levels.push(cur);
        for (const lv of levels) {
            lv.durationMs = lv.endMs - lv.startMs;
            lv.topNames = Object.entries(lv.names)
                .sort((a, b) => b[1] - a[1])
                .slice(0, 3)
                .map(([n, k]) => `${n}×${k}`);
            delete lv.names;
        }
        return {
            callCount: calls.length,
            waterfallMs: Math.round(last - first),
            calls: callsRel.map(c => ({ name: c.name, duration: c.duration, startTime: c.startTime })),
            levels: levels,
        };
    }""")

    if min_calls > 0 and result.get("callCount", 0) < min_calls:
        result["waterfallMs"] = 0
        result["incomplete"] = True

    if validation["terminal_state"] != "pass":
        result["waterfallMs"] = 0
        result["incomplete"] = True

    nav = page.evaluate("""() => {
        const t = performance.getEntriesByType('navigation')[0];
        if (!t) return {};
        return {
            domContentLoaded: Math.round(t.domContentLoadedEventEnd - t.startTime),
            loadComplete: Math.round(t.loadEventEnd - t.startTime),
        };
    }""")

    try:
        page.remove_listener("response", _on_response)
    except Exception:
        pass

    # Below-the-fold scroll for the per-cell video. Runs LAST — after the
    # /call waterfall read (~1451) and the navigation-timing read (~1506) —
    # so it never perturbs the latency capture. Best-effort; never raises.
    _scroll_capture_for_video(page, page_path)

    ok_count = sum(1 for s in _call_statuses if s["status"] == 200)
    err_count = len(_call_statuses) - ok_count

    return {
        "label": label,
        "callCount": result.get("callCount", 0),
        "waterfallMs": result.get("waterfallMs", 0),
        "domContentLoaded": (nav or {}).get("domContentLoaded", 0),
        "loadComplete": (nav or {}).get("loadComplete", 0),
        "calls": result.get("calls", []),
        "levels": result.get("levels", []),
        "httpOk": ok_count,
        "httpErr": err_count,
        "validation": validation,
        "incomplete": result.get("incomplete", False),
    }


# Backward-compat alias.
_browser_measure_navigation = browser_measure_navigation


def _browser_wait_for_call_stability(page, min_calls=0, timeout_s=120):
    """Wait for /call XHR requests to stabilize after a page navigation.

    DEPRECATED — kept for the legacy Phase 4 entry point. The production
    path is `browser_measure_navigation`, which embeds an inline stability
    loop with the same shape (3-consecutive-identical-counts + min_calls
    floor). This helper exists so callers in the worktree source that
    haven't migrated yet still resolve.

    Returns the final /call count observed.
    """
    stable_streak = 0
    prev_count = -1
    for _ in range(timeout_s):
        cur_count = page.evaluate(
            "() => performance.getEntriesByType('resource')"
            ".filter(e => e.name.includes('/call')).length")
        if cur_count == prev_count and cur_count > 0 and cur_count >= min_calls:
            stable_streak += 1
            if stable_streak >= 2:
                break
        else:
            stable_streak = 0
        prev_count = cur_count
        page.wait_for_timeout(1000)
    return prev_count if prev_count >= 0 else 0


# ─── Composition-count verifiers (used by VERIFY poll) ──────────────────────


def verify_composition_count_api(token):
    """Verify composition count by calling the piechart widget API.

    Calls dashboard-compositions-panel-row-piechart and reads
    `status.widgetData.title` (a string like "1200"). This is exactly
    what the dashboard piechart displays.

    Returns the observed count or -1 if verification was not possible.
    """
    try:
        _ms, code, body = http_get_json(
            '/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1'
            '&resource=piecharts'
            '&name=dashboard-compositions-panel-row-piechart'
            '&namespace=krateo-system',
            token,
        )
        if code != 200 or not isinstance(body, dict):
            return -1
        title = (body.get("status") or {}).get("widgetData", {}).get("title")
        if title is None:
            return -1
        return int(title)
    except Exception:
        return -1


# Backward-compat alias.
_verify_composition_count_api = verify_composition_count_api


def verify_composition_count_ui(page):
    """Verify composition count by fetching the piechart widget from the
    browser context.

    The piechart is rendered on a Canvas element, so the count cannot be
    read from the DOM. Instead, we fetch the piechart widget endpoint
    from the browser context using the browser's own auth token
    (localStorage K_user). This verifies the same data path the UI uses:
    browser -> snowplow -> cache.

    Returns the observed count or -1 if verification was not possible.
    """
    try:
        result = page.evaluate("""(snowplowUrl) => {
            return (async () => {
                try {
                    const userData = JSON.parse(localStorage.getItem('K_user') || '{}');
                    const token = userData.accessToken;
                    if (!token) return -1;
                    const resp = await fetch(
                        snowplowUrl + '/call?apiVersion=widgets.templates.krateo.io'
                        + '%2Fv1beta1&resource=piecharts'
                        + '&name=dashboard-compositions-panel-row-piechart'
                        + '&namespace=krateo-system',
                        { headers: { 'Authorization': 'Bearer ' + token } }
                    );
                    if (resp.status !== 200) return -1;
                    const body = await resp.json();
                    const title = (body.status || {}).widgetData?.title;
                    if (title === undefined || title === null) return -1;
                    return parseInt(title, 10);
                } catch (e) { return -1; }
            })();
        }""", SNOWPLOW)
        return result
    except Exception:
        return -1


# Backward-compat alias.
_verify_composition_count_ui = verify_composition_count_ui


def _poll_piechart_progression(token, stop_event, interval=5):
    """Background-thread helper: poll piechart API during S6 deploy+
    stabilize to log value progression.

    Logs each sample with elapsed time, piechart value (from cache), and
    cluster truth (kubectl count_compositions). Logs only on value change
    or every 30s to avoid noise.
    """
    start = time.time()
    sample = 0
    prev_api = None
    while not stop_event.is_set():
        sample += 1
        api_count = verify_composition_count_api(token) if token else -1
        cluster_count = _count_compositions()
        elapsed_s = int(time.time() - start)
        if api_count != prev_api or sample == 1 or elapsed_s % 30 < interval:
            api_str = f"{api_count}" if api_count >= 0 else "?"
            _log(f"    S6 PIECHART t={elapsed_s:>4d}s  piechart={api_str}  "
                 f"cluster={cluster_count}")
            prev_api = api_count
        stop_event.wait(interval)


# ─── Browser measure stage (the sole convergence-poll site) ─────────────────


def browser_measure_stage(page, stage_num, stage_desc, cache_mode,
                          token=None, num_navs=3, user="admin",
                          verify_against_cluster=True,
                          verify_timeout: int = 300,
                          verify_interval: int = 3,
                          screenshots_dir: Path | None = None,
                          deleted_ns=None,
                          page_slots: dict | None = None):
    """Navigate browser to each page num_navs times, return timing data.

    Expects an already-logged-in page object and an admin JWT token.

    `user` tags every emitted navigation dict so the canonical-row
    exporter can bucket samples per cell (admin/cyberjoker × ON/OFF).
    The page must already be logged in as `user`; this argument is
    metadata only.

    `verify_against_cluster` controls the S6 piechart convergence check.
    For admin (cluster-wide RBAC) the piechart value must converge to
    the cluster's total composition count; for cyberjoker (RBAC scoped
    to a single namespace) the piechart will legitimately show fewer,
    so we fall back to an intra-user UI-vs-API consistency check
    (api_count == ui_count).

    On VERIFY-poll deadline expiry with `matched=False`, this function
    RAISES `ConvergenceTimeout(stage, user, api, ui, cluster, timeout_secs)`
    rather than silently logging TIMEOUT/MISMATCH (worktree source
    6465-6475's silent-skip bug). Block 4's stage runner catches it,
    persists a stage proof with passed=False + convergence_timeout=true,
    then re-raises to abort the run with exit 4.

    `page_slots` (Task #267 FIX 2 + #307 ArchLazyDash + #310b — film both
    pages, lazily): ONE slot map {page_name: slot} the recording path passes
    (the same map `phases._open_stage_recording_pages` returns). Each `slot`
    is a dict carrying BOTH the routing sentinel and the shadow factory that
    used to live in the parallel `pages_by_name` / `page_factories` dicts:

      slot["page"] — the routing sentinel: None means "lazy, no live page
                     yet" (route into the factory); a live page object means
                     "measure on this page directly".
      slot["make"] — a zero-arg factory invoked at ITS page's loop iteration
                     below; it creates that page's dedicated recording
                     BrowserContext (Playwright records exactly one .webm per
                     context) and writes the materialised page back into the
                     SAME slot — so the dashboard context's video contains
                     only dashboard navs and the compositions context's only
                     compositions navs, and neither context (nor its video
                     clock) exists before its own nav.

    When `page_slots` is None (every unit test's no-video path) OR a given
    page_name has no slot, the single `page` argument is used for that
    page_name, exactly as before. The VERIFY/convergence/content block runs
    in the Dashboard branch on the materialised page, so its logic is
    untouched. #310b collapsed the prior two parallel dicts (pages_by_name +
    page_factories, keyed identically by page_name) into this one slot map —
    same routing + lazy-materialise semantics, one structure.
    """
    ns_count, comp_count = _count_bench_ns(), _count_compositions()
    _log(f"Cluster: {ns_count} bench ns, {comp_count} compositions")

    if screenshots_dir is None:
        screenshots_dir = Path(__file__).parent / "screenshots"
    else:
        screenshots_dir = Path(screenshots_dir)

    pages_data = {}
    for page_name, page_path in BROWSER_SCALING_PAGES:
        # #310b: ONE slot map. Recording path supplies a slot per page whose
        # `page` is the routing sentinel (None → lazy) and whose `make` is the
        # shadow factory. No-video path: page_slots is None (or this page has
        # no slot) → the shared `page` is used.
        slot = (page_slots or {}).get(page_name)
        nav_page = slot.get("page") if slot else page

        # Task #307 / ArchLazyDash: a LAZY page (now EVERY page, incl. the
        # Dashboard) has nav_page None here and a `make` factory in its slot.
        # Materialise it NOW — at this page's loop iteration, which is the FIRST
        # statement of the iteration body (before the per-page nav loop and the
        # Dashboard VERIFY/convergence block below). So each page's recording
        # context — and thus its video clock — does not exist until ITS nav
        # begins: the Dashboard `.webm` starts at the /dashboard nav rather than
        # filming the in-frame count_compositions() header probe + cold-nav poll
        # idle, and the Compositions context still materialises strictly AFTER
        # the Dashboard VERIFY poll (falsifiers F-B / F-B′). The factory is
        # best-effort; on failure we fall back to the shared `page` so the
        # stage still measures (no recording for that page).
        if nav_page is None and slot and callable(slot.get("make")):
            try:
                nav_page = slot["make"]()
            except Exception as e:
                _log(f"    WARNING: lazy recording-context create for "
                     f"{page_name} failed ({type(e).__name__}: {e}); "
                     f"measuring on shared page")
                nav_page = page
        if nav_page is None:
            nav_page = page

        navs = []
        cold_calls = 0
        for nav_num in range(1, num_navs + 1):
            m = browser_measure_navigation(
                nav_page, page_path,
                f"S{stage_num} {cache_mode} nav#{nav_num} {page_name}",
                min_calls=cold_calls,
                user=user, token=token, deleted_ns=deleted_ns)
            cold_warm = 'COLD' if nav_num == 1 else 'WARM'
            m["nav_num"] = nav_num
            m["cold_warm"] = cold_warm
            m["user"] = user
            navs.append(m)
            if nav_num == 1:
                cold_calls = m["callCount"]
            http_info = (f"  http={m['httpOk']}ok"
                         if m.get("httpOk", 0) + m.get("httpErr", 0) > 0
                         else "")
            if m.get("httpErr", 0) > 0:
                http_info += f"/{m['httpErr']}err"
            _log(f"  {cold_warm} {page_name:<15s} "
                 f"waterfall={m['waterfallMs']:>5d}ms  "
                 f"load={m['loadComplete']:>5d}ms  "
                 f"calls={m['callCount']}{http_info}")

            if page_name == "Dashboard" and nav_num == num_navs:
                screenshots_dir.mkdir(parents=True, exist_ok=True)

                verify_start = time.time()
                deadline = verify_start + verify_timeout
                api_count = -1
                ui_count = -1
                fresh_comp_count = _count_compositions()
                last_cluster_check = time.time()
                matched = False

                # Task #334a (report-only): snapshot the refresher-completed
                # expvar + an RFC3339 window-open stamp. Window-close snapshot
                # + pod-log fetch happen after convergence below; the assembled
                # discrimination tuple is attached to `m` alongside
                # convergence_ms. None-safe (cache-off / unreadable expvar).
                conv_disc_since_iso = time.strftime(
                    "%Y-%m-%dT%H:%M:%SZ", time.gmtime(verify_start))
                conv_disc_refresher_before = read_snowplow_expvar_int(
                    "snowplow_refresher_completed_total")

                poll_num = 0
                while time.time() < deadline:
                    poll_num += 1
                    if time.time() - last_cluster_check > 60:
                        fresh_comp_count = _count_compositions()
                        last_cluster_check = time.time()
                    api_count = (verify_composition_count_api(token)
                                 if token else -1)
                    ui_count = verify_composition_count_ui(nav_page)
                    elapsed_ms = int((time.time() - verify_start) * 1000)
                    api_str_p = f"{api_count}" if api_count >= 0 else "?"
                    ui_str_p = f"{ui_count}" if ui_count >= 0 else "?"
                    _log(f"    VERIFY poll {poll_num}: api={api_str_p} "
                         f"ui={ui_str_p} cluster={fresh_comp_count} ({elapsed_ms}ms)")

                    try:
                        nav_page.evaluate("""() => {
                            const walker = document.createTreeWalker(
                                document.body, NodeFilter.SHOW_TEXT, null);
                            let target = null;
                            while (walker.nextNode()) {
                                const node = walker.currentNode;
                                if (node.textContent.trim() === 'Compositions') {
                                    const el = node.parentElement;
                                    if (el.closest('nav, [class*="sidebar"], [class*="menu"], [class*="sider"]'))
                                        continue;
                                    target = el;
                                }
                            }
                            if (target) {
                                target.scrollIntoView({ block: 'start', behavior: 'instant' });
                                return true;
                            }
                            window.scrollTo(0, document.body.scrollHeight);
                            return false;
                        }""")
                        nav_page.wait_for_timeout(500)
                    except Exception:
                        pass
                    if SCREENSHOTS:
                        ss_name = (f"S{stage_num}_{cache_mode}_{user}_poll"
                                   f"{poll_num}_api{api_str_p}_ui{ui_str_p}_"
                                   f"cluster{fresh_comp_count}.png")
                        try:
                            nav_page.screenshot(path=str(screenshots_dir / ss_name))
                            _log(f"    screenshot: {ss_name}")
                        except Exception as e:
                            _log(f"    screenshot failed: {e}")

                    if verify_against_cluster:
                        api_ok = (api_count >= 0 and api_count == fresh_comp_count)
                        ui_ok = (ui_count >= 0 and ui_count == fresh_comp_count)
                        if api_ok and ui_ok:
                            matched = True
                            break
                    else:
                        if (api_count >= 0 and ui_count >= 0
                                and api_count == ui_count):
                            matched = True
                            break
                    time.sleep(verify_interval)

                convergence_ms = int((time.time() - verify_start) * 1000)
                m["verified_api"] = api_count
                m["verified_ui"] = ui_count
                m["convergence_ms"] = convergence_ms if matched else -1
                api_str = f"{api_count}" if api_count >= 0 else "?"
                ui_str = f"{ui_count}" if ui_count >= 0 else "?"

                # Task #334a (report-only): window-close refresher snapshot +
                # one bounded pod-log fetch over [verify_start, now]; assemble
                # the discrimination tuple and attach it to the nav dict
                # alongside convergence_ms. Sibling key — NOT read by
                # convergence_p99_for_stage / compute_verdict. Best-effort: any
                # failure leaves None fields (it never aborts the stage).
                try:
                    conv_disc_refresher_after = read_snowplow_expvar_int(
                        "snowplow_refresher_completed_total")
                    conv_disc_log = _snowplow_pod_log_window(
                        since_iso=conv_disc_since_iso)
                    m["conv_discrimination"] = capture_conv_discrimination(
                        conv_disc_since_iso,
                        max(time.time() - verify_start, 0.0),
                        conv_disc_refresher_before,
                        conv_disc_refresher_after,
                        log_text=conv_disc_log)
                except Exception as e:
                    _log(f"    conv_discrimination capture failed "
                         f"({type(e).__name__}: {e}); recording None tuple")
                    m["conv_discrimination"] = capture_conv_discrimination(
                        None, None, conv_disc_refresher_before, None,
                        log_text=None)

                if not matched:
                    # ConvergenceTimeout — replaces the source-script's
                    # silent TIMEOUT/MISMATCH log at worktree line
                    # 6465-6475. Block 4's stage runner catches this,
                    # writes a stage proof with passed=False, and
                    # re-raises to abort the run with exit 4.
                    _log(f"    VERIFY ✗ api={api_str} ui={ui_str} "
                         f"cluster={fresh_comp_count} converged=TIMEOUT — "
                         f"raising ConvergenceTimeout")
                    raise ConvergenceTimeout(
                        stage=stage_num,
                        user=user,
                        api=api_count,
                        ui=ui_count,
                        cluster=fresh_comp_count,
                        timeout_secs=verify_timeout,
                    )

                _log(f"    VERIFY ✓ api={api_str} ui={ui_str} "
                     f"cluster={fresh_comp_count} converged={convergence_ms}ms")

                # Content-level correctness check: compare composition NAMES,
                # not just counts. Catches silent cache corruption where the
                # count matches but the cached items are the wrong set
                # (stale entries present, recent deletes missing, etc.).
                #
                # RBAC-aware (task #181): only admin (verify_against_cluster
                # = True) can compare its cached view against cluster-truth
                # via `_list_composition_names()`, which returns ALL
                # compositions cluster-wide. Doing the same diff for cj
                # always produces a spurious "missing=N" because cj is
                # RBAC-scoped to a subset of namespaces (~1000/54500 at
                # SCALE=50K) — cluster-truth is not cj's authoritative
                # truth. For cj we instead consistency-check the cache
                # against the same VERIFY-poll api_count/ui_count that
                # already passed above (intra-user truth, mirrors VERIFY's
                # `verify_against_cluster=False` branch at line 1326-1330).
                #
                # Mirrors `verify_against_cluster` from the surrounding
                # VERIFY block — single source of truth for the
                # "admin compares vs cluster, cj compares vs self"
                # convention.
                if cache_mode == "ON" and token:
                    cached = list_composition_names_from_cache(token)
                    if verify_against_cluster:
                        truth = _list_composition_names()
                        if truth is not None and cached is not None:
                            if truth == cached:
                                _log(f"    CONTENT ✓ {len(truth)} composition "
                                     f"names match (vs cluster-truth)")
                                m["content_match"] = True
                                m["content_truth_source"] = "cluster"
                            else:
                                missing = truth - cached
                                extra = cached - truth
                                _log(f"    CONTENT ✗ DRIFT — "
                                     f"missing={len(missing)} extra={len(extra)}")
                                if missing:
                                    _log(f"      missing: {sorted(missing)[:3]}...")
                                if extra:
                                    _log(f"      extra: {sorted(extra)[:3]}...")
                                m["content_match"] = False
                                m["content_truth_source"] = "cluster"
                                m["content_missing"] = len(missing)
                                m["content_extra"] = len(extra)
                    else:
                        # RBAC-scoped user (e.g. cj): the intra-user CONTENT
                        # comparison is STRUCTURALLY MISMATCHED and is here
                        # DEMOTED to an explicitly-labeled, non-load-bearing
                        # DIAGNOSTIC (Task #298, trace
                        # docs/task-296-298-s10-and-content-trace-2026-06-10.md
                        # TRACE 2 §"cj intra-user branch").
                        #
                        # WHY: `cached` comes from
                        # list_composition_names_from_cache(token), which
                        # reads the CLUSTER-WIDE `compositions-list`
                        # RESTAction. cj has no cluster-scoped LIST
                        # permission (narrow RoleBinding), so that list is
                        # LEGITIMATELY ~0 for cj — whereas `api_count` is the
                        # RBAC-NARROWED piechart aggregate (e.g. 999). Comparing
                        # cached_count(0) == api_count(999) is therefore
                        # PERMANENTLY false: it never reflected a real
                        # correctness signal, only the structural mismatch
                        # between two different views. A proper RBAC-scoped
                        # name-list source is not reachable here in a small
                        # change (the piechart aggregates via a different
                        # widget path, not a flat name-list endpoint), so per
                        # the trace we record this as a diagnostic and DO NOT
                        # set content_match (which reads as a real check).
                        # The admin cluster-truth CONTENT check above remains
                        # the load-bearing CONTENT gate
                        # (feedback_validate_content_not_just_status).
                        if cached is not None:
                            cached_count = len(cached)
                            _log(f"    CONTENT [diagnostic, non-load-bearing] "
                                 f"cj cluster-wide compositions-list "
                                 f"cached_names={cached_count} vs RBAC-narrowed "
                                 f"api_count={api_count} ui_count={ui_count} "
                                 f"(structurally mismatched by design — Task "
                                 f"#298; NOT a CONTENT pass/fail)")
                            m["content_truth_source"] = "cj_diagnostic_non_load_bearing"
                            m["content_cj_cached_count"] = cached_count
                            m["content_cj_api_count"] = api_count
                            m["content_cj_ui_count"] = ui_count

                # Final VERIFY screenshot — reload dashboard for fresh data.
                try:
                    nav_page.goto(f"{FRONTEND}/dashboard",
                                  wait_until="networkidle", timeout=30000)
                    nav_page.wait_for_timeout(2000)
                except Exception:
                    pass
                try:
                    nav_page.evaluate("""() => {
                        const walker = document.createTreeWalker(
                            document.body, NodeFilter.SHOW_TEXT, null);
                        let target = null;
                        while (walker.nextNode()) {
                            const node = walker.currentNode;
                            if (node.textContent.trim() === 'Compositions') {
                                const el = node.parentElement;
                                if (el.closest('nav, [class*="sidebar"], [class*="menu"], [class*="sider"]'))
                                    continue;
                                target = el;
                            }
                        }
                        if (target) {
                            target.scrollIntoView({ block: 'start', behavior: 'instant' });
                            return true;
                        }
                        window.scrollTo(0, document.body.scrollHeight);
                        return false;
                    }""")
                    nav_page.wait_for_timeout(500)
                except Exception:
                    pass
                if SCREENSHOTS:
                    ss_final = (f"S{stage_num}_{cache_mode}_{user}_VERIFY_PASS_"
                                f"api{api_str}_ui{ui_str}_{convergence_ms}ms.png")
                    try:
                        nav_page.screenshot(path=str(screenshots_dir / ss_final))
                        _log(f"    screenshot: {ss_final}")
                    except Exception as e:
                        _log(f"    screenshot failed: {e}")

        wf_vals = [n["waterfallMs"] for n in navs if n["waterfallMs"] > 0]
        lc_vals = [n["loadComplete"] for n in navs if n["loadComplete"] > 0]
        last_nav = navs[-1] if navs else {}
        pages_data[page_name] = {
            "navigations": navs,
            "waterfall_p50": _pct(wf_vals, 50) if wf_vals else 0,
            "waterfall_p90": _pct(wf_vals, 90) if wf_vals else 0,
            "waterfall_warm_last": last_nav.get("waterfallMs", 0),
            "loadComplete_p50": _pct(lc_vals, 50) if lc_vals else 0,
            "loadComplete_p90": _pct(lc_vals, 90) if lc_vals else 0,
            "callCount": navs[0]["callCount"] if navs else 0,
            "levels_warm_last": last_nav.get("levels", []),
            "levels_cold": (navs[0].get("levels", []) if navs else []),
        }

    return {
        "stage": stage_num, "desc": stage_desc, "cache": cache_mode,
        "bench_ns": ns_count, "compositions": comp_count,
        "pages": pages_data,
        # cache_supported defaults True for any actually-measured cell.
        # Block 4's stage runner overrides this to False for synthetic
        # N/A rows on stripped charts.
        "cache_supported": True,
    }


# Backward-compat alias.
_browser_measure_stage = browser_measure_stage
