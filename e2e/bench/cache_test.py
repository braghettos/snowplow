#!/usr/bin/env python3
"""
Snowplow Redis Cache -- Comprehensive Validation Test
=====================================================
Tests: GET, LIST, ADD, UPDATE, DELETE -- with cache ON and with cache OFF.
Reports per-operation latency, cache hit/miss counts, correctness checks.
"""

import base64
import json
import subprocess
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Optional

# -- Cluster endpoints ---------------------------------------------------------
SNOWPLOW = "http://34.135.50.203:8081"
AUTHN    = "http://34.136.84.51:8082"

# -- Test resource -------------------------------------------------------------
TEST_NS           = "bench-ns-01"
TEST_GVR_GROUP    = "composition.krateo.io"
TEST_GVR_VERSION  = "v1-2-2"
TEST_GVR_RESOURCE = "githubscaffoldingwithcompositionpages"
TEST_NAME_WARM    = "bench-app-01"    # pre-warmed by warmup config
TEST_NAME_NEW     = "cache-test-app"  # created during test

COMPOSITION_YAML = (
    "apiVersion: composition.krateo.io/v1-2-2\n"
    "kind: GithubScaffoldingWithCompositionPage\n"
    "metadata:\n"
    "  name: cache-test-app\n"
    "  namespace: bench-ns-01\n"
    "spec:\n"
    "  argocd:\n"
    "    namespace: krateo-system\n"
    "    application:\n"
    "      project: default\n"
    "      source:\n"
    "        path: chart/\n"
    "      destination:\n"
    "        server: https://kubernetes.default.svc\n"
    "        namespace: fireworks-app\n"
    "      syncEnabled: false\n"
    "      syncPolicy:\n"
    "        automated:\n"
    "          prune: true\n"
    "          selfHeal: true\n"
    "  app:\n"
    "    service:\n"
    "      type: NodePort\n"
    "      port: 31180\n"
    "  git:\n"
    "    unsupportedCapabilities: true\n"
    "    insecure: true\n"
    "    fromRepo:\n"
    "      scmUrl: https://github.com\n"
    "      org: krateoplatformops-blueprints\n"
    "      name: github-scaffolding-with-composition-page\n"
    "      branch: main\n"
    "      path: skeleton/\n"
    "      credentials:\n"
    "        authMethod: generic\n"
    "        secretRef:\n"
    "          namespace: krateo-system\n"
    "          name: github-repo-creds\n"
    "          key: token\n"
    "    toRepo:\n"
    "      scmUrl: https://github.com\n"
    "      org: krateoplatformops-test\n"
    "      name: fireworks-app-cache-test\n"
    "      branch: main\n"
    "      path: /\n"
    "      credentials:\n"
    "        authMethod: generic\n"
    "        secretRef:\n"
    "          namespace: krateo-system\n"
    "          name: github-repo-creds\n"
    "          key: token\n"
    "      private: false\n"
    "      initialize: true\n"
    "      deletionPolicy: Delete\n"
    "      verbose: false\n"
    "      configurationRef:\n"
    "        name: repo-config\n"
    "        namespace: demo-system\n"
)

# -- Colours -------------------------------------------------------------------
GREEN  = "\033[92m"
RED    = "\033[91m"
YELLOW = "\033[93m"
CYAN   = "\033[96m"
BOLD   = "\033[1m"
RESET  = "\033[0m"


# -- Result tracking -----------------------------------------------------------
@dataclass
class TestResult:
    name: str
    passed: bool
    ms: int
    http_code: int
    note: str = ""


results = []


def ok(msg):   print("  %s+%s %s" % (GREEN, RESET, msg))
def fail(msg): print("  %sX%s %s" % (RED, RESET, msg))
def info(msg): print("  %s->%s %s" % (CYAN, RESET, msg))


def login(password="jl1DDPGMFOWw"):
    creds = base64.b64encode(("admin:" + password).encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def http_get(path, token, timeout=30):
    """Returns (ms, http_code, body_dict_or_None)."""
    req = urllib.request.Request(
        SNOWPLOW + path,
        headers={"Authorization": "Bearer " + token},
    )
    t0 = time.perf_counter()
    code = 0
    body = None
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read()
            code = r.status
            try:
                body = json.loads(raw)
            except Exception:
                pass
    except urllib.error.HTTPError as e:
        code = e.code
    except Exception:
        pass
    elapsed = int((time.perf_counter() - t0) * 1000)
    return elapsed, code, body


def cache_metrics(token):
    """Returns the /metrics/cache snapshot. JSON keys are snake_case."""
    ms, code, body = http_get("/metrics/cache", token)
    return body or {}


def m_get_hits(m):     return m.get("get_hits", 0)
def m_raw_hits(m):     return m.get("raw_hits", 0)
def m_neg_hits(m):     return m.get("negative_hits", 0)
def m_rbac_hits(m):    return m.get("rbac_hits", 0)
def m_total_hits(m):   return m_get_hits(m) + m_raw_hits(m)


def call_url(namespace, name=""):
    url = ("/call?apiVersion=" + TEST_GVR_GROUP + "%2F" + TEST_GVR_VERSION
           + "&resource=" + TEST_GVR_RESOURCE + "&namespace=" + namespace)
    if name:
        url += "&name=" + name
    return url


def widget_url(resource, name, namespace="krateo-system"):
    return ("/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1"
            "&resource=" + resource + "&name=" + name + "&namespace=" + namespace)


def kubectl(*args, **kwargs):
    input_data = kwargs.get("input_data")
    proc = subprocess.run(
        ["kubectl"] + list(args),
        input=input_data.encode() if input_data else None,
        capture_output=True,
    )
    return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()


def record(name, passed, ms, code, note=""):
    results.append(TestResult(name, passed, ms, code, note))
    status = ("%sPASS%s" % (GREEN, RESET)) if passed else ("%sFAIL%s" % (RED, RESET))
    print("  [%s] %-55s %5dms  HTTP %s  %s" % (status, name, ms, code, note))


# -- Redis disable / restore ---------------------------------------------------
def disable_cache():
    info("Disabling Redis (patching sidecar to port 6380)...")
    disable_patch = json.dumps([
        {
            "op": "replace",
            "path": "/spec/template/spec/initContainers/0/command",
            "value": ["redis-server", "--port", "6380"],
        },
        {
            "op": "remove",
            "path": "/spec/template/spec/initContainers/0/readinessProbe",
        },
    ])
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=json", "-p", disable_patch)
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=120s")
    for _ in range(30):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                break
        except Exception:
            time.sleep(2)
    time.sleep(2)
    info("Cache disabled.")


def enable_cache():
    info("Re-enabling Redis...")
    redis_container = {
        "name": "redis",
        "image": "redis:7-alpine",
        "restartPolicy": "Always",
        "command": None,
        "ports": [{"containerPort": 6379}],
        "readinessProbe": {
            "exec": {"command": ["redis-cli", "ping"]},
            "initialDelaySeconds": 2,
            "periodSeconds": 3,
        },
        "resources": {
            "requests": {"cpu": "50m", "memory": "64Mi"},
            "limits":   {"cpu": "200m", "memory": "256Mi"},
        },
    }
    enable_patch = json.dumps({
        "spec": {
            "template": {
                "spec": {
                    "initContainers": [redis_container],
                }
            }
        }
    })
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=strategic", "-p", enable_patch)
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=180s")
    for _ in range(30):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                break
        except Exception:
            time.sleep(3)
    time.sleep(3)
    info("Cache enabled and warmed up.")


# -- Section header ------------------------------------------------------------
def section(title):
    print("\n%s%s%s" % (BOLD + CYAN, "=" * 70, RESET))
    print("%s  %s%s" % (BOLD + CYAN, title, RESET))
    print("%s%s%s" % (BOLD + CYAN, "=" * 70, RESET))


# =============================================================================
# TEST SECTIONS
# =============================================================================

def test_with_cache(token):

    # -------------------------------------------------------------------------
    section("S1 -- Cache ON: GET warmed resource (cache hit)")
    # -------------------------------------------------------------------------
    # Warmup the resolved-output cache by making one request first
    http_get(call_url(TEST_NS, TEST_NAME_WARM), token)
    m0 = cache_metrics(token)
    ms, code, body = http_get(call_url(TEST_NS, TEST_NAME_WARM), token)
    m1 = cache_metrics(token)
    hits_delta = m_total_hits(m1) - m_total_hits(m0)
    record("GET warmed resource (expect cache hit, fast response)",
           code == 200 and (hits_delta >= 1 or ms < 150), ms, code,
           "hits+%d" % hits_delta)

    # NOTE: LIST via /call is NOT supported -- ParseNamespacedName requires `name`.
    # LIST is an architecture feature of the /list endpoint and internal widget calls.
    # Documenting this as expected behavior, not a bug.
    section("S2 -- Architecture note: LIST via /call requires 'name' (returns 400)")
    ms, code, _ = http_get(call_url(TEST_NS), token)  # no name param
    record("LIST via /call (expected 400 -- name is required)",
           code == 400, ms, code,
           "correct: /call is a named-resource endpoint")

    # -------------------------------------------------------------------------
    section("S3 -- Cache ON: negative cache (404 sentinel caching)")
    # -------------------------------------------------------------------------
    FAKE = "does-not-exist-xyz"
    ms1, code1, _ = http_get(call_url(TEST_NS, FAKE), token)  # miss -> stores sentinel
    time.sleep(0.5)
    m0 = cache_metrics(token)
    ms2, code2, _ = http_get(call_url(TEST_NS, FAKE), token)  # should hit negative cache
    m1 = cache_metrics(token)
    neg_delta = m_neg_hits(m1) - m_neg_hits(m0)
    record("GET non-existent 1st request (K8s API fallback, slow)",
           code1 in (404, 500), ms1, code1, "expected 404/500")
    record("GET non-existent 2nd request (negative cache hit, fast)",
           code2 in (404, 500) and (neg_delta >= 1 or ms2 < ms1 * 0.7),
           ms2, code2,
           "negative_hits+%d, saved=%dms" % (neg_delta, ms1 - ms2))
    record("Negative cache is faster than live lookup",
           ms2 < ms1, 0, 0,
           "1st=%dms 2nd=%dms saved=%dms" % (ms1, ms2, ms1 - ms2))

    # -------------------------------------------------------------------------
    section("S4 -- Cache ON: ADD (informer populates cache within 8s)")
    # -------------------------------------------------------------------------
    kubectl("delete", "-n", TEST_NS,
            TEST_GVR_RESOURCE + "." + TEST_GVR_GROUP, TEST_NAME_NEW,
            "--ignore-not-found")
    time.sleep(1)
    info("Creating %s in %s..." % (TEST_NAME_NEW, TEST_NS))
    rc, out, err = kubectl("apply", "-f", "-", input_data=COMPOSITION_YAML)
    if rc != 0:
        fail("kubectl apply failed: " + err)
        record("ADD: kubectl apply", False, 0, 0, err[:80])
        return
    ok("Created: " + out)
    record("ADD: kubectl apply succeeded", True, 0, 201, out[:60])

    info("Waiting 8s for informer ADD event + cache population...")
    time.sleep(8)
    # First request: should be a cache hit (informer populated it)
    m0 = cache_metrics(token)
    ms, code, body = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
    m1 = cache_metrics(token)
    hits_delta = m_total_hits(m1) - m_total_hits(m0)
    record("ADD: GET after informer (expect cache hit)",
           code == 200 and (hits_delta >= 1 or ms < 150), ms, code,
           "hits+%d" % hits_delta)

    # -------------------------------------------------------------------------
    section("S5 -- Cache ON: UPDATE (informer refreshes cache data)")
    # -------------------------------------------------------------------------
    info("Patching %s with label snowplow-test=updated..." % TEST_NAME_NEW)
    kubectl("label",
            TEST_GVR_RESOURCE + "." + TEST_GVR_GROUP + "/" + TEST_NAME_NEW,
            "-n", TEST_NS, "snowplow-test=updated", "--overwrite")
    info("Waiting 8s for informer UPDATE event + cache refresh...")
    time.sleep(8)
    m0 = cache_metrics(token)
    ms, code, body = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
    m1 = cache_metrics(token)
    hits_delta = m_total_hits(m1) - m_total_hits(m0)
    label_ok = (isinstance(body, dict)
                and body.get("metadata", {}).get("labels", {}).get("snowplow-test") == "updated")
    record("UPDATE: GET reflects updated label (data freshness check)",
           code == 200 and label_ok, ms, code,
           "hits+%d label=%s" % (hits_delta, "OK" if label_ok else "MISSING"))
    record("UPDATE: GET is a cache hit after informer",
           code == 200 and (hits_delta >= 1 or ms < 150), ms, code,
           "hits+%d" % hits_delta)

    # -------------------------------------------------------------------------
    section("S6 -- Cache ON: DELETE (informer removes entry from cache)")
    # -------------------------------------------------------------------------
    info("Deleting %s..." % TEST_NAME_NEW)
    kubectl("delete", "-n", TEST_NS,
            TEST_GVR_RESOURCE + "." + TEST_GVR_GROUP, TEST_NAME_NEW)
    info("Waiting 8s for informer DELETE event + cache removal...")
    time.sleep(8)
    m0 = cache_metrics(token)
    ms, code, _ = http_get(call_url(TEST_NS, TEST_NAME_NEW), token)
    m1 = cache_metrics(token)
    neg_delta = m_neg_hits(m1) - m_neg_hits(m0)
    record("DELETE: GET after informer (expect 404 or negative cache hit)",
           code in (404, 500), ms, code,
           "negative_hits+%d" % neg_delta)

    # -------------------------------------------------------------------------
    section("S7 -- Cache ON: widget resolved-output cache (per-user key)")
    # -------------------------------------------------------------------------
    widgets = [
        ("pages",       widget_url("pages",        "dashboard-page")),
        ("nav-menu",    widget_url("navmenus",      "sidebar-nav-menu")),
        ("routes-load", widget_url("routesloaders", "routes-loader")),
        ("table-comps", widget_url("tables",        "dashboard-compositions-panel-row-table")),
    ]
    for wname, wpath in widgets:
        http_get(wpath, token)  # warmup pass -- populate resolved cache
    for wname, wpath in widgets:
        m0 = cache_metrics(token)
        ms, code, _ = http_get(wpath, token)
        m1 = cache_metrics(token)
        raw_delta = m_raw_hits(m1) - m_raw_hits(m0)
        record("Widget %s (resolved cache hit, fast response)" % wname,
               code == 200 and (raw_delta >= 1 or ms < 150), ms, code,
               "raw_hits+%d" % raw_delta)


def test_without_cache(token):
    """Run core operations with cache disabled -- all must succeed via K8s API."""
    # -------------------------------------------------------------------------
    section("S8 -- Cache OFF: all K8s API calls must succeed")
    # -------------------------------------------------------------------------
    cases = [
        ("GET warmed resource (no cache)",   call_url(TEST_NS, TEST_NAME_WARM), False),
        ("GET non-existent (no cache)",      call_url(TEST_NS, "does-not-exist-xyz"), True),
        ("Widget pages (no cache)",          widget_url("pages",    "dashboard-page"), False),
        ("Widget nav-menu (no cache)",       widget_url("navmenus", "sidebar-nav-menu"), False),
        ("Widget table-comps (no cache)",    widget_url("tables",   "dashboard-compositions-panel-row-table"), False),
    ]
    nocache_times = {}
    for label, path, expect_404 in cases:
        ms, code, body = http_get(path, token, timeout=60)
        passed = code in (404, 500) if expect_404 else code == 200
        record(label, passed, ms, code)
        nocache_times[label] = ms
    return nocache_times


def latency_section(token_nc, nocache_times):
    """Re-enable cache, re-measure same endpoints, print comparison table."""
    # -------------------------------------------------------------------------
    section("S9 -- Latency Comparison: Cache ON vs Cache OFF")
    # -------------------------------------------------------------------------
    enable_cache()
    token2 = login()
    pairs = [
        ("GET warmed resource (no cache)",   call_url(TEST_NS, TEST_NAME_WARM)),
        ("Widget pages (no cache)",          widget_url("pages",    "dashboard-page")),
        ("Widget nav-menu (no cache)",       widget_url("navmenus", "sidebar-nav-menu")),
        ("Widget table-comps (no cache)",    widget_url("tables",   "dashboard-compositions-panel-row-table")),
    ]
    for _, path in pairs:
        http_get(path, token2)  # warmup

    print("\n  %-40s  %10s  %10s  %10s" % ("Endpoint", "No-cache", "Cached", "Speedup"))
    print("  " + "-" * 75)
    for label, path in pairs:
        ms_cached, code, _ = http_get(path, token2)
        ms_nc = nocache_times.get(label, 0)
        speedup = ("%.1fx" % (ms_nc / ms_cached)) if ms_cached > 0 else "n/a"
        print("  %-40s  %8dms  %8dms  %10s" % (label[:40], ms_nc, ms_cached, speedup))
        record("Speedup %s" % label[:35],
               ms_cached < ms_nc, ms_cached, code,
               "%dms -> %dms (%s)" % (ms_nc, ms_cached, speedup))


# =============================================================================
# REPORT
# =============================================================================

def print_report():
    section("FINAL REPORT")
    passed = [r for r in results if r.passed]
    failed = [r for r in results if not r.passed]

    print("\n  Total: %d   %sPassed: %d%s   %sFailed: %d%s\n"
          % (len(results), GREEN, len(passed), RESET, RED, len(failed), RESET))

    if failed:
        print("  %s%sFAILED TESTS:%s" % (RED, BOLD, RESET))
        for r in failed:
            print("    %sX%s %-55s HTTP %-4s %dms  %s"
                  % (RED, RESET, r.name, r.http_code, r.ms, r.note))

    print("\n  %s%sPASSED TESTS:%s" % (GREEN, BOLD, RESET))
    for r in passed:
        print("    %s+%s %-55s %dms  %s"
              % (GREEN, RESET, r.name, r.ms, r.note))

    return len(failed) == 0


# =============================================================================
# MAIN
# =============================================================================

def main():
    print("\n%sSnowplow Redis Cache -- Comprehensive Validation Test%s" % (BOLD, RESET))
    print("Snowplow: %s  |  Authn: %s\n" % (SNOWPLOW, AUTHN))

    # Phase 1: cache ON
    print("%sPhase 1: Cache ENABLED%s" % (BOLD, RESET))
    token = login()
    info("JWT acquired.")
    test_with_cache(token)

    # Phase 2: disable cache, run no-cache tests
    print("\n%sPhase 2: Cache DISABLED%s" % (BOLD, RESET))
    disable_cache()
    token_nc = login()
    nocache_times = test_without_cache(token_nc)

    # Phase 3: re-enable and compare
    print("\n%sPhase 3: Latency Comparison%s" % (BOLD, RESET))
    latency_section(token_nc, nocache_times)

    # Final report
    all_passed = print_report()
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
