"""Command-line surface for the bench harness.

Subcommands: check, calibrate, cleanup, storm, converge, measure, phase6,
phase7, phase8, report. Every entrypoint goes through `_gke_context_guard`
first per feedback_kubectl_verify_gke_context.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §A.9 + §G Block 4.

Exit codes:
  0 — success
  1 — generic failure
  2 — preflight gate fail (one or more gates FAILED; stderr names each)
  3 — non-GKE kubectl context (refused to operate)
  4 — convergence timeout (raised by browser.ConvergenceTimeout)
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

from bench import cluster
from bench.cluster import (
    NS,
    kubectl,
)


__all__ = [
    "main",
    "cmd_check",
    "cmd_calibrate",
    "cmd_cleanup",
    "cmd_storm",
    "cmd_converge",
    "cmd_measure",
    "cmd_phase6",
    "cmd_phase7",
    "cmd_phase8",
    "cmd_verify_serve_stale",
    "cmd_report",
    "verify_deployed_image",
]


# ─── ANSI helpers + logger ──────────────────────────────────────────────────

RESET = "\033[0m"
BOLD = "\033[1m"
RED = "\033[31m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
BLUE = "\033[34m"
CYAN = "\033[36m"


def _enabled_ansi():
    """ANSI on for TTYs unless BENCH_NO_COLOR is set."""
    if os.environ.get("BENCH_NO_COLOR", "").strip():
        return False
    return sys.stdout.isatty()


def _setup_logger():
    """Return (log, section) callables wired to the coloured ANSI helpers.

    Per PM carry-forward #1: this replaces lifecycle.py's `log()` shim.
    The functions stay simple — string in, prefixed timestamp + colour
    out. No globals; callers reuse the returned closures (cli.py's
    `log`/`section` re-export them, and lifecycle.py imports those).
    """
    use_ansi = _enabled_ansi()

    def log(msg):
        ts = time.strftime("%H:%M:%S")
        if use_ansi:
            print(f"  [{CYAN}{ts}{RESET}] {msg}", flush=True)
        else:
            print(f"  [{ts}] {msg}", flush=True)

    def section(title):
        if use_ansi:
            print(f"\n  {BOLD}=== {title} ==={RESET}", flush=True)
        else:
            print(f"\n  === {title} ===", flush=True)

    return log, section


log, section = _setup_logger()


# ─── GKE context guard — SUBSET wrapper around cluster.gke_context_guard ────


def _gke_context_guard(allow_non_gke=None):
    """SUBSET of `cluster.gke_context_guard`.

    Per PM carry-forward #2: this MUST stay a subset of the canonical
    cluster.py guard. We delegate — no second, non-conformant guard
    surface. Args + exit semantics mirror cluster.gke_context_guard():
        allow_non_gke=True or BENCH_ALLOW_NON_GKE=1 → bypass.
        Exit 3 on context mismatch.

    Returns the current context string on PASS (for cmd_check to log).
    Calls sys.exit(3) on FAIL.
    """
    # Delegate to cluster.py's canonical implementation. We then read the
    # current-context one more time for the caller's logging — this is
    # additive, not a re-implementation of the gate.
    cluster.gke_context_guard(allow_non_gke=allow_non_gke)
    try:
        proc = subprocess.run(
            ["kubectl", "config", "current-context"],
            capture_output=True, timeout=10,
        )
        return (proc.stdout.decode() or "").strip()
    except Exception:
        return ""


# ─── verify_deployed_image (moved from worktree source 951-984) ─────────────


def verify_deployed_image(expected_tag: str | None = None) -> bool:
    """Ensure the snowplow deployment runs the expected image.

    Per plan §A.9 (`MOVED` from source 951-984). Differences from the
    source version:
      - `expected_tag` arg is now first-class (was the EXPECTED_IMAGE_TAG
        env var only).
      - Returns bool instead of `sys.exit(1)` on miss; caller decides
        what exit code to emit. This makes the function testable.
      - SKIP_IMAGE_CHECK=1 env still bypasses (legacy operator escape
        hatch).

    Returns True on match, False on mismatch / kubectl failure. Writes
    operator-facing diagnostics to stderr on failure.
    """
    if os.environ.get("SKIP_IMAGE_CHECK", "0") == "1":
        log("SKIP_IMAGE_CHECK=1 — skipping image version check")
        return True

    expected = (expected_tag
                or os.environ.get("EXPECTED_IMAGE_TAG", "")
                or "").strip()
    if not expected:
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: expected image tag required.{RESET}\n"
            f"  pass --tag <tag> or set EXPECTED_IMAGE_TAG.\n"
            f"  example:\n"
            f"    python -m bench check --tag 0.25.19\n\n"
            f"  Or set SKIP_IMAGE_CHECK=1 to bypass (not recommended).\n"
        )
        return False

    rc, out, err = kubectl(
        "get", "deployment", "snowplow", "-n", NS,
        "-o", "jsonpath={.spec.template.spec.containers[?(@.name==\"snowplow\")].image}",
    )
    if rc != 0 or not out.strip():
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: Could not get snowplow deployment image.{RESET}\n"
            f"  kubectl failed or snowplow not found in {NS}. Ensure cluster access.\n"
            f"  stderr: {err[:200] if err else 'none'}\n"
        )
        return False

    current_image = out.strip()
    current_tag = current_image.split(":")[-1] if ":" in current_image else ""
    if current_tag != expected:
        sys.stderr.write(
            f"\n{RED}{BOLD}ERROR: Deployed image does not match expected.{RESET}\n"
            f"  Expected tag: {expected}\n"
            f"  Current image: {current_image}\n"
            f"  Deploy the new image first, then run check.\n"
        )
        return False

    log(f"Deployed image verified: {current_image}")
    return True


# ─── Gate helpers (each returns (passed: bool, msg: str)) ───────────────────


def _gate_pod_ready() -> tuple[bool, str]:
    """Gate 2: snowplow pod ready in krateo-system.

    Checks deployment readiness: 1/1 ready replicas, all containers ready,
    and no restart-storm signal.
    """
    rc, out, err = kubectl(
        "get", "deployment", "snowplow", "-n", NS,
        "-o", "json",
    )
    if rc != 0 or not out.strip():
        return False, (f"snowplow_pod_ready: FAIL (kubectl rc={rc}: "
                       f"{err[:120] if err else 'no output'})")
    try:
        deploy = json.loads(out)
    except json.JSONDecodeError as e:
        return False, f"snowplow_pod_ready: FAIL (JSON parse: {e})"
    status = deploy.get("status", {}) or {}
    desired = status.get("replicas", 0) or 0
    ready = status.get("readyReplicas", 0) or 0
    avail = status.get("availableReplicas", 0) or 0
    if desired == 0:
        return False, "snowplow_pod_ready: FAIL (desired=0; deployment scaled to zero)"
    if ready < desired or avail < desired:
        return False, (f"snowplow_pod_ready: FAIL "
                       f"(desired={desired} ready={ready} available={avail})")
    return True, f"snowplow_pod_ready: PASS (replicas {ready}/{desired})"


def _gate_image_match(expected_tag: str) -> tuple[bool, str]:
    """Gate 3: deployment image tag matches the --tag argument."""
    ok = verify_deployed_image(expected_tag=expected_tag)
    if ok:
        return True, f"image_tag_match: PASS (tag={expected_tag})"
    return False, f"image_tag_match: FAIL (expected={expected_tag})"


def _gate_crds_present(timeout=10) -> tuple[bool, str]:
    """Gate 4: CRDs present — WIDGET_KINDS + composition CRD."""
    from bench.lifecycle import WIDGET_KINDS  # deferred (Block 1 module)

    composition_crd = "compositiondefinitions.core.krateo.io"
    crds_to_check = list(WIDGET_KINDS) + [composition_crd]
    deadline = time.time() + timeout
    missing = list(crds_to_check)

    while time.time() < deadline and missing:
        still_missing = []
        for crd in missing:
            rc, out, _ = kubectl(
                "get", "crd", crd, "--ignore-not-found", "-o", "name",
                timeout_secs=5,
            )
            if rc != 0 or not out.strip():
                still_missing.append(crd)
        missing = still_missing
        if missing:
            time.sleep(1)

    if missing:
        return False, (f"crds_present: FAIL (missing: "
                       f"{', '.join(missing[:3])}{'...' if len(missing) > 3 else ''})")
    return True, f"crds_present: PASS ({len(crds_to_check)} CRDs)"


def _gate_helm_lockstep(expected_tag: str) -> tuple[bool, str]:
    """Gate 5: helm release image tag matches the deployment.

    Catches OBS-1 / 0.30.186-style drift where the deployment carries a
    tag the helm release does NOT know about. See feedback_chart_release_lockstep.
    """
    try:
        proc = subprocess.run(
            ["helm", "get", "values", "snowplow", "-n", NS, "-o", "json"]
            + cluster.helm_context_args(),
            capture_output=True, timeout=15,
        )
    except FileNotFoundError:
        return False, "helm_release_lockstep: FAIL (helm binary not found)"
    except subprocess.TimeoutExpired:
        return False, "helm_release_lockstep: FAIL (helm get values timed out)"
    if proc.returncode != 0:
        err_text = (proc.stderr.decode() or "").strip()
        return False, (f"helm_release_lockstep: FAIL "
                       f"(helm rc={proc.returncode}: {err_text[:120]})")
    try:
        values = json.loads(proc.stdout.decode() or "{}")
    except json.JSONDecodeError as e:
        return False, f"helm_release_lockstep: FAIL (JSON parse: {e})"

    # Look for the tag under common helm chart paths.
    candidates = []

    def _walk(node, path=""):
        if isinstance(node, dict):
            for k, v in node.items():
                if k == "tag" and isinstance(v, (str, int, float)):
                    candidates.append((path + "." + k, str(v)))
                _walk(v, path + "." + str(k))
        elif isinstance(node, list):
            for i, v in enumerate(node):
                _walk(v, path + f"[{i}]")

    _walk(values)

    if not candidates:
        return False, ("helm_release_lockstep: FAIL "
                       "(no .image.tag-like key in helm get values)")

    matches = [c for c in candidates if c[1].strip() == expected_tag.strip()]
    if not matches:
        observed = sorted({c[1] for c in candidates})
        return False, (f"helm_release_lockstep: FAIL "
                       f"(expected tag={expected_tag}, found {observed})")
    return True, f"helm_release_lockstep: PASS (tag={expected_tag})"


def _gate_frontend_reachable() -> tuple[bool, str]:
    """Gate 6: frontend LB reachable (HTTP 200 on /login)."""
    frontend = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080").strip()
    if not frontend:
        return False, "frontend_lb_reachable: FAIL (FRONTEND_URL not set)"
    url = frontend.rstrip("/") + "/login"
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            code = r.getcode()
    except urllib.error.HTTPError as e:
        # Some portal builds return 200 + HTML; non-2xx is a fail.
        return False, (f"frontend_lb_reachable: FAIL "
                       f"(HTTP {e.code} from {url})")
    except urllib.error.URLError as e:
        return False, (f"frontend_lb_reachable: FAIL "
                       f"(connection error: {e.reason})")
    except Exception as e:
        return False, (f"frontend_lb_reachable: FAIL "
                       f"({type(e).__name__}: {e})")
    if code != 200:
        return False, f"frontend_lb_reachable: FAIL (HTTP {code} from {url})"
    return True, f"frontend_lb_reachable: PASS ({url})"


def _gate_overlay_freshness(allow_stale: bool) -> tuple[bool, str]:
    """Gate 7: overlay freshness or --allow-stale-overlay."""
    from bench.expected import (
        OverlayStale,
        overlay_age_seconds,
        overlay_freshness_or_die,
    )

    if allow_stale:
        age = overlay_age_seconds()
        if age == float("inf"):
            age_str = "missing"
        else:
            age_str = f"{age / 86400.0:.1f}d"
        # Per dispatch: loud-log when bypassed.
        sys.stderr.write(
            f"  {YELLOW}{BOLD}WARNING: --allow-stale-overlay bypasses freshness gate "
            f"(overlay age={age_str}). Run `python -m bench calibrate` to "
            f"refresh.{RESET}\n"
        )
        return True, f"overlay_freshness: BYPASS (age={age_str})"

    try:
        overlay_freshness_or_die(max_age_days=14)
    except OverlayStale as e:
        return False, f"overlay_freshness: FAIL ({e})"
    age = overlay_age_seconds()
    age_str = "missing" if age == float("inf") else f"{age / 86400.0:.1f}d"
    return True, f"overlay_freshness: PASS (age={age_str})"


# ─── cmd_check — the 7-item preflight gate ──────────────────────────────────


def cmd_check(args) -> int:
    """8-item preflight gate. No mutations; 60s wall-clock budget.

    Returns:
        0 — all 8 gates PASS
        2 — one or more gates FAIL (stderr names each)
        3 — non-GKE kubectl context (handled inside _gke_context_guard)

    Gates (per plan §G Block 2; gate 8 added by the #320 follow-up):
        1. GKE context match
        2. snowplow pod ready in krateo-system
        3. image tag matches --tag
        4. CRDs present (WIDGET_KINDS + composition CRD)
        5. helm release lockstep
        6. frontend LB reachable
        7. overlay freshness (--allow-stale-overlay to bypass)
        8. in-process kubernetes client importable + initialized
    """
    start = time.time()
    section("bench check — preflight (8 gates)")

    # Gate 1: GKE context.
    allow_non_gke = bool(getattr(args, "allow_non_gke", False))
    ctx = _gke_context_guard(allow_non_gke=allow_non_gke)
    log(f"gke_context: PASS (context={ctx!r})")

    expected_tag = getattr(args, "tag", None) or os.environ.get("EXPECTED_IMAGE_TAG", "")
    expected_tag = (expected_tag or "").strip()

    results: list[tuple[str, bool, str]] = []

    # Gate 2.
    ok, msg = _gate_pod_ready()
    results.append(("snowplow_pod_ready", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 3.
    if not expected_tag:
        msg = "image_tag_match: FAIL (no --tag provided)"
        results.append(("image_tag_match", False, msg))
        _stderr_log(msg)
    else:
        ok, msg = _gate_image_match(expected_tag)
        results.append(("image_tag_match", ok, msg))
        (log if ok else _stderr_log)(msg)

    # Gate 4.
    ok, msg = _gate_crds_present(timeout=10)
    results.append(("crds_present", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 5.
    if not expected_tag:
        msg = "helm_release_lockstep: FAIL (no --tag provided)"
        results.append(("helm_release_lockstep", False, msg))
        _stderr_log(msg)
    else:
        ok, msg = _gate_helm_lockstep(expected_tag)
        results.append(("helm_release_lockstep", ok, msg))
        (log if ok else _stderr_log)(msg)

    # Gate 6.
    ok, msg = _gate_frontend_reachable()
    results.append(("frontend_lb_reachable", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 7.
    allow_stale = bool(getattr(args, "allow_stale_overlay", False))
    ok, msg = _gate_overlay_freshness(allow_stale=allow_stale)
    results.append(("overlay_freshness", ok, msg))
    (log if ok else _stderr_log)(msg)

    # Gate 8 (#320 follow-up): in-process kubernetes client.
    ok, msg = _gate_k8s_client()
    results.append(("k8s_client", ok, msg))
    (log if ok else _stderr_log)(msg)

    elapsed = time.time() - start
    failed = [name for name, ok, _ in results if not ok]
    section(f"check complete in {elapsed:.1f}s")
    if failed:
        sys.stderr.write(
            f"{RED}{BOLD}check FAILED: "
            f"{len(failed)}/{len(results) + 1} gates failed: "
            f"{', '.join(failed)}{RESET}\n"
        )
        return 2
    n = len(results) + 1  # +1 = the context gate logged before the loop
    log(f"{GREEN}{BOLD}check PASS ({n}/{n} gates){RESET}")
    return 0


def _gate_k8s_client() -> tuple[bool, str]:
    """Gate 8 (#320 follow-up, the 0.30.258 INVALID-run lesson): the
    in-process kubernetes client must import AND initialize. The
    fixture/RBAC stages (S8/S9/S8b/S8c) mutate CRs through it and used to
    degrade SILENTLY to `*_unavailable` errors mid-run when the venv
    lacked the lib — invalidating the run hours in instead of failing
    preflight.
    """
    if not cluster._K8S_LIB_AVAILABLE:
        return False, ("k8s_client: FAIL (python 'kubernetes' lib not "
                       "importable in this venv — pip install "
                       "'kubernetes>=28.0.0' per requirements.txt)")
    if not cluster._k8s_init():
        return False, ("k8s_client: FAIL (lib present but client init "
                       "failed — kubeconfig/context problem; pinned "
                       f"context is {cluster.canonical_gke_context()!r})")
    return True, "k8s_client: PASS (in-process client initialized)"


def _stderr_log(msg):
    """Write a gate-failure line to stderr with a timestamp prefix."""
    ts = time.strftime("%H:%M:%S")
    sys.stderr.write(f"  [{ts}] {msg}\n")


# ─── cmd_calibrate / cmd_cleanup / cmd_storm (Block 4) ──────────────────────


def cmd_calibrate(args) -> int:
    """Refresh the EXPECTED_CALLS overlay against the live cluster.

    Wraps `bench.expected.run_calibrate_expected_calls`. Block 4 wires the
    overlay-writer path; an acceptance run captures the actual overlay at
    Block 5 time.
    """
    from bench.expected import run_calibrate_expected_calls
    try:
        out = run_calibrate_expected_calls()
        log(f"calibrate: wrote overlay to {out}")
        return 0
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}calibrate FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1


def cmd_cleanup(args) -> int:
    """Cluster cleanup: clean_environment + assert_clean."""
    from bench.lifecycle import clean_environment, assert_clean
    allow_destructive = bool(getattr(args, "allow_destructive", False))
    try:
        clean_environment()
        assert_clean(retry_with_cleanup=True,
                     allow_destructive=allow_destructive)
        log("cleanup: complete")
        return 0
    except SystemExit as e:
        # destructive_clean_guard raises SystemExit; preserve its exit code.
        return int(e.code) if isinstance(e.code, int) else 1
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}cleanup FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1


def cmd_storm(args) -> int:
    """Run a disruptive scenario (storm.crb_delete_burst etc.)."""
    from bench import browser, storm
    scenario = getattr(args, "scenario", "crb-delete")
    try:
        tokens = browser.login_all()
    except Exception:
        tokens = {}
    if not tokens:
        sys.stderr.write(f"{RED}storm: login_all returned no tokens{RESET}\n")
        return 1
    if scenario == "crb-delete":
        storm.crb_delete_burst(tokens)
    elif scenario == "user-scaling":
        storm.run_user_scaling(tokens)
    else:
        sys.stderr.write(
            f"{RED}storm: unknown scenario {scenario!r}{RESET}\n")
        return 1
    return 0


# ─── cmd_converge / cmd_measure (Block 4) ───────────────────────────────────


def cmd_converge(args) -> int:
    """Run a single Phase 6 stage with VERIFY poll only.

    Wraps `phases.STAGE_REGISTRY[args.stage]` against the supplied
    --run-dir + --tag + --scale.
    """
    from bench import phases, browser
    stage_id = getattr(args, "stage", None)
    if not stage_id or stage_id not in phases.STAGE_REGISTRY:
        sys.stderr.write(
            f"{RED}converge: --stage required (one of "
            f"{sorted(phases.STAGE_REGISTRY)}){RESET}\n")
        return 1
    run_dir = Path(getattr(args, "run_dir", None) or _default_run_dir(args))
    run_dir.mkdir(parents=True, exist_ok=True)
    try:
        tokens = browser.login_all()
    except Exception:
        tokens = {}
    ctx = phases.StageContext(
        tag=(getattr(args, "tag", None) or "").strip() or "unknown",
        scale=int(getattr(args, "scale", None)
                  or os.environ.get("SCALE", "5000")),
        tokens=tokens,
        admin_token=tokens.get("admin"),
        run_dir=run_dir,
        state_path=run_dir / "state.json",
        cache_mode=getattr(args, "cache_mode", "OFF"),
    )
    try:
        phases.STAGE_REGISTRY[stage_id](ctx)
        return 0
    except phases.ConvergenceTimeout as ct:
        sys.stderr.write(
            f"{RED}{BOLD}ConvergenceTimeout: {ct}{RESET}\n")
        return 4
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}converge FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1


def cmd_measure(args) -> int:
    """Alias of cmd_converge that REFUSES lifecycle mutation stages.

    Allowed stages: S0, S1, S9. The S2-S8 stages mutate cluster state by
    design (create_bench_namespaces, deploy_compositions, etc.), so
    `bench measure` would not be a no-op.
    """
    measure_allowed = {"S0", "S1", "S9"}
    stage_id = getattr(args, "stage", None)
    if stage_id not in measure_allowed:
        sys.stderr.write(
            f"{RED}measure: --stage must be in {sorted(measure_allowed)} "
            f"(others mutate cluster state — use `converge`).{RESET}\n")
        return 1
    return cmd_converge(args)


# ─── cmd_phase6 / cmd_phase7 / cmd_phase8 (Block 4) ─────────────────────────


def _default_run_dir(args) -> Path:
    tag = (getattr(args, "tag", None) or
           os.environ.get("EXPECTED_IMAGE_TAG", "unknown")).strip() or "unknown"
    ts = time.strftime("%Y%m%d-%H%M%S")
    return Path("/tmp/snowplow-runs") / tag / f"run-{ts}"


_RUN_LOCK_PATH = "/tmp/snowplow-bench.lock"


def _acquire_run_lock():
    """Acquire an exclusive filesystem lock so two `phase6` invocations
    cannot mutate the same cluster concurrently.

    Returns the open file handle (keep it alive for the run duration). On
    contention, prints the PID stored in the lockfile and raises RuntimeError.
    """
    import fcntl
    fh = open(_RUN_LOCK_PATH, "a+")
    try:
        fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        fh.seek(0)
        existing = fh.read().strip() or "unknown"
        fh.close()
        raise RuntimeError(
            f"another bench phase6 is already running (lock={_RUN_LOCK_PATH}, "
            f"pid={existing})"
        )
    fh.seek(0)
    fh.truncate(0)
    fh.write(str(os.getpid()))
    fh.flush()
    return fh


def cmd_phase6(args) -> int:
    """Run Phase 6 with --from-stage / --to-stage / --video / --scale.

    On ConvergenceTimeout (raised by any stage runner after persisting
    state.json), exits 4. On other failures, exits 1. On stale overlay
    (>14d, no --allow-stale-overlay), exits 2 with stderr pointing to
    `python -m bench calibrate`. On lock contention (another phase6 already
    running on this host), exits 5.
    """
    from bench import phases, browser
    tag = (getattr(args, "tag", None) or
           os.environ.get("EXPECTED_IMAGE_TAG", "unknown")).strip() or "unknown"
    scale = int(getattr(args, "scale", None) or os.environ.get("SCALE", "5000"))
    from_stage = getattr(args, "from_stage", None)
    to_stage = getattr(args, "to_stage", None)
    cache_mode = getattr(args, "cache_mode", "OFF")
    video = getattr(args, "video", "representative")

    # Overlay-freshness gate (acceptance (g)). Refuses to start when the
    # overlay is stale (>14d) unless --allow-stale-overlay was passed.
    # Reuses the same path as `bench check` Gate 7.
    allow_stale = bool(getattr(args, "allow_stale_overlay", False))
    ok, msg = _gate_overlay_freshness(allow_stale=allow_stale)
    if not ok:
        sys.stderr.write(
            f"{RED}{BOLD}phase6: {msg}. "
            f"Run `python -m bench calibrate` to refresh.{RESET}\n"
        )
        return 2

    # #320 follow-up (0.30.258 INVALID-run lesson): the in-process k8s
    # client is required by the fixture/RBAC stages (S8/S9/S8b/S8c) and
    # declared in requirements.txt. A missing/broken client used to
    # degrade those stages silently HOURS into the run — fail at minute 0.
    ok, msg = _gate_k8s_client()
    if not ok:
        sys.stderr.write(f"{RED}{BOLD}phase6: {msg}{RESET}\n")
        return 2

    try:
        lock_fh = _acquire_run_lock()
    except RuntimeError as e:
        sys.stderr.write(f"{RED}{BOLD}phase6: {e}{RESET}\n")
        return 5

    run_dir_arg = getattr(args, "run_dir", None)
    run_dir = Path(run_dir_arg) if run_dir_arg else _default_run_dir(args)

    section(f"phase6 — {tag} scale={scale} from={from_stage} to={to_stage}")
    log(f"run_dir={run_dir}")
    log(f"run_lock pid={os.getpid()} path={_RUN_LOCK_PATH}")
    try:
        phases.run_phase6(
            tag, scale,
            from_stage=from_stage, to_stage=to_stage,
            cache_mode=cache_mode, video=video, run_dir=run_dir,
        )
        return 0
    except browser.ConvergenceTimeout as ct:
        sys.stderr.write(
            f"{RED}{BOLD}phase6 ConvergenceTimeout: {ct}{RESET}\n")
        return 4
    except phases.IncompatibleStateSchema as e:
        sys.stderr.write(
            f"{RED}{BOLD}phase6: incompatible state.json schema: {e}{RESET}\n")
        return 2
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}phase6 FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1
    finally:
        try:
            lock_fh.close()
        except Exception:
            pass


def cmd_phase7(args) -> int:
    """Run Phase 7 (multi-user warmup-after-restart)."""
    from bench import phases
    try:
        phases.run_phase7_user_scaling(
            tag=getattr(args, "tag", None),
            run_dir=getattr(args, "run_dir", None),
        )
        return 0
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}phase7 FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1


def cmd_phase8(args) -> int:
    """Run Phase 8 (per-mutation convergence)."""
    from bench import phases
    run_dir = getattr(args, "run_dir", None)
    try:
        phases.run_phase8_per_mutation(
            tag=getattr(args, "tag", None),
            run_dir=Path(run_dir) if run_dir else None,
        )
        return 0
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}phase8 FAILED: {type(e).__name__}: {e}{RESET}\n")
        return 1


def cmd_verify_serve_stale(args) -> int:
    """Run the serve-stale-while-refresh verifier (#159).

    Goes through _gke_context_guard first (the verifier mutates a
    bench-owned fixture). Returns the verifier's own exit code:
        0 PASS / 1 INDETERMINATE / 2 FAIL_* / 3 non-GKE.
    """
    _gke_context_guard(allow_non_gke=getattr(args, "allow_non_gke", False))
    from bench import verify_serve_stale
    run_dir = getattr(args, "run_dir", None) or _default_run_dir(args)
    Path(run_dir).mkdir(parents=True, exist_ok=True)
    try:
        exit_code, _bundle = verify_serve_stale.run_verify_serve_stale(
            user=getattr(args, "user", "admin"),
            target=getattr(args, "target", "widget-echo"),
            tag=getattr(args, "tag", None),
            run_dir=Path(run_dir),
            warmup=getattr(args, "warmup", None)
            if getattr(args, "warmup", None) is not None
            else verify_serve_stale._DEFAULT_WARMUP_PROBES,
            log=log,
            section=section,
        )
        return int(exit_code)
    except verify_serve_stale._VerifyError as e:
        sys.stderr.write(
            f"{RED}{BOLD}verify-serve-stale ERROR: {e}{RESET}\n")
        return 1
    except Exception as e:
        sys.stderr.write(
            f"{RED}{BOLD}verify-serve-stale FAILED: "
            f"{type(e).__name__}: {e}{RESET}\n")
        return 1


# ─── cmd_report (Block 4) ───────────────────────────────────────────────────


def cmd_report(args) -> int:
    """Read state.json + per-stage proofs; build ledger row + summary."""
    from bench import phases, ledger
    run_dir_arg = getattr(args, "run_dir", None)
    if not run_dir_arg:
        sys.stderr.write(
            f"{RED}report: --run-dir is required{RESET}\n")
        return 1
    run_dir = Path(run_dir_arg)
    try:
        state = phases.load_state(run_dir)
    except phases.IncompatibleStateSchema as e:
        sys.stderr.write(
            f"{RED}report: incompatible state.json schema: {e}{RESET}\n")
        return 2
    if not state:
        sys.stderr.write(
            f"{RED}report: state.json missing under {run_dir}{RESET}\n")
        return 1
    # Reconstruct all_results from the stage_proofs ingest hook is not
    # done today — the bench harness keeps Phase 6 measurements in the
    # ctx.all_results buffer during the live run and writes them via
    # ledger.write_run_bundle at S9. cmd_report supports the post-hoc
    # case where a run was interrupted before S9; we emit a row with
    # whatever the state.json captures (verdict will likely be INVALID).
    proofs = state.get("stage_proofs") or {}
    # Try to recover any all_results stored in S6 / S9 proof for a
    # backward-compat path. Today's stages don't stash all_results in
    # the proof body — this is a forward-compat shim for future migrations.
    fallback = []
    for sid, p in proofs.items():
        ar = (p.get("proof") or {}).get("all_results")
        if ar:
            fallback.extend(ar)
    row = ledger.write_run_bundle(
        run_dir, fallback,
        per_stage_proofs=proofs,
        tag=state.get("tag"),
        scale=state.get("scale"),
    )
    log(f"report: ledger row verdict={row['verdict']}")
    print(json.dumps(row, indent=2, default=str))
    return 0


# ─── argparse wiring ─────────────────────────────────────────────────────────


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="bench",
        description=(
            "Snowplow bench harness (Path B, 2026-06-02). Drives Playwright + "
            "cluster mutations + canonical ledger emission against the GKE "
            "benchmark cluster."
        ),
    )
    p.add_argument(
        "--allow-non-gke", action="store_true",
        help=argparse.SUPPRESS,  # hidden — kind/minikube only
    )

    sub = p.add_subparsers(dest="cmd")

    p_check = sub.add_parser(
        "check",
        help="Preflight (7 gates, 60s, no mutations).",
    )
    p_check.add_argument(
        "--tag", required=False, default=None,
        help="Expected image tag (e.g. 0.30.232). Falls back to "
             "EXPECTED_IMAGE_TAG env when unset.",
    )
    p_check.add_argument(
        "--allow-stale-overlay", action="store_true",
        help="Bypass the 14-day overlay freshness gate with a loud-log.",
    )
    p_check.set_defaults(func=cmd_check)

    # ─── calibrate ──────────────────────────────────────────────────────
    p_cal = sub.add_parser(
        "calibrate",
        help="Refresh EXPECTED_CALLS overlay against the live cluster.",
    )
    p_cal.add_argument("--tag", default=None,
                       help="Image tag context (logged only).")
    p_cal.set_defaults(func=cmd_calibrate)

    # ─── cleanup ────────────────────────────────────────────────────────
    p_clean = sub.add_parser(
        "cleanup",
        help="clean_environment + assert_clean (consults destructive guard).",
    )
    p_clean.add_argument("--allow-destructive", action="store_true",
                         help="Bypass destructive_clean_guard (DANGER).")
    p_clean.set_defaults(func=cmd_cleanup)

    # ─── storm ──────────────────────────────────────────────────────────
    p_storm = sub.add_parser(
        "storm",
        help="Disruptive scenarios: crb-delete, user-scaling.",
    )
    p_storm.add_argument("--scenario",
                         choices=["crb-delete", "user-scaling"],
                         default="crb-delete")
    p_storm.set_defaults(func=cmd_storm)

    # ─── converge / measure ─────────────────────────────────────────────
    def _add_stage_args(parser, allowed=None):
        parser.add_argument("--tag", default=None,
                            help="Image tag under test.")
        parser.add_argument("--scale", type=int, default=None,
                            help="Target composition count.")
        parser.add_argument("--stage", required=True,
                            help="Stage ID (S0..S9).")
        parser.add_argument("--cache-mode",
                            dest="cache_mode",
                            choices=["ON", "OFF"], default="OFF")
        parser.add_argument("--run-dir", dest="run_dir", default=None)

    p_conv = sub.add_parser("converge",
                            help="Run a single Phase 6 stage.")
    _add_stage_args(p_conv)
    p_conv.set_defaults(func=cmd_converge)

    p_meas = sub.add_parser("measure",
                            help="Single-stage measurement (no mutations).")
    _add_stage_args(p_meas)
    p_meas.set_defaults(func=cmd_measure)

    # ─── phase6 ─────────────────────────────────────────────────────────
    p_p6 = sub.add_parser(
        "phase6",
        help="Browser scaling (S1-S8) with --from-stage / --to-stage.",
    )
    p_p6.add_argument("--tag", default=None,
                      help="Image tag under test.")
    p_p6.add_argument("--scale", type=int, default=None,
                      help="Target composition count.")
    p_p6.add_argument("--from-stage", dest="from_stage", default=None,
                      help="Resume from this stage (S0..S9).")
    p_p6.add_argument("--to-stage", dest="to_stage", default=None,
                      help="Stop after this stage (S0..S9, inclusive).")
    p_p6.add_argument("--cache-mode", dest="cache_mode",
                      choices=["ON", "OFF", "BOTH"], default="OFF")
    p_p6.add_argument("--video", choices=["none", "representative", "all"],
                      default="representative")
    p_p6.add_argument("--run-dir", dest="run_dir", default=None,
                      help="Output bundle root.")
    p_p6.add_argument("--allow-stale-overlay", action="store_true",
                      dest="allow_stale_overlay")
    p_p6.set_defaults(func=cmd_phase6)

    # ─── phase7 / phase8 ────────────────────────────────────────────────
    p_p7 = sub.add_parser("phase7",
                          help="User scaling (synthetic users, warmup).")
    p_p7.add_argument("--tag", default=None)
    p_p7.add_argument("--run-dir", dest="run_dir", default=None)
    p_p7.set_defaults(func=cmd_phase7)

    p_p8 = sub.add_parser("phase8",
                          help="Per-mutation convergence (HOT/WARM/COLD).")
    p_p8.add_argument("--tag", default=None)
    p_p8.add_argument("--run-dir", dest="run_dir", default=None)
    p_p8.set_defaults(func=cmd_phase8)

    # ─── verify-serve-stale (#159) ──────────────────────────────────────
    p_vss = sub.add_parser(
        "verify-serve-stale",
        help="Serve-stale-while-refresh contract verifier "
             "(3 probes around one bench-fixture mutation).",
    )
    p_vss.add_argument("--tag", default=None,
                       help="Image tag context (logged + run-dir).")
    p_vss.add_argument(
        "--user", default="admin",
        help="Subject identity for the scored /call probes (default admin). "
             "cyberjoker is selectable but requires an active phase6 "
             "lifecycle granting a rolebinding into the bench fixture "
             "namespace bench-ns-01; standalone cyberjoker runs FAIL 403. "
             "The cache layer is subject-agnostic — admin exercises the "
             "identical widgets L1 cell + dirty-mark/refresh path.")
    p_vss.add_argument(
        "--target", default="widget-echo",
        help="Mutation target (default widget-echo — a bench-owned Button "
             "whose spec.widgetData.label echoes into the /call body).")
    p_vss.add_argument(
        "--warmup", type=int, default=None,
        help="Unscored warm-up /calls before the first scored probe "
             "(default 2). 0 disables.")
    p_vss.add_argument("--run-dir", dest="run_dir", default=None)
    p_vss.set_defaults(func=cmd_verify_serve_stale)

    # ─── report ─────────────────────────────────────────────────────────
    p_rep = sub.add_parser(
        "report",
        help="Read run state + emit canonical ledger row.",
    )
    p_rep.add_argument("--run-dir", dest="run_dir", required=True)
    p_rep.set_defaults(func=cmd_report)

    return p


def main(argv: list[str] | None = None) -> int:
    """Entry point for `python -m bench`.

    Returns the exit code; callers should `sys.exit(main())`.
    Per feedback_kubectl_verify_gke_context: bench.cluster's import-time
    guard runs first (already exited if non-GKE). Subcommand handlers
    receive args with `--allow-non-gke` available; tests bypass via env.
    """
    parser = _build_parser()
    args = parser.parse_args(argv)

    if args.cmd is None:
        parser.print_help()
        return 0

    func = getattr(args, "func", None)
    if func is None:
        parser.print_help()
        return 0
    try:
        return int(func(args))
    except SystemExit:
        raise
    except KeyboardInterrupt:
        return 130
