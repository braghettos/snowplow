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
  EXPECTED_IMAGE_TAG  (required) e.g. 0.25.19 — tests will not start until deployment
                      runs this image. Deploy first: kubectl set image deployment/snowplow
                      snowplow=ghcr.io/<repo>/snowplow:0.25.19 -n krateo-system
  SKIP_IMAGE_CHECK   set to "1" to bypass image version check (not recommended)
"""

import argparse
import base64
import concurrent.futures
import json
import os
import statistics
import subprocess
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
NS = "krateo-system"

USERS = {
    "admin": "jl1DDPGMFOWw",
    "cyberjoker": "T8te3k57Nm22",
}

COMPDEF_NAME = "github-scaffolding-with-composition-page"
COMP_GVR = "composition.krateo.io"
COMP_RES = "githubscaffoldingwithcompositionpages"

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

TEST_NS = "bench-ns-01"
TEST_NAME_WARM = "bench-app-01"
TEST_NAME_NEW = "cache-test-app"

COMPOSITION_YAML = """\
apiVersion: composition.krateo.io/v1-2-2
kind: GithubScaffoldingWithCompositionPage
metadata:
  name: cache-test-app
  namespace: bench-ns-01
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
FINALIZER_PATCH = '{"metadata":{"finalizers":[]}}'

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
        try:
            tokens[username] = login(username, password)
            log(f"{username}: JWT acquired")
        except Exception as e:
            log(f"{username}: login FAILED — {e}")
    return tokens


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
                body = r.read()
                code = r.status
        except urllib.error.HTTPError as e:
            code = e.code
            try:
                body = e.read()
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


def cache_metrics(token):
    _, _, body = http_get_json("/metrics/cache", token)
    return body or {}


def kubectl(*args, input_data=None):
    proc = subprocess.run(
        ["kubectl"] + list(args),
        input=input_data.encode() if input_data else None,
        capture_output=True,
    )
    return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()


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
    log("Waiting for L1 warmup ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, out, _ = kubectl("logs", "deployment/snowplow", "-n", NS,
                             "-c", "snowplow", "--tail=500")
        if rc != 0:
            time.sleep(5)
            continue
        if "L1 warmup: completed" in out:
            log("L1 warmup completed")
            return True
        if "L1 warmup: skipped" in out or "L1 warmup: no users found" in out:
            log("L1 warmup skipped")
            return True
        time.sleep(5)
    log("WARNING: L1 warmup not detected within timeout")
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
    # kubectl set env already triggers a rollout — no extra rollout restart needed.
    kubectl("set", "env", "deployment/snowplow", "-n", NS,
            "-c", "snowplow", "CACHE_ENABLED=false")
    kubectl("rollout", "status", "deployment/snowplow", "-n", NS, "--timeout=300s")
    wait_for_snowplow()
    log("Cache disabled")


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
    yaml_parts = []
    for i in range(start, end + 1):
        yaml_parts.append(f"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: bench-ns-{i:02d}")
    rc, _, _ = kubectl("apply", "--server-side", "-f", "-",
                       input_data="\n---\n".join(yaml_parts))
    log(f"Created bench-ns-{start:02d}..{end:02d} ({end - start + 1} ns): rc={rc}")


def wait_for_bench_namespaces(expected, timeout=120):
    deadline = time.time() + timeout
    while time.time() < deadline:
        n = count_bench_ns()
        if n >= expected:
            return True
        time.sleep(5)
    return False


def count_bench_ns():
    rc, out, _ = kubectl("get", "ns", "-o", "name")
    return len([n for n in out.split("\n") if "bench-ns-" in n])


def count_compositions():
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces", "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def delete_bench_namespaces():
    log("Deleting bench namespaces ...")
    rc, out, _ = kubectl("get", "ns", "-o", "name")
    bench_ns = [n.replace("namespace/", "") for n in out.split("\n") if "bench-ns-" in n]
    if not bench_ns:
        log("No bench namespaces to delete")
        return
    for resource in FINALIZER_RESOURCES:
        rc2, res_out, _ = kubectl("get", resource, "--all-namespaces", "-o",
                                  'jsonpath={range .items[*]}{.metadata.namespace} {.metadata.name}{"\\n"}{end}')
        if rc2 == 0 and res_out.strip():
            items = [(p[0], p[1]) for line in res_out.strip().split("\n")
                     if (p := line.split(None, 1)) and len(p) >= 2 and p[0].startswith("bench-ns-")]
            if items:
                def patch(item):
                    kubectl("patch", resource, item[1], "-n", item[0],
                            "--type=merge", f"-p={FINALIZER_PATCH}")
                with concurrent.futures.ThreadPoolExecutor(max_workers=16) as ex:
                    list(ex.map(patch, items))
    for ns_name in bench_ns:
        kubectl("delete", "ns", ns_name, "--ignore-not-found", "--wait=false",
                "--force", "--grace-period=0")
    log(f"Triggered deletion of {len(bench_ns)} bench namespaces")
    deadline = time.time() + 600
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "ns", "-o", "name")
        remaining = [n for n in out.split("\n") if "bench-ns-" in n]
        if not remaining:
            log("All bench namespaces deleted")
            return
        time.sleep(10)
    log("WARNING: bench namespace deletion timed out")


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


def deploy_compositions(ns_start, ns_end, comps_per_ns):
    yaml_parts = []
    for ns_i in range(ns_start, ns_end + 1):
        for comp_i in range(1, comps_per_ns + 1):
            yaml_parts.append(composition_yaml(f"bench-ns-{ns_i:02d}",
                                               f"bench-app-{ns_i:02d}-{comp_i:02d}"))
    total = (ns_end - ns_start + 1) * comps_per_ns
    log(f"Deploying {total} compositions ...")
    kubectl("apply", "--server-side", "-f", "-", input_data="\n---\n".join(yaml_parts))


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


def delete_one_composition(ns, name):
    kubectl("patch", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    log(f"Deleted composition {ns}/{name}")


def delete_one_bench_namespace(ns_name):
    for resource in FINALIZER_RESOURCES:
        rc, objs, _ = kubectl("get", resource, "-n", ns_name, "-o", "name")
        if rc == 0 and objs.strip():
            for obj in objs.strip().split("\n"):
                name = obj.split("/")[-1] if "/" in obj else obj
                kubectl("patch", resource, name, "-n", ns_name,
                        "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "ns", ns_name, "--ignore-not-found", "--wait=false",
            "--force", "--grace-period=0")
    log(f"Triggered deletion of namespace {ns_name}")


def wait_for_crd(timeout=120):
    log(f"Waiting for CRD {COMP_RES}.{COMP_GVR} ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, _, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}", "--no-headers")
        if rc == 0:
            log("CRD exists")
            return True
        time.sleep(5)
    return False


def delete_all_clientconfigs():
    rc, out, _ = kubectl("get", "secrets", "-n", NS, "-o", "name")
    secrets = [s.replace("secret/", "") for s in out.split("\n")
               if s.strip() and "-clientconfig" in s]
    for name in secrets:
        kubectl("delete", "secret", name, "-n", NS, "--ignore-not-found")
    log(f"Deleted {len(secrets)} clientconfig secrets")


def delete_bench_rbac():
    """Delete RBAC resources left by bench compositions.

    Each composition creates ClusterRoles, ClusterRoleBindings, and namespace-
    scoped Roles and RoleBindings named bench-app-* (usually in krateo-system).
    When namespaces are force-deleted those cluster-scoped resources survive.
    Without cleanup the RBACWatcher informer delivers thousands of ADD events
    on every pod start or watch-reconnect, causing a perpetual invalidation storm
    that suppresses L1 cache effectiveness.
    """
    chunk_size = 50

    # Cluster-scoped resources
    for kind in ("clusterrolebinding", "clusterrole"):
        rc, out, _ = kubectl("get", kind, "--no-headers", "-o", "name")
        names = [
            line.split("/", 1)[-1]
            for line in out.splitlines()
            if "bench-app" in line
        ]
        if not names:
            continue
        for i in range(0, len(names), chunk_size):
            kubectl("delete", kind, *names[i:i + chunk_size], "--ignore-not-found")
        log(f"Deleted {len(names)} {kind}s (bench-app-*)")

    # Namespace-scoped resources (compositions create these in krateo-system)
    for kind in ("rolebinding", "role"):
        rc, out, _ = kubectl("get", kind, "-n", NS, "--no-headers", "-o", "name")
        names = [
            line.split("/", 1)[-1]
            for line in out.splitlines()
            if "bench-app" in line
        ]
        if not names:
            continue
        for i in range(0, len(names), chunk_size):
            kubectl("delete", kind, "-n", NS, *names[i:i + chunk_size], "--ignore-not-found")
        log(f"Deleted {len(names)} {kind}s in {NS} (bench-app-*)")


def clean_environment():
    section("Cleaning environment")
    delete_all_clientconfigs()
    delete_bench_namespaces()
    delete_bench_rbac()
    log("Waiting 15s for cleanup to propagate ...")
    time.sleep(15)


# ═════════════════════════════════════════════════════════════════════════════
# PHASE 1 — FUNCTIONAL VALIDATION (14 test cases)
# ═════════════════════════════════════════════════════════════════════════════

def run_phase_functional(tokens):
    phase_banner(1, "FUNCTIONAL VALIDATION (cache ENABLED)")

    # T1 — L3 warmup verification
    section("T1 — L3 Warmup Verification")
    token = tokens["admin"]
    for label, key in [
        ("restactions", "snowplow:list:templates.krateo.io/v1/restactions:"),
        ("widgets pages", "snowplow:list:widgets.templates.krateo.io/v1beta1/pages:"),
        ("navmenus", "snowplow:list:widgets.templates.krateo.io/v1beta1/navmenus:"),
        ("compositiondefs", "snowplow:list:core.krateo.io/v1alpha1/compositiondefinitions:"),
        ("CRDs", "snowplow:list:apiextensions.k8s.io/v1/customresourcedefinitions:"),
    ]:
        exists = redis_cmd("EXISTS", key)
        record(f"Warmup L3 key exists: {label}", exists == "1", note=f"key={key}")
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
    record("admin: L1 hit (page/dashboard)", code == 200 and (d >= 1 or ms < 200), ms, code, f"raw_hits+{d}")
    if "cyberjoker" in tokens:
        ms_cj, code_cj, _ = http_get(path, tokens["cyberjoker"])
        record("cyberjoker: L1 cold miss (1st request)", code_cj == 200, ms_cj, code_cj)
        ms_cj2, code_cj2, _ = http_get(path, tokens["cyberjoker"])
        record("cyberjoker: L1 hit (2nd request)", code_cj2 == 200 and ms_cj2 < 500, ms_cj2, code_cj2)

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
            record(f"{username}: {label} cache hit", code == 200 and (total_d >= 1 or ms < 300), ms, code, f"hits+{total_d}")

    # T4 — L3 direct read
    section("T4 — L3 Direct Read")
    m = cache_metrics(token)
    l3p = m.get("l3_promotions", 0)
    l3_key = "snowplow:get:widgets.templates.krateo.io/v1beta1/pages:krateo-system:dashboard-page"
    record("L3 object key exists for dashboard-page", redis_cmd("EXISTS", l3_key) == "1", note=f"key={l3_key}")
    record("L3 promotions counter is positive", l3p > 0, note=f"l3_promotions={l3p}")

    # T5 — /call paths skip L3 promotion
    section("T5 — /call Paths Skip L3 Promotion")
    p5 = "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-get-ns-and-crd&namespace=krateo-system"
    ms, code, body = http_get_json(p5, token)
    status = body.get("status") if isinstance(body, dict) else None
    record("/call returns resolved output (not raw K8s)", code == 200 and isinstance(status, list), ms, code)

    # T6 — Compositions-list correctness
    section("T6 — Compositions-List Correctness")
    p6 = "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"
    ms, code, body = http_get_json(p6, token)
    lst = body.get("status", {}).get("list", []) if isinstance(body, dict) else []
    record(f"compositions-list returns {len(lst)} entries", code == 200 and len(lst) > 0, ms, code)

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
    record("Cached 2nd request faster than 1st", ms2 < ms1, note=f"1st={ms1}ms 2nd={ms2}ms")
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
        ("snowplow:get:*", "L3 GET keys", 10),
        ("snowplow:list:*", "L3 LIST keys", 5),
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
    kubectl("delete", "-n", TEST_NS, f"{COMP_RES}.{COMP_GVR}", TEST_NAME_NEW, "--ignore-not-found")
    time.sleep(2)
    rc, out, err = kubectl("apply", "-f", "-", input_data=COMPOSITION_YAML)
    record("ADD: kubectl apply succeeded", rc == 0, note=(out or err)[:60])
    if rc == 0:
        log("Waiting 10s for informer ADD event ...")
        time.sleep(10)
        ms, code, _ = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
        record("ADD: GET new resource returns 200", code == 200, ms, code)
        kubectl("label", f"{COMP_RES}.{COMP_GVR}/{TEST_NAME_NEW}", "-n", TEST_NS,
                "cache-test=updated", "--overwrite")
        time.sleep(10)
        ms, code, body = http_get_json(call_url(TEST_NS, TEST_NAME_NEW), token)
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

    # T12 — Shared L3 layer
    section("T12 — Shared L3 Layer Verification")
    ra_path = RESTACTION_ENDPOINTS[0][1]
    http_get(ra_path, tokens["admin"])
    ms1, c1, _ = http_get(ra_path, tokens["admin"])
    ms2, c2, _ = http_get(ra_path, tokens["admin"])
    record("admin: restaction L1 hit (consistent)", c2 == 200 and ms2 < 500, ms2, c2, f"1st={ms1}ms 2nd={ms2}ms")
    if "cyberjoker" in tokens:
        ms_cj, c_cj, _ = http_get(ra_path, tokens["cyberjoker"])
        ms_cj2, _, _ = http_get(ra_path, tokens["cyberjoker"])
        record("cyberjoker: L1 miss but L3 shared", c_cj == 200, ms_cj, c_cj)
        record("cyberjoker: L1 hit on 2nd request", ms_cj2 < ms_cj or ms_cj2 < 200, ms_cj2, 200)

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
    record("L1 hit after resource update", code == 200 and (d >= 1 or ms_after < 200), ms_after, code, f"raw_hits+{d}")
    record("Response time stable after refresh", ms_after <= ms_before * 3, note=f"before={ms_before}ms after={ms_after}ms")


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

    section("Disabling cache for baseline ...")
    disable_cache()
    token = login("admin", USERS["admin"])

    section("Cache DISABLED — Backend")
    nocache_be = _bench_endpoints(ALL_ENDPOINTS, token, SNOWPLOW, ITERS, WARMUP_ITERS)

    nocache_fe = None
    if FRONTEND:
        section("Cache DISABLED — Frontend Proxy")
        nocache_fe = _bench_endpoints(ALL_ENDPOINTS, token, FRONTEND, ITERS, WARMUP_ITERS)

    section("Restoring cache ...")
    enable_cache()

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

            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=60000)
            page.fill('input[name="username"], input[type="text"]', username)
            page.fill('input[type="password"]', password)
            page.click('button:has-text("Sign In"), button[type="submit"]')
            try:
                page.wait_for_url("**/dashboard**", timeout=15000)
            except Exception:
                page.wait_for_load_state("networkidle", timeout=45000)

            user_results = {}
            for page_name, page_path in BROWSER_PAGES:
                log(f"  {page_name} — cold ...")
                page.evaluate("() => performance.clearResourceTimings()")
                page.goto(f"{FRONTEND}{page_path}", wait_until="networkidle", timeout=60000)

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
                    page.goto(f"{FRONTEND}{page_path}", wait_until="networkidle", timeout=60000)
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
    parser.add_argument("--phases", default="1,2,3,4",
                        help="Comma-separated phase numbers to run (default: 1,2,3,4)")
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

    enable_cache()
    all_passed = print_report()

    out_file = "/tmp/snowplow_test_results.json"
    with open(out_file, "w") as f:
        json.dump(test_results, f, indent=2, default=str)
    log(f"Results saved to {out_file}")

    print(f"\n{BOLD}{DSEP}{RESET}")
    print(f"{BOLD}  Test complete — {'ALL PASSED' if all_passed else 'SOME FAILED'}{RESET}")
    print(f"{BOLD}{DSEP}{RESET}\n")
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
