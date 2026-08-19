"""Behavioural tests for the `bench check` preflight gate.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.8 — the
check-related cases. The rest of test_cli.py (calibrate, phase6, etc.)
lands in Block 4.

Cases:
  1. _gke_context_guard exits 3 on non-GKE context.
  2. _gke_context_guard passes on the canonical cluster.
  3. _gke_context_guard --allow-non-gke bypasses the gate.
  4. cmd_check exits 2 with named failures (acceptance criterion (e)).
  5. cmd_check passes all 7 gates with stubs (smoke).
  6. cmd_check FAIL when --tag is missing.

No source-introspection. No live cluster. Every test stays in-process.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
from unittest import mock

import pytest


import bench.cli as cli_mod
from bench.cli import (
    _gke_context_guard,
    cmd_check,
    verify_deployed_image,
)
import bench.cluster as cluster_mod


# ─── Case 1: GKE context guard exits 3 on non-GKE ───────────────────────────


def test_gke_context_guard_exits_3_on_non_gke_context(monkeypatch):
    """Non-canonical kubectl context → guard exits with code 3."""
    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, "kind-some-other-cluster\n")

    monkeypatch.setattr(subprocess, "run", _run)
    with pytest.raises(SystemExit) as exc_info:
        _gke_context_guard()
    assert exc_info.value.code == 3


# ─── Case 2: GKE context guard passes on the canonical cluster ──────────────


def test_gke_context_guard_passes_on_neon_481711_cluster(monkeypatch):
    """Canonical kubectl context → guard returns the context string."""
    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)
    canonical = cluster_mod.CANONICAL_GKE_CONTEXT

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, canonical + "\n")

    monkeypatch.setattr(subprocess, "run", _run)
    ctx = _gke_context_guard()
    assert ctx == canonical


# ─── Case 3: --allow-non-gke (or env override) bypasses the gate ────────────


def test_gke_context_guard_allow_non_gke_flag_bypasses(monkeypatch):
    """Passing allow_non_gke=True bypasses the gate even on a wrong
    context — escape hatch for kind/minikube local dev."""
    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        # Wrong context — but allow_non_gke should bypass anyway.
        return _Fake(0, "kind-local\n")

    monkeypatch.setattr(subprocess, "run", _run)
    # Must NOT raise.
    ctx = _gke_context_guard(allow_non_gke=True)
    # Returns the actual current-context for logging.
    assert ctx == "kind-local"


# ─── Case 4: cmd_check returns 2 with named gate failures ───────────────────


def test_check_command_exits_2_with_named_failures(monkeypatch, capsys):
    """When any of gates 2-7 fails, cmd_check returns 2 AND every failed
    gate name appears on stderr — acceptance criterion (e).
    """
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    # Stub every gate to FAIL with a deterministic name.
    monkeypatch.setattr(cli_mod, "_gate_pod_ready",
                        lambda: (False, "snowplow_pod_ready: FAIL (stub)"))
    monkeypatch.setattr(cli_mod, "_gate_image_match",
                        lambda tag: (False, "image_tag_match: FAIL (stub)"))
    monkeypatch.setattr(cli_mod, "_gate_crds_present",
                        lambda timeout=10: (False, "crds_present: FAIL (stub)"))
    monkeypatch.setattr(cli_mod, "_gate_helm_lockstep",
                        lambda tag: (False, "helm_release_lockstep: FAIL (stub)"))
    monkeypatch.setattr(cli_mod, "_gate_frontend_reachable",
                        lambda: (False, "frontend_lb_reachable: FAIL (stub)"))
    monkeypatch.setattr(cli_mod, "_gate_overlay_freshness",
                        lambda allow_stale: (False, "overlay_freshness: FAIL (stub)"))
    # Wedge the GKE guard (so it doesn't try kubectl).
    monkeypatch.setattr(cli_mod, "_gke_context_guard",
                        lambda allow_non_gke=False: "stub-context")

    args = argparse.Namespace(
        cmd="check", tag="0.30.232",
        allow_non_gke=True, allow_stale_overlay=False,
    )
    rc = cmd_check(args)
    assert rc == 2
    err = capsys.readouterr().err
    # Every failed gate must be named on stderr.
    for name in (
        "snowplow_pod_ready",
        "image_tag_match",
        "crds_present",
        "helm_release_lockstep",
        "frontend_lb_reachable",
        "overlay_freshness",
    ):
        assert name in err, f"missing gate name on stderr: {name!r}; err=\n{err}"


# ─── Case 5: cmd_check passes all 7 gates with stubs ────────────────────────


def test_check_command_passes_all_7_gates(monkeypatch, capsys):
    """All gates pass → cmd_check returns 0."""
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    monkeypatch.setattr(cli_mod, "_gate_pod_ready",
                        lambda: (True, "snowplow_pod_ready: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_image_match",
                        lambda tag: (True, "image_tag_match: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_crds_present",
                        lambda timeout=10: (True, "crds_present: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_helm_lockstep",
                        lambda tag: (True, "helm_release_lockstep: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_frontend_reachable",
                        lambda: (True, "frontend_lb_reachable: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_overlay_freshness",
                        lambda allow_stale: (True, "overlay_freshness: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (True, "k8s_client: PASS"))
    monkeypatch.setattr(cli_mod, "_gke_context_guard",
                        lambda allow_non_gke=False: "stub-context")

    args = argparse.Namespace(
        cmd="check", tag="0.30.232",
        allow_non_gke=True, allow_stale_overlay=False,
    )
    rc = cmd_check(args)
    assert rc == 0
    err = capsys.readouterr().err
    # No FAIL lines on stderr.
    assert "FAIL" not in err


# ─── Case 6: cmd_check fails when --tag is missing ──────────────────────────


def test_check_command_fails_without_tag(monkeypatch, capsys):
    """Missing --tag AND missing EXPECTED_IMAGE_TAG → image+helm gates FAIL."""
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    monkeypatch.delenv("EXPECTED_IMAGE_TAG", raising=False)
    # Other gates: stub PASS so we isolate the no-tag failure.
    monkeypatch.setattr(cli_mod, "_gate_pod_ready",
                        lambda: (True, "snowplow_pod_ready: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_crds_present",
                        lambda timeout=10: (True, "crds_present: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_frontend_reachable",
                        lambda: (True, "frontend_lb_reachable: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_overlay_freshness",
                        lambda allow_stale: (True, "overlay_freshness: PASS"))
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (True, "k8s_client: PASS"))
    monkeypatch.setattr(cli_mod, "_gke_context_guard",
                        lambda allow_non_gke=False: "stub-context")

    args = argparse.Namespace(
        cmd="check", tag=None,
        allow_non_gke=True, allow_stale_overlay=False,
    )
    rc = cmd_check(args)
    assert rc == 2
    err = capsys.readouterr().err
    assert "image_tag_match" in err
    assert "helm_release_lockstep" in err


# ─── Bonus: verify_deployed_image is testable + returns bool ────────────────


def test_verify_deployed_image_matches(monkeypatch):
    """verify_deployed_image returns True when the deployment carries the
    expected tag (replaces sys.exit on the source 951-984 version)."""
    monkeypatch.delenv("SKIP_IMAGE_CHECK", raising=False)

    def fake_kubectl(*args, **kwargs):
        # The function calls `kubectl get deployment snowplow ... -o jsonpath=...`
        return (0, "ghcr.io/krateo-platformops/snowplow:0.30.232", "")

    monkeypatch.setattr(cli_mod, "kubectl", fake_kubectl)
    assert verify_deployed_image(expected_tag="0.30.232") is True


def test_verify_deployed_image_mismatch_returns_false(monkeypatch, capsys):
    """verify_deployed_image returns False on tag mismatch (was sys.exit(1)
    in the source). Mismatch banner lands on stderr."""
    monkeypatch.delenv("SKIP_IMAGE_CHECK", raising=False)

    def fake_kubectl(*args, **kwargs):
        return (0, "ghcr.io/krateo-platformops/snowplow:0.30.999", "")

    monkeypatch.setattr(cli_mod, "kubectl", fake_kubectl)
    assert verify_deployed_image(expected_tag="0.30.232") is False
    err = capsys.readouterr().err
    assert "0.30.232" in err
    assert "0.30.999" in err
