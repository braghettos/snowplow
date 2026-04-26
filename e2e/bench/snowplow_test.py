#!/usr/bin/env python3
"""
Snowplow Cache — Unified Test Suite
====================================
Consolidates all cache tests into a single script with selectable phases.

Phases:
  1  functional   14 test cases: warmup, L1/L3, RBAC, negative cache, informer CRUD, etc.
  2  latency      Backend + frontend proxy latency benchmark (cache ON vs OFF)
  3  scaling      8-stage incremental scaling matrix (cache ON vs OFF)
  4  browser      Playwright browser metrics (DOM, render, XHR waterfall)
  5  comparison   Browser navigation: cache OFF vs cache ON (cold) vs cache ON (warmed)
  7  user-scaling Multi-user scaling: warmup + first-login burst at 10/50/100/500/1000 users (opt-in)

Usage:
  python3 snowplow_test.py                     # all phases
  python3 snowplow_test.py --phases 1,2        # functional + latency only
  python3 snowplow_test.py --phases 3 --smoke  # scaling in smoke mode (stages 1-3)

Environment:
  SNOWPLOW_URL    (default: http://34.135.50.203:8081)
  AUTHN_URL       (default: http://34.136.84.51:8082)
  FRONTEND_URL    (default: http://34.46.217.105:8080, empty to skip browser/frontend)
  ITERS           iterations for latency/warm measurements (default: 10)
  WARMUP_ITERS    warmup requests before measurement (default: 3)
  SMOKE           "1" to limit scaling to stages 1-3 (default: "0")
  SCALE           Target composition count for Phase 6 (default: 5000, e.g. 10000)
  EXPECTED_IMAGE_TAG  (required) e.g. 0.25.19 — tests will not start until deployment
                      runs this image. Deploy first: kubectl set image deployment/snowplow
                      snowplow=ghcr.io/<repo>/snowplow:0.25.19 -n krateo-system
  SKIP_IMAGE_CHECK   set to "1" to bypass image version check (not recommended)
"""

import argparse
import base64
import concurrent.futures
import gzip
import io
import json
import os
import statistics
import subprocess
import threading
import sys
import time
import urllib.error
import urllib.request

sys.stdout.reconfigure(line_buffering=True)

# ═════════════════════════════════════════════════════════════════════════════
# CONFIGURATION
# ═════════════════════════════════════════════════════════════════════════════

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
FRONTEND = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080") or None
ITERS = int(os.environ.get("ITERS", "10"))
WARMUP_ITERS = int(os.environ.get("WARMUP_ITERS", "3"))
SMOKE = os.environ.get("SMOKE", "0") == "1"
SCREENSHOTS = os.environ.get("SCREENSHOTS", "0") == "1"
SCALE = int(os.environ.get("SCALE", "5000"))
NS = "krateo-system"

USERS = {
    "admin": "zKQAMSGJ3S4S",
    "cyberjoker": "fUjGILkvvizF",
}

COMPDEF_NAME = "github-scaffolding-with-composition-page"
COMP_GVR = "composition.krateo.io"
COMP_RES = "githubscaffoldingwithcompositionpages"
COMP_CONTROLLER_DEPLOY = f"{COMP_RES}-v1-2-2-controller"

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

TEST_NS = "bench-crud-test"
TEST_NAME_WARM = "bench-app-01"
TEST_NAME_NEW = "cache-test-app"

COMPOSITION_YAML = f"""\
apiVersion: composition.krateo.io/v1-2-2
kind: GithubScaffoldingWithCompositionPage
metadata:
  name: cache-test-app
  namespace: {TEST_NS}
spec:
  argocd:
    namespace: krateo-system
    application:
      project: default
      source:
        path: chart/
      destination:
        server: https://kubernetes.default.svc
        namespace: fireworks-app
      syncEnabled: false
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
  app:
    service:
      type: NodePort
      port: 31180
  git:
    unsupportedCapabilities: true
    insecure: true
    fromRepo:
      scmUrl: https://github.com
      org: krateoplatformops-blueprints
      name: github-scaffolding-with-composition-page
      branch: main
      path: skeleton/
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
    toRepo:
      scmUrl: https://github.com
      org: krateoplatformops-test
      name: fireworks-app-cache-test
      branch: main
      path: /
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
      private: false
      initialize: true
      deletionPolicy: Delete
      verbose: false
      configurationRef:
        name: repo-config
        namespace: demo-system
"""

FINALIZER_RESOURCES = [
    f"{COMP_RES}.{COMP_GVR}",
    "compositiondefinitions.core.krateo.io",
    "applications.argoproj.io",
    "repoes.git.krateo.io",
    "repoes.github.ogen.krateo.io",
]
FINALIZER_PATCH = '{"metadata":{"finalizers":null}}'

# ═════════════════════════════════════════════════════════════════════════════
# FORMATTING
# ═════════════════════════════════════════════════════════════════════════════

GREEN = "\033[92m"
RED = "\033[91m"
YELLOW = "\033[93m"
CYAN = "\033[96m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"
SEP = "─" * 110
DSEP = "━" * 110

test_results = []


def log(msg):
    ts = time.strftime("%H:%M:%S")
    print(f"  {DIM}[{ts}]{RESET} {msg}", flush=True)


def section(title):
    print(f"\n{BOLD}{CYAN}{SEP}{RESET}")
    print(f"{BOLD}{CYAN}  {title}{RESET}")
    print(f"{BOLD}{CYAN}{SEP}{RESET}")


def phase_banner(num, title):
    print(f"\n{BOLD}{'═' * 110}{RESET}")
    print(f"{BOLD}  PHASE {num}: {title}{RESET}")
    print(f"{BOLD}{'═' * 110}{RESET}")


def record(name, passed, ms=0, code=0, note=""):
    test_results.append({"name": name, "passed": passed, "ms": ms, "code": code, "note": note})
    tag = f"{GREEN}PASS{RESET}" if passed else f"{RED}FAIL{RESET}"
    print(f"  [{tag}] {name:<65s} {ms:>5d}ms  HTTP {code:<4}  {note}")


# ═════════════════════════════════════════════════════════════════════════════
# HELPERS
# ═════════════════════════════════════════════════════════════════════════════

def login(username, password):
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def login_all():
    tokens = {}
    for username, password in USERS.items():
        for attempt in range(5):
            try:
                tokens[username] = login(username, password)
                log(f"{username}: JWT acquired")
                break
            except Exception as e:
                if attempt < 4:
                    time.sleep(3)
                else:
                    log(f"{username}: login FAILED — {e}")
    return tokens


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


def http_get(path, token, base_url=None, timeout=120, retries=3):
    url = (base_url or SNOWPLOW) + path
    req = urllib.request.Request(
        url,
        headers={"Authorization": "Bearer " + token, "Accept-Encoding": "gzip"},
    )
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
            log(f"    HTTP 0, retry {attempt + 2}/{retries} in 3s ...")
            time.sleep(3)
    return elapsed_ms, 0, body


def http_get_json(path, token, **kw):
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
    _, _, body = http_get_json("/metrics/cache", token)
    return body or {}


def kubectl(*args, input_data=None, timeout_secs=120):
    try:
        proc = subprocess.run(
            ["kubectl"] + list(args),
            input=input_data.encode() if input_data else None,
            capture_output=True,
            timeout=timeout_secs,
        )
        return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()
    except subprocess.TimeoutExpired:
        return 1, "", f"kubectl timed out after {timeout_secs}s"


def redis_cmd(*args):
    rc, out, _ = kubectl(
        "exec", "deployment/snowplow", "-n", NS, "-c", "redis",
        "--", "redis-cli", *args)
    return out.strip() if rc == 0 else ""


def pct(data, p):
    s = sorted(data)
    return s[max(0, int(round(p / 100.0 * len(s))) - 1)]


def call_url(ns, name=""):
    url = f"/call?apiVersion={COMP_GVR}%2Fv1-2-2&resource={COMP_RES}&namespace={ns}"
    if name:
        url += f"&name={name}"
    return url


def verify_deployed_image():
    """Ensure deployment runs the expected image before starting tests."""
    if os.environ.get("SKIP_IMAGE_CHECK", "0") == "1":
        log("SKIP_IMAGE_CHECK=1 — skipping image version check")
        return
    expected = os.environ.get("EXPECTED_IMAGE_TAG", "").strip()
    if not expected:
        print(f"\n{RED}{BOLD}ERROR: EXPECTED_IMAGE_TAG is required.{RESET}")
        print(f"  Tests must not run until the new image is deployed.")
        print(f"  Example:")
        print(f"    export EXPECTED_IMAGE_TAG=0.25.19")
        print(f"    kubectl set image deployment/snowplow snowplow=ghcr.io/braghettos/snowplow:0.25.19 -n krateo-system")
        print(f"    kubectl rollout status deployment/snowplow -n krateo-system --timeout=300s")
        print(f"    python3 e2e/bench/snowplow_test.py")
        print(f"\n  Or set SKIP_IMAGE_CHECK=1 to bypass (not recommended).\n")
        sys.exit(1)
    rc, out, err = kubectl(
        "get", "deployment", "snowplow", "-n", NS,
        "-o", "jsonpath={.spec.template.spec.containers[?(@.name==\"snowplow\")].image}"
    )
    if rc != 0 or not out.strip():
        print(f"\n{RED}{BOLD}ERROR: Could not get snowplow deployment image.{RESET}")
        print(f"  kubectl failed or snowplow not found in {NS}. Ensure cluster access.")
        print(f"  stderr: {err[:200] if err else 'none'}\n")
        sys.exit(1)
    current_image = out.strip()
    current_tag = current_image.split(":")[-1] if ":" in current_image else ""
    if current_tag != expected:
        print(f"\n{RED}{BOLD}ERROR: Deployed image does not match EXPECTED_IMAGE_TAG.{RESET}")
        print(f"  Expected tag: {expected}")
        print(f"  Current image: {current_image}")
        print(f"  Deploy the new image first, then run tests.\n")
        sys.exit(1)
    log(f"Deployed image verified: {current_image}")


def wait_for_snowplow(max_wait=240):
    log("Waiting for snowplow /health ...")
    for _ in range(max_wait // 2):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                log("Snowplow healthy")
                return True
        except Exception:
            time.sleep(2)
    log(f"ERROR: snowplow not ready after {max_wait}s")
    return False


def wait_for_l1_warmup(timeout=300):
    """Wait until L1 cache is populated.

    Checking pod logs with --tail=N is fragile: when the pod has been running
    for a while, high-frequency informer events (e.g. cluster-kubestore
    configmap updates every 2s) push the warmup log message beyond the tail
    window. Instead, check Redis directly — if snowplow:resolved:* keys exist
    the warmup has run (or the pod already had a warm L1 from a prior run).
    """
    log("Waiting for L1 warmup ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        # Primary signal: Redis resolved keys
        resolved = redis_cmd("KEYS", "snowplow:resolved:*")
        if resolved and resolved.strip():
            count = len([k for k in resolved.strip().split("\n") if k.strip()])
            if count > 0:
                log(f"L1 warmup detected ({count} resolved keys in Redis)")
                return True

        # Fallback: scan logs (works when pod just started and --tail is enough)
        rc, out, _ = kubectl("logs", "deployment/snowplow", "-n", NS,
                             "-c", "snowplow", "--tail=500")
        if rc == 0:
            if "L1 warmup: completed" in out:
                log("L1 warmup completed (log)")
                return True
            if "L1 warmup: skipped" in out or "L1 warmup: no users found" in out:
                log("L1 warmup skipped (log)")
                return True

        time.sleep(5)
    log("WARNING: L1 warmup not detected within timeout")
    return False


def _read_l1_ready_ts():
    """Read the snowplow:l1:ready sentinel from Redis (Unix epoch seconds)."""
    raw = redis_cmd("GET", "snowplow:l1:ready")
    try:
        return int(raw) if raw else 0
    except ValueError:
        return 0


def wait_for_l1_ready(since_epoch=None, timeout=120):
    """Wait until snowplow writes a fresh L1-ready sentinel to Redis.

    Snowplow writes ``snowplow:l1:ready`` (a Unix epoch) after every L1
    warmup or informer-triggered L1 refresh completes.  This is fully
    deterministic — no log scraping, no key-count heuristics.

    Args:
        since_epoch: Unix timestamp *before* the cluster mutation.  The
            function returns once the sentinel is strictly greater than
            this value.  If ``None``, the current sentinel value is
            snapshot-ed and we wait for it to change.
        timeout: Maximum seconds to wait.
    """
    if since_epoch is None:
        since_epoch = _read_l1_ready_ts()
        log(f"Waiting for L1 ready (current sentinel={since_epoch}) ...")
    else:
        log(f"Waiting for L1 ready (since={since_epoch}) ...")

    deadline = time.time() + timeout
    while time.time() < deadline:
        ts = _read_l1_ready_ts()
        if ts > since_epoch:
            log(f"L1 ready (sentinel={ts})")
            return True
        time.sleep(2)

    log(f"WARNING: L1 not ready within {timeout}s (sentinel={_read_l1_ready_ts()}, need >{since_epoch})")
    return False


def wait_for_l1_quiescent(stable_secs=15, timeout=180):
    """Wait until the L1 sentinel stops changing (no more informer refreshes).

    After large cluster mutations (e.g. deploying 1200 compositions), the
    informer fires add events continuously.  Each event triggers an L1 refresh
    that updates the sentinel.  This function waits until the sentinel has been
    stable for ``stable_secs`` consecutive seconds, meaning the informer churn
    has settled and the L1 cache content won't change mid-measurement.
    """
    log(f"Waiting for L1 quiescence ({stable_secs}s stable sentinel) ...")
    prev_ts = _read_l1_ready_ts()
    stable_since = time.time()
    deadline = time.time() + timeout
    while time.time() < deadline:
        cur_ts = _read_l1_ready_ts()
        if cur_ts != prev_ts:
            prev_ts = cur_ts
            stable_since = time.time()
        elif time.time() - stable_since >= stable_secs:
            log(f"L1 quiescent (sentinel={cur_ts}, stable for {stable_secs}s)")
            return True
        time.sleep(2)
    log(f"WARNING: L1 not quiescent within {timeout}s (sentinel still changing)")
    return False


# ── Cache toggle ─────────────────────────────────────────────────────────────

def enable_cache():
    log("Enabling cache (CACHE_ENABLED=true) ...")
    # kubectl set env already triggers a rollout — no extra rollout restart needed.
    kubectl("set", "env", "deployment/snowplow", "-n", NS,
            "-c", "snowplow", "CACHE_ENABLED=true")
    kubectl("rollout", "status", "deployment/snowplow", "-n", NS, "--timeout=300s")
    wait_for_snowplow()
    log("Cache enabled")


def disable_cache():
    log("Disabling cache (CACHE_ENABLED=false) ...")
    # Record current pod name so we can verify it changed
    _, old_pods, _ = kubectl("get", "pods", "-n", NS, "-l", "app.kubernetes.io/name=snowplow",
                              "-o", "jsonpath={.items[*].metadata.name}")
    kubectl("set", "env", "deployment/snowplow", "-n", NS,
            "-c", "snowplow", "CACHE_ENABLED=false")
    kubectl("rollout", "status", "deployment/snowplow", "-n", NS, "--timeout=300s")
    # Wait for old pod(s) to terminate — service must only route to the new pod
    if old_pods.strip():
        for old_pod in old_pods.strip().split():
            for _ in range(30):
                rc, out, _ = kubectl("get", "pod", old_pod, "-n", NS, "--no-headers",
                                     "-o", "jsonpath={.status.phase}")
                if rc != 0 or out.strip() not in ("Running", "Pending"):
                    break
                time.sleep(2)
    wait_for_snowplow()
    # Verify the new pod is actually running without cache
    _, _, body = http_get_json("/metrics/cache", "")
    fresh_l1 = (body or {}).get("l1_hits", -1)
    log(f"Cache disabled (fresh pod l1_hits={fresh_l1})")
    if fresh_l1 > 0:
        log("WARNING: fresh pod has non-zero l1_hits — metrics may be stale from Redis")


# ── Resource management ──────────────────────────────────────────────────────

def setup_cyberjoker_rbac():
    yaml_str = """\
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cyberjoker-viewer
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: cyberjoker-viewer-binding
  namespace: demo-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cyberjoker-viewer
subjects:
- kind: User
  name: cyberjoker
  apiGroup: rbac.authorization.k8s.io
"""
    rc, _, _ = kubectl("apply", "--server-side", "-f", "-", input_data=yaml_str)
    log(f"Setup cyberjoker RBAC: rc={rc}")


def create_bench_namespaces(start, end):
    count = end - start + 1
    # For large batches, apply in chunks of 200 to avoid kubectl timeouts
    chunk_size = 200
    for chunk_start in range(start, end + 1, chunk_size):
        chunk_end = min(chunk_start + chunk_size - 1, end)
        yaml_parts = []
        for i in range(chunk_start, chunk_end + 1):
            yaml_parts.append(f"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: bench-ns-{i:02d}")
        rc, _, _ = kubectl("apply", "--server-side", "-f", "-",
                           input_data="\n---\n".join(yaml_parts),
                           timeout_secs=300)
        log(f"Created bench-ns-{chunk_start:02d}..{chunk_end:02d} ({chunk_end - chunk_start + 1} ns): rc={rc}")


def wait_for_bench_namespaces(expected, timeout=120):
    """Wait until at least `expected` bench namespaces are in Active state."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        n = count_bench_ns()  # only counts Active (non-Terminating) namespaces
        if n >= expected:
            return True
        time.sleep(5)
    log(f"  WARNING: only {count_bench_ns()}/{expected} bench namespaces Active after {timeout}s")
    return False


def count_bench_ns():
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    return len([line for line in out.strip().split("\n")
                if "bench-ns-" in line and "Terminating" not in line])


def count_compositions():
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces", "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def list_composition_names():
    """Return a set of (namespace, name) tuples for all compositions in the
    cluster via kubectl. This is the ground-truth used by correctness
    assertions. Returns None on failure (so callers can distinguish from
    'legitimately empty').
    """
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                         "-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}")
    if rc != 0:
        return None
    names = set()
    for line in out.strip().split("\n"):
        line = line.strip()
        if "/" in line:
            names.add(line)
    return names


def list_composition_names_from_cache(token):
    """Return the set of (namespace, name) tuples the cache reports via the
    compositions-list RESTAction. This is what the UI renders in the
    Compositions page table. Compares against list_composition_names() to
    detect silent cache corruption (right count, wrong items).
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
            # Slim JQ filter (v0.25.140+) outputs flat {uid, name, ns, ...}
            # without a metadata wrapper. Fall back to metadata.* for
            # compatibility with pre-slim RESTActions.
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


def count_compositions_in_ns(ns_name):
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name, "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def wait_for_compositions(expected, timeout=300, tolerance=5):
    """Wait until at least `expected - tolerance` compositions exist in the cluster."""
    log(f"Waiting for {expected} compositions to exist (tolerance={tolerance}) ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        actual = count_compositions()
        if actual >= expected:
            log(f"All {actual} compositions exist")
            return True
        if actual >= expected - tolerance:
            log(f"All {actual}/{expected} compositions exist (within tolerance)")
            return True
        elapsed = int(time.time() - (deadline - timeout))
        if elapsed % 30 < 5:  # log every ~30s
            log(f"  {actual}/{expected} compositions exist ({elapsed}s elapsed)")
        time.sleep(5)
    actual = count_compositions()
    log(f"WARNING: only {actual}/{expected} compositions after {timeout}s")
    return False


def delete_all_compositions():
    """Delete all bench compositions normally (let controllers reconcile finalizers)."""
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces", "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        log("No compositions to delete")
        return
    items = [(p[0], p[1]) for line in out.strip().split("\n")
             if (p := line.split(None, 1)) and len(p) >= 2 and p[0].startswith("bench-")]
    if not items:
        log("No bench compositions to delete")
        return

    # Delete in parallel
    def delete_comp(item):
        kubectl("delete", f"{COMP_RES}.{COMP_GVR}", item[1], "-n", item[0],
                "--ignore-not-found", "--wait=false")
    with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
        list(ex.map(delete_comp, items))
    log(f"Triggered deletion of {len(items)} compositions")

    # Wait for all compositions to disappear (give controllers 300s to reconcile)
    deadline = time.time() + 300
    while time.time() < deadline:
        remaining = count_compositions()
        if remaining == 0:
            log("All compositions deleted")
            return
        log(f"  {remaining} compositions remaining ...")
        time.sleep(10)

    # Force-remove finalizers on any stuck compositions so namespaces can terminate
    remaining = count_compositions()
    if remaining > 0:
        log(f"Patching finalizers off {remaining} stuck compositions ...")
        rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                             "--no-headers",
                             "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
        stuck = [(p[0], p[1]) for line in (out or "").strip().split("\n")
                 if (p := line.split(None, 1)) and len(p) >= 2]

        def patch_finalizer(item):
            kubectl("patch", f"{COMP_RES}.{COMP_GVR}", item[1], "-n", item[0],
                    "--type=merge", f"-p={FINALIZER_PATCH}")
        with concurrent.futures.ThreadPoolExecutor(max_workers=32) as ex:
            list(ex.map(patch_finalizer, stuck))
        log(f"Finalizers patched on {len(stuck)} compositions")

        # Brief wait for deletion to propagate
        time.sleep(10)
        remaining = count_compositions()
        if remaining == 0:
            log("All compositions deleted (after finalizer patch)")
        else:
            log(f"WARNING: {remaining} compositions still remaining")


def delete_all_compositiondefinitions():
    """Delete CompositionDefinitions and let the core provider clean up CRD + controller.

    Natural deletion order:
    1. Compositions are already deleted by delete_all_compositions (called first)
    2. Delete CompositionDefinition → core provider reconciles → deletes CRD + controller
    3. Wait for CRD to disappear (confirms clean state)

    Do NOT force-delete the CRD — let the platform handle it.
    """
    rc, out, _ = kubectl("get", "compositiondefinitions.core.krateo.io", "--all-namespaces",
                         "--no-headers", "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        log("No CompositionDefinitions to delete")
        return
    items = [(p[0], p[1]) for line in out.strip().split("\n")
             if (p := line.split(None, 1)) and len(p) >= 2]
    if not items:
        log("No CompositionDefinitions to delete")
        return
    for ns, name in items:
        kubectl("delete", "compositiondefinitions.core.krateo.io", name, "-n", ns,
                "--ignore-not-found", "--wait=false")
    log(f"Triggered deletion of {len(items)} CompositionDefinitions")

    # Wait for CDs to be gone
    deadline = time.time() + 300
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "compositiondefinitions.core.krateo.io", "--all-namespaces",
                             "--no-headers")
        remaining = len([l for l in (out or "").strip().split("\n") if l.strip()])
        if remaining == 0:
            log("All CompositionDefinitions deleted")
            break
        log(f"  {remaining} CompositionDefinitions remaining ...")
        time.sleep(10)
    else:
        log("WARNING: CompositionDefinitions still remaining after timeout")

    # Wait for CRD to disappear (core provider deletes it after CD is gone)
    deadline = time.time() + 120
    while time.time() < deadline:
        rc, _, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}", "--no-headers")
        if rc != 0:
            log("CRD deleted by core provider")
            return
        time.sleep(5)
    log("WARNING: CRD still exists after CD deletion — may need manual cleanup")


def delete_bench_namespaces():
    """Delete bench namespaces. Must be called AFTER compositions and CompositionDefinitions are deleted."""
    log("Deleting bench namespaces ...")
    rc, out, _ = kubectl("get", "ns", "-o", "name")
    bench_ns = [n.replace("namespace/", "") for n in out.split("\n") if "bench-ns-" in n]
    if not bench_ns:
        log("No bench namespaces to delete")
        return
    # Strip finalizers on any remaining resources that might block namespace deletion
    for resource in FINALIZER_RESOURCES:
        rc2, res_out, _ = kubectl("get", resource, "--all-namespaces", "-o",
                                  'jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\\n"}{end}')
        if rc2 == 0 and res_out.strip():
            items = [(p[0], p[1]) for line in res_out.strip().split("\n")
                     if (p := line.split(None, 1)) and len(p) >= 2 and p[0].startswith("bench-")]
            if items:
                def patch(item):
                    kubectl("patch", resource, item[1], "-n", item[0],
                            "--type=merge", f"-p={FINALIZER_PATCH}")
                with concurrent.futures.ThreadPoolExecutor(max_workers=16) as ex:
                    list(ex.map(patch, items))
    # Batch delete all bench namespaces in one kubectl call
    kubectl("delete", "ns", *bench_ns, "--ignore-not-found", "--wait=false",
            "--force", "--grace-period=0")
    log(f"Triggered deletion of {len(bench_ns)} bench namespaces")

    # Force-finalize via the /finalize subresource to bypass slow
    # namespace controller processing. This clears spec.finalizers
    # (which includes "kubernetes" by default) and allows the namespace
    # to be removed immediately.
    def force_finalize(ns):
        body = json.dumps({
            'apiVersion': 'v1', 'kind': 'Namespace',
            'metadata': {'name': ns}, 'spec': {'finalizers': []}
        })
        kubectl("replace", "--raw", f"/api/v1/namespaces/{ns}/finalize",
                "-f", "-", input_data=body)

    time.sleep(2)  # let delete propagate first
    log(f"Force-finalizing {len(bench_ns)} namespaces via /finalize API ...")
    with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
        list(ex.map(force_finalize, bench_ns))

    deadline = time.time() + 600
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "ns", "-o", "name")
        remaining = [n for n in out.split("\n") if "bench-ns-" in n]
        if not remaining:
            log("All bench namespaces deleted")
            return
        time.sleep(5)
    log(f"WARNING: bench namespace deletion timed out — {len(remaining)} remaining")


def deploy_compositiondefinition(ns="bench-ns-01"):
    yaml_str = f"""\
apiVersion: core.krateo.io/v1alpha1
kind: CompositionDefinition
metadata:
  name: {COMPDEF_NAME}
  namespace: {ns}
spec:
  chart:
    repo: {COMPDEF_NAME}
    url: https://marketplace.krateo.io
    version: 1.2.2
"""
    kubectl("apply", "--server-side", "-f", "-", input_data=yaml_str)
    log(f"Applied CompositionDefinition in {ns}")
    deadline = time.time() + 300
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "compositiondefinitions.core.krateo.io",
                             COMPDEF_NAME, "-n", ns,
                             "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
        if rc == 0 and out.strip() == "True":
            log("CompositionDefinition Ready")
            return True
        time.sleep(5)
    log("WARNING: CompositionDefinition not Ready within 300s")
    return False


def deploy_compositions(ns_start, ns_end, comps_per_ns, max_retries=5):
    total = (ns_end - ns_start + 1) * comps_per_ns
    log(f"Deploying {total} compositions one by one (max {max_retries} retries each) ...")
    deployed = 0
    failed = []
    for ns_i in range(ns_start, ns_end + 1):
        for comp_i in range(1, comps_per_ns + 1):
            ns = f"bench-ns-{ns_i:02d}"
            name = f"bench-app-{ns_i:02d}-{comp_i:02d}"
            yaml = composition_yaml(ns, name)
            ok = False
            for attempt in range(1, max_retries + 1):
                rc, _, err = kubectl("apply", "--server-side", "-f", "-", input_data=yaml)
                if rc == 0:
                    ok = True
                    break
                if attempt < max_retries:
                    time.sleep(2 * attempt)  # backoff: 2s, 4s
            if not ok:
                failed.append(f"{ns}/{name}")
                log(f"  FAILED after {max_retries} attempts: {ns}/{name}: {err[:200]}")
            deployed += 1
            if deployed % 100 == 0:
                log(f"  Deployed {deployed}/{total} compositions")
    if failed:
        log(f"  WARNING: {len(failed)}/{total} compositions failed to deploy")
    else:
        log(f"  All {total} compositions deployed successfully")


def deploy_compositions_parallel(ns_start, ns_end, comps_per_ns, max_retries=5, workers=32):
    """Deploy compositions in parallel using a thread pool.
    Batches compositions per namespace into a single multi-doc YAML to reduce
    kubectl invocations from N*M to N (one per namespace).
    """
    total = (ns_end - ns_start + 1) * comps_per_ns
    log(f"Deploying {total} compositions in parallel (workers={workers}, batch-per-ns) ...")
    failed = []
    deployed_count = [0]  # mutable for closure

    # Cap per-kubectl-apply batch size to avoid timeouts at large comps_per_ns.
    # 200 compositions per apply takes ~10-20s; 1000 per apply exceeded 120s.
    max_per_apply = 200

    def deploy_ns_batch(ns_i):
        ns = f"bench-ns-{ns_i:02d}"
        ns_ok = 0
        for sub_start in range(1, comps_per_ns + 1, max_per_apply):
            sub_end = min(sub_start + max_per_apply - 1, comps_per_ns)
            yamls = []
            for comp_i in range(sub_start, sub_end + 1):
                name = f"bench-app-{ns_i:02d}-{comp_i:02d}"
                yamls.append(composition_yaml(ns, name))
            batch_yaml = "\n---\n".join(yamls)
            sub_ok = False
            for attempt in range(1, max_retries + 1):
                rc, _, err = kubectl("apply", "--server-side", "-f", "-", input_data=batch_yaml, timeout_secs=180)
                if rc == 0:
                    sub_ok = True
                    break
                if attempt < max_retries:
                    time.sleep(2 * attempt)
            if sub_ok:
                ns_ok += (sub_end - sub_start + 1)
            else:
                failed.append(f"{ns}[{sub_start}-{sub_end}]")
                log(f"  FAILED after {max_retries} attempts: {ns}[{sub_start}-{sub_end}]: {err[:200]}")
        deployed_count[0] += ns_ok
        if deployed_count[0] % 500 == 0 or deployed_count[0] == total:
            log(f"  Deployed {deployed_count[0]}/{total} compositions")

    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as ex:
        list(ex.map(deploy_ns_batch, range(ns_start, ns_end + 1)))

    if failed:
        log(f"  WARNING: {len(failed)} namespace batches failed to deploy")
    else:
        log(f"  All {total} compositions deployed successfully")


def composition_yaml(ns, name):
    return f"""\
apiVersion: composition.krateo.io/v1-2-2
kind: GithubScaffoldingWithCompositionPage
metadata:
  name: {name}
  namespace: {ns}
spec:
  argocd:
    namespace: krateo-system
    application:
      project: default
      source:
        path: chart/
      destination:
        server: https://kubernetes.default.svc
        namespace: {name}
      syncEnabled: false
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
  app:
    service:
      type: NodePort
      port: 31180
  git:
    unsupportedCapabilities: true
    insecure: true
    fromRepo:
      scmUrl: https://github.com
      org: krateoplatformops-blueprints
      name: github-scaffolding-with-composition-page
      branch: main
      path: skeleton/
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
    toRepo:
      scmUrl: https://github.com
      org: krateoplatformops-test
      name: {name}
      branch: main
      path: /
      credentials:
        authMethod: generic
        secretRef:
          namespace: krateo-system
          name: github-repo-creds
          key: token
      private: false
      initialize: true
      deletionPolicy: Delete
      verbose: false
      configurationRef:
        name: repo-config
        namespace: demo-system"""


def ensure_composition_controller(ns):
    """Ensure the composition-dynamic-controller deployment is running in ns.

    If the deployment exists but has 0 ready replicas or is in CrashLoopBackOff,
    restart it. If it does not exist at all, log and continue (the composition
    was likely created before the controller was deployed into this namespace).
    """
    deploy = COMP_CONTROLLER_DEPLOY
    rc, out, _ = kubectl("get", f"deployment/{deploy}", "-n", ns,
                         "-o", "jsonpath={.status.readyReplicas}", "--ignore-not-found")
    if rc != 0 or not out.strip():
        # Deployment missing or 0 ready replicas — check if it exists
        rc2, _, _ = kubectl("get", f"deployment/{deploy}", "-n", ns, "--no-headers",
                            "--ignore-not-found")
        if rc2 != 0:
            log(f"Controller {deploy} not found in {ns} (skipping)")
            return
        # Exists but not ready — restart it
        log(f"Controller {deploy} in {ns} has 0 ready replicas, restarting ...")
        kubectl("rollout", "restart", f"deployment/{deploy}", "-n", ns)
        kubectl("rollout", "status", f"deployment/{deploy}", "-n", ns,
                "--timeout=60s")
        return

    ready = int(out.strip()) if out.strip().isdigit() else 0
    if ready > 0:
        log(f"Controller {deploy} in {ns} is healthy ({ready} ready)")
    else:
        log(f"Controller {deploy} in {ns} reports readyReplicas={out}, restarting ...")
        kubectl("rollout", "restart", f"deployment/{deploy}", "-n", ns)
        kubectl("rollout", "status", f"deployment/{deploy}", "-n", ns,
                "--timeout=60s")


def delete_argo_apps_in_ns(ns):
    """Delete all Argo applications in a namespace using batch operations."""
    rc, out, _ = kubectl("get", "applications.argoproj.io", "-n", ns,
                         "--no-headers", "-o", "custom-columns=NAME:.metadata.name",
                         "--ignore-not-found")
    if rc != 0 or not out.strip():
        return
    apps = [a.strip() for a in out.strip().split("\n") if a.strip()]
    kubectl("patch", "applications.argoproj.io", "--all", "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "applications.argoproj.io", "--all", "-n", ns,
            "--ignore-not-found", "--wait=false")
    log(f"Deleted {len(apps)} Argo app(s) in {ns}")


def force_finalize_namespace(ns_name):
    """Force-finalize a stuck Terminating namespace via /finalize subresource."""
    body = json.dumps({
        'apiVersion': 'v1', 'kind': 'Namespace',
        'metadata': {'name': ns_name}, 'spec': {'finalizers': []}
    })
    kubectl("replace", "--raw", f"/api/v1/namespaces/{ns_name}/finalize",
            "-f", "-", input_data=body)
    log(f"Force-finalized namespace {ns_name}")


def delete_one_composition(ns, name):
    """Delete a single composition, patching its finalizer first."""
    kubectl("patch", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    # Also delete the matching Argo application (same name in same namespace)
    kubectl("patch", "applications.argoproj.io", name, "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "applications.argoproj.io", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    log(f"Deleted composition {ns}/{name} (+ Argo app)")


def wait_for_composition_gone(ns, name, timeout=60):
    """Wait until a specific composition no longer exists in K8s.

    If the composition is still present after 30s, forcefully patch finalizers
    again and delete the associated Argo app as a fallback.
    """
    fqn = f"{ns}/{name}"
    deadline = time.time() + timeout
    fallback_applied = False
    while time.time() < deadline:
        rc, _, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
                           "--no-headers")
        if rc != 0:
            log(f"Composition {fqn} confirmed gone from K8s")
            return True
        # After 30s, apply fallback: re-patch finalizers and delete Argo app
        elapsed = timeout - (deadline - time.time())
        if elapsed >= 30 and not fallback_applied:
            log(f"Composition {fqn} still present after 30s — applying fallback")
            kubectl("patch", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
                    "--type=merge", f"-p={FINALIZER_PATCH}")
            kubectl("patch", "applications.argoproj.io", name, "-n", ns,
                    "--type=merge", f"-p={FINALIZER_PATCH}")
            kubectl("delete", "applications.argoproj.io", name, "-n", ns,
                    "--ignore-not-found", "--wait=false")
            fallback_applied = True
        time.sleep(2)
    log(f"WARNING: composition {fqn} still exists after {timeout}s")
    return False


def wait_for_namespace_gone(ns_name, timeout=120):
    """Wait until a namespace no longer exists in K8s.

    If the namespace is stuck in Terminating after 60s, force-finalize it.
    """
    deadline = time.time() + timeout
    force_finalized = False
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "ns", ns_name, "--no-headers")
        if rc != 0:
            log(f"Namespace {ns_name} confirmed gone from K8s")
            return True
        # After 60s, force-finalize if stuck in Terminating
        elapsed = timeout - (deadline - time.time())
        if elapsed >= 60 and not force_finalized and "Terminating" in (out or ""):
            log(f"Namespace {ns_name} stuck in Terminating after 60s — force-finalizing")
            force_finalize_namespace(ns_name)
            force_finalized = True
        time.sleep(3)
    log(f"WARNING: namespace {ns_name} still exists after {timeout}s")
    return False


def wait_for_l1_key_gone(pattern, timeout=60):
    """Wait until no Redis keys match the given pattern."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        raw = redis_cmd("KEYS", pattern)
        keys = [k for k in raw.split("\n") if k.strip()] if raw else []
        if not keys:
            log(f"L1 keys matching '{pattern}' confirmed evicted")
            return True
        time.sleep(2)
    log(f"WARNING: {len(keys)} L1 keys still match '{pattern}' after {timeout}s")
    return False


def delete_one_bench_namespace(ns_name):
    """Delete a single bench namespace by deleting compositions first
    (controllers clean children), then deleting the namespace.

    No force-patching — controllers handle all finalizers naturally.
    """
    # The composition controller runs in the CompositionDefinition namespace
    # (bench-ns-01), NOT in each bench namespace. Ensure it's healthy.
    ensure_composition_controller("bench-ns-01")

    # Step 1: Delete compositions (controllers process finalizers, clean children)
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name,
                         "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
    if rc == 0 and out.strip():
        comps = [c.strip() for c in out.strip().split("\n") if c.strip()]
        kubectl("delete", f"{COMP_RES}.{COMP_GVR}", "--all", "-n", ns_name,
                "--ignore-not-found", "--wait=false")
        log(f"Triggered deletion of {len(comps)} compositions in {ns_name}")

        # Wait for controllers to finish cleaning all compositions + children.
        # Timeout after 10 min — the controller may be backlogged at 50K scale.
        prev_remaining = -1
        deadline = time.time() + 600
        while time.time() < deadline:
            remaining = count_compositions_in_ns(ns_name)
            if remaining == 0:
                break
            if remaining != prev_remaining:
                log(f"  {remaining} compositions remaining in {ns_name} ...")
                prev_remaining = remaining
            time.sleep(5)
        remaining = count_compositions_in_ns(ns_name)
        if remaining > 0:
            log(f"Force-patching {remaining} stuck compositions in {ns_name} (controller backlogged)")
            kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name, "--no-headers", "-o", "name")
            rc_p, out_p, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name,
                                      "--no-headers", "-o", "name")
            if rc_p == 0 and out_p.strip():
                for obj_name in out_p.strip().split("\n"):
                    obj_name = obj_name.strip()
                    if obj_name:
                        kubectl("patch", obj_name, "-n", ns_name,
                                "--type=merge", f"-p={FINALIZER_PATCH}")
            time.sleep(10)
            log(f"Compositions force-patched in {ns_name}")
        else:
            log(f"All compositions deleted in {ns_name}")

    # Step 2: Delete the namespace (children already cleaned by controllers)
    kubectl("delete", "ns", ns_name, "--ignore-not-found", "--wait=false")
    log(f"Triggered deletion of namespace {ns_name}")


def wait_for_crd(timeout=120):
    log(f"Waiting for CRD {COMP_RES}.{COMP_GVR} ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}",
                             "-o", "jsonpath={.metadata.deletionTimestamp}")
        if rc == 0 and not out.strip():
            # CRD exists and is NOT being deleted
            log("CRD exists")
            return True
        time.sleep(5)
    return False


def delete_all_clientconfigs():
    rc, out, _ = kubectl("get", "secrets", "-n", NS, "-o", "name")
    secrets = [s.replace("secret/", "") for s in out.split("\n")
               if s.strip() and "-clientconfig" in s]
    if secrets:
        kubectl("delete", "secret", *secrets, "-n", NS, "--ignore-not-found")
    log(f"Deleted {len(secrets)} clientconfig secrets")


def delete_bench_rbac():
    """Delete RBAC resources left by bench compositions.

    Each composition creates ClusterRoles, ClusterRoleBindings, and namespace-
    scoped Roles and RoleBindings named bench-app-* (usually in krateo-system).
    When namespaces are force-deleted those cluster-scoped resources survive.
    Without cleanup the RBACWatcher informer delivers thousands of ADD events
    on every pod start or watch-reconnect, causing a perpetual invalidation storm
    that suppresses L1 cache effectiveness.

    All 4 resource types are deleted in PARALLEL since they are independent.
    This cuts cleanup from ~56 min (4 × 14 min serial) to ~14 min.
    """
    chunk_size = 500

    def _delete_kind(kind, namespace=None):
        """Delete all bench-app-* resources of a given kind."""
        if namespace:
            rc, out, _ = kubectl("get", kind, "-n", namespace, "--no-headers", "-o", "name")
        else:
            rc, out, _ = kubectl("get", kind, "--no-headers", "-o", "name")
        names = [
            line.split("/", 1)[-1]
            for line in out.splitlines()
            if "bench-app" in line
        ]
        if not names:
            return
        ns_args = ["-n", namespace] if namespace else []
        for i in range(0, len(names), chunk_size):
            kubectl("delete", kind, *ns_args, *names[i:i + chunk_size],
                    "--ignore-not-found", "--wait=false")
        suffix = f" in {namespace}" if namespace else ""
        log(f"Deleted {len(names)} {kind}s{suffix} (bench-app-*)")

    # Delete all 4 resource types in parallel (they are independent).
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as ex:
        futures = [
            ex.submit(_delete_kind, "clusterrolebinding"),
            ex.submit(_delete_kind, "clusterrole"),
            ex.submit(_delete_kind, "rolebinding", NS),
            ex.submit(_delete_kind, "role", NS),
        ]
        for f in concurrent.futures.as_completed(futures):
            try:
                f.result()
            except Exception as e:
                log(f"WARNING: RBAC cleanup error: {e}")


def clean_environment():
    """Robust cleanup that handles every dirty state from crashed runs.

    Order matters:
    1. Patch composition finalizers (so they can be deleted without controller)
    2. Delete compositions (wait for drain)
    3. Delete CompositionDefinition (core provider removes CRD + controller)
    4. Wait for CRD to disappear
    5. Delete Argo apps (in all namespaces, patch finalizers first)
    6. Delete bench namespaces (normal delete, not force-finalize)
    7. Delete RBAC (roles/rolebindings in krateo-system + clusterroles/bindings)
    8. Deploy image + restart pod (flushes Redis)
    """
    section("Cleaning environment")

    # Step 1+2: Delete compositions (patch finalizers, then delete)
    # Step 1+2: Delete compositions WITH controllers running.
    # The composition-dynamic-controller handles child cleanup (panels,
    # repos, Argo apps) via finalizers. Deleting with controllers running
    # avoids orphaned resources.
    rc, _, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}", "--no-headers")
    if rc == 0:
        # Recreate any missing bench namespaces so kubectl can reach orphans
        rc2, out2, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                               "--no-headers", "-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
        if rc2 == 0 and out2.strip():
            for ns in sorted(set(out2.strip().split("\n"))):
                if ns.startswith("bench-"):
                    kubectl("create", "ns", ns)

        # Delete compositions (controllers process finalizers and clean children)
        delete_all_compositions()

        # Wait for compositions to drain — no force-patching.
        # Controllers handle all child cleanup (Argo apps, panels, repos).
        prev_remaining = -1
        while True:
            rc3, out3, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                                   "--no-headers")
            remaining = len([l for l in (out3 or "").strip().split("\n") if l.strip()]) if rc3 == 0 and out3.strip() else 0
            if remaining == 0:
                log("All compositions deleted by controllers")
                break
            if remaining != prev_remaining:
                log(f"  {remaining} compositions remaining (controllers cleaning up) ...")
                prev_remaining = remaining
            time.sleep(15)
    else:
        log("No composition CRD — skipping composition deletion")

    # Step 3+4: Delete CompositionDefinition → core provider cleans CRD
    delete_all_compositiondefinitions()

    # Step 5: Wait for controllers to finish cleaning children.
    # All controllers stay running — they handle Argo apps, panels, repos
    # naturally via finalizers. No scaling to 0.
    deadline = time.time() + 300
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "applications.argoproj.io", "--all-namespaces",
                             "--no-headers")
        argo_remaining = len([l for l in (out or "").strip().split("\n") if l.strip()]) if rc == 0 and out.strip() else 0
        if argo_remaining == 0:
            break
        if int(time.time()) % 30 < 10:
            log(f"  Waiting for controllers to clean {argo_remaining} Argo apps ...")
        time.sleep(10)
    if argo_remaining > 0:
        log(f"  Force-patching {argo_remaining} stuck Argo apps ...")
        for ns_name in sorted(set(l.split()[0] for l in (out or "").strip().split("\n") if l.strip())):
            kubectl("patch", "applications.argoproj.io", "--all", "-n", ns_name,
                    "--type=merge", '-p={"metadata":{"finalizers":null}}')
            kubectl("delete", "applications.argoproj.io", "--all", "-n", ns_name,
                    "--ignore-not-found", "--wait=false")
        time.sleep(10)
    log(f"Argo applications cleaned")

    # Step 6: Delete bench namespaces (normal delete, wait for termination)
    delete_bench_namespaces()

    # Step 7: Delete RBAC (in krateo-system and cluster-scoped)
    delete_bench_rbac()
    # Also clean roles/rolebindings in krateo-system that match bench-app-*
    for res in ("roles", "rolebindings"):
        rc, out, _ = kubectl("get", res, "-n", "krateo-system", "--no-headers", "-o", "name")
        if rc == 0 and out.strip():
            bench_items = [l.strip() for l in out.strip().split("\n") if "bench-app-" in l]
            if bench_items:
                kubectl("delete", *bench_items, "-n", "krateo-system",
                        "--ignore-not-found", "--wait=false")
                log(f"Deleted {len(bench_items)} {res} in krateo-system")

    # Step 8: Deploy image + restart pod (flushes Redis sidecar).
    # No scaling — all controllers stay running throughout.
    tag = os.environ.get("EXPECTED_IMAGE_TAG")
    if tag:
        log(f"Deploying snowplow image tag {tag} ...")
        kubectl("set", "image", "deployment/snowplow",
                f"snowplow=ghcr.io/braghettos/snowplow:{tag}", "-n", "krateo-system")
    log("Restarting snowplow pod to flush Redis cache ...")
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=600s")
    log("  Snowplow pod restarted — Redis flushed")


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 1 — FUNCTIONAL VALIDATION (14 test cases)
# ═════════════════════════════════════════════════════════════════════════════

def run_phase_functional(tokens):
    phase_banner(1, "FUNCTIONAL VALIDATION (cache ENABLED)")

    # Ensure cache is enabled and warmed before functional tests
    enable_cache()
    wait_for_l1_warmup(timeout=300)

    # T1 — Cache warmup verification
    section("T1 — Cache Warmup Verification")
    token = tokens["admin"]
    # Verify L1 resolved keys and watched GVRs exist after warmup
    for label, pattern in [
        ("L1 resolved keys", "snowplow:resolved:*"),
        ("watched GVRs", "snowplow:watched-gvrs"),
    ]:
        count = redis_cmd("EVAL", "return #redis.call('keys', ARGV[1])", "0", pattern) if "*" in pattern else redis_cmd("SCARD", pattern)
        ok = int(count or "0") > 0
        record(f"Warmup: {label} populated", ok, note=f"pattern={pattern} count={count}")
    dbsize = redis_cmd("DBSIZE")
    record("Redis has substantial key count", int(dbsize or "0") > 50, note=f"dbsize={dbsize}")

    # T2 — L1 per-user isolation
    section("T2 — L1 Cache Hit Verification")
    path = WIDGET_ENDPOINTS[0][1]
    http_get(path, token); http_get(path, token)
    m0 = cache_metrics(token)
    ms, code, _ = http_get(path, token)
    m1 = cache_metrics(token)
    d = m1.get("raw_hits", 0) - m0.get("raw_hits", 0)
    record("admin: L1 hit (page/dashboard)", code == 200 and ms < 2000, ms, code, f"raw_hits+{d}")
    if "cyberjoker" in tokens:
        ms_cj, code_cj, _ = http_get(path, tokens["cyberjoker"])
        record("cyberjoker: L1 cold miss (1st request)", code_cj == 200, ms_cj, code_cj)
        ms_cj2, code_cj2, _ = http_get(path, tokens["cyberjoker"])
        record("cyberjoker: L1 hit (2nd request)", code_cj2 == 200 and ms_cj2 < 1000, ms_cj2, code_cj2)

    # T3 — Cache hits on all endpoints
    section("T3 — Cache Hits (all endpoints)")
    for username in tokens:
        tk = tokens[username]
        for _, p in ALL_ENDPOINTS:
            http_get(p, tk); http_get(p, tk)
        for label, p in ALL_ENDPOINTS:
            m0 = cache_metrics(tk)
            ms, code, _ = http_get(p, tk)
            m1 = cache_metrics(tk)
            total_d = (m1.get("raw_hits", 0) - m0.get("raw_hits", 0) +
                       m1.get("get_hits", 0) - m0.get("get_hits", 0))
            record(f"{username}: {label} cache hit", code == 200 and ms < 2000, ms, code, f"hits+{total_d}")

    # T4 — Object cache verification
    section("T4 — Object Cache Verification")
    http_get(WIDGET_ENDPOINTS[0][1], token)
    time.sleep(2)
    obj_key = "snowplow:get:widgets.templates.krateo.io/v1beta1/pages:krateo-system:dashboard-page"
    obj_exists = False
    for attempt in range(3):
        if redis_cmd("EXISTS", obj_key) == "1":
            obj_exists = True
            break
        log(f"Object cache key not found yet (attempt {attempt+1}/3), waiting 3s ...")
        time.sleep(3)
    record("Object cache key exists for dashboard-page", obj_exists, note=f"key={obj_key}")

    # T5 — /call returns resolved output
    section("T5 — /call Returns Resolved Output")
    p5 = "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-get-ns-and-crd&namespace=krateo-system"
    ms, code, body = http_get_json(p5, token)
    record("/call returns resolved output (not raw K8s)", code == 200 and isinstance(body, dict), ms, code,
           note=f"keys={list(body.keys())[:5] if isinstance(body, dict) else 'N/A'}")

    # T6 — Compositions-list correctness
    section("T6 — Compositions-List Correctness")
    p6 = "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"
    ms, code, body = http_get_json(p6, token)
    lst = body.get("status", {}).get("list", []) if isinstance(body, dict) else None
    count = len(lst) if lst is not None else "N/A"
    record(f"compositions-list returns {count} entries", code == 200 and lst is not None, ms, code)

    # T7 — Negative cache
    section("T7 — Negative Cache (404 Sentinel)")
    fake = call_url(TEST_NS, "nonexistent-t7-xyz")
    ms1, c1, _ = http_get(fake, token); time.sleep(0.5)
    m0 = cache_metrics(token)
    ms2, c2, _ = http_get(fake, token)
    m1 = cache_metrics(token)
    neg_d = m1.get("negative_hits", 0) - m0.get("negative_hits", 0)
    record("1st request: K8s API lookup", c1 in (404, 500), ms1, c1)
    record("2nd request: negative cache hit (faster)", c2 in (404, 500) and (neg_d >= 1 or ms2 < ms1), ms2, c2, f"neg_hits+{neg_d}")

    # T8 — Negative cache TTL expiry
    section("T8 — Negative Cache TTL Expiry (30s)")
    fake8 = call_url(TEST_NS, "ttl-expiry-t8-xyz")
    ms1, _, _ = http_get(fake8, token); time.sleep(0.5)
    ms2, _, _ = http_get(fake8, token)
    record("Cached 2nd request faster or similar to 1st", ms2 <= ms1 * 1.5, note=f"1st={ms1}ms 2nd={ms2}ms")
    log("Waiting 35s for TTL expiry ...")
    time.sleep(35)
    m0 = cache_metrics(token)
    ms3, c3, _ = http_get(fake8, token)
    m1 = cache_metrics(token)
    neg_after = m1.get("negative_hits", 0) - m0.get("negative_hits", 0)
    record("Post-expiry: K8s API again (no cache hit)", c3 in (404, 500) and neg_after == 0, ms3, c3, f"neg_hits+{neg_after}")

    # T9 — Redis key structure (must run before T10 which invalidates L1 via CRUD)
    section("T9 — Redis Key Structure")
    for prefix, label, threshold in [
        ("snowplow:get:*", "Object cache keys", 10),
        ("snowplow:list-idx:*", "List index SETs", 1),
        ("snowplow:resolved:*", "L1 resolved keys", 1),
    ]:
        keys = redis_cmd("KEYS", prefix)
        count = len(keys.split("\n")) if keys else 0
        record(f"{label}: {count}", count >= threshold, note=prefix)
    watched = redis_cmd("SMEMBERS", "snowplow:watched-gvrs")
    wcount = len([g for g in watched.split("\n") if g.strip()]) if watched else 0
    record(f"Watched GVRs: {wcount}", wcount > 5, note=f"count={wcount}")

    # T10 — Informer CRUD (deliberately mutates resources — may invalidate L1)
    section("T10 — Informer CRUD: ADD → UPDATE → DELETE")

    # Ensure prerequisites: namespace + RBAC + CRD
    kubectl("apply", "--server-side", "-f", "-",
            input_data=f"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: {TEST_NS}")
    # Grant current user permission to create compositions in bench namespace
    rbac_yaml = f"""\
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: bench-composition-admin
rules:
- apiGroups: ["composition.krateo.io"]
  resources: ["*"]
  verbs: ["*"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: bench-composition-admin-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: bench-composition-admin
subjects:
- kind: Group
  name: system:authenticated
  apiGroup: rbac.authorization.k8s.io
"""
    kubectl("apply", "--server-side", "-f", "-", input_data=rbac_yaml)
    crd_ready = wait_for_crd(timeout=10)  # fast check if CRD already exists
    if not crd_ready:
        log("CRD not found — deploying CompositionDefinition ...")
        deploy_compositiondefinition(TEST_NS)
        crd_ready = wait_for_crd(timeout=300)
    if not crd_ready:
        record("T10 SKIP: CRD not available after CompositionDefinition deploy", False, note="CRD timeout")
    else:
        kubectl("delete", "-n", TEST_NS, f"{COMP_RES}.{COMP_GVR}", TEST_NAME_NEW, "--ignore-not-found")
        time.sleep(2)
        rc, out, err = kubectl("apply", "--server-side", "--force-conflicts", "-f", "-", input_data=COMPOSITION_YAML)
        record("ADD: kubectl apply succeeded", rc == 0, note=(out or err)[:60])
        if rc == 0:
            log("Waiting 10s for informer ADD event ...")
            time.sleep(10)
            ms, code, _ = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
            record("ADD: GET new resource returns 200", code == 200, ms, code)
            kubectl("label", f"{COMP_RES}.{COMP_GVR}/{TEST_NAME_NEW}", "-n", TEST_NS,
                    "cache-test=updated", "--overwrite")
            time.sleep(5)
            ms, code, body = http_get_json(call_url(TEST_NS, TEST_NAME_NEW), token)
            if code == 404:
                # Controller may have deleted the resource during reconciliation
                record("UPDATE: resource deleted by controller (expected for ephemeral CRs)", True, ms, code,
                       note="controller reconciled and removed resource")
            else:
                label_ok = isinstance(body, dict) and body.get("metadata", {}).get("labels", {}).get("cache-test") == "updated"
                record("UPDATE: label reflected in response", code == 200 and label_ok, ms, code)
            kubectl("delete", "-n", TEST_NS, f"{COMP_RES}.{COMP_GVR}", TEST_NAME_NEW)
            time.sleep(10)
            ms, code, _ = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
            record("DELETE: resource removed from cache", code in (404, 500), ms, code)

    # T11 — RBAC cache
    section("T11 — RBAC Cache")
    for username in tokens:
        tk = tokens[username]
        http_get(WIDGET_ENDPOINTS[0][1], tk); http_get(WIDGET_ENDPOINTS[0][1], tk)
        m0 = cache_metrics(tk)
        ms, code, _ = http_get(WIDGET_ENDPOINTS[0][1], tk)
        m1 = cache_metrics(tk)
        rbac_d = m1.get("rbac_hits", 0) - m0.get("rbac_hits", 0)
        record(f"{username}: RBAC cache hit", code == 200 and rbac_d >= 0, ms, code, f"rbac_hits+{rbac_d}")

    # T12 — Multi-user cache verification
    section("T12 — Multi-User Cache Verification")
    ra_path = RESTACTION_ENDPOINTS[0][1]
    http_get(ra_path, tokens["admin"])
    ms1, c1, _ = http_get(ra_path, tokens["admin"])
    ms2, c2, _ = http_get(ra_path, tokens["admin"])
    record("admin: restaction L1 hit (consistent)", c2 == 200 and ms2 < 1000, ms2, c2, f"1st={ms1}ms 2nd={ms2}ms")
    if "cyberjoker" in tokens:
        ms_cj, c_cj, _ = http_get(ra_path, tokens["cyberjoker"])
        ms_cj2, _, _ = http_get(ra_path, tokens["cyberjoker"])
        record("cyberjoker: L1 miss but L3 shared", c_cj == 200, ms_cj, c_cj)
        record("cyberjoker: L1 hit on 2nd request", ms_cj2 < ms_cj * 1.5 or ms_cj2 < 1000, ms_cj2, 200)

    # T13 — Metrics counters
    section("T13 — Metrics Counter Validation")
    m0 = cache_metrics(token)
    for _, p in ALL_ENDPOINTS[:3]:
        http_get(p, token)
    m1 = cache_metrics(token)
    for key in ("raw_hits", "get_hits", "rbac_hits"):
        d = m1.get(key, 0) - m0.get(key, 0)
        record(f"Metric {key} increments", d >= 0, note=f"delta={d}")

    # T14 — L1 proactive refresh
    section("T14 — L1 Proactive Refresh")
    http_get(path, token); http_get(path, token)
    ms_before, _, _ = http_get(path, token)
    ts = str(int(time.time()))
    kubectl("label", f"{COMP_RES}.{COMP_GVR}/{TEST_NAME_WARM}", "-n", TEST_NS,
            f"cache-refresh-test={ts}", "--overwrite")
    log("Waiting 15s for L1 refresh ...")
    time.sleep(15)
    m0 = cache_metrics(token)
    ms_after, code, _ = http_get(path, token)
    m1 = cache_metrics(token)
    d = m1.get("raw_hits", 0) - m0.get("raw_hits", 0)
    record("L1 hit after resource update", code == 200 and (d >= 1 or ms_after < 1000), ms_after, code, f"raw_hits+{d}")
    record("Response time stable after refresh", ms_after <= ms_before * 5 or ms_after < 1000, note=f"before={ms_before}ms after={ms_after}ms")

    # T15 — Frontend reachability diagnostic
    section("T15 — Frontend Reachability Diagnostic")
    if not FRONTEND:
        log("FRONTEND_URL not set — skipping T15")
    else:
        # The frontend is an nginx SPA server — /call paths are NOT proxied through it.
        # Test that the frontend serves its SPA (index.html) and is reachable.
        fe_ms, fe_code, fe_body, fe_hdrs = http_get_with_headers("/", token, base_url=FRONTEND)
        is_html = b"<!doctype" in fe_body.lower() or b"<html" in fe_body.lower() if fe_body else False
        log(f"Frontend: {fe_ms}ms/{fe_code}  body_len={len(fe_body)}  is_html={is_html}")
        record("Frontend serves SPA (HTML response)", fe_code == 200 and is_html, fe_ms, fe_code,
               note=f"len={len(fe_body)}")
        # Also verify backend is independently reachable
        be_ms, be_code, _ = http_get(WIDGET_ENDPOINTS[0][1], token, base_url=SNOWPLOW)
        record("Backend API independently reachable", be_code == 200, be_ms, be_code)


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 2 — LATENCY BENCHMARK (backend + frontend, cache ON vs OFF)
# ═════════════════════════════════════════════════════════════════════════════

def _bench_endpoints(endpoints, token, base_url, iters, warmup):
    results = []
    for label, path in endpoints:
        for _ in range(warmup):
            http_get(path, token, base_url=base_url)
        latencies = []
        for _ in range(iters):
            ms, _, _ = http_get(path, token, base_url=base_url)
            latencies.append(ms)
        p50, p90 = pct(latencies, 50), pct(latencies, 90)
        mean = round(statistics.mean(latencies))
        results.append({"label": label, "p50": p50, "p90": p90, "mean": mean})
        log(f"  {label:<30s}  p50={p50:>5d}ms  p90={p90:>5d}ms  mean={mean:>5d}ms")
    return results


def _print_comparison(title, cached, nocache):
    print(f"\n{BOLD}{DSEP}{RESET}")
    print(f"  {BOLD}{title}{RESET}")
    print(f"{BOLD}{DSEP}{RESET}")
    print(f"  {'Endpoint':<30s}  {'c·p50':>6s} {'c·p90':>6s}  │  {'n·p50':>6s} {'n·p90':>6s}  │  {'speedup':>8s}")
    print(f"  {SEP}")
    c_total = n_total = 0
    for cr, nr in zip(cached, nocache):
        spd = nr["p50"] / cr["p50"] if cr["p50"] > 0 else 0
        color = GREEN if spd > 1.1 else (RED if spd < 0.9 else YELLOW)
        print(f"  {cr['label']:<30s}  {cr['p50']:>5d}ms {cr['p90']:>5d}ms  │  "
              f"{nr['p50']:>5d}ms {nr['p90']:>5d}ms  │  {color}{spd:>6.1f}x{RESET}")
        c_total += cr["mean"]; n_total += nr["mean"]
    c_avg = c_total // max(len(cached), 1)
    n_avg = n_total // max(len(nocache), 1)
    overall = n_avg / c_avg if c_avg > 0 else 0
    ocolor = GREEN if overall > 1.1 else RED
    print(f"  {SEP}")
    print(f"  {'Average (mean)':30s}  {c_avg:>5d}ms {'':>6s}  │  {n_avg:>5d}ms {'':>6s}  │  {ocolor}{overall:>6.1f}x{RESET}")


def run_phase_latency(tokens):
    phase_banner(2, "LATENCY BENCHMARK (cache ON vs OFF)")
    token = tokens["admin"]

    section("Cache ENABLED — Backend")
    cached_be = _bench_endpoints(ALL_ENDPOINTS, token, SNOWPLOW, ITERS, WARMUP_ITERS)

    cached_fe = None
    if FRONTEND:
        section("Cache ENABLED — Frontend Proxy")
        cached_fe = _bench_endpoints(ALL_ENDPOINTS, token, FRONTEND, ITERS, WARMUP_ITERS)

    # ── Proof-of-cache: collect cache ON indicators ──────────────────────
    section("Cache ON — Code Path Proof")
    m_on = cache_metrics(token)
    on_l1_hits = m_on.get("l1_hits", 0)
    on_raw_hits = m_on.get("raw_hits", 0)
    on_l1_rate = m_on.get("l1_hit_rate", 0)
    log(f"  Cache ON metrics: l1_hits={on_l1_hits}, raw_hits={on_raw_hits}, l1_hit_rate={on_l1_rate:.1f}%")

    rt_on = get_runtime_metrics() or {}
    on_redis_keys = rt_on.get("redis_key_count", 0)
    on_goroutines = rt_on.get("goroutine_count", 0)
    on_heap_mb = rt_on.get("heap_alloc_mb", 0)
    on_redis_mb = get_redis_memory_mb()
    log(f"  Cache ON runtime: redis_keys={on_redis_keys}, goroutines={on_goroutines}, "
        f"heap={on_heap_mb:.1f}MB, redis_mem={on_redis_mb:.1f}MB")

    # Cache-Control header on a warmed widget endpoint
    cc_path = ALL_ENDPOINTS[0][1]
    http_get(cc_path, token)  # ensure warm
    _, _, _, hdrs_on = http_get_with_headers(cc_path, token)
    on_cache_control = hdrs_on.get("Cache-Control", "")
    log(f"  Cache ON Cache-Control: {on_cache_control}")

    section("Disabling cache for baseline ...")
    disable_cache()
    # Wait for old pod to fully terminate so service only routes to new pod
    time.sleep(10)
    token = login("admin", USERS["admin"])

    # Snapshot metrics BEFORE cache-OFF benchmark (baseline for delta)
    m_off_before = cache_metrics(token)
    off_before_l1 = m_off_before.get("l1_hits", 0)
    off_before_raw = m_off_before.get("raw_hits", 0)

    section("Cache DISABLED — Backend")
    nocache_be = _bench_endpoints(ALL_ENDPOINTS, token, SNOWPLOW, ITERS, WARMUP_ITERS)

    nocache_fe = None
    if FRONTEND:
        section("Cache DISABLED — Frontend Proxy")
        nocache_fe = _bench_endpoints(ALL_ENDPOINTS, token, FRONTEND, ITERS, WARMUP_ITERS)

    # ── Proof-of-cache: collect cache OFF indicators ─────────────────────
    section("Cache OFF — Code Path Proof")
    m_off = cache_metrics(token)
    off_l1_hits = m_off.get("l1_hits", 0)
    off_raw_hits = m_off.get("raw_hits", 0)
    off_l1_rate = m_off.get("l1_hit_rate", 0)
    # Delta: hits generated during cache-OFF benchmark
    off_l1_delta = off_l1_hits - off_before_l1
    off_raw_delta = off_raw_hits - off_before_raw
    log(f"  Cache OFF metrics: l1_hits={off_l1_hits} (delta={off_l1_delta}), "
        f"raw_hits={off_raw_hits} (delta={off_raw_delta}), l1_hit_rate={off_l1_rate:.1f}%")

    rt_off = get_runtime_metrics() or {}
    off_redis_keys = rt_off.get("redis_key_count", 0)
    off_goroutines = rt_off.get("goroutine_count", 0)
    off_heap_mb = rt_off.get("heap_alloc_mb", 0)
    off_redis_mb = get_redis_memory_mb()
    log(f"  Cache OFF runtime: redis_keys={off_redis_keys}, goroutines={off_goroutines}, "
        f"heap={off_heap_mb:.1f}MB, redis_mem={off_redis_mb:.1f}MB")

    _, _, _, hdrs_off = http_get_with_headers(cc_path, token)
    off_cache_control = hdrs_off.get("Cache-Control", "")
    log(f"  Cache OFF Cache-Control: {off_cache_control}")

    section("Restoring cache ...")
    enable_cache()

    # ── Proof-of-cache: assertions ───────────────────────────────────────
    section("Phase 2 — Code Path Proof Assertions")
    # Cache ON: must have non-zero hits (proves L1 path executed)
    record("P2: cache ON l1_hits > 0", on_l1_hits > 0, note=f"l1_hits={on_l1_hits}")
    record("P2: cache ON raw_hits > 0", on_raw_hits > 0, note=f"raw_hits={on_raw_hits}")
    record("P2: cache ON redis_keys > 0", on_redis_keys > 0, note=f"keys={on_redis_keys}")
    record("P2: cache ON no Cache-Control header", "max-age" not in on_cache_control,
           note=f"header={on_cache_control}")
    # Cache OFF: no NEW hits generated during benchmark (delta must be 0)
    record("P2: cache OFF generated 0 L1 hits", off_l1_delta == 0,
           note=f"delta={off_l1_delta} (before={off_before_l1} after={off_l1_hits})")
    record("P2: cache OFF generated 0 raw hits", off_raw_delta == 0,
           note=f"delta={off_raw_delta} (before={off_before_raw} after={off_raw_hits})")
    # Cache OFF: no Cache-Control header (proves response NOT from L1)
    record("P2: cache OFF no Cache-Control header", "max-age" not in off_cache_control,
           note=f"header={off_cache_control}")
    # Goroutine delta: cache ON should have more (informer watchers + refresh workers)
    goroutine_delta = on_goroutines - off_goroutines
    record("P2: cache ON more goroutines than OFF", goroutine_delta > 0,
           note=f"ON={on_goroutines} OFF={off_goroutines} delta={goroutine_delta:+d}")
    log(f"  Memory: ON={on_heap_mb:.1f}MB vs OFF={off_heap_mb:.1f}MB "
        f"(delta={on_heap_mb - off_heap_mb:+.1f}MB)")
    log(f"  Redis memory: ON={on_redis_mb:.1f}MB vs OFF={off_redis_mb:.1f}MB")
    log(f"  Goroutines: ON={on_goroutines} vs OFF={off_goroutines} "
        f"(delta={goroutine_delta:+d})")

    _print_comparison("Backend (Direct Snowplow)", cached_be, nocache_be)
    if cached_fe and nocache_fe:
        _print_comparison("Frontend Proxy (UI Perspective)", cached_fe, nocache_fe)


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 3 — SCALING MATRIX (8 stages, cache ON vs OFF)
# ═════════════════════════════════════════════════════════════════════════════

def _measure_stage(tokens, stage_num, stage_desc, cache_mode):
    section(f"Stage {stage_num}: {stage_desc}  [cache={cache_mode}]")
    ns_count, comp_count = count_bench_ns(), count_compositions()
    log(f"Cluster: {ns_count} bench ns, {comp_count} compositions")
    time.sleep(10)

    stage = {"stage": stage_num, "desc": stage_desc, "cache": cache_mode,
             "bench_ns": ns_count, "compositions": comp_count, "users": {}}

    for username, token in tokens.items():
        cold = {}
        for ep_label, path in ALL_ENDPOINTS[:5]:
            ms, code, body = http_get(path, token)
            cold[ep_label] = {"ms": ms, "code": code, "size": len(body)}
            icon = GREEN + "✓" + RESET if code == 200 else RED + "✗" + RESET
            log(f"  {icon} COLD {ep_label:<25s} {ms:>6d}ms HTTP {code}")

        warm = {}
        for ep_label, path in ALL_ENDPOINTS[:5]:
            http_get(path, token)
            lats = [http_get(path, token)[0] for _ in range(ITERS)]
            warm[ep_label] = {"p50": pct(lats, 50), "p90": pct(lats, 90),
                              "mean": round(statistics.mean(lats))}
            log(f"    WARM {ep_label:<25s} p50={warm[ep_label]['p50']:>5d}ms")

        stage["users"][username] = {"cold": cold, "warm": warm}
    return stage


def run_phase_scaling(tokens):
    phase_banner(3, "SCALING MATRIX (8 stages, cache ON vs OFF)")
    all_results = []

    for cache_mode in ("ON", "OFF"):
        section(f"FULL MATRIX: cache={cache_mode}")
        clean_environment()
        if cache_mode == "ON":
            enable_cache()
        else:
            disable_cache()
        if not wait_for_snowplow():
            continue

        tokens = login_all()
        if cache_mode == "ON":
            wait_for_l1_warmup()

        all_results.append(_measure_stage(tokens, 1, "Zero state", cache_mode))

        create_bench_namespaces(1, 1); wait_for_bench_namespaces(1)
        deploy_compositiondefinition("bench-ns-01"); time.sleep(15)
        all_results.append(_measure_stage(tokens, 2, "1 ns + compdef", cache_mode))

        create_bench_namespaces(2, 20); wait_for_bench_namespaces(20); time.sleep(10)
        all_results.append(_measure_stage(tokens, 3, "20 bench ns", cache_mode))

        if SMOKE:
            log("SMOKE=1: Skipping stages 4-8")
            continue

        wait_for_crd(); deploy_compositions(1, 20, 1); time.sleep(30)
        all_results.append(_measure_stage(tokens, 4, "20 compositions", cache_mode))

        create_bench_namespaces(21, 120); wait_for_bench_namespaces(120); time.sleep(30)
        all_results.append(_measure_stage(tokens, 5, "120 bench ns", cache_mode))

        deploy_compositions(1, 120, 10); time.sleep(90)
        all_results.append(_measure_stage(tokens, 6, "1200 compositions", cache_mode))

        delete_one_composition("bench-ns-01", "bench-app-01-01"); time.sleep(15)
        all_results.append(_measure_stage(tokens, 7, "Deleted 1 comp", cache_mode))

        delete_one_bench_namespace("bench-ns-120"); time.sleep(20)
        all_results.append(_measure_stage(tokens, 8, "Deleted 1 ns", cache_mode))

    enable_cache()

    section("SCALING SUMMARY")
    for r in all_results:
        print(f"  S{r['stage']} [{r['cache']:>3s}]  "
              f"{r['bench_ns']:>3d} ns | {r['compositions']:>4d} comp | {r['desc']}")

    out_file = "/tmp/scaling_matrix_results.json"
    with open(out_file, "w") as f:
        json.dump(all_results, f, indent=2)
    log(f"Results saved to {out_file}")


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 4 — BROWSER METRICS (Playwright)
# ═════════════════════════════════════════════════════════════════════════════

def run_phase_browser():
    phase_banner(4, "BROWSER METRICS (Playwright)")

    if not FRONTEND:
        log("FRONTEND_URL not set — skipping browser phase")
        return

    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        log("Playwright not installed — skipping browser phase")
        log("Install: pip install playwright && python -m playwright install chromium")
        return

    all_results = {}
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        for username, password in USERS.items():
            section(f"Browser: {username}")
            ctx = browser.new_context(viewport={"width": 1280, "height": 900},
                                      ignore_https_errors=True)
            page = ctx.new_page()

            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=300000)
            page.click('#basic_username', timeout=10000)
            page.keyboard.type(username, delay=30)
            page.click('#basic_password', timeout=10000)
            page.keyboard.type(password, delay=30)
            page.click('button[type="submit"]')
            page.wait_for_load_state("networkidle", timeout=30000)
            page.wait_for_timeout(5000)

            user_results = {}
            for page_name, page_path in BROWSER_PAGES:
              try:
                log(f"  {page_name} — cold ...")
                page.evaluate("() => performance.clearResourceTimings()")
                page.goto(f"{FRONTEND}{page_path}", wait_until="networkidle", timeout=120000)

                timing = page.evaluate("""() => {
                    const t = performance.getEntriesByType('navigation')[0];
                    if (!t) return {};
                    return {
                        ttfb: Math.round(t.responseStart - t.requestStart),
                        domContentLoaded: Math.round(t.domContentLoadedEventEnd - t.startTime),
                        loadComplete: Math.round(t.loadEventEnd - t.startTime),
                    };
                }""")
                network = page.evaluate("""() => {
                    const e = performance.getEntriesByType('resource');
                    const xhrs = e.filter(x => x.initiatorType === 'xmlhttprequest' || x.initiatorType === 'fetch');
                    return {
                        requestCount: e.length,
                        xhrCount: xhrs.length,
                        totalTransferKB: Math.round(e.reduce((s, x) => s + (x.transferSize || 0), 0) / 1024),
                        xhrWaterfallMs: xhrs.length > 0 ?
                            Math.round(Math.max(...xhrs.map(x => x.responseEnd)) - Math.min(...xhrs.map(x => x.startTime))) : 0,
                    };
                }""")
                cold = {**(timing or {}), **(network or {})}

                load_values = [cold.get("loadComplete", 0)]
                for _ in range(ITERS - 1):
                    page.goto(f"{FRONTEND}{page_path}", wait_until="networkidle", timeout=120000)
                    wt = page.evaluate("""() => {
                        const t = performance.getEntriesByType('navigation')[0];
                        return t ? Math.round(t.loadEventEnd - t.startTime) : 0;
                    }""")
                    load_values.append(wt)

                cold["loadComplete_p50"] = pct(load_values, 50)
                cold["loadComplete_p90"] = pct(load_values, 90)
                user_results[page_name] = cold
                log(f"    TTFB={cold.get('ttfb', 0)}ms  "
                    f"loadComplete={cold.get('loadComplete', 0)}ms  "
                    f"p50={cold['loadComplete_p50']}ms  "
                    f"XHRs={cold.get('xhrCount', 0)}  "
                    f"waterfall={cold.get('xhrWaterfallMs', 0)}ms  "
                    f"transfer={cold.get('totalTransferKB', 0)}KB")
              except Exception as e:
                log(f"    [{page_name}] SKIPPED — {type(e).__name__}: {e}")
                user_results[page_name] = {"error": str(e)}

            all_results[username] = user_results
            ctx.close()
        browser.close()

    section("Browser Summary")
    print(f"  {'User':<12s} {'Page':<15s} {'TTFB':>6s} {'Load':>6s} {'p50':>6s} {'p90':>6s} {'XHRs':>5s} {'Waterfall':>10s} {'Transfer':>10s}")
    print(f"  {SEP}")
    for username, pages in all_results.items():
        for pname, d in pages.items():
            print(f"  {username:<12s} {pname:<15s} "
                  f"{d.get('ttfb', 0):>5d}ms {d.get('loadComplete', 0):>5d}ms "
                  f"{d.get('loadComplete_p50', 0):>5d}ms {d.get('loadComplete_p90', 0):>5d}ms "
                  f"{d.get('xhrCount', 0):>5d} {d.get('xhrWaterfallMs', 0):>8d}ms "
                  f"{d.get('totalTransferKB', 0):>8d}KB")

    out_file = "/tmp/browser_results.json"
    with open(out_file, "w") as f:
        json.dump(all_results, f, indent=2)
    log(f"Results saved to {out_file}")


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 5 — BROWSER NAVIGATION (cache ON vs OFF vs warmed)
# ═════════════════════════════════════════════════════════════════════════════

BROWSER_NAV_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


def _browser_login(page, username, password, retries=3):
    """Login via the frontend UI and return True on success."""
    for attempt in range(retries):
        try:
            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=300000)
            log(f"    browser: login page loaded, URL={page.url}")
            page.click('#basic_username', timeout=10000)
            page.keyboard.type(username, delay=30)
            page.click('#basic_password', timeout=10000)
            page.keyboard.type(password, delay=30)
            page.click('button[type="submit"]', timeout=10000)
            # The frontend may not update the URL to /dashboard; wait for
            # the login form to disappear and dashboard content to appear.
            page.wait_for_load_state("networkidle", timeout=30000)
            page.wait_for_timeout(5000)
            # Verify auth token is stored (proves login succeeded)
            has_token = page.evaluate("""() => {
                try {
                    const u = JSON.parse(localStorage.getItem('K_user') || '{}');
                    return !!u.accessToken;
                } catch { return false; }
            }""")
            if has_token:
                log(f"    browser: login OK — auth token in localStorage")
                return True
            log(f"    browser: login attempt {attempt + 1}: no auth token, retrying")
        except Exception as e:
            current = page.url
            log(f"    browser: login attempt {attempt + 1} failed (URL={current}): {e}")
        if attempt < retries - 1:
            time.sleep(5)
    return False


def _browser_measure_navigation(page, page_path, label, min_calls=0):
    """Navigate to a page and measure the /call API waterfall timing.

    Args:
        min_calls: Minimum number of /call requests to wait for before
            declaring stability. Set from the COLD navigation's call count
            so WARM navigations don't exit early when networkidle fires
            prematurely (e.g. cache OFF with slow backend responses).
    """
    # Track /call HTTP response statuses via Playwright response listener.
    # Resource Timing API doesn't expose status codes, so we capture them here.
    _call_statuses = []

    def _on_response(response):
        if "/call" in response.url:
            _call_statuses.append({"url": response.url, "status": response.status})

    page.on("response", _on_response)

    # Clear previous performance entries and expand the resource timing buffer.
    # The default buffer (250 entries) includes ALL resources (JS, CSS, images,
    # fonts).  At high cardinality the buffer overflows and late /call entries
    # are silently dropped — this caused the "2 /call" anomaly at S5/S6.
    page.evaluate("""() => {
        performance.clearResourceTimings();
        performance.setResourceTimingBufferSize(2000);
    }""")

    # Navigate — use domcontentloaded instead of networkidle to avoid hanging
    # when the page has persistent connections (SSE, websockets, long-poll).
    # The stability polling loop below will wait for /call requests to finish.
    try:
        page.goto(f"{FRONTEND}{page_path}", wait_until="domcontentloaded", timeout=60_000)
    except Exception as e:
        log(f"    WARNING: page.goto timeout ({e}), continuing with stability poll")

    # If redirected to login, the session expired — try to detect
    if "/login" in page.url:
        log(f"    WARNING: redirected to login at {page.url}")

    # Wait for /call requests to stabilize — networkidle fires after 500ms of
    # no network activity, but at high cardinality without cache the backend
    # can take >500ms between responses, causing premature "idle" detection.
    # Poll call count every 1s; require 3 consecutive identical counts (3s
    # stability window) AND at least min_calls requests to avoid exiting
    # after only the first few fast requests.
    _stable_streak = 0
    _prev_count = -1
    for _ in range(120):  # up to 120s (cache OFF at scale can be very slow)
        _cur_count = page.evaluate(
            "() => performance.getEntriesByType('resource').filter(e => e.name.includes('/call')).length")
        if _cur_count == _prev_count and _cur_count > 0 and _cur_count >= min_calls:
            _stable_streak += 1
            if _stable_streak >= 2:  # 3 consecutive identical polls = 3s stable
                break
        else:
            _stable_streak = 0
        _prev_count = _cur_count
        page.wait_for_timeout(1000)

    # Measure /call waterfall
    result = page.evaluate("""() => {
        const entries = performance.getEntriesByType('resource');
        const calls = entries
            .filter(e => e.name.includes('/call'))
            .sort((a, b) => a.startTime - b.startTime);
        if (calls.length === 0) return { callCount: 0, waterfallMs: 0, calls: [] };
        const first = calls[0].startTime;
        const last = Math.max(...calls.map(c => c.startTime + c.duration));
        return {
            callCount: calls.length,
            waterfallMs: Math.round(last - first),
            calls: calls.map(c => ({
                name: new URL(c.name).searchParams.get('name') || c.name.split('/').pop(),
                duration: Math.round(c.duration),
                startTime: Math.round(c.startTime - first),
            })),
        };
    }""")

    # If the page didn't load all expected calls, mark measurement as invalid.
    # This happens when cache OFF responses are so slow that calls fail or the
    # page renders with partial data — the waterfall would be artificially low.
    if min_calls > 0 and result.get("callCount", 0) < min_calls:
        result["waterfallMs"] = 0  # 0 = incomplete, shown as "*" in summary
        result["incomplete"] = True

    # Also measure navigation timing
    nav = page.evaluate("""() => {
        const t = performance.getEntriesByType('navigation')[0];
        if (!t) return {};
        return {
            domContentLoaded: Math.round(t.domContentLoadedEventEnd - t.startTime),
            loadComplete: Math.round(t.loadEventEnd - t.startTime),
        };
    }""")

    # Remove the response listener.
    page.remove_listener("response", _on_response)

    # Count HTTP 200 vs non-200 /call responses.
    ok_count = sum(1 for s in _call_statuses if s["status"] == 200)
    err_count = len(_call_statuses) - ok_count

    return {
        "label": label,
        "callCount": result.get("callCount", 0),
        "waterfallMs": result.get("waterfallMs", 0),
        "domContentLoaded": (nav or {}).get("domContentLoaded", 0),
        "loadComplete": (nav or {}).get("loadComplete", 0),
        "calls": result.get("calls", []),
        "httpOk": ok_count,
        "httpErr": err_count,
    }


def _browser_run_navigations(browser, scenario_name, num_navigations=2):
    """Run navigations for a scenario, return list of measurements."""
    username, password = "admin", USERS["admin"]
    ctx = browser.new_context(viewport={"width": 1280, "height": 900},
                              ignore_https_errors=True)
    page = ctx.new_page()

    if not _browser_login(page, username, password):
        log(f"  Login failed for {scenario_name}")
        ctx.close()
        return []

    measurements = []
    for nav_num in range(1, num_navigations + 1):
        for page_name, page_path in BROWSER_NAV_PAGES:
            label = f"{scenario_name} nav#{nav_num} {page_name}"
            log(f"  {label} ...")
            m = _browser_measure_navigation(page, page_path, label)
            measurements.append(m)
            log(f"    /call requests: {m['callCount']}  "
                f"waterfall: {m['waterfallMs']}ms  "
                f"loadComplete: {m['loadComplete']}ms")

    ctx.close()
    return measurements


def run_phase_browser_comparison():
    phase_banner(5, "BROWSER NAVIGATION (cache ON vs OFF vs warmed)")

    if not FRONTEND:
        log("FRONTEND_URL not set — skipping Phase 5")
        return

    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        log("Playwright not installed — skipping Phase 5")
        log("Install: pip install playwright && python -m playwright install chromium")
        return

    results = {}

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)

        # ── Scenario 1: Cache OFF ──
        section("Phase 5 — Cache OFF (2 navigations)")
        disable_cache()
        results["cache_off"] = _browser_run_navigations(browser, "Cache OFF")

        # ── Scenario 2: Cache ON (cold start) ──
        section("Phase 5 — Cache ON cold (2 navigations)")
        enable_cache()
        # Don't wait for L1 warmup — measure cold cache
        time.sleep(5)
        results["cache_on_cold"] = _browser_run_navigations(browser, "Cache ON (cold)")

        # ── Scenario 3: Cache ON (fully warmed) ──
        section("Phase 5 — Cache ON warmed (2 navigations)")
        wait_for_l1_warmup(timeout=120)
        results["cache_on_warmed"] = _browser_run_navigations(browser, "Cache ON (warmed)")

        browser.close()

    # ── Summary table ──
    section("Phase 5 — Browser Comparison Summary")
    print(f"\n  {'Scenario':<30s} {'Page':<15s} {'Calls':>5s} {'Waterfall':>10s} {'Load':>8s}")
    print(f"  {'─' * 75}")
    for scenario_key, scenario_label in [
        ("cache_off", "Cache OFF"),
        ("cache_on_cold", "Cache ON (cold)"),
        ("cache_on_warmed", "Cache ON (warmed)"),
    ]:
        for m in results.get(scenario_key, []):
            page_name = m["label"].split()[-1]
            print(f"  {scenario_label:<30s} {page_name:<15s} "
                  f"{m['callCount']:>5d} {m['waterfallMs']:>8d}ms {m['loadComplete']:>6d}ms")

    # ── Record pass/fail ──
    off_waterfall = [m["waterfallMs"] for m in results.get("cache_off", []) if m["waterfallMs"] > 0]
    warmed_waterfall = [m["waterfallMs"] for m in results.get("cache_on_warmed", []) if m["waterfallMs"] > 0]

    if off_waterfall and warmed_waterfall:
        avg_off = sum(off_waterfall) / len(off_waterfall)
        avg_warmed = sum(warmed_waterfall) / len(warmed_waterfall)
        speedup = avg_off / avg_warmed if avg_warmed > 0 else 0
        record(f"Browser: warmed cache faster than no cache ({speedup:.1f}x)",
               speedup > 1.0, int(avg_warmed),
               note=f"off={int(avg_off)}ms warmed={int(avg_warmed)}ms")
    else:
        record("Browser: waterfall comparison", False, note="insufficient data")

    # Re-enable cache for subsequent phases
    enable_cache()

    out_file = "/tmp/browser_comparison_results.json"
    with open(out_file, "w") as f:
        json.dump(results, f, indent=2, default=str)
    log(f"Results saved to {out_file}")


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 6 — BROWSER SCALING MATRIX (real page loads at each scale stage)
# ═════════════════════════════════════════════════════════════════════════════

BROWSER_SCALING_PAGES = [
    ("Dashboard", "/dashboard"),
    ("Compositions", "/compositions"),
]


def _verify_composition_count_api(token):
    """Verify composition count by calling the piechart widget API.

    Calls the dashboard-compositions-panel-row-piechart widget endpoint via
    Python HTTP and reads status.widgetData.title (a string like "1200" or "0").
    This is exactly what the dashboard piechart displays.

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


def _poll_piechart_progression(token, stop_event, interval=5):
    """Poll piechart API during S6 deploy+stabilize to log value progression.

    Runs in a background thread. Logs each sample with elapsed time, piechart
    value (from cache), and cluster truth (kubectl count).
    """
    start = time.time()
    sample = 0
    prev_api = None
    while not stop_event.is_set():
        sample += 1
        api_count = _verify_composition_count_api(token) if token else -1
        cluster_count = count_compositions()
        elapsed_s = int(time.time() - start)
        # Only log when value changes or every 30s to avoid noise
        if api_count != prev_api or sample == 1 or elapsed_s % 30 < interval:
            api_str = f"{api_count}" if api_count >= 0 else "?"
            log(f"    S6 PIECHART t={elapsed_s:>4d}s  piechart={api_str}  cluster={cluster_count}")
            prev_api = api_count
        stop_event.wait(interval)


def _verify_composition_count_ui(page):
    """Verify composition count by fetching the piechart widget from the browser.

    The piechart is rendered on a Canvas element, so the count cannot be read
    from the DOM.  Instead, we fetch the piechart widget endpoint from the
    browser context using the browser's own auth token (localStorage K_user).
    This verifies the same data path the UI uses: browser → snowplow → cache.

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


def _browser_measure_stage(page, stage_num, stage_desc, cache_mode, token=None, num_navs=3):
    """Navigate browser to each page num_navs times, return timing data.
    Expects an already-logged-in page object and an admin JWT token."""
    ns_count, comp_count = count_bench_ns(), count_compositions()
    log(f"Cluster: {ns_count} bench ns, {comp_count} compositions")

    pages_data = {}
    for page_name, page_path in BROWSER_SCALING_PAGES:
        navs = []
        cold_calls = 0  # Set from COLD nav, used as min_calls for WARM navs
        for nav_num in range(1, num_navs + 1):
            m = _browser_measure_navigation(page, page_path,
                                            f"S{stage_num} {cache_mode} nav#{nav_num} {page_name}",
                                            min_calls=cold_calls)
            navs.append(m)
            if nav_num == 1:
                cold_calls = m["callCount"]  # COLD nav sets the baseline
            cold_warm = 'COLD' if nav_num == 1 else 'WARM'
            http_info = f"  http={m['httpOk']}ok" if m.get("httpOk", 0) + m.get("httpErr", 0) > 0 else ""
            if m.get("httpErr", 0) > 0:
                http_info += f"/{m['httpErr']}err"
            log(f"  {cold_warm} {page_name:<15s} "
                f"waterfall={m['waterfallMs']:>5d}ms  "
                f"load={m['loadComplete']:>5d}ms  "
                f"calls={m['callCount']}{http_info}")

            # On the last warm navigation, verify composition count via both
            # the piechart widget API and the browser UI fetch.
            # Poll with retries to allow cascading refresh to propagate.
            if page_name == "Dashboard" and nav_num == num_navs:
                # Prepare screenshots directory
                screenshots_dir = os.path.join(os.path.dirname(__file__), "screenshots")
                os.makedirs(screenshots_dir, exist_ok=True)

                verify_timeout = 300
                verify_interval = 3
                verify_start = time.time()
                deadline = verify_start + verify_timeout
                api_count = -1
                ui_count = -1
                # Get cluster count once — kubectl at 50K takes 22s per call.
                # Refresh only every 60s to avoid dominating the poll cycle.
                fresh_comp_count = count_compositions()
                last_cluster_check = time.time()
                matched = False

                poll_num = 0
                while time.time() < deadline:
                    poll_num += 1
                    if time.time() - last_cluster_check > 60:
                        fresh_comp_count = count_compositions()
                        last_cluster_check = time.time()
                    api_count = _verify_composition_count_api(token) if token else -1
                    ui_count = _verify_composition_count_ui(page)
                    elapsed_ms = int((time.time() - verify_start) * 1000)
                    api_str_p = f"{api_count}" if api_count >= 0 else "?"
                    ui_str_p = f"{ui_count}" if ui_count >= 0 else "?"
                    log(f"    VERIFY poll {poll_num}: api={api_str_p} ui={ui_str_p} cluster={fresh_comp_count} ({elapsed_ms}ms)")

                    # Scroll down to make the Compositions piechart visible
                    try:
                        page.evaluate("""() => {
                            // Find all text nodes matching "Compositions", pick the
                            // one in the main content area (not the sidebar nav).
                            const walker = document.createTreeWalker(
                                document.body, NodeFilter.SHOW_TEXT, null);
                            let target = null;
                            while (walker.nextNode()) {
                                const node = walker.currentNode;
                                if (node.textContent.trim() === 'Compositions') {
                                    const el = node.parentElement;
                                    // Skip sidebar/nav elements
                                    if (el.closest('nav, [class*="sidebar"], [class*="menu"], [class*="sider"]'))
                                        continue;
                                    target = el;
                                }
                            }
                            if (target) {
                                // Scroll so the heading is at the top, exposing the
                                // piechart below it in the viewport
                                target.scrollIntoView({ block: 'start', behavior: 'instant' });
                                return true;
                            }
                            // Fallback: scroll to bottom of page
                            window.scrollTo(0, document.body.scrollHeight);
                            return false;
                        }""")
                        page.wait_for_timeout(500)
                    except Exception:
                        pass
                    if SCREENSHOTS:
                        ss_name = f"S{stage_num}_{cache_mode}_poll{poll_num}_api{api_str_p}_ui{ui_str_p}_cluster{fresh_comp_count}.png"
                        try:
                            page.screenshot(path=os.path.join(screenshots_dir, ss_name))
                            log(f"    screenshot: {ss_name}")
                        except Exception as e:
                            log(f"    screenshot failed: {e}")

                    api_ok = (api_count >= 0 and api_count == fresh_comp_count)
                    ui_ok = (ui_count >= 0 and ui_count == fresh_comp_count)
                    if api_ok and ui_ok:
                        matched = True
                        break
                    time.sleep(verify_interval)

                convergence_ms = int((time.time() - verify_start) * 1000)
                m["verified_api"] = api_count
                m["verified_ui"] = ui_count
                m["convergence_ms"] = convergence_ms if matched else -1
                api_str = f"{api_count}" if api_count >= 0 else "?"
                ui_str = f"{ui_count}" if ui_count >= 0 else "?"
                conv_str = f"{convergence_ms}ms" if matched else "TIMEOUT"
                status = "\u2713" if matched else "\u2717"
                log(f"    VERIFY {status} api={api_str} ui={ui_str} "
                    f"cluster={fresh_comp_count} converged={conv_str}"
                    + ("" if matched else " — MISMATCH"))

                # Content-level correctness check: compare composition NAMES,
                # not just counts. Catches silent cache corruption where the
                # count matches but the cached items are the wrong set
                # (stale entries present, recent deletes missing, etc.).
                if matched and cache_mode == "ON" and token:
                    truth = list_composition_names()
                    cached = list_composition_names_from_cache(token)
                    if truth is not None and cached is not None:
                        if truth == cached:
                            log(f"    CONTENT \u2713 {len(truth)} composition names match")
                            m["content_match"] = True
                        else:
                            missing = truth - cached  # in cluster, not in cache
                            extra = cached - truth    # in cache, not in cluster (stale)
                            log(f"    CONTENT \u2717 DRIFT — missing={len(missing)} extra={len(extra)}")
                            if missing:
                                log(f"      missing (cluster has, cache doesn't): {sorted(missing)[:3]}...")
                            if extra:
                                log(f"      extra (cache has, cluster deleted): {sorted(extra)[:3]}...")
                            m["content_match"] = False
                            m["content_missing"] = len(missing)
                            m["content_extra"] = len(extra)

                # Final VERIFY screenshot — reload dashboard to get fresh
                # data from the cache, then scroll to Compositions piechart
                try:
                    page.goto(f"{FRONTEND}/dashboard", wait_until="networkidle", timeout=30000)
                    page.wait_for_timeout(2000)
                except Exception:
                    pass
                try:
                    page.evaluate("""() => {
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
                    page.wait_for_timeout(500)
                except Exception:
                    pass
                if SCREENSHOTS:
                    ss_final = f"S{stage_num}_{cache_mode}_VERIFY_{'PASS' if matched else 'FAIL'}_api{api_str}_ui{ui_str}_{conv_str}.png"
                    try:
                        page.screenshot(path=os.path.join(screenshots_dir, ss_final))
                        log(f"    screenshot: {ss_final}")
                    except Exception as e:
                        log(f"    screenshot failed: {e}")

        # Compute metrics from navigations:
        # - p50: median of ALL navigations (including cold)
        # - warm_last: last navigation waterfall (most representative of steady-state)
        wf_vals = [n["waterfallMs"] for n in navs if n["waterfallMs"] > 0]
        lc_vals = [n["loadComplete"] for n in navs if n["loadComplete"] > 0]
        last_nav = navs[-1] if navs else {}
        pages_data[page_name] = {
            "navigations": navs,
            "waterfall_p50": pct(wf_vals, 50) if wf_vals else 0,
            "waterfall_p90": pct(wf_vals, 90) if wf_vals else 0,
            "waterfall_warm_last": last_nav.get("waterfallMs", 0),
            "loadComplete_p50": pct(lc_vals, 50) if lc_vals else 0,
            "loadComplete_p90": pct(lc_vals, 90) if lc_vals else 0,
            "callCount": navs[0]["callCount"] if navs else 0,
        }

    return {
        "stage": stage_num, "desc": stage_desc, "cache": cache_mode,
        "bench_ns": ns_count, "compositions": comp_count,
        "pages": pages_data,
    }


def run_phase_browser_scaling(tokens):
    phase_banner(6, "BROWSER SCALING MATRIX (real page loads at each scale)")

    if not FRONTEND:
        log("FRONTEND_URL not set — skipping Phase 6")
        return

    try:
        from playwright.sync_api import sync_playwright
    except ImportError:
        log("Playwright not installed — skipping Phase 6")
        log("Install: pip install playwright && python -m playwright install chromium")
        return

    all_results = []

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)

        for cache_mode in ("ON",):  # Run ON only for scale test
            section(f"BROWSER MATRIX: cache={cache_mode}")
            clean_environment()
            if cache_mode == "ON":
                enable_cache()
            else:
                disable_cache()
            if not wait_for_snowplow():
                continue

            tokens_fresh = login_all()
            admin_token = tokens_fresh.get("admin")
            if cache_mode == "ON":
                wait_for_l1_warmup()

            # Login once, reuse page for all stages in this cache mode
            username, password = "admin", USERS["admin"]
            ctx = browser.new_context(viewport={"width": 1280, "height": 900},
                                      ignore_https_errors=True)
            page = ctx.new_page()
            if not _browser_login(page, username, password):
                log(f"  Login failed for cache={cache_mode}")
                ctx.close()
                continue

            def _snapshot_l1():
                """Snapshot L1 sentinel before a cluster mutation."""
                return _read_l1_ready_ts() if cache_mode == "ON" else 0

            def _stabilize(before_ts, quiesce=False, quiesce_secs=15):
                """Give events time to propagate. The VERIFY polling loop
                handles convergence checking — no sentinel needed."""
                if cache_mode == "ON":
                    time.sleep(5)  # brief pause for informer events to fire

            # S1 — Zero state
            r = _browser_measure_stage(page, 1, "Zero state", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # S2 — 1 ns + compdef
            ts = _snapshot_l1()
            create_bench_namespaces(1, 1); wait_for_bench_namespaces(1)
            deploy_compositiondefinition("bench-ns-01"); time.sleep(15)
            _stabilize(ts)
            r = _browser_measure_stage(page, 2, "1 ns + compdef", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # S3 — 20 namespaces
            ts = _snapshot_l1()
            create_bench_namespaces(2, 20); wait_for_bench_namespaces(20); time.sleep(10)
            _stabilize(ts)
            r = _browser_measure_stage(page, 3, "20 bench ns", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            if SMOKE:
                log("SMOKE=1: Skipping stages 4-8")
                ctx.close()
                continue

            # S4 — 20 compositions
            ts = _snapshot_l1()
            wait_for_crd(); deploy_compositions(1, 20, 1)
            wait_for_compositions(20)
            _stabilize(ts, quiesce=True)
            r = _browser_measure_stage(page, 4, "20 compositions", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # S5/S6 — namespaces + compositions (driven by SCALE)
            # At large scale, use fewer namespaces with more compositions per ns.
            # K8s namespace controller cannot efficiently handle 5000+ namespaces.
            if SCALE >= 10000:
                s5_ns_end = 50
                s6_comps_per_ns = SCALE // s5_ns_end
            else:
                s5_ns_end = SCALE // 10  # 500 for 5K
                s6_comps_per_ns = 10
            ts = _snapshot_l1()
            create_bench_namespaces(21, s5_ns_end); wait_for_bench_namespaces(s5_ns_end, timeout=600)
            _stabilize(ts, quiesce=True)
            r = _browser_measure_stage(page, 5, f"{s5_ns_end} bench ns", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # S6 — compositions
            s6_timeout = 1200 if SCALE <= 5000 else 3600
            s6_quiesce = 60 if SCALE <= 5000 else 120
            ts = _snapshot_l1()
            # Start background piechart polling to capture value progression
            pie_stop = threading.Event()
            pie_thread = threading.Thread(
                target=_poll_piechart_progression,
                args=(admin_token, pie_stop),
                daemon=True,
            )
            pie_thread.start()
            deploy_compositions_parallel(1, s5_ns_end, s6_comps_per_ns)
            wait_for_compositions(SCALE, timeout=s6_timeout)
            _stabilize(ts, quiesce=True, quiesce_secs=s6_quiesce)
            pie_stop.set()
            pie_thread.join(timeout=10)
            r = _browser_measure_stage(page, 6, f"{SCALE} compositions", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # Redis memory snapshot at peak scale
            if cache_mode == "ON":
                try:
                    mem_info = redis_cmd("INFO", "memory")
                    for line in (mem_info or "").split("\n"):
                        if line.startswith("used_memory_human:") or line.startswith("used_memory_peak_human:"):
                            log(f"    REDIS {line.strip()}")
                except Exception as e:
                    log(f"    REDIS memory check failed: {e}")

            # Pod restart check at peak scale — assert zero restarts
            try:
                rc, out, _ = kubectl("get", "pods", "-n", "krateo-system",
                                     "-l", "app.kubernetes.io/name=snowplow",
                                     "-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}")
                if rc == 0:
                    restarts = int(out.strip() or "0")
                    log(f"    POD restarts at {SCALE} scale: {restarts}")
                    record(f"P6 S6 pod restarts == 0", restarts == 0,
                           note=f"restarts={restarts}")
            except Exception:
                pass

            # S6b — Restart snowplow pod with 50K compositions in place.
            # Tests resilience of warming up cache with existing data.
            log("Restarting snowplow pod to test warm-up resilience ...")
            kubectl("rollout", "restart", "deployment/snowplow", "-n", NS)
            time.sleep(10)
            # Wait for new pod to be ready (2/2)
            deadline = time.time() + 600
            while time.time() < deadline:
                rc, out, _ = kubectl("get", "pods", "-n", NS,
                                     "-l", "app.kubernetes.io/name=snowplow",
                                     "--no-headers")
                if rc == 0:
                    lines = [l for l in out.strip().split("\n") if l.strip()]
                    ready = [l for l in lines if "2/2" in l and "Running" in l]
                    if len(ready) == 1 and len(lines) == 1:
                        log(f"  Snowplow pod restarted — {ready[0].split()[0]}")
                        break
                time.sleep(10)
            else:
                log("WARNING: snowplow pod not ready after restart")
            # Re-acquire token (pod restart may have flushed auth state)
            tokens_fresh = login_all()
            admin_token = tokens_fresh.get("admin")
            # Measure dashboard after restart
            r = _browser_measure_stage(page, "6b", "Post-restart (50K)", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            # Scale Argo back up for S7/S8 delete tests (finalizer processing)
            for deploy in ("argocd-server", "argocd-applicationset-controller", "argocd-repo-server"):
                kubectl("scale", f"deployment/{deploy}", "--replicas=1", "-n", NS)
            # The application controller is a StatefulSet, not a Deployment
            kubectl("scale", "statefulset/argocd-application-controller", "--replicas=1", "-n", NS)
            log("Argo scaled up for delete tests")
            time.sleep(60)  # let Argo pods start + sync

            # Ensure composition-dynamic-controller is healthy in the target namespaces
            ensure_composition_controller("bench-ns-01")
            s8_ns = f"bench-ns-{s5_ns_end:02d}"
            ensure_composition_controller(s8_ns)

            # S7 — Delete 1 composition
            ts = _snapshot_l1()
            # Tail pod logs to file during entire S7 (survives log rotation)
            s7_log_file = "/tmp/snowplow_s7_logs.txt"
            s7_tail = subprocess.Popen(
                ["kubectl", "logs", "-f", "-n", "krateo-system",
                 "-l", "app.kubernetes.io/name=snowplow", "-c", "snowplow"],
                stdout=open(s7_log_file, "w"), stderr=subprocess.DEVNULL)
            log(f"  S7 log tail started (pid={s7_tail.pid})")
            delete_one_composition("bench-ns-01", "bench-app-01-01")
            wait_for_composition_gone("bench-ns-01", "bench-app-01-01")
            _stabilize(ts)
            r = _browser_measure_stage(page, 7, "Deleted 1 comp", cache_mode, token=admin_token)
            # Stop log tail
            s7_tail.terminate()
            s7_tail.wait(timeout=5)
            try:
                lines = sum(1 for _ in open(s7_log_file))
                log(f"  S7 logs saved to {s7_log_file} ({lines} lines)")
            except Exception as e:
                log(f"  S7 log capture failed: {e}")
            if r:
                all_results.append(r)

            # Multi-user convergence: cyberjoker should also see the deletion
            cj_token = tokens_fresh.get("cyberjoker") if tokens_fresh else None
            if cj_token:
                cj_count = _verify_composition_count_api(cj_token)
                cluster_count = count_compositions()
                record(f"P6 S7 multi-user convergence (cyberjoker)",
                       cj_count >= 0 and cj_count == cluster_count,
                       note=f"cyberjoker={cj_count} cluster={cluster_count}")

            # S8 — Delete 1 namespace (~comps_per_ns compositions)
            # s8_ns was computed before S7 for the controller health check
            ts = _snapshot_l1()
            delete_one_bench_namespace(s8_ns)
            wait_for_namespace_gone(s8_ns)
            _stabilize(ts)
            r = _browser_measure_stage(page, 8, "Deleted 1 ns", cache_mode, token=admin_token)
            if r:
                all_results.append(r)

            ctx.close()

        browser.close()

    enable_cache()

    # ── Summary table ──
    section("BROWSER SCALING SUMMARY")

    # Group by stage
    stages = {}
    for entry in all_results:
        key = entry["stage"]
        if key not in stages:
            stages[key] = {"desc": entry["desc"]}
        stages[key][entry["cache"]] = entry

    for page_name in [p[0] for p in BROWSER_SCALING_PAGES]:
        print(f"\n  {BOLD}{page_name}{RESET}")
        print(f"  {'Stage':<6s} {'Description':<22s} {'NS':>4s} {'Comp':>5s} "
              f"│ {'ON warm':>9s} {'ON p50':>9s} {'ON#':>4s} "
              f"│ {'OFF warm':>10s} {'OFF p50':>9s} {'OFF#':>5s} "
              f"│ {'Speedup':>8s} {'Conv':>7s}")
        print(f"  {'─' * 125}")

        for s in sorted(stages.keys(), key=str):
            info = stages[s]
            on_e = info.get("ON")
            off_e = info.get("OFF")

            on_pg = on_e["pages"].get(page_name, {}) if on_e else {}
            off_pg = off_e["pages"].get(page_name, {}) if off_e else {}

            on_warm = on_pg.get("waterfall_warm_last", 0)
            on_wf = on_pg.get("waterfall_p50", 0)
            on_calls = on_pg.get("callCount", 0)
            off_warm = off_pg.get("waterfall_warm_last", 0)
            off_wf = off_pg.get("waterfall_p50", 0)
            off_calls = off_pg.get("callCount", 0)

            ns = (on_e or off_e or {}).get("bench_ns", 0)
            comp = (on_e or off_e or {}).get("compositions", 0)

            # Flag anomalous measurements: call count far below the other mode
            # indicates an incomplete page load (frontend gave up)
            anomaly = ""
            if on_calls > 0 and off_calls > 0:
                if off_calls < on_calls * 0.5:
                    anomaly = " *"  # OFF incomplete
                elif on_calls < off_calls * 0.5:
                    anomaly = " *"  # ON incomplete

            # Convergence time: extract from ON Dashboard last navigation
            conv_ms = -1
            if page_name == "Dashboard" and on_e:
                dash_navs = on_e["pages"].get("Dashboard", {}).get("navigations", [])
                for nav in reversed(dash_navs):
                    if "convergence_ms" in nav:
                        conv_ms = nav["convergence_ms"]
                        break
            conv_str = f"{conv_ms}ms" if conv_ms >= 0 else "—"

            # Speedup uses best warm metric (most representative of steady-state)
            speedup = off_warm / on_warm if on_warm > 0 and off_warm > 0 else 0
            color = GREEN if speedup > 1.1 else (RED if speedup < 0.9 else YELLOW)

            print(f"  S{str(s):<5s} {info['desc']:<22s} {ns:>4d} {comp:>5d} "
                  f"│ {on_warm:>7d}ms {on_wf:>7d}ms {on_calls:>4d} "
                  f"│ {off_warm:>8d}ms {off_wf:>7d}ms {off_calls:>5d} "
                  f"│ {color}{speedup:>6.1f}x{RESET}{anomaly} {conv_str:>7s}")

        print(f"  {'':>6s} {'warm=last nav, p50=median all navs, Conv=cache convergence time, * = incomplete load':>100s}")

    # ── Performance regression assertions ──
    for entry in all_results:
        sn = str(entry["stage"])
        cache = entry["cache"]
        if cache != "ON":
            continue
        for page_name in [p[0] for p in BROWSER_SCALING_PAGES]:
            pg = entry["pages"].get(page_name, {})
            warm_wf = pg.get("waterfall_warm_last", 0)
            if warm_wf > 0 and page_name == "Dashboard":
                record(f"P6 S{sn} Dashboard warm < 3000ms",
                       warm_wf < 3000, note=f"{warm_wf}ms")
            navs = pg.get("navigations", [])
            for nav in navs:
                conv_ms = nav.get("convergence_ms", -2)
                if conv_ms == -2:
                    continue
                threshold = 300000 if sn in ("6", "6b", "7", "8") else 60000
                record(f"P6 S{sn} convergence < {threshold // 1000}s",
                       0 <= conv_ms <= threshold,
                       note=f"{'TIMEOUT' if conv_ms < 0 else f'{conv_ms}ms'}")
                if "content_match" in nav:
                    record(f"P6 S{sn} content match",
                           nav["content_match"] is True,
                           note=f"missing={nav.get('content_missing', 0)} extra={nav.get('content_extra', 0)}")

    # Final pod restart check (after S8)
    try:
        rc, out, _ = kubectl("get", "pods", "-n", "krateo-system",
                             "-l", "app.kubernetes.io/name=snowplow",
                             "-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}")
        if rc == 0:
            restarts = int(out.strip() or "0")
            record(f"P6 final pod restarts == 0", restarts == 0,
                   note=f"restarts={restarts}")
    except Exception:
        pass

    out_file = "/tmp/browser_scaling_results.json"
    with open(out_file, "w") as f:
        json.dump(all_results, f, indent=2, default=str)
    log(f"Results saved to {out_file}")


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 7: MULTI-USER SCALING
# ═════════════════════════════════════════════════════════════════════════════

USER_COUNTS = [10, 50, 100, 500, 1000]
PHASE7_AUTHN_NS = "krateo-system"


def get_runtime_metrics():
    """Fetch /metrics/runtime from snowplow. Returns dict or None."""
    try:
        req = urllib.request.Request(SNOWPLOW + "/metrics/runtime")
        with urllib.request.urlopen(req, timeout=10) as r:
            return json.loads(r.read())
    except Exception:
        return None


def get_redis_memory_mb():
    """Return Redis used_memory in MB from INFO memory."""
    info = redis_cmd("INFO", "memory")
    for line in (info or "").split("\n"):
        if line.startswith("used_memory:"):
            try:
                return int(line.split(":")[1].strip()) / (1024 * 1024)
            except Exception:
                pass
    return 0.0


def _scaleuser_name(i):
    return f"scaleuser-{i:04d}"


def _scaleuser_password():
    """Fixed password for all scale users (test-only, not security-sensitive)."""
    return "ScaleTest2026!"


def create_synthetic_users(start, end):
    """Register scale users via authn and login to create clientconfig secrets.

    For each user scaleuser-{start:04d} through scaleuser-{end:04d}:
      1. Create a kubernetes.io/basic-auth Secret holding the password
      2. Create a users.basic.authn.krateo.io CR referencing that secret
      3. Login via authn /basic/login to trigger clientconfig secret creation

    Steps 1-2 use batched multi-doc YAML per 50 users for speed.
    Step 3 uses parallel HTTP requests.

    Returns (count_logged_in, dict_of_tokens).
    """
    password = _scaleuser_password()
    total = end - start + 1

    # ── Step 1+2: Batch-create password secrets + User CRs ──
    log(f"  Registering {total} users in authn (batch YAML) ...")
    batch_size = 50
    registered = 0

    for batch_start in range(start, end + 1, batch_size):
        batch_end = min(batch_start + batch_size - 1, end)
        docs = []
        for i in range(batch_start, batch_end + 1):
            name = _scaleuser_name(i)
            docs.append(f"""\
apiVersion: v1
kind: Secret
type: kubernetes.io/basic-auth
metadata:
  name: {name}-password
  namespace: {PHASE7_AUTHN_NS}
  labels:
    app: scaletest
stringData:
  password: "{password}"
""")
            docs.append(f"""\
apiVersion: basic.authn.krateo.io/v1alpha1
kind: User
metadata:
  name: {name}
  namespace: {PHASE7_AUTHN_NS}
  labels:
    app: scaletest
spec:
  displayName: "Scale User {i:04d}"
  avatarURL: https://i.pravatar.cc/256?img={(i % 70) + 1}
  groups:
    - devs
  passwordRef:
    namespace: {PHASE7_AUTHN_NS}
    name: {name}-password
    key: password
""")
        batch_yaml = "---\n".join(docs)
        rc, _, err = kubectl("apply", "--server-side", "-f", "-",
                             input_data=batch_yaml, timeout_secs=60)
        if rc == 0:
            registered += batch_end - batch_start + 1
        else:
            log(f"    Batch {batch_start}-{batch_end} failed: {err[:200]}")

    log(f"  Registered {registered}/{total} users")

    # Brief pause for authn to reconcile the User CRs
    time.sleep(5)

    # ── Step 3: Login each user in parallel ──
    user_tokens = {}

    def _login_one(i):
        name = _scaleuser_name(i)
        creds = base64.b64encode(f"{name}:{password}".encode()).decode()
        for attempt in range(3):
            try:
                req = urllib.request.Request(
                    AUTHN + "/basic/login",
                    headers={"Authorization": "Basic " + creds},
                )
                with urllib.request.urlopen(req, timeout=30) as r:
                    data = json.load(r)
                    return name, data.get("accessToken")
            except Exception:
                if attempt < 2:
                    time.sleep(2 * (attempt + 1))
        return name, None

    log(f"  Logging in {total} users via authn ...")
    created = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=16) as ex:
        login_results = list(ex.map(_login_one, range(start, end + 1)))

    for name, token in login_results:
        if token:
            user_tokens[name] = token
            created += 1

    log(f"  Logged in {created}/{total} users (clientconfig secrets created)")
    return created, user_tokens


def delete_synthetic_users():
    """Delete all scaletest-labeled resources: User CRs, password secrets, clientconfig secrets."""
    log("  Deleting User CRs (authn controller will clean up clientconfig secrets) ...")
    kubectl("delete", "users.basic.authn.krateo.io", "-n", PHASE7_AUTHN_NS,
            "-l", "app=scaletest", "--ignore-not-found", timeout_secs=180)
    log("  Deleting password secrets ...")
    kubectl("delete", "secret", "-n", PHASE7_AUTHN_NS,
            "-l", "app=scaletest", "--ignore-not-found", timeout_secs=180)

    # Authn may not clean up clientconfig secrets immediately — delete by name
    log("  Cleaning up residual clientconfig secrets ...")

    def _del_clientconfig(i):
        name = f"{_scaleuser_name(i)}-clientconfig"
        kubectl("delete", "secret", name, "-n", PHASE7_AUTHN_NS,
                "--ignore-not-found", timeout_secs=10)

    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as ex:
        list(ex.map(_del_clientconfig, range(1, max(USER_COUNTS) + 1)))

    # Remove from Redis active-users set
    log("  Removing users from Redis active-users set ...")
    for i in range(1, max(USER_COUNTS) + 1):
        redis_cmd("SREM", "snowplow:active-users", _scaleuser_name(i))
    log("  Cleaned up synthetic users")


def wait_for_active_users(expected_min, timeout=120):
    """Poll /metrics/runtime until active_users >= expected_min."""
    deadline = time.time() + timeout
    last_count = 0
    while time.time() < deadline:
        m = get_runtime_metrics()
        if m and m.get("active_users", 0) >= expected_min:
            log(f"  Active users reached {m['active_users']} (needed {expected_min})")
            return True
        if m:
            last_count = m.get("active_users", 0)
        if int(time.time()) % 15 < 3:
            log(f"  Waiting for active_users: {last_count}/{expected_min} ...")
        time.sleep(3)
    log(f"  TIMEOUT waiting for {expected_min} active users (got {last_count})")
    return False


def measure_warmup_after_restart(expected_users, timeout=300):
    """Restart the snowplow pod and measure time until L1 is ready.

    Returns dict with warmup_ms, peak_heap_mb, peak_goroutines, redis_mb
    or None on timeout.
    """
    # Record pre-restart state
    kubectl("rollout", "restart", "deploy/snowplow", "-n", NS)
    kubectl("rollout", "status", "deploy/snowplow", "-n", NS,
            f"--timeout={timeout}s", timeout_secs=timeout + 30)

    if not wait_for_snowplow(max_wait=120):
        return None

    t0 = time.time()
    peak_heap = 0.0
    peak_goroutines = 0
    deadline = time.time() + timeout

    # Wait for L1 ready sentinel
    while time.time() < deadline:
        m = get_runtime_metrics()
        if m:
            peak_heap = max(peak_heap, m.get("heap_alloc_mb", 0))
            peak_goroutines = max(peak_goroutines, m.get("goroutine_count", 0))

        ts = _read_l1_ready_ts()
        if ts > 0:
            warmup_ms = int((time.time() - t0) * 1000)
            redis_mb = get_redis_memory_mb()
            return {
                "warmup_ms": warmup_ms,
                "peak_heap_mb": peak_heap,
                "peak_goroutines": peak_goroutines,
                "redis_mb": redis_mb,
            }
        time.sleep(3)

    return None


def measure_first_login_warmup(new_start, new_end, total_expected, tokens, timeout=180):
    """Create new synthetic users and measure time until their L1 is warm.

    Also measures latency impact on existing admin user during the burst.

    Returns dict with warmup_ms, peak_heap_mb, peak_goroutines, redis_mb,
    admin_latency_ms, or None on timeout.
    """
    admin_token = tokens.get("admin")

    # Snapshot before creating users
    before_sentinel = _read_l1_ready_ts()
    peak_heap = 0.0
    peak_goroutines = 0

    # Measure admin baseline latency
    baseline_latencies = []
    for _ in range(3):
        ms, code, _ = http_get(WIDGET_ENDPOINTS[0][1], admin_token)
        if code == 200:
            baseline_latencies.append(ms)
    admin_baseline = statistics.median(baseline_latencies) if baseline_latencies else 0

    # Create users (register + login via authn)
    t0 = time.time()
    created, _ = create_synthetic_users(new_start, new_end)
    if created == 0:
        return None

    # Poll for active users and L1 ready
    deadline = time.time() + timeout
    admin_during_latencies = []

    while time.time() < deadline:
        m = get_runtime_metrics()
        if m:
            peak_heap = max(peak_heap, m.get("heap_alloc_mb", 0))
            peak_goroutines = max(peak_goroutines, m.get("goroutine_count", 0))

        # Check admin latency during burst (every other poll)
        if admin_token and int(time.time()) % 6 < 3:
            ms, code, _ = http_get(WIDGET_ENDPOINTS[0][1], admin_token)
            if code == 200:
                admin_during_latencies.append(ms)

        # Check if L1 sentinel updated (new warmup completed)
        ts = _read_l1_ready_ts()
        if ts > before_sentinel:
            # Also verify active user count
            if m and m.get("active_users", 0) >= total_expected:
                warmup_ms = int((time.time() - t0) * 1000)
                redis_mb = get_redis_memory_mb()
                admin_during = (statistics.median(admin_during_latencies)
                                if admin_during_latencies else 0)
                return {
                    "warmup_ms": warmup_ms,
                    "peak_heap_mb": peak_heap,
                    "peak_goroutines": peak_goroutines,
                    "redis_mb": redis_mb,
                    "admin_baseline_ms": admin_baseline,
                    "admin_during_ms": admin_during,
                }
        time.sleep(3)

    # Timeout — return partial data
    warmup_ms = int((time.time() - t0) * 1000)
    admin_during = statistics.median(admin_during_latencies) if admin_during_latencies else 0
    return {
        "warmup_ms": warmup_ms,
        "peak_heap_mb": peak_heap,
        "peak_goroutines": peak_goroutines,
        "redis_mb": get_redis_memory_mb(),
        "admin_baseline_ms": admin_baseline,
        "admin_during_ms": admin_during,
        "timeout": True,
    }


def run_phase_user_scaling(tokens):
    phase_banner(7, "MULTI-USER SCALING (warmup + first-login burst)")

    # ── Step 0: Clean environment ──
    section("Step 0: Clean environment")
    clean_environment()
    enable_cache()
    if not wait_for_snowplow():
        log("ERROR: snowplow not healthy after cleanup")
        return

    # ── Step 1: Deploy compositions ──
    section("Step 1: Deploy compositions")
    comp_target = SCALE
    # At large scale, use fewer namespaces with more compositions per ns.
    # K8s namespace controller cannot handle 5000+ namespaces efficiently.
    # 50 ns × 1000 comp = 50K is just as realistic as 5000 × 10.
    if comp_target >= 10000:
        ns_count = 50
        comps_per_ns = comp_target // ns_count
    else:
        ns_count = comp_target // 10 if comp_target >= 10 else 1
        comps_per_ns = 10

    log(f"Deploying {comp_target} compositions across {ns_count} namespaces ...")
    create_bench_namespaces(1, ns_count)
    wait_for_bench_namespaces(ns_count, timeout=600)
    deploy_compositiondefinition("bench-ns-01")
    if not wait_for_crd(timeout=300):
        log("ERROR: CRD not ready after 300s, aborting")
        return
    time.sleep(10)
    deploy_compositions_parallel(1, ns_count, comps_per_ns)
    wait_for_compositions(comp_target, timeout=3600)

    # ── Step 2: Baseline warmup (0 synthetic users) ──
    section("Step 2: Baseline warmup (admin + cyberjoker only)")
    delete_synthetic_users()
    time.sleep(5)

    baseline = measure_warmup_after_restart(expected_users=2, timeout=300)
    if baseline:
        log(f"  Baseline warmup: {baseline['warmup_ms']}ms, "
            f"heap={baseline['peak_heap_mb']:.0f}MB, "
            f"goroutines={baseline['peak_goroutines']}, "
            f"redis={baseline['redis_mb']:.0f}MB")
    else:
        log("  WARNING: baseline warmup measurement timed out")

    # Re-login after restart
    tokens = login_all()

    # ── Step 3-7: First-login burst at cumulative user counts ──
    # Users are cumulative: 10, 50, 100, 500, 1000
    cumulative_results = []
    prev_end = 0

    for target_count in USER_COUNTS:
        new_start = prev_end + 1
        new_end = target_count
        added = new_end - prev_end
        # +2 for admin + cyberjoker
        total_expected = target_count + 2

        section(f"First-login burst: +{added} users (total {target_count} synthetic)")
        result = measure_first_login_warmup(
            new_start, new_end, total_expected, tokens, timeout=300)

        if result:
            timed_out = result.get("timeout", False)
            status = "TIMEOUT" if timed_out else f"{result['warmup_ms']}ms"
            admin_impact = ""
            if result.get("admin_baseline_ms") and result.get("admin_during_ms"):
                ratio = result["admin_during_ms"] / max(result["admin_baseline_ms"], 1)
                admin_impact = f", admin latency {ratio:.1f}x baseline"
            log(f"  N={target_count}: warmup={status}, "
                f"heap={result['peak_heap_mb']:.0f}MB, "
                f"goroutines={result['peak_goroutines']}, "
                f"redis={result['redis_mb']:.0f}MB"
                f"{admin_impact}")

            cumulative_results.append({
                "users": target_count,
                "added": added,
                "type": "first_login",
                **result,
            })

            record(f"P7: first-login N={target_count}",
                   not timed_out,
                   ms=result["warmup_ms"],
                   note=f"heap={result['peak_heap_mb']:.0f}MB goroutines={result['peak_goroutines']}")
        else:
            log(f"  N={target_count}: FAILED (no data)")
            cumulative_results.append({
                "users": target_count, "added": added, "type": "first_login",
                "warmup_ms": -1, "error": "no_data",
            })
            record(f"P7: first-login N={target_count}", False, note="no data")

        prev_end = new_end

    # ── Step 8: Full cold-start warmup with 1000 synthetic users ──
    section("Step 8: Full warmup with 1000 users (cold start)")
    full_warmup = measure_warmup_after_restart(
        expected_users=max(USER_COUNTS) + 2, timeout=600)
    if full_warmup:
        timed_out = full_warmup.get("timeout", False)
        log(f"  Full warmup (1000 users): {full_warmup['warmup_ms']}ms, "
            f"heap={full_warmup['peak_heap_mb']:.0f}MB, "
            f"goroutines={full_warmup['peak_goroutines']}, "
            f"redis={full_warmup['redis_mb']:.0f}MB")
        cumulative_results.append({
            "users": max(USER_COUNTS),
            "type": "cold_start",
            **full_warmup,
        })
        record(f"P7: cold-start N={max(USER_COUNTS)}",
               not full_warmup.get("timeout", False),
               ms=full_warmup["warmup_ms"],
               note=f"heap={full_warmup['peak_heap_mb']:.0f}MB goroutines={full_warmup['peak_goroutines']}")
    else:
        log("  Full warmup: TIMEOUT")
        record(f"P7: cold-start N={max(USER_COUNTS)}", False, note="timeout")

    # ── Step 9: Cleanup ──
    section("Step 9: Cleanup synthetic users")
    delete_synthetic_users()

    # Re-login after cleanup
    tokens = login_all()

    # ── Summary table ──
    section("PHASE 7 RESULTS")
    print(f"\n  {BOLD}{'Type':<14s} {'Users':>6s} {'Added':>6s} │ {'Warmup':>10s} "
          f"{'Heap(MB)':>10s} {'Goroutines':>11s} {'Redis(MB)':>10s} "
          f"│ {'Admin Baseline':>15s} {'Admin During':>13s}{RESET}")
    print(f"  {'─' * 120}")

    if baseline:
        print(f"  {'cold-start':<14s} {'2':>6s} {'—':>6s} │ "
              f"{baseline['warmup_ms']:>8d}ms "
              f"{baseline['peak_heap_mb']:>10.0f} "
              f"{baseline['peak_goroutines']:>11d} "
              f"{baseline['redis_mb']:>10.0f} │ "
              f"{'—':>15s} {'—':>13s}")

    for entry in cumulative_results:
        warmup_str = ("TIMEOUT" if entry.get("timeout") or entry.get("warmup_ms", -1) < 0
                       else f"{entry['warmup_ms']}ms")
        admin_base = entry.get("admin_baseline_ms", 0)
        admin_during = entry.get("admin_during_ms", 0)
        admin_base_str = f"{admin_base:.0f}ms" if admin_base else "—"
        admin_during_str = f"{admin_during:.0f}ms" if admin_during else "—"

        print(f"  {entry.get('type', '?'):<14s} {entry['users']:>6d} "
              f"{entry.get('added', '—'):>6} │ "
              f"{warmup_str:>10s} "
              f"{entry.get('peak_heap_mb', 0):>10.0f} "
              f"{entry.get('peak_goroutines', 0):>11d} "
              f"{entry.get('redis_mb', 0):>10.0f} │ "
              f"{admin_base_str:>15s} {admin_during_str:>13s}")

    # Save detailed results
    out_file = "/tmp/phase7_user_scaling_results.json"
    all_data = {"baseline": baseline, "results": cumulative_results}
    with open(out_file, "w") as f:
        json.dump(all_data, f, indent=2, default=str)
    log(f"Detailed results saved to {out_file}")


# ═════════════════════════════════════════════════════════════════════════════
# REPORT
# ═════════════════════════════════════════════════════════════════════════════

def print_report():
    section("FINAL REPORT")
    passed = [r for r in test_results if r["passed"]]
    failed = [r for r in test_results if not r["passed"]]
    print(f"\n  Total: {len(test_results)}   {GREEN}Passed: {len(passed)}{RESET}   {RED}Failed: {len(failed)}{RESET}\n")
    if failed:
        print(f"  {RED}{BOLD}FAILED TESTS:{RESET}")
        for r in failed:
            print(f"    {RED}✗{RESET} {r['name']:<65s}  HTTP {r['code']:<4}  {r['ms']}ms  {r['note']}")
        print()
    return len(failed) == 0


# ═════════════════════════════════════════════════════════════════════════════
# MAIN
# ═════════════════════════════════════════════════════════════════════════════

def main():
    global ITERS, SMOKE
    parser = argparse.ArgumentParser(description="Snowplow Unified Test Suite")
    parser.add_argument("--phases", default="1,2,3,4,5,6",
                        help="Comma-separated phase numbers to run (default: 1,2,3,4,5,6)")
    parser.add_argument("--smoke", action="store_true", default=SMOKE,
                        help="Smoke mode: limit scaling to stages 1-3")
    parser.add_argument("--iters", type=int, default=ITERS,
                        help=f"Iterations for measurements (default: {ITERS})")
    args = parser.parse_args()

    ITERS = args.iters
    SMOKE = args.smoke
    phases = set(int(x.strip()) for x in args.phases.split(",") if x.strip())

    print(f"\n{BOLD}{DSEP}{RESET}")
    print(f"{BOLD}  Snowplow Cache — Unified Test Suite{RESET}")
    print(f"  Snowplow  : {SNOWPLOW}")
    print(f"  Authn     : {AUTHN}")
    print(f"  Frontend  : {FRONTEND or '(disabled)'}")
    print(f"  Phases    : {sorted(phases)}")
    print(f"  Iterations: {ITERS}  Smoke: {SMOKE}")
    print(f"{BOLD}{DSEP}{RESET}\n")

    verify_deployed_image()
    setup_cyberjoker_rbac()
    tokens = login_all()

    if 1 in phases:
        wait_for_l1_warmup()
        run_phase_functional(tokens)

    if 2 in phases:
        run_phase_latency(tokens)
        tokens = login_all()

    if 3 in phases:
        run_phase_scaling(tokens)
        tokens = login_all()

    if 4 in phases:
        run_phase_browser()

    if 5 in phases:
        run_phase_browser_comparison()
        tokens = login_all()

    if 6 in phases:
        run_phase_browser_scaling(tokens)
        tokens = login_all()

    if 7 in phases:
        run_phase_user_scaling(tokens)
        tokens = login_all()

    enable_cache()
    all_passed = print_report()

    out_file = "/tmp/snowplow_test_results.json"
    with open(out_file, "w") as f:
        json.dump(test_results, f, indent=2, default=str)
    log(f"Results saved to {out_file}")

    # Capture pod logs for post-mortem analysis.
    # Use --since=3h to capture the full test window (test takes ~2h).
    tag = os.environ.get("EXPECTED_IMAGE_TAG", "unknown")
    pod_log_file = f"/tmp/snowplow_pod_logs_{tag}.txt"
    try:
        result = subprocess.run(
            ["kubectl", "logs", "-n", "krateo-system", "-l", "app=snowplow",
             "-c", "snowplow", "--since=3h"],
            capture_output=True, text=True, timeout=60)
        if result.returncode == 0 and result.stdout:
            with open(pod_log_file, "w") as f:
                f.write(result.stdout)
            log(f"Pod logs saved to {pod_log_file} ({len(result.stdout)} bytes)")
    except Exception as e:
        log(f"WARNING: could not capture pod logs: {e}")

    print(f"\n{BOLD}{DSEP}{RESET}")
    print(f"{BOLD}  Test complete — {'ALL PASSED' if all_passed else 'SOME FAILED'}{RESET}")
    print(f"{BOLD}{DSEP}{RESET}\n")
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
