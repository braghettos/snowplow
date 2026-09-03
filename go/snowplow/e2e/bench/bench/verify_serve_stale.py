"""bench verify-serve-stale — explicit serve-stale-while-refresh contract test.

Implements the design at
docs/bench-verify-serve-stale-design-2026-06-02.md (cache-architect)
under PM AUTHORIZE-with-tightening 2026-06-02 (Diego's
feedback_l1_hit_miss_via_metrics_not_timing +
feedback_mutation_serves_stale_while_refresh).

The harness fires N UNSCORED warm-up probes (#159 — so the first scored
probe never eats a cold-fill miss), then THREE scored probes around a
single mutation of a BENCH-OWNED fixture's echo field:

    (warm-up × N)  (unscored; fill the cold per-cohort L1 cell)
    T-1s   probe   (steady-state, pre-mutation; body carries setup_marker)
    MUTATE         (patch the fixture echo field -> mutate_marker)
    T+50ms probe   (refresh-in-flight: cache MUST serve STALE — i.e. the
                    body still carries setup_marker, NOT mutate_marker)
    T+10s  probe   (post-refresh: cache MUST carry mutate_marker;
                    widened from 5s for the #318-R1a refresher rate-floor)

#159 DEFECT-2 fix: the default `widget-echo` target uses a bench-owned
`Button` whose `spec.widgetData.label` scalar echoes verbatim into the
served /call body (S8bc-proven). The prior `compositions-list` target
GET the RESTAction CR definition (~1537B) — a body that can NEVER carry
a composition mutation, so the marker assertion was structurally
always-FAIL. See _TARGET_REGISTRY for the live-confirmed shape evidence.

Two independent sources confirm L1 hit/miss verdict per probe:

    PRIMARY   — /debug/vars snowplow_dispatch_l1_lookups counters
                (miss_delta == 0 across all 3 probes is the load-bearing
                assertion).
    SECONDARY — kubectl logs deployment/snowplow filtered on the bench's
                client-supplied X-Krateo-TraceId — verifies each probe's
                `resolved_cache.lookup` log emits `hit:true`.

If PRIMARY and SECONDARY disagree → SOURCES_DISAGREE (snowplow defect).

Plus PM tightening (Option A): the harness verifies the informer
dirty-marked the patched object's L1 entry BEFORE the T+50ms probe fires
— a streaming kubectl logs tail scans `cache_event.consumed` lines for
the patched (gvr, ns, name). If absent at T+50ms → emit
INDETERMINATE_INFORMER_NOT_ACKED.

Output: JSON bundle at /tmp/snowplow-runs/<tag>/verify-serve-stale-<ts>.json
plus an ANSI-coloured stdout summary that mirrors cli.py:_setup_logger.

VERDICT MATRIX (#159 PM review C1, 2026-06-12 §1). THE CONTRACT is
"the customer never ate a cold miss AND freshness arrived by T+10s" — NOT
"the body was literally stale at +50ms". Serving fresh EARLY (the refresh
beat the probe RTT) is strictly-better, not a defect.

    0 — PASS: every probe HTTP 200 + informer ACK'd + mid offset in-window
        + miss_delta == 0 (no cold miss) + every probe a SECONDARY hit
        + T+10s carries the mutate_marker and differs from pre (bounded
        freshness). The bundle records mid_observation ∈ {served_stale,
        served_fresh_early} INFORMATIONALLY — it is NEVER gated on.
    1 — INDETERMINATE: window slip / informer not ACK'd / log filter
        unavailable / cache OFF / debug-vars unreachable / login / setup /
        patch failed; OR INDETERMINATE_MID_BODY_UNEXPECTED (mid body
        changed but carries NEITHER the pre snapshot NOR the new marker —
        a real "something else mutated the entry" anomaly, not a
        serve-stale FAIL).
    2 — FAIL: FAIL_HTTP_<code>_ON_<window> / FAIL_SYNC_COLD_FILL_ON_MID
        (miss_delta != 0 — THE serve-stale violation) / FAIL_SOURCES_DISAGREE
        / FAIL_REFRESH_NOT_COMPLETED_BY_10S (post lacks marker or == pre).
    3 — non-GKE context (inherits cli.py:_gke_context_guard).

(RETIRED #159 C1: FAIL_STALE_NOT_SERVED_ON_MID — the over-strict
literal-stale-at-+50ms mapping; its real-anomaly residue moved to
INDETERMINATE_MID_BODY_UNEXPECTED.)

Spec — design doc §3 Q1-Q6 + §4 LOC budget + §5 falsifier + §6 risks;
#159 PM review docs/ship-159-pm-review-2026-06-12.md §1 (verdict re-map).
"""

from __future__ import annotations

import hashlib
import json
import os
import queue
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

from bench import browser, cluster
from bench.cluster import NS, kubectl, kubectl_context_args

# ─── Constants ──────────────────────────────────────────────────────────────

# When the mid-probe overshoots this many ms past T_mutate we emit
# INDETERMINATE_MID_WINDOW_SLIPPED per design Q4 + risk register R2.
_MID_WINDOW_HARD_LIMIT_MS = 2000

# Default count of UNSCORED warm-up probes (#159 v1.1). Tunable via
# --warmup; one is enough to fill a cold per-cohort cell, but the
# default of 2 gives margin against a single transport hiccup.
_DEFAULT_WARMUP_PROBES = 2

# ─── Target abstraction (#159 DEFECT 2 fix — mistargeted marker) ──────────────
#
# THE DEFECT (live-confirmed against 0.30.261 on 2026-06-12): the prior
# `compositions-list` target GET'd
#   /call?…resource=restactions&name=compositions-list&namespace=krateo-system
# whose body is the RESTAction CR DEFINITION itself
#   {"kind":"RESTAction",…,"spec":{"api":[…],"filter":…}}   (1537 B)
# — the RA's OWN metadata/spec, NOT a serialized composition collection.
# A composition-annotation marker can therefore NEVER appear in that body,
# so the T+10s marker-presence assertion was structurally ALWAYS-FAIL.
# (The post-#300/C1 chart did NOT collapse the RA /call output to an
# aggregate; `restactions` /call serves the raw manifest.)
#
# THE FIX (Task #159 option (b) — the S8bc echo-field pattern, which the
# bench already proves works): a BENCH-OWNED `Button` widget with a plain
# `spec.widgetData.label` scalar and NO apiRef / widgetDataTemplate routes
# to the per-cohort `widgets` L1 cell — the exact cell a customer /call
# hits — and echoes that scalar VERBATIM into the served /call body
# (internal/resolvers/widgets/resolve.go:182-238;
#  phases.py:2195-2230 _widget_fixture_yaml prior art). A `kubectl patch`
# of `spec.widgetData.label` v1->v2 therefore changes the served body and
# gives a GUARANTEED-PRESENT, patchable marker. The dirty-mark flows
# through the recordWidgetDeps self-dep on the `buttons` GVR (the same
# path S8b exercises), so the informer-ACK gate fires on
# `cache_event.consumed gvr=widgets.templates.krateo.io/v1beta1,
#  Resource=buttons`.
#
# Each target is fully self-describing (path/cell_key/gvr_string +
# setup/mutate/cleanup hooks) so the harness body and verdict logic never
# special-case a target (feedback_no_special_cases). New targets are
# purely additive.

# Bench widget fixture — group/version/plural + the per-cohort `widgets`
# cell key. Mirrors phases.py:_WIDGET_FIXTURE_GVR so S8b and this verifier
# agree on the GVR shape.
_WIDGET_GROUP = "widgets.templates.krateo.io"
_WIDGET_VERSION = "v1beta1"
_WIDGET_PLURAL = "buttons"
_WIDGET_FIXTURE_NS = "bench-ns-01"
_WIDGET_FIXTURE_NAME = "bench-vss-widget-probe"

# expvar /debug/vars cell key — "<handlerKind>|<gvrString>" per
# dispatchers/l1_lookup_metrics.go. GVR.String() emits
# "<group>/<version>, Resource=<resource>" verbatim per apimachinery
# pkg/runtime/schema/group_version.go (the ", Resource=" prefix is
# load-bearing — the prior compositions cell key proved that shape).
_WIDGET_CELL_KEY = (
    f"widgets|{_WIDGET_GROUP}/{_WIDGET_VERSION}, Resource={_WIDGET_PLURAL}")

# GVR.String() shape for the informer-ACK watcher (matches deps.go's
# slog.String("gvr", gvr.String())).
_WIDGET_GVR_STRING = (
    f"{_WIDGET_GROUP}/{_WIDGET_VERSION}, Resource={_WIDGET_PLURAL}")


def _widget_call_path(name: str, ns: str) -> str:
    """Build the snowplow /call query path for a widget CR (mirrors
    phases.py:_call_url_for — apiVersion is `<group>/<version>`,
    url-encoded)."""
    api_version_enc = f"{_WIDGET_GROUP}/{_WIDGET_VERSION}".replace("/", "%2F")
    return (f"/call?apiVersion={api_version_enc}"
            f"&resource={_WIDGET_PLURAL}&name={name}&namespace={ns}")


_WIDGET_PROBE_PATH = _widget_call_path(_WIDGET_FIXTURE_NAME,
                                       _WIDGET_FIXTURE_NS)


def _widget_fixture_yaml(label: str) -> str:
    """Bench Button fixture: a plain widget whose `spec.widgetData.label`
    scalar echoes verbatim into the served /call body (the marker carrier).

    No apiRef / widgetDataTemplate → routes to the per-cohort `widgets`
    L1 cell and is NOT RBAC-sensitive (widgets.go dispatcher ->
    isRBACSensitiveApiRefWidget). Identical shape to
    phases.py:_widget_fixture_yaml (S8b prior art).
    """
    return f"""\
---
apiVersion: {_WIDGET_GROUP}/{_WIDGET_VERSION}
kind: Button
metadata:
  name: {_WIDGET_FIXTURE_NAME}
  namespace: {_WIDGET_FIXTURE_NS}
spec:
  widgetData:
    actions: {{}}
    clickActionId: nop
    type: text
    label: "{label}"
"""


def _widget_namespace_yaml() -> str:
    """A bare Namespace doc for the bench fixture namespace.

    Server-side-applied idempotently before the Button so the fixture has
    a home even on a freshly-drained cluster where the phase6 lifecycle
    (which normally creates bench-ns-*) has not run. Bench-owned and
    bench-named — the feedback_never_kubectl_apply "bench-internal kubectl
    is exempt" carve-out applies (same as the S8b/S8c fixtures).
    """
    return f"""\
---
apiVersion: v1
kind: Namespace
metadata:
  name: {_WIDGET_FIXTURE_NS}
"""


def _setup_widget_target(marker: str) -> tuple[str, str]:
    """Ensure the bench namespace, then create the bench Button fixture
    with `label=<marker>`.

    Returns (namespace, name) — the mutation coordinates. The fixture is
    applied with the marker ALREADY as its label so the pre/mid scored
    bodies carry it (steady state). The MUTATION (later) flips the label
    to a fresh marker so the post body must change. Raises _VerifyError on
    apply failure.

    The PRIME GET happens implicitly via the warm-up probes + the T-1s
    scored probe; we only need apply here.
    """
    ns_ok, ns_diag = cluster.k8s_apply_yaml(_widget_namespace_yaml())
    if not ns_ok:
        raise _VerifyError(
            f"widget fixture namespace apply failed: {ns_diag}")
    ok, diag = cluster.k8s_apply_yaml(_widget_fixture_yaml(marker))
    if not ok:
        raise _VerifyError(f"widget fixture apply failed: {diag}")
    return _WIDGET_FIXTURE_NS, _WIDGET_FIXTURE_NAME


def _mutate_widget_label(ns: str, name: str,
                         marker: str) -> tuple[int, float, str]:
    """Flip the bench Button's `spec.widgetData.label` to `<marker>`.

    Returns (rc, t_mutate_monotonic, stderr) — the uniform mutate-hook
    contract every target implements, so the harness body stays
    target-agnostic. rc=0 on success. t_mutate is captured AFTER the
    apiserver acknowledged the patch (k8s_patch_cr_spec is synchronous).
    """
    ok, diag = cluster.k8s_patch_cr_spec(
        _WIDGET_GROUP, _WIDGET_VERSION, _WIDGET_PLURAL, ns, name,
        {"widgetData": {"label": marker}})
    return (0 if ok else 1), time.monotonic(), ("" if ok else diag)


def _cleanup_widget_target(ns: str, name: str) -> bool:
    """Delete the bench Button fixture. Returns True on success/404."""
    return cluster.k8s_delete_custom(
        _WIDGET_GROUP, _WIDGET_VERSION, _WIDGET_PLURAL, ns, name)


# Targets enumerated by --target. `widget-echo` is the wired default — its
# /call body provably carries the patchable marker (live-confirmed). New
# targets are purely additive per feedback_no_special_cases; each supplies
# its own path/cell_key/gvr_string + setup/mutate/cleanup hooks.
_TARGET_REGISTRY = {
    "widget-echo": {
        "path": _WIDGET_PROBE_PATH,
        "cell_key": _WIDGET_CELL_KEY,
        "gvr_string": _WIDGET_GVR_STRING,
        "setup_fn": _setup_widget_target,
        "mutate_fn": _mutate_widget_label,
        "cleanup_fn": _cleanup_widget_target,
        # The marker carrier is the widget LABEL itself; the post-mutation
        # body must contain the NEW label and the pre/mid bodies the OLD.
        "marker_is_label": True,
    },
}


class _VerifyError(Exception):
    """Raised for hard-fail conditions that short-circuit the verdict."""


# ─── /debug/vars snapshot ───────────────────────────────────────────────────


def _snapshot_debug_vars(base_url: str | None = None,
                         timeout: int = 10) -> dict:
    """Fetch /debug/vars from snowplow and parse the JSON envelope.

    Returns a dict (the whole expvar map). Raises _VerifyError on
    transport failure or non-200.

    Used as the PRIMARY hit/miss source per design Q5-A — atomic
    counters bumped in emitResolvedCacheLookup (helpers.go:241-250).

    As of snowplow 1.12.3 (O-D3) /debug/vars is JWT-gated, so the GET
    carries the bearer browser._debug_auth_headers resolves; an anonymous
    read of a 1.12.3 pod is a 401 and raises _VerifyError, which the
    caller already maps to INDETERMINATE_DEBUG_VARS_UNREACHABLE.
    """
    url = (base_url or browser.SNOWPLOW) + "/debug/vars"
    req = urllib.request.Request(
        url, headers=browser._debug_auth_headers())
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            if r.status != 200:
                raise _VerifyError(
                    f"/debug/vars HTTP {r.status}")
            return json.loads(r.read())
    except urllib.error.URLError as e:
        raise _VerifyError(f"/debug/vars unreachable: {e}") from e


def _cell_delta(vars_before: dict, vars_after: dict,
                cell_key: str) -> tuple[int, int, bool]:
    """Compute (miss_delta, hit_delta, cell_key_inexact) from two
    /debug/vars snapshots for the given cell.

    Risk register R5 fallback: if the exact cell_key is not present
    (snowplow version may use a different separator), sum across all
    cells with the same handlerKind prefix. Operator sees the inexact
    flag in the JSON bundle.
    """
    map_before = (vars_before or {}).get("snowplow_dispatch_l1_lookups") or {}
    map_after = (vars_after or {}).get("snowplow_dispatch_l1_lookups") or {}

    inexact = False
    if cell_key in map_after or cell_key in map_before:
        before_cell = map_before.get(cell_key) or {}
        after_cell = map_after.get(cell_key) or {}
    else:
        # Fallback: sum across all "<handlerKind>|*" cells.
        handler_kind = cell_key.split("|", 1)[0]
        before_cell = {"hit_total": 0, "miss_total": 0}
        after_cell = {"hit_total": 0, "miss_total": 0}
        for k, v in map_before.items():
            if k.startswith(handler_kind + "|"):
                before_cell["hit_total"] += v.get("hit_total", 0) or 0
                before_cell["miss_total"] += v.get("miss_total", 0) or 0
        for k, v in map_after.items():
            if k.startswith(handler_kind + "|"):
                after_cell["hit_total"] += v.get("hit_total", 0) or 0
                after_cell["miss_total"] += v.get("miss_total", 0) or 0
        inexact = True

    miss_delta = int(after_cell.get("miss_total", 0) or 0) \
        - int(before_cell.get("miss_total", 0) or 0)
    hit_delta = int(after_cell.get("hit_total", 0) or 0) \
        - int(before_cell.get("hit_total", 0) or 0)
    return miss_delta, hit_delta, inexact


# ─── Probe primitive ────────────────────────────────────────────────────────


def _probe(target_path: str, token: str, trace_id: str,
           timeout: int = 30) -> dict:
    """Fire one /call request, return a probe-result dict.

    Returns:
        {
            "trace_id": "...",
            "http_ms": int,
            "code": int,
            "body_sha256": "...",  # SHA-256 of response bytes
            "marker_present": False,  # set later by harness
            "body_bytes": bytes,  # retained for marker scan
        }

    Per design Q3: direct urllib HTTP (no Playwright), bench-managed JWT,
    X-Krateo-TraceId header for log correlation.

    #159 DEFECT 1 fix (API DRIFT): the probe attaches the trace id via the
    `headers=` param of browser.http_get (added #159) under the
    plumbing-canonical header name `X-Krateo-TraceId`
    (plumbing v0.9.3 context/context.go:21 LabelKrateoTraceId; the server's
    logger middleware server/use/logger.go:17 binds it onto the request's
    slog logger so every `resolved_cache.lookup` line carries it as
    `traceId`). The pre-#159 call passed a non-existent `trace_id=` kwarg
    and only ran via an injected adapter.
    """
    ms, code, body = browser.http_get(
        target_path, token, timeout=timeout, retries=1,
        headers={"X-Krateo-TraceId": trace_id})
    sha = hashlib.sha256(body or b"").hexdigest()
    return {
        "trace_id": trace_id,
        "http_ms": ms,
        "code": code,
        "body_sha256": sha,
        "body_bytes": body or b"",
        "marker_present": False,
    }


def _warmup_probes(target_path: str, token: str, count: int,
                   probe_fn, log=print) -> int:
    """Fire `count` UNSCORED warm-up /calls before the first scored probe.

    #159 v1.1 scope: the first scored probe must never eat a cold-fill
    miss (an empty per-cohort L1 cell → synchronous resolve → the PRIMARY
    `miss_delta == 0` invariant would trip on a benign first-touch). The
    warm-ups run BEFORE `vars_before` is snapshotted, so their misses are
    excluded from the scored counter window.

    Each warm-up uses its own throwaway trace id (never scored, never
    log-correlated). Returns the number of warm-ups that returned HTTP 200
    (diagnostic only — a warm-up that 404s/500s does not abort the run;
    the scored probes' own HTTP-code gate is the load-bearing check).
    """
    ok = 0
    for i in range(max(0, count)):
        wu = probe_fn(target_path, token, f"bench-vss-warmup-{i}")
        if wu.get("code") == 200:
            ok += 1
    if count > 0:
        log(f"warmup       ... fired {count} unscored probe(s), {ok} ok")
    return ok


# ─── Pod log filter (post-hoc SECONDARY) ────────────────────────────────────


def _grep_pod_logs_for_traces(trace_ids: list[str],
                              since: str = "30s",
                              tail: int = 4000,
                              retries: int = 3) -> dict:
    """Fetch deployment/snowplow logs and filter by trace_id.

    Returns {trace_id: [parsed_event, ...]}. Each parsed_event is the
    raw JSON dict of a `resolved_cache.lookup` log line whose `traceId`
    matches. Missing trace_ids are present in the dict with an empty
    list.

    Risk register R7 mitigation — retry up to `retries` with 1s back-off
    if any trace_id yields 0 events.
    """
    out_per_tid: dict[str, list] = {tid: [] for tid in trace_ids}
    wanted = set(trace_ids)
    for attempt in range(retries):
        rc, out, err = kubectl(
            "logs", "-n", NS, "deployment/snowplow",
            f"--since={since}", f"--tail={tail}")
        if rc != 0:
            if attempt < retries - 1:
                time.sleep(1.0)
                continue
            raise _VerifyError(
                f"kubectl logs rc={rc}: {err[:200] if err else 'no output'}")
        # Reset accumulator each retry — only the latest fetch counts.
        out_per_tid = {tid: [] for tid in trace_ids}
        for line in out.splitlines():
            if not line or not line.startswith("{"):
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if ev.get("msg") != "resolved_cache.lookup":
                continue
            tid = ev.get("traceId") or ev.get("trace_id")
            if tid in wanted:
                out_per_tid[tid].append(ev)
        if all(out_per_tid[tid] for tid in trace_ids):
            return out_per_tid
        if attempt < retries - 1:
            time.sleep(1.0)
    return out_per_tid


# ─── Informer ACK watcher (PM Q3 — Option A) ────────────────────────────────


class _InformerAckWatcher(threading.Thread):
    """Tails `kubectl logs --since=10s -f deployment/snowplow` and reports
    when a `cache_event.consumed` line with the patched (gvr, ns, name)
    arrives.

    Caller flow:
        watcher = _InformerAckWatcher(gvr, ns, name)
        watcher.start()      # subscribes BEFORE the mutation
        ...                  # run mutation
        seen = watcher.seen_by(deadline_monotonic)  # blocks until deadline
        watcher.stop()

    PM-required (Option A): if the informer has NOT logged the
    dirty-mark by the time the mid-probe fires, the harness emits
    INDETERMINATE_INFORMER_NOT_ACKED — caller decides what to do.

    Risk register R7 — pod log stream may lag. We mitigate by starting
    the stream BEFORE the mutation (subscribed → kubectl tail buffers
    events; we never poll-after-the-fact).
    """

    def __init__(self, gvr_string: str, namespace: str, name: str):
        super().__init__(daemon=True)
        self._gvr_string = gvr_string
        self._namespace = namespace
        self._name = name
        self._proc: subprocess.Popen | None = None
        self._seen_event = threading.Event()
        self._stop = threading.Event()
        self._err_queue: queue.Queue[str] = queue.Queue()

    def run(self) -> None:
        try:
            self._proc = subprocess.Popen(
                ["kubectl", *kubectl_context_args(),
                 "logs", "-n", NS,
                 "deployment/snowplow",
                 "--since=10s", "-f", "--tail=500"],
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True, bufsize=1)
        except Exception as e:
            self._err_queue.put(f"popen failed: {e}")
            return
        try:
            self._parse_stream(self._proc.stdout)
        except Exception as e:
            self._err_queue.put(f"stdout read failed: {e}")

    def _parse_stream(self, stream) -> None:
        """Parse slog JSON lines from `stream` until a matching
        `cache_event.consumed` line lands OR self._stop fires.

        Extracted as its own method so the meta-falsifier can drive it
        with a fake stdin stream (PM tightening #2, 2026-06-02) — no
        subprocess + no thread required for the test.

        Matching contract (TRACED to internal/cache/deps.go:549-557 for
        OnAdd/OnUpdate and 634-644 for OnDelete):
            ev["msg"] == "cache_event.consumed"
            ev["gvr"] == self._gvr_string   (verbatim GVR.String() shape)
            ev["ns"]  == self._namespace
            ev["name"] == self._name
        """
        for line in stream:
            if self._stop.is_set():
                return
            if not line or not line.startswith("{"):
                continue
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                continue
            if ev.get("msg") != "cache_event.consumed":
                continue
            if ev.get("gvr") != self._gvr_string:
                continue
            if ev.get("ns") != self._namespace:
                continue
            if ev.get("name") != self._name:
                continue
            self._seen_event.set()
            return

    def seen_by(self, deadline_monotonic: float) -> bool:
        """Block until either the dirty-mark line is observed OR the
        deadline passes. Returns True on observation, False otherwise.
        """
        timeout = max(0.0, deadline_monotonic - time.monotonic())
        return self._seen_event.wait(timeout=timeout)

    def stop(self) -> None:
        """Best-effort termination of the kubectl logs subprocess.

        PM tightening #3 (2026-06-02): terminate() + wait(timeout=2.0)
        + kill() fallback so a hung kubectl never orphans on the bench
        laptop after the harness exits.
        """
        self._stop.set()
        if self._proc is None:
            return
        try:
            self._proc.terminate()
        except Exception:
            pass
        try:
            self._proc.wait(timeout=2.0)
        except subprocess.TimeoutExpired:
            try:
                self._proc.kill()
            except Exception:
                pass
            try:
                self._proc.wait(timeout=1.0)
            except Exception:
                pass
        except Exception:
            pass


# ─── Marker presence in body ────────────────────────────────────────────────


def _marker_in_body(body: bytes, marker: str) -> bool:
    """True if `marker` appears anywhere in `body` (UTF-8 decoded).

    For the widget-echo target the marker is the widget's
    `spec.widgetData.label` scalar, which echoes verbatim into the served
    /call body — so a substring scan is exact (live-confirmed against
    0.30.261 on 2026-06-12: the resolved Button body embeds the label).
    """
    if not body or not marker:
        return False
    try:
        return marker in body.decode("utf-8", errors="replace")
    except Exception:
        return False


# ─── Verdict logic ──────────────────────────────────────────────────────────


def _decide_verdict(probes: list[dict],
                    miss_delta: int,
                    hit_delta: int,
                    log_events: dict,
                    informer_ack: str,
                    cell_inexact: bool) -> tuple[str, int, str | None]:
    """Apply the verdict matrix (#159 PM review C1, 2026-06-12 §1).

    Returns (verdict_string, exit_code, mid_observation). Verdict strings
    match the falsifier table cases exactly so the meta-tests can grep on
    them. `mid_observation` ("served_stale" | "served_fresh_early") is set
    only on PASS (informational — NEVER gated on); None otherwise.

    THE CONTRACT (per feedback_mutation_serves_stale_while_refresh): the
    cache must NOT block or do a synchronous cold-fill MISS during a
    refresh. Serving fresh EARLY (the refresh beat the probe RTT) is
    strictly-better, NOT a defect. So PASS = "customer never ate a cold
    miss (miss_delta==0) AND freshness arrived by T+10s". The literal
    stale-body-at-+50ms state is recorded informationally, not gated.
    The retired FAIL_STALE_NOT_SERVED_ON_MID was an over-strict mapping
    that graded a better-than-contract outcome as a hard FAIL.
    """
    pre, mid, post = probes

    # Hard-fail HTTP codes first.
    for p in probes:
        if p["code"] != 200:
            return (f"FAIL_HTTP_{p['code']}_ON_{p['window']}", 2, None)

    # PM Q3 — Option A informer-ack gate (INDETERMINATE if mid fires
    # before deps_watch logged the dirty-mark for the patched object).
    if informer_ack == "NOT_ACKED":
        return ("INDETERMINATE_INFORMER_NOT_ACKED", 1, None)

    # Window slip: design Q4 + risk register R2.
    if abs(mid["offset_ms"]) > _MID_WINDOW_HARD_LIMIT_MS:
        return ("INDETERMINATE_MID_WINDOW_SLIPPED", 1, None)

    # PRIMARY counter assertion — the load-bearing no-cold-miss invariant.
    # miss_delta != 0 is the REAL serve-stale violation (a synchronous
    # cold-fill on the mutation path); stays a hard FAIL.
    if miss_delta != 0:
        return ("FAIL_SYNC_COLD_FILL_ON_MID", 2, None)

    # SECONDARY log presence per probe — primary already passed.
    missing = [p["window"] for p in probes
               if not log_events.get(p["trace_id"])]
    if missing:
        return ("INDETERMINATE_LOG_FILTER_UNAVAILABLE", 1, None)

    # Sources-disagree: PRIMARY says no miss but a per-probe log line
    # has hit:false.
    for p in probes:
        for ev in log_events.get(p["trace_id"], []):
            if ev.get("hit") is False:
                return ("FAIL_SOURCES_DISAGREE", 2, None)

    # ── Bounded-freshness (load-bearing): the refresh MUST have arrived
    #    by T+10s — the post body carries the new marker AND differs from
    #    the pre snapshot. Otherwise the refresh never completed. ─────────
    if (not post["marker_present"]
            or post["body_sha256"] == pre["body_sha256"]):
        return ("FAIL_REFRESH_NOT_COMPLETED_BY_10S", 2, None)

    # ── Mid-window classification (#159 C1 §1) ──────────────────────────
    # served_stale       — mid == pre snapshot AND no marker (canonical
    #                       serve-stale; refresh had not yet swapped).
    # served_fresh_early — mid already carries the marker (refresh beat the
    #                       probe RTT; strictly-better than stale — the
    #                       idle-cluster smoke case).
    # Neither — mid body CHANGED but does NOT carry the just-written marker:
    #           a genuine "something else mutated the entry" anomaly worth a
    #           human look, but NOT a serve-stale FAIL → INDETERMINATE.
    if mid["marker_present"]:
        return ("PASS", 0, "served_fresh_early")
    if pre["body_sha256"] == mid["body_sha256"]:
        return ("PASS", 0, "served_stale")
    return ("INDETERMINATE_MID_BODY_UNEXPECTED", 1, None)


# ─── Public entry point ─────────────────────────────────────────────────────


def run_verify_serve_stale(user: str = "admin",
                           target: str = "widget-echo",
                           tag: str | None = None,
                           run_dir: Path | None = None,
                           rng_seed: int | None = None,
                           warmup: int = _DEFAULT_WARMUP_PROBES,
                           log=print,
                           section=print,
                           # I/O stub hooks for the falsifier:
                           _probe_fn=None,
                           _snapshot_fn=None,
                           _grep_fn=None,
                           _setup_fn=None,
                           _patch_fn=None,
                           _cleanup_fn=None,
                           _ack_fn=None,
                           _sleep_fn=None) -> tuple[int, dict]:
    """Run the 3-probe serve-stale-while-refresh verifier.

    Returns (exit_code, bundle_dict). Caller writes the bundle and
    returns the exit code; cli.cmd_verify_serve_stale is the only
    production caller.

    #159 C2: `user` defaults to "admin". The widget-echo fixture lives in
    bench-ns-01; cyberjoker has NO RBAC there standalone (FAIL_HTTP_403)
    unless an active phase6 lifecycle has granted a rolebinding into that
    namespace. admin exercises the IDENTICAL `widgets` L1 cell + dirty-mark
    → refresh mechanism (the cache layer is subject-agnostic), so this is a
    contract gate, not a north-star scorer. cyberjoker stays selectable.

    All I/O is parameterised via the `_*_fn` stub hooks (defaults wired
    to the target's registry hooks). The falsifier meta-tests inject
    deterministic stubs — no cluster contact.

    #159 mechanism (DEFECT 2 fix): the mutation target is fully described
    by the registry entry (path / cell_key / gvr_string + setup/mutate/
    cleanup hooks). The default `widget-echo` target creates a bench-owned
    Button whose `spec.widgetData.label` echoes into the served /call body
    — a guaranteed-present, patchable marker. TWO markers drive the
    contract: `setup_marker` (the initial label, present in the pre/mid
    steady-state bodies) and `mutate_marker` (the post-mutation label,
    which MUST appear only in the T+10s body). `marker_present` on every
    probe is scanned against the `mutate_marker`.

    #159 warm-up scope: `warmup` UNSCORED /calls fire after fixture setup
    but BEFORE the scored `vars_before` baseline snapshot, so the first
    scored probe never eats a cold-fill miss.
    """
    target_cfg = _TARGET_REGISTRY.get(target)
    if target_cfg is None:
        raise _VerifyError(
            f"unknown --target {target!r}; known: "
            f"{sorted(_TARGET_REGISTRY)}")

    probe_fn = _probe_fn or _probe
    snapshot_fn = _snapshot_fn or _snapshot_debug_vars
    grep_fn = _grep_fn or _grep_pod_logs_for_traces
    setup_fn = _setup_fn or target_cfg["setup_fn"]
    patch_fn = _patch_fn or target_cfg["mutate_fn"]
    cleanup_fn = _cleanup_fn or target_cfg["cleanup_fn"]
    sleep_fn = _sleep_fn or time.sleep
    ack_fn = _ack_fn  # None → real _InformerAckWatcher

    # Pre-flight: /debug/vars reachable AND publishes the cell? (Reachability
    # + CACHE_ENABLED gate — the SCORED counter baseline is re-snapshotted
    # AFTER the warm-ups below.)
    try:
        vars_preflight = snapshot_fn()
    except _VerifyError as e:
        return 1, {"verdict": "INDETERMINATE_DEBUG_VARS_UNREACHABLE",
                   "error": str(e), "exit_code": 1}
    if "snowplow_dispatch_l1_lookups" not in (vars_preflight or {}):
        # Risk register R8 — CACHE_ENABLED=false.
        return 1, {"verdict": "INDETERMINATE_CACHE_OFF",
                   "exit_code": 1,
                   "reason": "snowplow_dispatch_l1_lookups not published"}

    # JWT for the chosen user.
    tokens = browser.login_all()
    token = tokens.get(user)
    if not token:
        return 1, {"verdict": "INDETERMINATE_LOGIN_FAILED",
                   "user": user, "exit_code": 1}

    # Two-marker model (see docstring). setup_marker is the initial widget
    # label (pre/mid steady-state); mutate_marker is the post label.
    # rng_seed is reserved for future multi-target random selection; the
    # widget-echo target uses a fixed bench-owned fixture name (no random
    # pick), so it does not consume the seed today.
    _ = rng_seed
    stamp = f"{int(time.time() * 1000)}-{uuid.uuid4().hex[:8]}"
    setup_marker = f"vss-v1-{stamp}"
    mutate_marker = f"vss-v2-{stamp}"
    run_id = uuid.uuid4().hex[:6]
    tid_pre = f"bench-vss-{run_id}-pre"
    tid_mid = f"bench-vss-{run_id}-mid"
    tid_post = f"bench-vss-{run_id}-post"

    target_path = target_cfg["path"]
    cell_key = target_cfg["cell_key"]

    section(f"verify-serve-stale ({user} / {target}) — tag={tag or 'unset'}")

    # ── Fixture setup (establishes the pre/mid steady-state body) ───────
    try:
        ns, name = setup_fn(setup_marker)
    except _VerifyError as e:
        return 1, {"verdict": "INDETERMINATE_SETUP_FAILED",
                   "error": str(e), "exit_code": 1}
    log(f"target: ns={ns} name={name} "
        f"setup_marker={setup_marker} mutate_marker={mutate_marker}")

    # ── WARM-UP (#159): unscored /calls fill the cold per-cohort cell ──
    # Fires AFTER setup (fixture exists) and BEFORE the scored vars_before
    # snapshot, so the first scored probe's resolve is a HIT, not a
    # synchronous cold-fill miss that would trip the PRIMARY
    # `miss_delta == 0` invariant on a benign first touch.
    warmup_ok = _warmup_probes(target_path, token, warmup, probe_fn, log=log)

    # ── SCORED baseline: re-snapshot /debug/vars AFTER the warm-ups ────
    try:
        vars_before = snapshot_fn()
    except _VerifyError as e:
        cleanup_fn(ns, name)
        return 1, {"verdict": "INDETERMINATE_DEBUG_VARS_UNREACHABLE",
                   "error": str(e), "exit_code": 1}

    # ── Subscribe to deps_watch log stream BEFORE the mutation ─────────
    # PM Option A (mandatory): start `kubectl logs --since=10s -f` BEFORE
    # the mutation so the dirty-mark `cache_event.consumed` line cannot
    # land before we're listening. The 10s --since gives a wide backfill
    # window so the kubectl-side stream-attach race is non-load-bearing.
    watcher = None
    if ack_fn is None:
        # gvr_string is the target's GVR.String() VERBATIM, matching what
        # snowplow's slog emits (deps.go: slog.String("gvr", gvr.String())).
        # Per apimachinery pkg/runtime/schema/group_version.go:
        #   "<group>/<version>, Resource=<resource>"
        # The ", Resource=" prefix is load-bearing — without it no real log
        # line matches and every run forces INDETERMINATE_INFORMER_NOT_ACKED
        # (PM tightening #1, 2026-06-02). The widget-echo target supplies
        # "widgets.templates.krateo.io/v1beta1, Resource=buttons".
        watcher = _InformerAckWatcher(target_cfg["gvr_string"], ns, name)
        watcher.start()
        # Grace for the kubectl logs -f subprocess to attach.
        sleep_fn(0.30)

    # ── T-1s probe (steady-state, pre-mutation) ────────────────────────
    t0 = time.monotonic()
    started_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    pre = probe_fn(target_path, token, tid_pre)
    pre["window"] = "pre"
    pre["offset_ms"] = -1000  # nominal; pre fires at t0
    pre["marker_present"] = _marker_in_body(pre["body_bytes"], mutate_marker)
    log(f"T-1s probe   ... code={pre['code']} body_sha={pre['body_sha256'][:8]}"
        f"... trace={tid_pre} ({pre['http_ms']}ms)")

    # Wait the remaining time to T_mutate (t0 + 1.0s).
    target_mutate = t0 + 1.000
    sleep_fn(max(0.0, target_mutate - time.monotonic()))

    # ── MUTATE (flip the echo field to the mutate_marker) ───────────────
    rc, t_mutate, err = patch_fn(ns, name, mutate_marker)
    if rc != 0:
        if watcher is not None:
            watcher.stop()
        cleanup_fn(ns, name)
        return 1, {"verdict": "INDETERMINATE_PATCH_FAILED",
                   "error": err, "exit_code": 1}
    log(f"MUTATE       ... rc={rc} t_mutate=+{(t_mutate - t0):.3f}s")

    # ── T+50ms probe ───────────────────────────────────────────────────
    mid_target = t_mutate + 0.050
    sleep_fn(max(0.0, mid_target - time.monotonic()))

    # PM Option A: verify informer ACK BEFORE firing the mid-probe.
    # ACK is a hard gate — NOT_ACKED → INDETERMINATE_INFORMER_NOT_ACKED.
    if ack_fn is not None:
        # Stub injection path (used by the meta-falsifier).
        informer_ack = ack_fn(ns, name, mutate_marker, t_mutate)
    elif watcher is not None:
        # seen_by blocks until either the dirty-mark line is observed or
        # the deadline (time.monotonic() — i.e. NOW, since we already
        # slept to mid_target) passes.
        seen = watcher.seen_by(time.monotonic())
        informer_ack = "ACKED" if seen else "NOT_ACKED"
        watcher.stop()
    else:
        # Defensive — both ack_fn and watcher None should be impossible.
        informer_ack = "NOT_ACKED"

    actual_mid_offset_ms = int((time.monotonic() - t_mutate) * 1000)
    mid = probe_fn(target_path, token, tid_mid)
    mid["window"] = "mid"
    mid["offset_ms"] = actual_mid_offset_ms
    mid["marker_present"] = _marker_in_body(mid["body_bytes"], mutate_marker)
    log(f"T+50ms probe ... code={mid['code']} body_sha={mid['body_sha256'][:8]}"
        f"... trace={tid_mid} (offset=+{actual_mid_offset_ms}ms, "
        f"informer={informer_ack})")

    # ── T+10s probe (post-refresh) ──────────────────────────────────────
    # #318-R1a PM-gate C2: was T+5s. The refresher rate-floor (default 2s,
    # RESOLVED_CACHE_REFRESHER_RATE_FLOOR_SECONDS) can defer the
    # post-mutation re-resolve by up to floor + resolve time; a 5s window
    # raced that and would spuriously FAIL a healthy floor. 10s clears
    # floor(2s) + worst-case resolve with margin while staying far under
    # the 30s convergence tier.
    post_target = t_mutate + 10.000
    sleep_fn(max(0.0, post_target - time.monotonic()))
    actual_post_offset_ms = int((time.monotonic() - t_mutate) * 1000)
    post = probe_fn(target_path, token, tid_post)
    post["window"] = "post"
    post["offset_ms"] = actual_post_offset_ms
    post["marker_present"] = _marker_in_body(post["body_bytes"], mutate_marker)
    log(f"T+10s probe  ... code={post['code']} body_sha={post['body_sha256'][:8]}"
        f"... trace={tid_post} (offset=+{actual_post_offset_ms}ms, "
        f"marker={'present' if post['marker_present'] else 'ABSENT'})")

    # ── PRIMARY: /debug/vars snapshot AFTER all probes ─────────────────
    try:
        vars_after = snapshot_fn()
    except _VerifyError as e:
        cleanup_fn(ns, name)
        return 1, {"verdict": "INDETERMINATE_DEBUG_VARS_UNREACHABLE",
                   "error": str(e), "exit_code": 1}
    miss_delta, hit_delta, cell_inexact = _cell_delta(
        vars_before, vars_after, cell_key)
    log(f"L1 counters  ... miss_delta={miss_delta} hit_delta={hit_delta}"
        f"{' (cell_key_inexact)' if cell_inexact else ''}")

    # ── SECONDARY: pod log post-hoc filter ─────────────────────────────
    log_events: dict
    try:
        # Brief wait so the post-probe log line has time to flush.
        sleep_fn(2.0)
        log_events = grep_fn([tid_pre, tid_mid, tid_post])
    except _VerifyError as e:
        log(f"pod log filter failed: {e}")
        log_events = {tid_pre: [], tid_mid: [], tid_post: []}

    # ── Cleanup (target-owned: delete the bench fixture) ───────────────
    cleanup_ok = cleanup_fn(ns, name)
    log(f"CLEANUP      ... fixture removed={cleanup_ok}")

    # ── Verdict ────────────────────────────────────────────────────────
    verdict, exit_code, mid_observation = _decide_verdict(
        [pre, mid, post], miss_delta, hit_delta,
        log_events, informer_ack, cell_inexact)
    section(f"VERDICT: {verdict}"
            f"{f' (mid={mid_observation})' if mid_observation else ''}")

    # ── Bundle ─────────────────────────────────────────────────────────
    def _strip_body(p: dict) -> dict:
        return {k: v for k, v in p.items() if k != "body_bytes"}

    bundle = {
        "verdict": verdict,
        "exit_code": exit_code,
        "started_at": started_at,
        "user": user,
        "target": target,
        "tag": tag,
        "mutation": {
            "namespace": ns,
            "name": name,
            "setup_marker": setup_marker,
            "mutate_marker": mutate_marker,
            "patch_rc": rc,
        },
        "warmup": {"requested": warmup, "http_200": warmup_ok},
        # #159 C1: informational classification of the mid-window state on
        # PASS — "served_stale" (canonical) | "served_fresh_early" (refresh
        # beat the probe RTT). None on non-PASS verdicts. NEVER gated on.
        "mid_observation": mid_observation,
        "probes": [_strip_body(pre), _strip_body(mid), _strip_body(post)],
        "l1_lookups": {
            "class": cell_key,
            "miss_delta": miss_delta,
            "hit_delta": hit_delta,
            "cell_key_inexact": cell_inexact,
        },
        "informer_ack_check": informer_ack,
        "log_events": {
            "pre":  [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_pre, [])],
            "mid":  [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_mid, [])],
            "post": [{"hit": e.get("hit"),
                      "key_hash": e.get("key_hash"),
                      "resident_bytes": e.get("resident_bytes")}
                     for e in log_events.get(tid_post, [])],
        },
        "stale_served": pre["body_sha256"] == mid["body_sha256"],
        "refresh_completed": post["body_sha256"] != pre["body_sha256"]
            and post["marker_present"],
        "sources_agree": all(
            ev.get("hit") is True
            for tid in (tid_pre, tid_mid, tid_post)
            for ev in log_events.get(tid, [])
        ),
        "cleanup": {"fixture_removed": cleanup_ok},
    }

    if run_dir is not None:
        try:
            Path(run_dir).mkdir(parents=True, exist_ok=True)
            ts = time.strftime("%Y%m%d-%H%M%S")
            out_path = Path(run_dir) / f"verify-serve-stale-{ts}.json"
            out_path.write_text(json.dumps(bundle, indent=2, default=str))
            log(f"bundle written to {out_path}")
        except Exception as e:
            log(f"bundle write failed: {e}")

    return exit_code, bundle


__all__ = ["run_verify_serve_stale"]
