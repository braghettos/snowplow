"""Behavioural tests for bench/lifecycle.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.2. 14 cases:
- 9 cover the destructive-clean guard truth table (replaces the
  source's `_self_test_destructive_clean_guard`, but as real pytest
  parametrize rather than source introspection).
- 5 cover cluster_dirty_state aggregation + cache toggle dispatch +
  assert_clean call-trace.

NOTE: every test stays in-process — no kubectl, no cluster I/O. The
guard truth-table tests need ZERO patching; the toggle/dispatch tests
monkey-patch subprocess.run via the `mock_kubectl` fixture or the
local `_kubectl` patch.
"""

from __future__ import annotations

import subprocess
import sys
from unittest import mock

import pytest


import bench.lifecycle as lifecycle_mod
from bench.lifecycle import (
    SCALE_GUARD_BENCH_NAMESPACES,
    SCALE_GUARD_COMPOSITIONS,
    WIDGET_KINDS,
    assert_clean,
    cluster_dirty_state,
    destructive_clean_guard,
)


# ─── Destructive-clean guard truth table (9 cases) ──────────────────────────

@pytest.mark.parametrize(
    "label, state, allow_destructive, expect_blocks",
    [
        # (1) clean cluster — allow proceed
        ("clean cluster",
         {"compositions": 0, "bench_namespaces": 0}, False, False),
        # (2) tiny dev dirt — allow proceed
        ("small dev dirt (50 comps)",
         {"compositions": 50, "bench_namespaces": 2}, False, False),
        # (3) Phase-6 baseline — BLOCK
        ("Phase-6 baseline shape",
         {"compositions": 46539, "bench_namespaces": 49}, False, True),
        # (4) Phase-6 baseline + opt-in — allow
        ("Phase-6 baseline + opt-in override",
         {"compositions": 46539, "bench_namespaces": 49}, True, False),
        # (5) just-over composition threshold — BLOCK
        ("just-over-comp threshold",
         {"compositions": SCALE_GUARD_COMPOSITIONS + 1,
          "bench_namespaces": 0}, False, True),
        # (6) exactly at composition threshold — boundary, allow
        ("at-comp threshold (boundary, not blocked)",
         {"compositions": SCALE_GUARD_COMPOSITIONS,
          "bench_namespaces": 0}, False, False),
        # (7) just-over namespace threshold — BLOCK
        ("just-over-ns threshold",
         {"compositions": 0,
          "bench_namespaces": SCALE_GUARD_BENCH_NAMESPACES + 1}, False, True),
        # (8) exactly at namespace threshold — boundary, allow
        ("at-ns threshold (boundary, not blocked)",
         {"compositions": 0,
          "bench_namespaces": SCALE_GUARD_BENCH_NAMESPACES}, False, False),
        # (9) missing keys treated as zero
        ("missing keys are treated as zero", {}, False, False),
    ],
)
def test_destructive_clean_guard_truth_table(
        label, state, allow_destructive, expect_blocks):
    """Each (state, allow_destructive) pair → expected blocks verdict."""
    blocks, reason = destructive_clean_guard(state, allow_destructive)
    assert blocks is expect_blocks, (
        f"{label}: got blocks={blocks!r} expected {expect_blocks!r} "
        f"reason={reason!r}"
    )
    # When NOT blocked, reason must be empty; when blocked, reason carries
    # a numeric breadcrumb so the operator can audit.
    if not blocks:
        assert reason == ""
    else:
        assert reason  # non-empty


# ─── cluster_dirty_state aggregation (10) ───────────────────────────────────

def test_cluster_dirty_state_aggregates_all_categories(monkeypatch):
    """cluster_dirty_state() returns the full 12-category dict.

    Task #275 / #276-B.1 added two categories: `bench_namespaces_terminating`
    (Terminating bench-ns the `bench_namespaces` counter excludes) and
    `comp_panels` (cluster-wide composition-panel CRs).
    """

    # Patch all the count helpers to fixed values.
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns", lambda: 3)
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns_terminating", lambda: 2)
    monkeypatch.setattr(lifecycle_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: 41)
    monkeypatch.setattr(lifecycle_mod, "count_compositions", lambda: 7)
    monkeypatch.setattr(lifecycle_mod, "_crd_exists", lambda crd: True)
    # `_count` / `_count_match` / `_count_bench_argo` live in cluster.py;
    # patch them in lifecycle.py's namespace (re-exported via the import).
    monkeypatch.setattr(lifecycle_mod, "_count",
                        lambda res: {"compositiondefinitions.core.krateo.io": 1,
                                     "repoes.github.ogen.krateo.io": 5,
                                     "repoes.git.krateo.io": 11}[res])
    monkeypatch.setattr(lifecycle_mod, "_count_bench_argo", lambda: 13)
    monkeypatch.setattr(
        lifecycle_mod, "_count_match",
        lambda res, name_prefix="", ns_prefix="": {
            "clusterrole": 17,
            "clusterrolebinding": 19,
            "role": 23,
            "rolebinding": 29,
        }[res])

    state = cluster_dirty_state()
    assert set(state.keys()) == {
        "bench_namespaces", "bench_namespaces_terminating", "comp_panels",
        "compositions", "compositiondefinitions",
        "argo_apps_bench", "ogen_repoes", "git_repoes",
        "bench_clusterroles", "bench_clusterrolebindings",
        "bench_roles_namespaced", "bench_rolebindings_namespaced",
    }
    assert state["bench_namespaces"] == 3
    assert state["bench_namespaces_terminating"] == 2
    assert state["comp_panels"] == 41
    assert state["compositions"] == 7
    assert state["compositiondefinitions"] == 1
    assert state["argo_apps_bench"] == 13
    assert state["ogen_repoes"] == 5
    assert state["git_repoes"] == 11
    assert state["bench_clusterroles"] == 17
    assert state["bench_clusterrolebindings"] == 19
    assert state["bench_roles_namespaced"] == 23
    assert state["bench_rolebindings_namespaced"] == 29


def test_cluster_dirty_state_comp_panels_none_coerces_to_zero(monkeypatch):
    """Task #275: a transport failure in count_compositions_with_panels_ready
    (returns None) must coerce to 0 — a flaky k8s client must NEVER
    false-positive the cluster as dirty on the new comp_panels counter.
    """
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns", lambda: 0)
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns_terminating", lambda: 0)
    monkeypatch.setattr(lifecycle_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: None)
    monkeypatch.setattr(lifecycle_mod, "count_compositions", lambda: 0)
    monkeypatch.setattr(lifecycle_mod, "_crd_exists", lambda crd: True)
    monkeypatch.setattr(lifecycle_mod, "_count", lambda res: 0)
    monkeypatch.setattr(lifecycle_mod, "_count_bench_argo", lambda: 0)
    monkeypatch.setattr(
        lifecycle_mod, "_count_match",
        lambda res, name_prefix="", ns_prefix="": 0)
    state = cluster_dirty_state()
    assert state["comp_panels"] == 0
    # The whole dict is clean → assert_clean would short-circuit.
    assert {k: v for k, v in state.items() if v > 0} == {}


def test_cluster_dirty_state_zeros_compositions_when_crd_missing(monkeypatch):
    """If the composition CRD is missing, compositions count is 0 (no kubectl call)."""
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns", lambda: 0)
    monkeypatch.setattr(lifecycle_mod, "count_bench_ns_terminating", lambda: 0)
    monkeypatch.setattr(lifecycle_mod, "count_compositions_with_panels_ready",
                        lambda target_ns=None: 0)
    monkeypatch.setattr(lifecycle_mod, "_crd_exists", lambda crd: False)
    # Whoever calls count_compositions when CRD missing is a bug — fail loudly:
    monkeypatch.setattr(lifecycle_mod, "count_compositions",
                        lambda: pytest.fail("count_compositions called with CRD missing"))
    monkeypatch.setattr(lifecycle_mod, "_count", lambda res: 0)
    monkeypatch.setattr(lifecycle_mod, "_count_bench_argo", lambda: 0)
    monkeypatch.setattr(
        lifecycle_mod, "_count_match",
        lambda res, name_prefix="", ns_prefix="": 0)
    state = cluster_dirty_state()
    assert state["compositions"] == 0


# ─── assert_clean call-trace (12) ────────────────────────────────────────────

def test_assert_clean_short_circuits_when_clean(monkeypatch):
    """When dirty_state returns all-zero, assert_clean returns without cleanup."""
    monkeypatch.setattr(lifecycle_mod, "cluster_dirty_state",
                        lambda: {"bench_namespaces": 0, "compositions": 0})

    def _no_clean():
        raise AssertionError("clean_environment must NOT be called when clean")

    monkeypatch.setattr(lifecycle_mod, "clean_environment", _no_clean)
    assert_clean()  # silent pass


def test_assert_clean_invokes_clean_environment_when_dirty(monkeypatch):
    """When dirty + guard does not block + retry_with_cleanup, cleanup runs."""
    call_log = []
    # First call returns dirty; after cleanup, returns clean.
    states = iter([
        {"bench_namespaces": 5, "compositions": 10},  # dirty
        {"bench_namespaces": 0, "compositions": 0},   # clean after cleanup
    ])
    monkeypatch.setattr(lifecycle_mod, "cluster_dirty_state",
                        lambda: next(states))
    monkeypatch.setattr(lifecycle_mod, "clean_environment",
                        lambda: call_log.append("clean_environment"))
    assert_clean()
    assert call_log == ["clean_environment"]


def test_assert_clean_respects_destructive_guard(monkeypatch):
    """When dirty + guard blocks, clean_environment is NOT called."""
    monkeypatch.setattr(lifecycle_mod, "cluster_dirty_state",
                        lambda: {"bench_namespaces": 49, "compositions": 46539})

    def _no_clean():
        raise AssertionError("clean_environment must NOT be called when guard blocks")

    monkeypatch.setattr(lifecycle_mod, "clean_environment", _no_clean)
    assert_clean()  # silent pass — guard blocked, no cleanup


def test_assert_clean_raises_when_still_dirty_after_cleanup(monkeypatch):
    """When cleanup runs and the cluster is STILL dirty, RuntimeError raises."""
    states = iter([
        {"bench_namespaces": 5},  # dirty pre-clean
        {"bench_namespaces": 1},  # still dirty post-clean
    ])
    monkeypatch.setattr(lifecycle_mod, "cluster_dirty_state",
                        lambda: next(states))
    monkeypatch.setattr(lifecycle_mod, "clean_environment", lambda: None)
    with pytest.raises(RuntimeError, match="STILL dirty"):
        assert_clean()


# ─── Cache toggle dispatch (3) ──────────────────────────────────────────────

def test_chart_supports_cache_toggle_returns_true_when_env_present(monkeypatch):
    """The kubectl jsonpath probe returns 'CACHE_ENABLED' when env wired."""
    monkeypatch.setattr(lifecycle_mod, "kubectl",
                        lambda *a, **kw: (0, "CACHE_ENABLED", ""))
    assert lifecycle_mod._chart_supports_cache_toggle() is True


def test_chart_supports_cache_toggle_returns_false_when_env_absent(monkeypatch):
    """The kubectl jsonpath probe returns empty when CACHE_ENABLED absent."""
    monkeypatch.setattr(lifecycle_mod, "kubectl",
                        lambda *a, **kw: (0, "", ""))
    assert lifecycle_mod._chart_supports_cache_toggle() is False


def test_set_cache_uses_helm_not_kubectl_set_env(monkeypatch):
    """_set_cache_via_helm dispatches `helm upgrade`, never `kubectl set env`.

    This is THE rule from feedback_chart_only_for_snowplow.md. Critical
    that we never regress to the kubectl-set-env path.
    """
    # Pretend the chart supports the toggle.
    monkeypatch.setattr(lifecycle_mod, "_chart_supports_cache_toggle",
                        lambda: True)
    captured_argv = []

    def _fake_run(argv, **kwargs):
        captured_argv.append(list(argv))

        class _R:
            returncode = 0
            stdout = ""
            stderr = ""
        return _R()

    monkeypatch.setattr(subprocess, "run", _fake_run)
    assert lifecycle_mod._set_cache_via_helm(True) is True
    # We MUST have invoked helm, NEVER kubectl set env.
    assert any(a and a[0] == "helm" for a in captured_argv), (
        f"_set_cache_via_helm did not run helm; argvs={captured_argv}"
    )
    assert all("set" != argv[0] for argv in captured_argv
               if argv and argv[0] == "kubectl"), (
        f"_set_cache_via_helm invoked kubectl set; argvs={captured_argv}"
    )
    # The argv must carry --reuse-values and the chart-values key.
    helm_argv = next(a for a in captured_argv if a and a[0] == "helm")
    assert "--reuse-values" in helm_argv
    assert any("env.CACHE_ENABLED=true" in token for token in helm_argv)


# ─── WIDGET_KINDS shape (14) ─────────────────────────────────────────────────

def test_widget_kinds_includes_panels_and_restactions():
    """WIDGET_KINDS covers panels (the 908-CR ghost case 2026-05-05) plus
    RESTActions. The full list is large; pin a few critical entries so a
    deletion regression is caught.
    """
    assert "panels.widgets.templates.krateo.io" in WIDGET_KINDS
    assert "restactions.templates.krateo.io" in WIDGET_KINDS
    assert "routesloaders.widgets.templates.krateo.io" in WIDGET_KINDS
    assert "navmenus.widgets.templates.krateo.io" in WIDGET_KINDS
    # Every entry MUST be a CRD-style "plural.group" string.
    for kind in WIDGET_KINDS:
        assert "." in kind, f"WIDGET_KINDS entry {kind!r} is not a plural.group string"


# ─── #320: _flush_snowplow_cache — verifier-only, the bench NEVER deploys ────


def _flush_kubectl_recorder(live_image="ghcr.io/krateo-platformops/snowplow:0.30.258"):
    """Recording kubectl stub: GET deployment returns `live_image`; every
    other call (rollout restart/status) succeeds."""
    calls = []

    def fake_kubectl(*args, **kwargs):
        calls.append(args)
        if args[:2] == ("get", "deployment"):
            return (0, live_image, "")
        return (0, "", "")

    return calls, fake_kubectl


def test_flush_verifies_matching_image_and_never_deploys(monkeypatch):
    """EXPECTED_IMAGE_TAG matches the live image → verify, then restart.
    The class kill (#320 / #319 trace): NO `kubectl set image` ever issued
    (feedback_chart_only_for_snowplow + feedback_chart_release_lockstep)."""
    calls, fake = _flush_kubectl_recorder()
    monkeypatch.setattr(lifecycle_mod, "kubectl", fake)
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    monkeypatch.setenv("EXPECTED_IMAGE_TAG", "0.30.258")

    lifecycle_mod._flush_snowplow_cache()

    assert not any("set" in c for c in calls)
    assert any(c[:2] == ("rollout", "restart") for c in calls)
    assert any(c[:2] == ("rollout", "status") for c in calls)


def test_flush_raises_on_image_mismatch_before_any_mutation(monkeypatch):
    """Live image behind the expected tag → RuntimeError directing the
    operator to helm-upgrade; nothing mutated (no set-image, no restart)."""
    calls, fake = _flush_kubectl_recorder(
        live_image="ghcr.io/krateo-platformops/snowplow:0.30.257")
    monkeypatch.setattr(lifecycle_mod, "kubectl", fake)
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    monkeypatch.setenv("EXPECTED_IMAGE_TAG", "0.30.258")

    with pytest.raises(RuntimeError, match="does not deploy"):
        lifecycle_mod._flush_snowplow_cache()

    assert not any("set" in c for c in calls)
    assert not any(c[:2] == ("rollout", "restart") for c in calls)


def test_flush_unreadable_image_raises_not_silently_restarts(monkeypatch):
    """kubectl get failing while a tag IS expected must abort, not flush a
    pod whose image we could not verify."""
    calls = []

    def fake_kubectl(*args, **kwargs):
        calls.append(args)
        return (1, "", "boom")

    monkeypatch.setattr(lifecycle_mod, "kubectl", fake_kubectl)
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    monkeypatch.setenv("EXPECTED_IMAGE_TAG", "0.30.258")

    with pytest.raises(RuntimeError, match="unreadable"):
        lifecycle_mod._flush_snowplow_cache()

    assert not any(c[:2] == ("rollout", "restart") for c in calls)


def test_flush_without_expected_tag_skips_verify_and_restarts(monkeypatch):
    """No EXPECTED_IMAGE_TAG → legacy behavior: no verify read, straight to
    the flush-restart (which stays — correct between-run hygiene)."""
    calls, fake = _flush_kubectl_recorder()
    monkeypatch.setattr(lifecycle_mod, "kubectl", fake)
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    monkeypatch.delenv("EXPECTED_IMAGE_TAG", raising=False)

    lifecycle_mod._flush_snowplow_cache()

    assert not any(c[:2] == ("get", "deployment") for c in calls)
    assert any(c[:2] == ("rollout", "restart") for c in calls)


# ─── Diego hard rule 2026-06-11: bench helm upgrades pin --kube-context ─────


def test_set_cache_via_helm_pins_kube_context(monkeypatch):
    """The cache-toggle helm upgrade MUST carry --kube-context to the
    canonical GKE context — an unpinned helm upgrade on a drifted
    current-context would mutate the WRONG cluster."""
    import subprocess as subprocess_mod
    import bench.cluster as cluster_mod

    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)
    monkeypatch.setattr(lifecycle_mod, "_chart_supports_cache_toggle",
                        lambda: True)
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    seen = {}

    class _P:
        returncode = 0
        stdout = ""
        stderr = ""

    def fake_run(argv, **kwargs):
        seen["argv"] = list(argv)
        return _P()

    monkeypatch.setattr(subprocess_mod, "run", fake_run)

    assert lifecycle_mod._set_cache_via_helm(True) is True
    argv = seen["argv"]
    assert argv[0] == "helm" and argv[1] == "upgrade"
    assert "--kube-context" in argv
    assert argv[argv.index("--kube-context") + 1] == \
        cluster_mod.CANONICAL_GKE_CONTEXT


# ─── Task #216: one-shot RESTAction count snapshot (telemetry) ──────────────


def test_restaction_count_snapshot_one_shot_none_safe(monkeypatch):
    """Task #216: restaction_count_snapshot() replaces the deleted blocking
    wait_for_restaction_steady_state. It is ONE kubectl call (no loop, no
    timeout, no stability streak), returns the line count on rc==0, and
    None on kubectl failure (never raises into S6)."""
    monkeypatch.setattr(lifecycle_mod, "log", lambda *a, **k: None)
    calls = {"n": 0}

    def _ok(*a, **kw):
        calls["n"] += 1
        return (0, "restaction/a\nrestaction/b\n\nrestaction/c\n", "")

    monkeypatch.setattr(lifecycle_mod, "kubectl", _ok)
    # Blank lines are not counted; exactly one kubectl invocation (no poll loop).
    assert lifecycle_mod.restaction_count_snapshot() == 3
    assert calls["n"] == 1

    # rc != 0 -> None (FAIL-soft telemetry, never blocks/raises).
    monkeypatch.setattr(lifecycle_mod, "kubectl",
                        lambda *a, **kw: (1, "", "boom"))
    assert lifecycle_mod.restaction_count_snapshot() is None


def test_wait_for_restaction_steady_state_is_deleted():
    """Task #216 regression guard: the blocking wait is gone from the module
    and its public __all__ export (no dead blocking code left behind)."""
    assert not hasattr(lifecycle_mod, "wait_for_restaction_steady_state")
    assert "wait_for_restaction_steady_state" not in lifecycle_mod.__all__
    assert "restaction_count_snapshot" in lifecycle_mod.__all__
