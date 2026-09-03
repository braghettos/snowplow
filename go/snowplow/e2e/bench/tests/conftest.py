"""Shared pytest fixtures for the bench harness (Block 1 scope).

Adds `e2e/bench/` to sys.path so `import bench.cluster` resolves without
requiring an editable install. Sets BENCH_ALLOW_NON_GKE=1 before any
test imports bench.cluster so the module-level gke_context_guard does
NOT exit during test collection.

Risk Register R1.1 mitigation: `reset_k8s_state` fixture (opt-in) nulls
the cluster.py module-level globals so tests don't share state.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path

import pytest

# ─── Test isolation: env vars that MUST be set before bench.* imports ────────

# Bypass the GKE context guard — tests should never contact a real cluster.
os.environ.setdefault("BENCH_ALLOW_NON_GKE", "1")

# BENCH_GKE_CONTEXT retargets the pinned context (cluster.canonical_gke_context).
# An operator who exports it in their shell to drive a real bench run would
# otherwise change what the context-pinning tests assert. Clear it so the suite
# is hermetic against the operator's environment; the tests that exercise the
# override set it explicitly via monkeypatch.
os.environ.pop("BENCH_GKE_CONTEXT", None)

# Add e2e/bench/ to sys.path so `import bench.cluster` works without install.
_E2E_BENCH_DIR = Path(__file__).resolve().parent.parent
if str(_E2E_BENCH_DIR) not in sys.path:
    sys.path.insert(0, str(_E2E_BENCH_DIR))


@pytest.fixture(autouse=True)
def _stub_k8s_client_preflight(monkeypatch):
    """#320: cmd_check gate 8 / the cmd_phase6 pre-check call
    cluster._k8s_init(), which loads a real kubeconfig — unit tests must
    never do that (and a successful init leaks K8S_CLIENT_AVAILABLE=True
    across tests). Stub the GATE; tests of the gate itself bind the real
    function at module import time, and wiring tests re-patch explicitly.
    """
    import bench.cli as cli_mod
    monkeypatch.setattr(cli_mod, "_gate_k8s_client",
                        lambda: (True, "k8s_client: PASS (test stub)"))


@pytest.fixture(autouse=True)
def _stub_deploy_fingerprint(monkeypatch):
    """#320: _run_stage fingerprints the live snowplow Deployment around
    every stage (`kubectl get deploy -o json`). Unit tests must never shell
    real kubectl — stub the fingerprint to None (annotation fields become
    None / pod_rerolled_mid_stage=False). Tests that exercise the guard
    re-set `phases._snowplow_deploy_fingerprint` themselves; the real
    function is capturable at test-module import time (collection runs
    before fixtures).
    """
    import bench.phases as phases_mod
    monkeypatch.setattr(phases_mod, "_snowplow_deploy_fingerprint",
                        lambda: None)


@pytest.fixture(autouse=True)
def _stub_conv_discrimination_probes(monkeypatch):
    """Task #334a: browser_measure_stage's VERIFY block now snapshots
    `snowplow_refresher_completed_total` (HTTP /debug/vars) and fetches a
    bounded pod-log window (`kubectl logs --since-time`) to assemble the
    convergence-cell discrimination tuple. Unit tests must never reach the
    live cluster — neutralise BOTH seams so the tuple fails soft to all-None
    (the same path a cache-off pod takes).

    - `_snowplow_pod_log_window` is brand-new and only called from the VERIFY
      path → stub straight to None.
    - `read_snowplow_expvar_int` is a SHARED helper with its own dedicated
      unit tests (test_browser.py) that pass an explicit `base_url=` to drive
      a mocked transport. We delegate to the REAL function ONLY when a
      `base_url` is supplied (those unit tests); the VERIFY-path callers pass
      no base_url and get None. test_phases_s8bc re-binds the attribute
      explicitly and so overrides this wrapper entirely (fixtures apply
      before the test body).
    - `read_snowplow_expvar_map` (Task #217) is the map-valued sibling, called
      with no base_url from _run_stage's per-stage L1 snapshot. Same wrapper:
      delegate to the REAL function only when a base_url is supplied (its
      transport unit tests); base_url-less _run_stage callers get None, so the
      proof's l1_lookup_delta is None — the same path a cache-off pod takes.
    """
    import bench.browser as browser_mod
    _real_expvar = browser_mod.read_snowplow_expvar_int
    _real_expvar_map = browser_mod.read_snowplow_expvar_map

    def _expvar_or_none(key, *, base_url=None, timeout=10):
        if base_url is not None:
            return _real_expvar(key, base_url=base_url, timeout=timeout)
        return None

    def _expvar_map_or_none(key, *, base_url=None, timeout=10):
        if base_url is not None:
            return _real_expvar_map(key, base_url=base_url, timeout=timeout)
        return None

    monkeypatch.setattr(browser_mod, "read_snowplow_expvar_int",
                        _expvar_or_none)
    monkeypatch.setattr(browser_mod, "read_snowplow_expvar_map",
                        _expvar_map_or_none)
    monkeypatch.setattr(browser_mod, "_snowplow_pod_log_window",
                        lambda *a, **k: None)


@pytest.fixture
def reset_k8s_state():
    """Reset bench.cluster k8s-client module globals between tests.

    Use this fixture in any test that monkey-patches `_k8s_init`,
    flips `K8S_CLIENT_AVAILABLE`, or sets the API client globals. Without
    it, mutations leak between cases.

    Risk Register §I R1.1 mitigation.
    """
    import bench.cluster as cluster_mod
    snapshot = {
        "K8S_CLIENT_AVAILABLE": cluster_mod.K8S_CLIENT_AVAILABLE,
        "_k8s_core": cluster_mod._k8s_core,
        "_k8s_rbac": cluster_mod._k8s_rbac,
        "_k8s_custom": cluster_mod._k8s_custom,
        "_k8s_apiext": cluster_mod._k8s_apiext,
        "_k8s_init_attempted": cluster_mod._k8s_init_attempted,
    }
    yield cluster_mod
    cluster_mod.K8S_CLIENT_AVAILABLE = snapshot["K8S_CLIENT_AVAILABLE"]
    cluster_mod._k8s_core = snapshot["_k8s_core"]
    cluster_mod._k8s_rbac = snapshot["_k8s_rbac"]
    cluster_mod._k8s_custom = snapshot["_k8s_custom"]
    cluster_mod._k8s_apiext = snapshot["_k8s_apiext"]
    cluster_mod._k8s_init_attempted = snapshot["_k8s_init_attempted"]


@pytest.fixture
def mock_kubectl(monkeypatch):
    """Install a recording subprocess.run; yield the call log.

    Each entry in the log is a dict {argv, input, rc, stdout, stderr}.
    By default every call returns rc=0 with empty stdout/stderr; the test
    can pre-load `log.replies` (a list of (rc, stdout, stderr) tuples) to
    return scripted outputs.
    """
    import subprocess as subprocess_mod

    class _Log:
        def __init__(self):
            self.calls: list[dict] = []
            self.replies: list[tuple[int, str, str]] = []

    log = _Log()

    def _fake_run(argv, **kwargs):
        rc, stdout, stderr = (0, "", "")
        if log.replies:
            rc, stdout, stderr = log.replies.pop(0)
        log.calls.append({
            "argv": list(argv),
            "input": kwargs.get("input"),
            "rc": rc,
            "stdout": stdout,
            "stderr": stderr,
        })

        class _FakeCompletedProcess:
            def __init__(self, rc, stdout, stderr):
                self.returncode = rc
                # subprocess.run with capture_output=True returns bytes
                # unless text=True; bench.cluster.kubectl uses bytes.
                if kwargs.get("text"):
                    self.stdout = stdout
                    self.stderr = stderr
                else:
                    self.stdout = stdout.encode() if isinstance(stdout, str) else stdout
                    self.stderr = stderr.encode() if isinstance(stderr, str) else stderr

        return _FakeCompletedProcess(rc, stdout, stderr)

    monkeypatch.setattr(subprocess_mod, "run", _fake_run)
    yield log


@pytest.fixture
def tmp_run_dir(tmp_path):
    """Yield a clean /tmp/snowplow-runs/{tag}/run-{ts}/-shaped path."""
    run_dir = tmp_path / "snowplow-runs" / "test-tag" / "run-20260602-test"
    run_dir.mkdir(parents=True, exist_ok=True)
    return run_dir


# ─── Block 3: Playwright fake-page fixture ──────────────────────────────────


class FakePage:
    """Minimal duck-typed Playwright Page stand-in for unit tests.

    Records every call so tests can assert on invocation order. The DOM
    state is a dict the test pre-populates; `evaluate()` dispatches on
    JS-string prefix to a method that consults the dict.

    Field semantics:
      _skeleton_scoped / _skeleton_raw / _skeleton_widget — return values
        for _WIDGET_SKELETON_COUNT_JS evaluate().
      _call_count — return value for the resource-timing /call probe.
      _errored_count — return value for `.ant-result-error`.count().
      _ui_count — return value for the verify_composition_count_ui JS.
      _has_token — return value for the K_user localStorage probe.
      _nav_timing — dict returned by the navigation-timing JS.
      _waterfall — dict returned by the waterfall-levels JS.
    """

    def __init__(self, *, skeleton_scoped: int = 0, skeleton_raw: int = 0,
                 skeleton_widget: int = 0, call_count: int = 0,
                 errored_count: int = 0, ui_count: int = -1,
                 has_token: bool = True,
                 rendered_cards: int = 0,
                 nav_timing: dict | None = None,
                 waterfall: dict | None = None,
                 url: str = "http://fake/login"):
        self._skeleton_scoped = skeleton_scoped
        self._skeleton_raw = skeleton_raw
        self._skeleton_widget = skeleton_widget
        # Task #284: return value for _RENDERED_COMP_CARDS_JS — the count of
        # composition cards the datagrid rendered on page 1 at nav-time.
        self._rendered_cards = rendered_cards
        self._call_count = call_count
        self._errored_count = errored_count
        self._ui_count = ui_count
        self._has_token = has_token
        self._nav_timing = nav_timing or {"domContentLoaded": 100,
                                          "loadComplete": 200}
        self._waterfall = waterfall or {"callCount": call_count,
                                        "waterfallMs": 0,
                                        "calls": [], "levels": []}
        self.url = url
        self.evaluate_log: list[str] = []
        self.goto_log: list[tuple] = []
        self.screenshot_log: list[str] = []
        self.response_listeners: list = []

    def evaluate(self, js, *args):
        self.evaluate_log.append(js)
        # Match by canonical prefix snippets — keep the heuristic robust
        # to whitespace tweaks in the JS literals.
        # Task #284 rendered-card count: the JS takes a `namePrefix` arg and
        # returns matched.size. Match on the distinctive `matched.size` /
        # `createTreeWalker` shape BEFORE the skeleton branch (the skeleton JS
        # also uses createTreeWalker-free querySelectorAll, so no overlap, but
        # be explicit to keep the dispatch order-robust).
        if "matched.size" in js and "namePrefix" in js:
            return self._rendered_cards
        if "EXCLUDED_ANCESTORS" in js:
            return {"scoped": self._skeleton_scoped,
                    "raw": self._skeleton_raw,
                    "widget_scoped": self._skeleton_widget}
        # Order-sensitive: the verify_composition_count_ui JS contains both
        # `K_user` AND `dashboard-compositions-panel-row-piechart`. Match the
        # more-specific UI-count probe FIRST so the login-token branch does
        # not eat the verify call.
        if "dashboard-compositions-panel-row-piechart" in js:
            return self._ui_count
        if "K_user" in js:
            return self._has_token
        if "performance.clearResourceTimings" in js:
            return None
        if "filter(e => e.name.includes('/call')).length" in js:
            return self._call_count
        if "GAP_MS" in js and "calls.length" in js:
            return dict(self._waterfall, callCount=self._call_count)
        if "navigation" in js and "loadEventEnd" in js:
            return self._nav_timing
        if "scrollIntoView" in js:
            return True
        return None

    def goto(self, url, **kwargs):
        self.goto_log.append((url, kwargs))
        self.url = url

    def click(self, selector, **kwargs):
        # Behavioural no-op for login flow.
        return None

    def wait_for_timeout(self, ms):
        return None

    def wait_for_load_state(self, state, **kwargs):
        return None

    def screenshot(self, path: str, **kwargs):
        self.screenshot_log.append(path)
        # Touch the file so callers that stat() the path see a file exists.
        try:
            from pathlib import Path
            Path(path).parent.mkdir(parents=True, exist_ok=True)
            Path(path).write_bytes(b"\x89PNG\r\n\x1a\n")
        except Exception:
            pass

    def on(self, event, handler):
        self.response_listeners.append((event, handler))

    def remove_listener(self, event, handler):
        self.response_listeners = [
            (e, h) for (e, h) in self.response_listeners
            if not (e == event and h is handler)
        ]

    # keyboard.type and locator are used by login + validation paths.
    @property
    def keyboard(self):
        class _KB:
            def type(self, text, delay=0):
                return None
        return _KB()

    def locator(self, selector):
        page = self

        class _Locator:
            def count(self_inner):
                if selector == ".ant-result-error":
                    return page._errored_count
                return 0
        return _Locator()


@pytest.fixture
def fake_page():
    """Yield a FakePage with sensible defaults (no skeletons, 16 /calls,
    valid token, dashboard-shaped UI count).
    """
    return FakePage(
        skeleton_scoped=0,
        skeleton_raw=0,
        skeleton_widget=0,
        call_count=16,
        errored_count=0,
        ui_count=0,
        has_token=True,
    )
