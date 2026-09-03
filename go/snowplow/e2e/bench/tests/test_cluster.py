"""Behavioural tests for bench/cluster.py.

Per docs/bench-restructure-path-b-plan-2026-06-02.md §C.1. No source
introspection — only real input → real output assertions.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from unittest import mock

import pytest


# ─── Import the module under test ────────────────────────────────────────────

# conftest.py adds e2e/bench/ to sys.path AND sets BENCH_ALLOW_NON_GKE=1.

import bench.cluster as cluster_mod
from bench.cluster import (
    CANONICAL_GKE_CONTEXT,
    call_url,
    composition_yaml,
    gke_context_guard,
    k8s_split_gvr,
    kubectl,
    pct,
)


# ─── G6 fixture: optionally exercise the gate against the real cluster ──────

@pytest.fixture
def with_env(monkeypatch):
    """Set env vars for the duration of a test."""
    def _set(**kwargs):
        for k, v in kwargs.items():
            if v is None:
                monkeypatch.delenv(k, raising=False)
            else:
                monkeypatch.setenv(k, v)
    return _set


# ─── Case 1: gke_context_guard exits on wrong context ───────────────────────

def test_gke_context_guard_exits_on_wrong_context(monkeypatch, with_env):
    """When kubectl current-context != canonical GKE, guard exits with 3."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, "kind-some-other-cluster\n")

    monkeypatch.setattr(subprocess, "run", _run)

    with pytest.raises(SystemExit) as exc_info:
        gke_context_guard()
    assert exc_info.value.code == 3


def test_gke_context_guard_passes_on_canonical_cluster(monkeypatch, with_env):
    """When kubectl current-context == canonical GKE, guard returns silently."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    class _Fake:
        def __init__(self, rc, stdout):
            self.returncode = rc
            self.stdout = stdout.encode()

    def _run(argv, **kwargs):
        return _Fake(0, CANONICAL_GKE_CONTEXT + "\n")

    monkeypatch.setattr(subprocess, "run", _run)
    # Should NOT raise / exit.
    gke_context_guard()


def test_gke_context_guard_bypassed_by_env_flag(monkeypatch, with_env):
    """BENCH_ALLOW_NON_GKE=1 bypasses the gate even on a kind cluster."""
    with_env(BENCH_ALLOW_NON_GKE="1")

    def _run(argv, **kwargs):
        raise AssertionError("kubectl should not be invoked when env flag set")

    monkeypatch.setattr(subprocess, "run", _run)
    gke_context_guard()  # no exception, no kubectl invocation


# ─── Case 2/3: kubectl wrapper success + failure paths ──────────────────────

def test_kubectl_returns_tuple_on_success(monkeypatch):
    """kubectl(...) returns (rc, stdout_str, stderr_str)."""

    class _Fake:
        def __init__(self):
            self.returncode = 0
            self.stdout = b"hello\n"
            self.stderr = b""

    monkeypatch.setattr(subprocess, "run", lambda *a, **kw: _Fake())
    rc, out, err = kubectl("version")
    assert rc == 0
    assert out == "hello"  # stripped
    assert err == ""


def test_kubectl_returns_rc1_on_timeout(monkeypatch):
    """A subprocess timeout returns rc=1 + a 'timed out' stderr."""
    def _raise(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout"))

    monkeypatch.setattr(subprocess, "run", _raise)
    rc, out, err = kubectl("get", "pods", timeout_secs=5)
    assert rc == 1
    assert out == ""
    assert "timed out" in err
    assert "5s" in err


# ─── kubectl wrapper context-injection (feedback_bench_tests_hermetic_isolation_all_cluster_paths)

def test_kubectl_injects_canonical_context_when_not_pinned(monkeypatch, with_env):
    """kubectl() pins --context=CANONICAL_GKE_CONTEXT when caller didn't."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    seen_argv = {}

    class _Fake:
        returncode = 0
        stdout = b""
        stderr = b""

    def _run(argv, **kwargs):
        seen_argv["argv"] = list(argv)
        return _Fake()

    monkeypatch.setattr(subprocess, "run", _run)
    kubectl("get", "ns")
    assert seen_argv["argv"][0] == "kubectl"
    assert f"--context={CANONICAL_GKE_CONTEXT}" in seen_argv["argv"]
    assert seen_argv["argv"].index(f"--context={CANONICAL_GKE_CONTEXT}") == 1


def test_kubectl_no_inject_when_caller_pinned_context(monkeypatch, with_env):
    """If caller already passes --context=..., no double-injection."""
    with_env(BENCH_ALLOW_NON_GKE=None)

    seen_argv = {}

    class _Fake:
        returncode = 0
        stdout = b""
        stderr = b""

    def _run(argv, **kwargs):
        seen_argv["argv"] = list(argv)
        return _Fake()

    monkeypatch.setattr(subprocess, "run", _run)
    kubectl("--context=foo", "get", "ns")
    # Only the caller's context flag should appear; no canonical injection.
    contexts = [a for a in seen_argv["argv"]
                if isinstance(a, str) and a.startswith("--context")]
    assert contexts == ["--context=foo"]


def test_kubectl_no_inject_when_env_bypass_set(monkeypatch, with_env):
    """BENCH_ALLOW_NON_GKE=1 disables canonical context injection."""
    with_env(BENCH_ALLOW_NON_GKE="1")

    seen_argv = {}

    class _Fake:
        returncode = 0
        stdout = b""
        stderr = b""

    def _run(argv, **kwargs):
        seen_argv["argv"] = list(argv)
        return _Fake()

    monkeypatch.setattr(subprocess, "run", _run)
    kubectl("get", "ns")
    contexts = [a for a in seen_argv["argv"]
                if isinstance(a, str) and a.startswith("--context")]
    assert contexts == []


# ─── BENCH_GKE_CONTEXT override (H-1) ───────────────────────────────────────
#
# The shipped CANONICAL_GKE_CONTEXT names a cluster whose GCP project no
# longer exists, so the harness as written pins a dead context on every live
# cluster. BENCH_GKE_CONTEXT retargets that pin. These arms exist to prove the
# override RETARGETS the pin rather than WEAKENING it — the distinction that
# matters, because the only pre-existing way to reach a non-neon cluster was
# BENCH_ALLOW_NON_GKE=1, which removes pinning entirely and would let a
# `helm upgrade` land on whatever current-context happens to name.

_OVERRIDE_CTX = "gke_operations-dev-krateo-io_europe-west3-a_krateo-057"


def _fake_run_factory(seen, stdout=b""):
    """subprocess.run stub that records argv and returns a success result."""

    class _Fake:
        returncode = 0
        stderr = b""

    def _run(argv, **kwargs):
        seen["argv"] = list(argv)
        result = _Fake()
        result.stdout = stdout
        return result

    return _run


def test_canonical_gke_context_falls_back_to_constant(with_env):
    """Unset (or blank) BENCH_GKE_CONTEXT → the hard-coded constant."""
    with_env(BENCH_GKE_CONTEXT=None)
    assert cluster_mod.canonical_gke_context() == CANONICAL_GKE_CONTEXT
    # Whitespace-only is treated as unset, not as a context named "  ".
    with_env(BENCH_GKE_CONTEXT="   ")
    assert cluster_mod.canonical_gke_context() == CANONICAL_GKE_CONTEXT


def test_canonical_gke_context_env_override(with_env):
    """BENCH_GKE_CONTEXT wins, and is read at CALL time (not import time)."""
    with_env(BENCH_GKE_CONTEXT=_OVERRIDE_CTX)
    assert cluster_mod.canonical_gke_context() == _OVERRIDE_CTX
    assert cluster_mod.canonical_gke_context() != CANONICAL_GKE_CONTEXT


def test_kubectl_injects_overridden_context(monkeypatch, with_env):
    """kubectl() pins the OVERRIDE — still position 1, still exactly one."""
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)

    seen = {}
    monkeypatch.setattr(subprocess, "run", _fake_run_factory(seen))
    kubectl("get", "ns")

    assert seen["argv"][1] == f"--context={_OVERRIDE_CTX}"
    contexts = [a for a in seen["argv"]
                if isinstance(a, str) and a.startswith("--context")]
    assert contexts == [f"--context={_OVERRIDE_CTX}"]
    # The dead constant must NOT survive anywhere in the invocation.
    assert CANONICAL_GKE_CONTEXT not in " ".join(seen["argv"])


def test_helm_context_args_use_override(with_env):
    """helm pins the override too — an unpinned `helm upgrade` is the
    scenario that would mutate the wrong cluster."""
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)
    assert cluster_mod.helm_context_args() == ["--kube-context", _OVERRIDE_CTX]


def test_kubectl_context_args_use_override(with_env):
    """The direct-Popen streamers (port-forward, logs -f) pin the override."""
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)
    assert cluster_mod.kubectl_context_args() == [f"--context={_OVERRIDE_CTX}"]


def test_gke_context_guard_passes_on_overridden_context(monkeypatch, with_env):
    """current-context == the override → guard returns silently."""
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)

    seen = {}
    monkeypatch.setattr(
        subprocess, "run",
        _fake_run_factory(seen, stdout=(_OVERRIDE_CTX + "\n").encode()))
    gke_context_guard()  # must not raise


def test_gke_context_guard_still_exits_on_stale_constant(monkeypatch, with_env):
    """THE safety arm: with the override set, sitting on the OLD canonical
    context must still exit 3.

    A "fix" that merely widened the guard into an allow-anything check would
    pass every other arm here and fail this one.
    """
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)

    seen = {}
    monkeypatch.setattr(
        subprocess, "run",
        _fake_run_factory(seen, stdout=(CANONICAL_GKE_CONTEXT + "\n").encode()))

    with pytest.raises(SystemExit) as exc_info:
        gke_context_guard()
    assert exc_info.value.code == 3


def test_allow_non_gke_still_bypasses_with_override_set(monkeypatch, with_env):
    """The kind/minikube escape hatch is unchanged: BENCH_ALLOW_NON_GKE=1
    removes pinning even when BENCH_GKE_CONTEXT names a cluster."""
    with_env(BENCH_ALLOW_NON_GKE="1", BENCH_GKE_CONTEXT=_OVERRIDE_CTX)

    seen = {}
    monkeypatch.setattr(subprocess, "run", _fake_run_factory(seen))
    kubectl("get", "ns")

    contexts = [a for a in seen["argv"]
                if isinstance(a, str) and a.startswith("--context")]
    assert contexts == []
    assert cluster_mod.helm_context_args() == []
    assert cluster_mod.kubectl_context_args() == []


def test_k8s_client_pins_overridden_context(monkeypatch, with_env,
                                            reset_k8s_state):
    """The in-process kubernetes client pins the override, so it and the
    subprocess kubectl calls cannot diverge onto two different clusters."""
    with_env(BENCH_ALLOW_NON_GKE=None, BENCH_GKE_CONTEXT=_OVERRIDE_CTX)
    cluster_mod = reset_k8s_state

    seen = {}

    class _FakeConfigMod:
        @staticmethod
        def load_kube_config(context=None):
            seen["context"] = context

        @staticmethod
        def load_incluster_config():
            raise AssertionError("must not fall back to in-cluster config")

    class _FakeApi:
        def __init__(self, *a, **kw):
            pass

    class _FakeClientMod:
        CoreV1Api = _FakeApi
        RbacAuthorizationV1Api = _FakeApi
        CustomObjectsApi = _FakeApi
        ApiextensionsV1Api = _FakeApi

    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    monkeypatch.setattr(cluster_mod, "_k8s_config_mod", _FakeConfigMod)
    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    monkeypatch.setattr(cluster_mod, "K8S_CLIENT_AVAILABLE", False)
    monkeypatch.setattr(cluster_mod, "_k8s_init_attempted", False)

    assert cluster_mod._k8s_init() is True
    assert seen["context"] == _OVERRIDE_CTX


# ─── Case 4/5: _k8s_init idempotence + one-shot retry semantics ─────────────

def test_k8s_init_short_circuits_when_lib_missing(reset_k8s_state):
    """When `_K8S_LIB_AVAILABLE=False`, `_k8s_init` returns False without retry."""
    cluster_mod = reset_k8s_state
    with mock.patch.object(cluster_mod, "_K8S_LIB_AVAILABLE", False):
        assert cluster_mod._k8s_init() is False
        # Second call also returns False (no side effects, no kubeconfig load).
        assert cluster_mod._k8s_init() is False


def test_k8s_init_one_shot_attempt_lock(reset_k8s_state, monkeypatch):
    """After a failed init attempt, `_k8s_init_attempted=True` short-circuits."""
    cluster_mod = reset_k8s_state
    # Simulate lib available but load_kube_config + load_incluster_config both fail.
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    cluster_mod._k8s_init_attempted = False

    class _FakeConfig:
        @staticmethod
        def load_kube_config():
            raise RuntimeError("no kubeconfig")

        @staticmethod
        def load_incluster_config():
            raise RuntimeError("not in-cluster")

    monkeypatch.setattr(cluster_mod, "_k8s_config_mod", _FakeConfig)
    # First call attempts + fails.
    assert cluster_mod._k8s_init() is False
    assert cluster_mod._k8s_init_attempted is True
    # Second call short-circuits (without re-trying load_kube_config).
    call_count = [0]

    def _counting_load_kube_config():
        call_count[0] += 1
        raise RuntimeError("should not be called again")

    monkeypatch.setattr(_FakeConfig, "load_kube_config", _counting_load_kube_config)
    assert cluster_mod._k8s_init() is False
    assert call_count[0] == 0


# ─── Case 6/7: k8s_split_gvr parses and rejects bad input ────────────────────

def test_k8s_split_gvr_parses_canonical_strings():
    """`plural.group` parses into (group, plural)."""
    assert k8s_split_gvr("compositions.composition.krateo.io") == \
        ("composition.krateo.io", "compositions")
    assert k8s_split_gvr("applications.argoproj.io") == ("argoproj.io", "applications")


def test_k8s_split_gvr_rejects_bare_input():
    """A string with no '.' returns (None, original)."""
    g, p = k8s_split_gvr("nogroup")
    assert g is None
    assert p == "nogroup"


# ─── Case 8: k8s_list_cluster_custom returns [] when CRD missing ────────────

def test_k8s_list_cluster_custom_returns_empty_when_missing(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound is swallowed and returns [] (not None or raise)."""
    cluster_mod = reset_k8s_state
    # Pretend k8s client lib available + init succeeded.
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            super().__init__(f"status={status}")
            self.status = status

    # Patch the kubernetes module's exception class so `_k8s_is_404` works.
    class _FakeExceptions:
        ApiException = _FakeAPIException

    class _FakeKubeClient:
        exceptions = _FakeExceptions

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeKubeClient)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural, _request_timeout):
            raise _FakeAPIException(404)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.k8s_list_cluster_custom("foo.example.com", "v1", "foos")
    assert out == []


# ─── Case 9: pct returns nearest-rank percentile ────────────────────────────

def test_pct_returns_nearest_rank_percentile():
    """pct([1..10], 50) = 5; pct([1..10], 99) = 10; pct([], 50) is None."""
    data = list(range(1, 11))
    assert pct(data, 50) == 5
    assert pct(data, 90) == 9
    assert pct(data, 99) == 10
    assert pct(data, 0) == 1
    assert pct([], 50) is None


# ─── Case 10: call_url constructs the bench composition GVR URL ─────────────

def test_call_url_constructs_authenticated_path():
    """call_url(ns) embeds GVR; call_url(ns, name) appends &name=."""
    url = call_url("bench-ns-01")
    assert url.startswith("/call?")
    assert "composition.krateo.io" in url
    assert "githubscaffoldingwithcompositionpages" in url
    assert "namespace=bench-ns-01" in url
    assert "name=" not in url

    url2 = call_url("bench-ns-01", "bench-app-01-01")
    assert "name=bench-app-01-01" in url2


# ─── Bonus: composition_yaml is YAML-shaped (sanity, not source-grep) ───────

def test_composition_yaml_contains_required_fields():
    """composition_yaml(ns, name) produces a parseable manifest with the
    bench composition GVR + correct ns/name.
    """
    body = composition_yaml("bench-ns-99", "bench-app-99-77")
    # YAML round-trip would require PyYAML; instead, assert key markers
    # (the manifest is a heredoc, not arbitrary user input — string
    # check is sufficient for the layering contract).
    assert "apiVersion: composition.krateo.io/v1-2-2" in body
    assert "kind: GithubScaffoldingWithCompositionPage" in body
    assert "name: bench-app-99-77" in body
    assert "namespace: bench-ns-99" in body


# ─── Task #250 Block 2 — new RBAC primitives ────────────────────────────────
#
# Hermetic-isolation contract (feedback_bench_tests_hermetic_isolation_all_
# cluster_paths): every test that touches `k8s_create_*` MUST monkeypatch
# `_k8s_init` so the test FAILS FAST against a real GKE context. The
# reset_k8s_state fixture + monkeypatch of _k8s_init satisfies the
# contract end-to-end (the apiserver client globals are nulled at fixture
# entry; the test installs fakes; the fixture restores at exit).


def test_k8s_create_namespaced_role_returns_false_when_init_fails(
        reset_k8s_state, monkeypatch):
    """When `_k8s_init` returns False (lib missing / cluster unreachable),
    `k8s_create_namespaced_role` returns (False, diag) — does NOT crash,
    does NOT contact a real cluster. Re-gate v2: shape is now (ok, diag).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get"],
    )
    assert ok is False
    assert "k8s client unavailable" in diag


def test_k8s_create_namespaced_role_uses_client_when_available(
        reset_k8s_state, monkeypatch):
    """The create call uses the rbac client's `create_namespaced_role`
    method with a V1Role body — single REST call, idempotent on 409.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            captured["namespace"] = namespace
            captured["body"] = body
            captured["timeout"] = _request_timeout

    class _FakeV1Role:
        def __init__(self, metadata=None, rules=None):
            self.metadata = metadata
            self.rules = rules

    class _FakeV1ObjectMeta:
        def __init__(self, name=None):
            self.name = name

    class _FakeV1PolicyRule:
        def __init__(self, api_groups=None, resources=None, verbs=None):
            self.api_groups = api_groups
            self.resources = resources
            self.verbs = verbs

    class _FakeClientMod:
        V1Role = _FakeV1Role
        V1ObjectMeta = _FakeV1ObjectMeta
        V1PolicyRule = _FakeV1PolicyRule

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get", "list"],
    )
    assert ok is True
    assert diag == ""
    assert captured["namespace"] == "bench-ns-01"
    body = captured["body"]
    assert body.metadata.name == "test-role"
    assert len(body.rules) == 1
    assert body.rules[0].api_groups == ["composition.krateo.io"]
    assert body.rules[0].resources == ["*"]
    assert body.rules[0].verbs == ["get", "list"]


def test_k8s_create_namespaced_role_swallows_409(
        reset_k8s_state, monkeypatch):
    """AlreadyExists (HTTP 409) is treated as success — idempotent on
    re-runs.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        V1Role = type("V1Role", (), {"__init__": lambda s, **kw: None})
        V1ObjectMeta = type("V1ObjectMeta", (),
                            {"__init__": lambda s, **kw: None})
        V1PolicyRule = type("V1PolicyRule", (),
                            {"__init__": lambda s, **kw: None})

        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            raise _FakeAPIException(status=409)

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get"],
    )
    assert ok is True
    assert diag == ""


def test_k8s_create_namespaced_role_binding_constructs_subject_list(
        reset_k8s_state, monkeypatch):
    """The RoleBinding body carries the role_ref + subjects exactly as
    threaded — no hardcoded cyberjoker / devs identity participates.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role_binding(self, namespace, body,
                                           _request_timeout):
            captured["namespace"] = namespace
            captured["body"] = body

    class _FakeV1RoleBinding:
        def __init__(self, metadata=None, role_ref=None, subjects=None):
            self.metadata = metadata
            self.role_ref = role_ref
            self.subjects = subjects

    class _FakeV1RoleRef:
        def __init__(self, api_group=None, kind=None, name=None):
            self.api_group = api_group
            self.kind = kind
            self.name = name

    class _FakeRbacV1Subject:
        # Re-gate v2: production now uses `RbacV1Subject` (NOT bare
        # `V1Subject` — that symbol was removed from kubernetes>=27).
        def __init__(self, kind=None, name=None, api_group=None,
                     namespace=None):
            self.kind = kind
            self.name = name
            self.api_group = api_group
            self.namespace = namespace

    class _FakeV1ObjectMeta:
        def __init__(self, name=None):
            self.name = name

    class _FakeClientMod:
        V1RoleBinding = _FakeV1RoleBinding
        V1RoleRef = _FakeV1RoleRef
        RbacV1Subject = _FakeRbacV1Subject
        V1ObjectMeta = _FakeV1ObjectMeta

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    ok, diag = cluster_mod.k8s_create_namespaced_role_binding(
        "bench-ns-01", "test-rb",
        role_ref=("Role", "test-role"),
        subjects=[
            {"kind": "Group", "name": "devs"},
            {"kind": "ServiceAccount", "name": "sa1",
             "namespace": "krateo-system"},
        ],
    )
    assert ok is True
    assert diag == ""
    body = captured["body"]
    assert body.metadata.name == "test-rb"
    assert body.role_ref.kind == "Role"
    assert body.role_ref.name == "test-role"
    assert body.role_ref.api_group == "rbac.authorization.k8s.io"
    assert len(body.subjects) == 2
    # Group subject: default api_group filled in.
    assert body.subjects[0].kind == "Group"
    assert body.subjects[0].name == "devs"
    assert body.subjects[0].api_group == "rbac.authorization.k8s.io"
    # SA subject: api_group empty, namespace honoured.
    assert body.subjects[1].kind == "ServiceAccount"
    assert body.subjects[1].name == "sa1"
    assert body.subjects[1].api_group == ""
    assert body.subjects[1].namespace == "krateo-system"


def test_k8s_read_namespaced_role_binding_returns_none_on_404(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound is swallowed and returns None (not raise)."""
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeRbacAPI:
        def read_namespaced_role_binding(self, name, namespace,
                                         _request_timeout):
            raise _FakeAPIException(status=404)

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    out = cluster_mod.k8s_read_namespaced_role_binding(
        "bench-ns-01", "missing-rb")
    assert out is None


def test_k8s_read_namespaced_role_binding_returns_object_on_200(
        reset_k8s_state, monkeypatch):
    """A successful read returns the rb object (not None, not raise)."""
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    sentinel = {"metadata": {"name": "test-rb"}}

    class _FakeRbacAPI:
        def read_namespaced_role_binding(self, name, namespace,
                                         _request_timeout):
            return sentinel

    cluster_mod._k8s_rbac = _FakeRbacAPI()
    out = cluster_mod.k8s_read_namespaced_role_binding(
        "bench-ns-01", "test-rb")
    assert out is sentinel


def test_k8s_create_helpers_never_touch_real_cluster_when_lib_unavailable(
        reset_k8s_state, monkeypatch):
    """Hermetic-isolation contract: with `_K8S_LIB_AVAILABLE=False`, no
    create/read helper should make any subprocess call OR raise. Returns
    False (create) / None (read) so the caller falls back deterministically.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False

    # subprocess.run must NOT be called by these helpers (they go through
    # the k8s client lib only). Monkeypatch subprocess.run to a tripwire.
    def _tripwire(*a, **kw):
        raise AssertionError(
            "k8s_* helper escaped to subprocess.run — hermetic-isolation "
            "contract violated")

    monkeypatch.setattr(subprocess, "run", _tripwire)

    # Re-gate v2: create helpers now return (ok, diag) — destructure
    # and assert ok=False without consuming the diagnostic.
    role_ok, _ = cluster_mod.k8s_create_namespaced_role(
        "ns", "name", api_groups=[], resources=[], verbs=[])
    assert role_ok is False
    rb_ok, _ = cluster_mod.k8s_create_namespaced_role_binding(
        "ns", "name", role_ref=("Role", "r"), subjects=[])
    assert rb_ok is False
    assert cluster_mod.k8s_read_namespaced_role_binding(
        "ns", "name") is None


# ─── Task #250 Block 2b re-gate fix — count_compositions_with_panels_ready ──


def test_count_comp_panels_returns_none_when_init_fails(
        reset_k8s_state, monkeypatch):
    """When `_k8s_init` returns False, the helper returns None so the
    caller falls back to BASE expected (does NOT crash, does NOT hit a
    real cluster).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns=None) is None
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01") is None


def test_count_comp_panels_cluster_wide_uses_list_cluster_custom_object(
        reset_k8s_state, monkeypatch):
    """target_ns=None → list_cluster_custom_object with the comp-panel
    label_selector. Asserts the GVR + label match the empirical
    ground-truth (probed against gke_neon-481711 at 2026-06-05).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            captured["group"] = group
            captured["version"] = version
            captured["plural"] = plural
            captured["label_selector"] = label_selector
            return {"items": [{"i": i} for i in range(4423)]}

        def list_namespaced_custom_object(self, **kw):
            raise AssertionError(
                "list_namespaced_custom_object called when target_ns=None")

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out == 4423
    # Empirical GVR + label match Task #250 design + 2026-06-05 probe.
    assert captured["group"] == "widgets.templates.krateo.io"
    assert captured["version"] == "v1beta1"
    assert captured["plural"] == "panels"
    assert captured["label_selector"] == "krateo.io/portal-page=compositions"


def test_count_comp_panels_namespaced_uses_list_namespaced_custom_object(
        reset_k8s_state, monkeypatch):
    """target_ns="bench-ns-01" → list_namespaced_custom_object with same
    label_selector + namespace. cyberjoker S8 case.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeCustom:
        def list_namespaced_custom_object(self, group, version, namespace,
                                          plural, label_selector,
                                          _request_timeout):
            captured["group"] = group
            captured["version"] = version
            captured["namespace"] = namespace
            captured["plural"] = plural
            captured["label_selector"] = label_selector
            # Empirical 2026-06-05 probe: bench-ns-01 had 143 comp-panels.
            return {"items": [{"i": i} for i in range(143)]}

        def list_cluster_custom_object(self, **kw):
            raise AssertionError(
                "list_cluster_custom_object called when target_ns is set")

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01")
    assert out == 143
    assert captured["namespace"] == "bench-ns-01"
    assert captured["plural"] == "panels"
    assert captured["label_selector"] == "krateo.io/portal-page=compositions"


def test_count_comp_panels_returns_zero_on_404(
        reset_k8s_state, monkeypatch):
    """A 404 NotFound (panel CRD not installed) → 0 (not None). Caller
    treats 0 as 'cluster has no comp-panels' which collapses the
    formula to BASE.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            raise _FakeAPIException(status=404)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out == 0


def test_count_comp_panels_returns_none_on_transport_failure(
        reset_k8s_state, monkeypatch):
    """Non-404 exception (transport / 5xx) → None so caller falls back
    to BASE. Mirrors the convention of all other k8s_* helpers in this
    module.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeAPIException(Exception):
        def __init__(self, status):
            self.status = status

    class _FakeClientMod:
        class exceptions:
            ApiException = _FakeAPIException

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)

    class _FakeCustom:
        def list_cluster_custom_object(self, group, version, plural,
                                       label_selector, _request_timeout):
            raise _FakeAPIException(status=503)

    cluster_mod._k8s_custom = _FakeCustom()
    out = cluster_mod.count_compositions_with_panels_ready(target_ns=None)
    assert out is None


def test_count_comp_panels_never_touches_subprocess(
        reset_k8s_state, monkeypatch):
    """Tripwire: the k8s-client path NEVER escapes to subprocess.run.
    Mirrors the contract for create/read primitives.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False

    def _tripwire(*a, **kw):
        raise AssertionError(
            "count_compositions_with_panels_ready escaped to "
            "subprocess.run — hermetic-isolation contract violated")

    monkeypatch.setattr(subprocess, "run", _tripwire)
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns=None) is None
    assert cluster_mod.count_compositions_with_panels_ready(
        target_ns="bench-ns-01") is None


# ─── Task #275 / #276-B.1 — count_bench_ns_terminating ──────────────────────
#
# The sibling count_bench_ns() EXCLUDES Terminating namespaces; this
# counter makes the Terminating residue (the #275 masking) visible.
# `kubectl get ns --no-headers` prints the phase in the STATUS column:
#   bench-ns-01   Terminating   12m
#   bench-ns-02   Active        12m


def test_count_bench_ns_terminating_counts_only_terminating(mock_kubectl):
    """Counts bench-ns-* in Terminating phase; ignores Active bench-ns and
    non-bench namespaces (Terminating or not)."""
    mock_kubectl.replies = [(0, (
        "bench-ns-01   Terminating   12m\n"
        "bench-ns-02   Terminating   12m\n"
        "bench-ns-03   Active        12m\n"
        "krateo-system Active        40d\n"
        "kube-system   Terminating   40d\n"   # non-bench Terminating: ignored
    ), "")]
    import bench.cluster as cluster_mod
    assert cluster_mod.count_bench_ns_terminating() == 2


def test_count_bench_ns_terminating_zero_when_all_active(mock_kubectl):
    """When every bench-ns is Active, the Terminating counter is 0 (the
    healthy steady state); the sibling count_bench_ns() would report 3."""
    mock_kubectl.replies = [(0, (
        "bench-ns-01   Active   12m\n"
        "bench-ns-02   Active   12m\n"
        "bench-ns-03   Active   12m\n"
    ), "")]
    import bench.cluster as cluster_mod
    assert cluster_mod.count_bench_ns_terminating() == 3 - 3  # == 0


def test_count_bench_ns_terminating_complements_count_bench_ns(mock_kubectl):
    """The two counters partition the bench-ns set: Active goes to
    count_bench_ns(), Terminating goes to count_bench_ns_terminating().
    Proves the exact masking the #275 trace named — count_bench_ns() sees
    only the 1 Active, the Terminating counter exposes the hidden 2."""
    out = (
        "bench-ns-01   Terminating   12m\n"
        "bench-ns-02   Terminating   12m\n"
        "bench-ns-03   Active        12m\n"
    )
    # Two identical replies — one per counter call (the fixture pops).
    mock_kubectl.replies = [(0, out, ""), (0, out, "")]
    import bench.cluster as cluster_mod
    assert cluster_mod.count_bench_ns() == 1
    assert cluster_mod.count_bench_ns_terminating() == 2


# ─── Task #250 Block 2b re-gate v2 — SDK-symbol presence + (ok, diag) shape ──
#
# Why this test exists: the hermetic tests above monkeypatch
# `_k8s_client_mod` so the production code's references to
# `_k8s_client_mod.RbacV1Subject`, `_k8s_client_mod.V1Role`, etc. NEVER
# actually exercise the real `kubernetes.client` module. The re-gate v2
# tester run surfaced an `AttributeError: V1Subject` against
# `kubernetes==35.0.0` — a SDK-version-drift bug that the hermetic
# tests cannot detect by construction.
#
# This test exercises the real `kubernetes.client` module dictionary
# (no HTTP, no subprocess) to confirm every symbol the production code
# references actually exists. A symbol rename on a kubernetes-client
# upgrade (e.g. `V1Subject → RbacV1Subject`) now fails here at
# `pytest e2e/bench/`, NOT during a live tester run.

def test_kubernetes_client_exposes_all_symbols_used_by_create_helpers():
    """Symbol-presence guard against SDK version drift.

    Production code in `bench/cluster.py:k8s_create_namespaced_role_binding`
    references the following symbols on `kubernetes.client`. If any of
    them disappear from the installed `kubernetes` package (as
    `V1Subject` did in v27+), the live RB-create raises AttributeError
    BEFORE any HTTP call.

    This test only reads `kubernetes.client.<symbol>` — NO HTTP, NO
    subprocess, NO real cluster contact. Skips silently when the
    `kubernetes` lib is absent (running pytest on a stripped venv is
    valid).
    """
    try:
        import kubernetes.client as _kc  # type: ignore
    except ImportError:
        pytest.skip("kubernetes lib not installed; symbol-presence test "
                    "requires the real module")

    required_symbols = [
        # Role construction
        "V1Role",
        "V1ObjectMeta",
        "V1PolicyRule",
        # RoleBinding construction — RbacV1Subject is the post-v27
        # name (NOT V1Subject). The re-gate v2 fix.
        "V1RoleBinding",
        "V1RoleRef",
        "RbacV1Subject",
        # Pre-check (SelfSubjectAccessReview)
        "AuthorizationV1Api",
        "V1SelfSubjectAccessReview",
        "V1SelfSubjectAccessReviewSpec",
        "V1ResourceAttributes",
        # RBAC API
        "RbacAuthorizationV1Api",
        # Custom resources (panels list for the comp-panel count)
        "CustomObjectsApi",
    ]
    missing = [s for s in required_symbols if not hasattr(_kc, s)]
    assert not missing, (
        f"kubernetes-client SDK is missing symbols used by bench/"
        f"cluster.py: {missing}. The installed version may have "
        f"renamed/removed these (e.g. V1Subject was renamed to "
        f"RbacV1Subject in kubernetes>=27). Bump the bench's expected "
        f"symbols or fix the cluster.py reference."
    )


def test_k8s_create_namespaced_role_returns_diag_on_attribute_error(
        reset_k8s_state, monkeypatch):
    """When `_k8s_client_mod` is missing a required attribute (SDK drift
    simulation), the create helper returns (False, diag) with the
    AttributeError type name in the diag string — NOT a silent swallow.
    Validates the re-gate v2 broadened exception handler.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    # Simulate SDK drift: V1Role attribute is GONE. Production code
    # references `_k8s_client_mod.V1Role(...)` which raises
    # AttributeError. Re-gate v1 swallowed this as a generic False;
    # re-gate v2 must surface it in the diag.
    class _FakeBrokenClientMod:
        # NO V1Role / NO V1PolicyRule — simulates SDK-drift scenarios.
        # Re-gate v3: production constructs V1PolicyRule before V1Role
        # (the list comprehension on rules runs first), so the missing
        # symbol surfaced first is V1PolicyRule. Assertion below
        # tolerates either symbol-name as long as the AttributeError
        # surfaces in the diag.
        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod",
                        _FakeBrokenClientMod)
    cluster_mod._k8s_rbac = object()  # would be touched only after body

    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get"],
    )
    assert ok is False
    assert "AttributeError" in diag, \
        f"diag must surface AttributeError, got: {diag!r}"
    # The diag must reference at least one of the missing symbols
    # production references on _k8s_client_mod. Accept either V1Role
    # or V1PolicyRule (order of construction may surface either first).
    assert ("V1Role" in diag) or ("V1PolicyRule" in diag), \
        f"diag must mention the missing symbol, got: {diag!r}"


def test_k8s_create_namespaced_role_binding_uses_RbacV1Subject_not_V1Subject(
        reset_k8s_state, monkeypatch):
    """Re-gate v2 anchor test: the rolebinding helper MUST construct
    subjects from `RbacV1Subject`, not from `V1Subject` (which was
    removed in kubernetes>=27). Asserts the FakeClientMod that lacks
    V1Subject can still satisfy the helper.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role_binding(self, namespace, body,
                                           _request_timeout):
            captured["subjects"] = body.subjects

    class _FakeRbacV1Subject:
        def __init__(self, kind=None, name=None, api_group=None,
                     namespace=None):
            self.kind = kind
            self.name = name
            self.api_group = api_group
            self.namespace = namespace

    class _FakeV1RoleBinding:
        def __init__(self, metadata=None, role_ref=None, subjects=None):
            self.subjects = subjects
            self.role_ref = role_ref
            self.metadata = metadata

    class _FakeClientMod:
        # NO V1Subject attribute — must use RbacV1Subject.
        RbacV1Subject = _FakeRbacV1Subject
        V1RoleBinding = _FakeV1RoleBinding
        V1RoleRef = type("V1RoleRef", (),
                         {"__init__": lambda s, **kw: None})
        V1ObjectMeta = type("V1ObjectMeta", (),
                            {"__init__": lambda s, **kw: None})

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    ok, diag = cluster_mod.k8s_create_namespaced_role_binding(
        "bench-ns-01", "test-rb",
        role_ref=("Role", "test-role"),
        subjects=[{"kind": "Group", "name": "devs"}],
    )
    assert ok is True
    assert diag == ""
    # Confirm RbacV1Subject was used (NOT V1Subject — which we
    # deliberately did NOT install on _FakeClientMod).
    assert len(captured["subjects"]) == 1
    assert isinstance(captured["subjects"][0], _FakeRbacV1Subject)


def test_k8s_can_i_create_rolebinding_returns_allowed_on_self_review_true(
        reset_k8s_state, monkeypatch):
    """SelfSubjectAccessReview returns status.allowed=True → precheck
    passes with empty diag.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeReviewStatus:
        allowed = True
        reason = ""

    class _FakeReviewResponse:
        status = _FakeReviewStatus()

    class _FakeAuthzAPI:
        def create_self_subject_access_review(self, body, _request_timeout):
            return _FakeReviewResponse()

    class _FakeClientMod:
        AuthorizationV1Api = lambda: _FakeAuthzAPI()
        V1SelfSubjectAccessReview = type(
            "V1SelfSubjectAccessReview", (),
            {"__init__": lambda s, **kw: None})
        V1SelfSubjectAccessReviewSpec = type(
            "V1SelfSubjectAccessReviewSpec", (),
            {"__init__": lambda s, **kw: None})
        V1ResourceAttributes = type(
            "V1ResourceAttributes", (),
            {"__init__": lambda s, **kw: None})

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    allowed, diag = cluster_mod.k8s_can_i_create_rolebinding(
        "bench-ns-01")
    assert allowed is True
    assert diag == ""


def test_k8s_can_i_create_rolebinding_returns_denied_with_reason(
        reset_k8s_state, monkeypatch):
    """SelfSubjectAccessReview returns status.allowed=False → precheck
    returns (False, descriptive diag with the apiserver reason).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    class _FakeReviewStatus:
        allowed = False
        reason = "RBAC: forbidden — user 'bench' cannot create rolebindings"

    class _FakeReviewResponse:
        status = _FakeReviewStatus()

    class _FakeAuthzAPI:
        def create_self_subject_access_review(self, body, _request_timeout):
            return _FakeReviewResponse()

    class _FakeClientMod:
        AuthorizationV1Api = lambda: _FakeAuthzAPI()
        V1SelfSubjectAccessReview = type(
            "V1SelfSubjectAccessReview", (),
            {"__init__": lambda s, **kw: None})
        V1SelfSubjectAccessReviewSpec = type(
            "V1SelfSubjectAccessReviewSpec", (),
            {"__init__": lambda s, **kw: None})
        V1ResourceAttributes = type(
            "V1ResourceAttributes", (),
            {"__init__": lambda s, **kw: None})

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    allowed, diag = cluster_mod.k8s_can_i_create_rolebinding(
        "bench-ns-01")
    assert allowed is False
    assert "rbac_precheck_denied" in diag
    assert "bench-ns-01" in diag


def test_k8s_can_i_create_rolebinding_returns_unavailable_when_init_fails(
        reset_k8s_state, monkeypatch):
    """When `_k8s_init` returns False → (False, 'rbac_precheck_unavailable').
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", False)
    cluster_mod.K8S_CLIENT_AVAILABLE = False
    allowed, diag = cluster_mod.k8s_can_i_create_rolebinding(
        "bench-ns-01")
    assert allowed is False
    assert "rbac_precheck_unavailable" in diag


# ─── Task #250 Block 2b re-gate v3+v4 — multi-rule k8s_create_namespaced_role
#
# The S8 stage runner grants cj TWO PolicyRules: composition CR
# get/list + widget GVR get/list (panels/markdowns/buttons/tablists).
# Closes #186 Option (a) per Diego 2026-06-05; tablists added in
# re-gate v4 per architect trace 2026-06-08 (Panel.spec.resourcesRefs[3]
# is tablists/GET, NOT a second Button as task-215 doc claimed).


def test_k8s_create_namespaced_role_constructs_one_PolicyRule_per_tuple(
        reset_k8s_state, monkeypatch):
    """The new `rules` kwarg accepts a list of (api_groups, resources,
    verbs) tuples; each tuple becomes ONE V1PolicyRule. Asserts the
    fan-out wires through correctly — no fan-in collapsing, no
    out-of-order field swapping. Mirrors the S8 runner's POST-v4
    grant (4 widget resources including tablists).
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            captured["namespace"] = namespace
            captured["body"] = body

    class _FakeV1Role:
        def __init__(self, metadata=None, rules=None):
            self.metadata = metadata
            self.rules = rules

    class _FakeV1ObjectMeta:
        def __init__(self, name=None):
            self.name = name

    class _FakeV1PolicyRule:
        def __init__(self, api_groups=None, resources=None, verbs=None):
            self.api_groups = api_groups
            self.resources = resources
            self.verbs = verbs

    class _FakeClientMod:
        V1Role = _FakeV1Role
        V1ObjectMeta = _FakeV1ObjectMeta
        V1PolicyRule = _FakeV1PolicyRule

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()

    # Mirror the S8 runner's two-rule grant (post-v4: includes tablists).
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        rules=[
            (["composition.krateo.io"], ["*"], ["get", "list"]),
            (["widgets.templates.krateo.io"],
             ["panels", "markdowns", "buttons", "tablists"],
             ["get", "list"]),
        ],
    )
    assert ok is True
    assert diag == ""
    body = captured["body"]
    assert body.metadata.name == "test-role"
    assert len(body.rules) == 2
    # Rule 0: composition GVR.
    r0 = body.rules[0]
    assert r0.api_groups == ["composition.krateo.io"]
    assert r0.resources == ["*"]
    assert r0.verbs == ["get", "list"]
    # Rule 1: widget GVR — empirically verified group string
    # `widgets.templates.krateo.io` (kubectl api-resources 2026-06-05).
    # Includes tablists per re-gate v4 (architect trace 2026-06-08:
    # Panel.spec.resourcesRefs[3] is tablists/GET).
    r1 = body.rules[1]
    assert r1.api_groups == ["widgets.templates.krateo.io"]
    assert r1.resources == ["panels", "markdowns", "buttons", "tablists"]
    assert r1.verbs == ["get", "list"]


def test_k8s_create_namespaced_role_legacy_kwargs_still_work(
        reset_k8s_state, monkeypatch):
    """Backward-compat: callers passing legacy single-rule kwargs
    (api_groups=, resources=, verbs=) MUST still work. Re-gate v3
    preserves the pre-v3 single-rule shape so prior tests + callers
    don't break.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True

    captured = {}

    class _FakeRbacAPI:
        def create_namespaced_role(self, namespace, body, _request_timeout):
            captured["body"] = body

    class _FakeClientMod:
        V1Role = type("V1Role", (), {
            "__init__": lambda s, metadata=None, rules=None: setattr(
                s, "rules", rules) or setattr(s, "metadata", metadata)})
        V1ObjectMeta = type("V1ObjectMeta", (),
                            {"__init__": lambda s, **kw: None})
        V1PolicyRule = type("V1PolicyRule", (), {
            "__init__": lambda s, api_groups=None, resources=None,
            verbs=None: (setattr(s, "api_groups", api_groups),
                         setattr(s, "resources", resources),
                         setattr(s, "verbs", verbs)) and None})

        class exceptions:
            class ApiException(Exception):
                def __init__(self, status=None):
                    self.status = status

    monkeypatch.setattr(cluster_mod, "_k8s_client_mod", _FakeClientMod)
    cluster_mod._k8s_rbac = _FakeRbacAPI()
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        api_groups=["composition.krateo.io"],
        resources=["*"],
        verbs=["get", "list"],
    )
    assert ok is True
    body = captured["body"]
    assert len(body.rules) == 1
    assert body.rules[0].api_groups == ["composition.krateo.io"]


def test_k8s_create_namespaced_role_rejects_empty_rules_list(
        reset_k8s_state, monkeypatch):
    """Passing `rules=[]` must surface a clean diagnostic, not create
    an empty-rules (no-op) Role.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role", rules=[])
    assert ok is False
    assert "rules must be a non-empty list" in diag


def test_k8s_create_namespaced_role_rejects_malformed_rule_tuple(
        reset_k8s_state, monkeypatch):
    """A rule that isn't a 3-tuple must produce a clean diagnostic,
    not an AttributeError downstream.
    """
    cluster_mod = reset_k8s_state
    monkeypatch.setattr(cluster_mod, "_K8S_LIB_AVAILABLE", True)
    cluster_mod.K8S_CLIENT_AVAILABLE = True
    ok, diag = cluster_mod.k8s_create_namespaced_role(
        "bench-ns-01", "test-role",
        rules=[(["x"], ["y"])])  # only 2 elements
    assert ok is False
    assert "rule #0 must be a 3-tuple" in diag


def test_k8s_create_namespaced_role_rejects_no_args():
    """Calling without rules OR legacy kwargs is a programmer error —
    produces a clean diagnostic.
    """
    from bench import cluster as cluster_mod
    # Stub the init check by patching _k8s_init to True; the function
    # must catch the no-args case BEFORE attempting any client call.
    import unittest.mock as mock
    with mock.patch.object(cluster_mod, "_k8s_init", return_value=True):
        ok, diag = cluster_mod.k8s_create_namespaced_role(
            "bench-ns-01", "test-role")
    assert ok is False
    assert "must pass either" in diag


# ─── Diego hard rule 2026-06-11: helm pins the cluster explicitly ───────────


def test_helm_context_args_pinned_by_default(monkeypatch):
    import bench.cluster as cluster_mod
    monkeypatch.delenv("BENCH_ALLOW_NON_GKE", raising=False)
    assert cluster_mod.helm_context_args() == [
        "--kube-context", cluster_mod.CANONICAL_GKE_CONTEXT]


def test_helm_context_args_empty_under_non_gke_escape(monkeypatch):
    import bench.cluster as cluster_mod
    monkeypatch.setenv("BENCH_ALLOW_NON_GKE", "1")
    assert cluster_mod.helm_context_args() == []


# ─── Task #348 — paginated cluster-wide count / name probes ─────────────────
#
# At ~49K compositions an UNPAGED `kubectl get ... --all-namespaces` is the
# slow path. The fix pages the SERVER-SIDE Table read via --chunk-size while
# keeping --no-headers (the Table printer — NOT -o name/-o json, which force
# full-object serialization and are empirically SLOWER). These tests stub
# kubectl via the conftest `mock_kubectl` seam and assert (a) the chunked
# flags reach the argv and (b) the Table line-count / name-set parse is
# correct and unchanged.


def test_count_compositions_passes_chunked_table_flags(mock_kubectl):
    """count_compositions() must shell a PAGED Table read: --no-headers
    (server-side Table printer, kept) + --chunk-size=<COMP_PROBE_CHUNK_SIZE>
    (Limit/Continue paging). It must NOT use -o name / -o json (those force
    full-object serialization — the slow path #348 fixes)."""
    import bench.cluster as cluster_mod
    mock_kubectl.replies = [(0, "bench-ns-01 c1 8h True\nbench-ns-01 c2 8h True", "")]
    cluster_mod.count_compositions()
    argv = mock_kubectl.calls[0]["argv"]
    assert argv[0] == "kubectl"
    assert "--no-headers" in argv, f"Table printer dropped; argv={argv}"
    assert f"--chunk-size={cluster_mod.COMP_PROBE_CHUNK_SIZE}" in argv, (
        f"--chunk-size paging flag missing; argv={argv}")
    assert "--all-namespaces" in argv
    # MUST stay an independent kubectl oracle on the Table printer — no
    # -o name / -o json (full-object serialization = the slow path).
    assert "name" not in argv and "json" not in argv, (
        f"probe must not request -o name/json; argv={argv}")


def test_count_compositions_counts_table_rows(mock_kubectl):
    """One Table row per composition → returned count == row count."""
    import bench.cluster as cluster_mod
    rows = "\n".join(f"bench-ns-01 bench-app-01-{i:02d} 8h True"
                     for i in range(7))
    mock_kubectl.replies = [(0, rows, "")]
    assert cluster_mod.count_compositions() == 7


def test_count_compositions_zero_on_empty_or_error(mock_kubectl):
    """Empty stdout or non-zero rc → 0 (unchanged contract)."""
    import bench.cluster as cluster_mod
    mock_kubectl.replies = [(0, "", "")]
    assert cluster_mod.count_compositions() == 0
    mock_kubectl.replies = [(1, "", "boom")]
    assert cluster_mod.count_compositions() == 0


def test_count_compositions_in_ns_passes_chunked_flags(mock_kubectl):
    """The namespace-scoped sibling carries the same paging flags + the
    -n <ns> scope, and counts Table rows."""
    import bench.cluster as cluster_mod
    mock_kubectl.replies = [(0, "c1 8h True\nc2 8h True\nc3 8h True", "")]
    n = cluster_mod.count_compositions_in_ns("bench-ns-07")
    argv = mock_kubectl.calls[0]["argv"]
    assert "--no-headers" in argv
    assert f"--chunk-size={cluster_mod.COMP_PROBE_CHUNK_SIZE}" in argv
    assert "-n" in argv and "bench-ns-07" in argv
    assert n == 3


def test_list_composition_names_passes_chunked_table_flags(mock_kubectl):
    """list_composition_names() must use the SAME paged Table read (not
    -o jsonpath, the old full-object slow path)."""
    import bench.cluster as cluster_mod
    mock_kubectl.replies = [(0, "bench-ns-01 c1 8h True", "")]
    cluster_mod.list_composition_names()
    argv = mock_kubectl.calls[0]["argv"]
    assert "--no-headers" in argv
    assert f"--chunk-size={cluster_mod.COMP_PROBE_CHUNK_SIZE}" in argv
    assert "--all-namespaces" in argv
    assert "jsonpath" not in " ".join(argv), (
        f"must not use the old -o jsonpath full-object path; argv={argv}")


def test_list_composition_names_parses_ns_name_from_table(mock_kubectl):
    """col0=ns, col1=name → {"ns/name", ...}; AGE/READY columns ignored.
    This is the exact "ns/name" set the CONTENT name-diff compares; the
    live cluster proved it byte-identical to the old jsonpath oracle."""
    import bench.cluster as cluster_mod
    out = (
        "bench-ns-01   bench-app-01-02   8h   True\n"
        "bench-ns-01   bench-app-01-03   8h   True\n"
        "bench-ns-02   bench-app-02-01   3m   False\n"
    )
    mock_kubectl.replies = [(0, out, "")]
    names = cluster_mod.list_composition_names()
    assert names == {
        "bench-ns-01/bench-app-01-02",
        "bench-ns-01/bench-app-01-03",
        "bench-ns-02/bench-app-02-01",
    }


def test_list_composition_names_none_on_error_empty_set_on_no_rows(
        mock_kubectl):
    """rc != 0 → None (caller distinguishes from 'legitimately empty');
    rc == 0 with no parseable rows → empty set (NOT None)."""
    import bench.cluster as cluster_mod
    mock_kubectl.replies = [(1, "", "boom")]
    assert cluster_mod.list_composition_names() is None
    mock_kubectl.replies = [(0, "", "")]
    assert cluster_mod.list_composition_names() == set()
