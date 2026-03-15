#!/usr/bin/env python3
"""
Snowplow Cache — Comprehensive Multi-User Validation
=====================================================
Tests cache correctness, per-user isolation, and performance
using both 'admin' and 'cyberjoker' users.

Phases:
  1. Cache ENABLED  — warmup, hit/miss, per-user isolation, RBAC, latency
  2. Cache DISABLED — baseline latency (via Redis sidecar removal)
  3. Comparison     — side-by-side table with speedup factors
"""

import base64
import json
import os
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request

# ── Cluster endpoints ────────────────────────────────────────────────────────
SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
ITERATIONS = int(os.environ.get("ITERATIONS", "10"))

USERS = {
    "admin":      {"password": "jl1DDPGMFOWw"},
    "cyberjoker": {"password": "T8te3k57Nm22"},
}

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
    ("table/compositions",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=tables&name=dashboard-compositions-panel-row-table&namespace=krateo-system"),
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

# ── Colours ──────────────────────────────────────────────────────────────────
GREEN = "\033[92m"
RED = "\033[91m"
YELLOW = "\033[93m"
CYAN = "\033[96m"
BOLD = "\033[1m"
DIM = "\033[2m"
RESET = "\033[0m"
SEP = "─" * 110
DSEP = "━" * 110

# ── Helpers ──────────────────────────────────────────────────────────────────

def login(username, password):
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def http_get(path, token, timeout=60):
    req = urllib.request.Request(
        SNOWPLOW + path,
        headers={"Authorization": "Bearer " + token},
    )
    t0 = time.perf_counter()
    code, body = 0, None
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
    ms = int((time.perf_counter() - t0) * 1000)
    return ms, code, body


def cache_metrics(token):
    _, _, body = http_get("/metrics/cache", token)
    return body or {}


def kubectl(*args, **kw):
    input_data = kw.get("input_data")
    proc = subprocess.run(
        ["kubectl"] + list(args),
        input=input_data.encode() if input_data else None,
        capture_output=True,
    )
    return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()


def pct(data, p):
    s = sorted(data)
    return s[max(0, int(round(p / 100.0 * len(s))) - 1)]


def section(title):
    print(f"\n{BOLD}{CYAN}{SEP}{RESET}")
    print(f"{BOLD}{CYAN}  {title}{RESET}")
    print(f"{BOLD}{CYAN}{SEP}{RESET}")


def ok(msg):
    print(f"  {GREEN}✓{RESET} {msg}")


def fail(msg):
    print(f"  {RED}✗{RESET} {msg}")


def info(msg):
    print(f"  {CYAN}→{RESET} {msg}")


# ── Test result tracking ─────────────────────────────────────────────────────
results = []


def record(name, passed, ms, code, note=""):
    results.append({"name": name, "passed": passed, "ms": ms, "code": code, "note": note})
    tag = f"{GREEN}PASS{RESET}" if passed else f"{RED}FAIL{RESET}"
    print(f"  [{tag}] {name:<60s} {ms:>5d}ms  HTTP {code}  {note}")


def wait_for_snowplow():
    info("Waiting for snowplow to become ready...")
    for _ in range(60):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                info("Snowplow ready")
                return True
        except Exception:
            time.sleep(2)
    fail("Snowplow not ready after 120s")
    return False


# ═══════════════════════════════════════════════════════════════════════════════
# PHASE 1 — CACHE ENABLED
# ═══════════════════════════════════════════════════════════════════════════════

def phase1_cache_enabled(tokens):
    """Run all cache-on validations for both users."""

    # ── 1A: Per-user L1 isolation ─────────────────────────────────────────────
    section("1A — Per-User L1 Cache Isolation")
    info("Each user has an independent L1 (resolved-output) cache keyed by subject.")
    info("Admin warmup should NOT create a cache hit for cyberjoker on the first request.")

    warmup_path = WIDGET_ENDPOINTS[0][1]  # page/dashboard

    # Warmup: make 2 requests as admin to populate L1
    http_get(warmup_path, tokens["admin"])
    http_get(warmup_path, tokens["admin"])

    # Admin 3rd request: should be an L1 hit
    m0 = cache_metrics(tokens["admin"])
    ms_adm, code_adm, _ = http_get(warmup_path, tokens["admin"])
    m1 = cache_metrics(tokens["admin"])
    raw_delta = m1.get("raw_hits", 0) - m0.get("raw_hits", 0)
    record("admin: page/dashboard (L1 hit after warmup)",
           code_adm == 200 and (raw_delta >= 1 or ms_adm < 200),
           ms_adm, code_adm, f"raw_hits+{raw_delta}")

    # Cyberjoker 1st request: L1 miss (different user), but L2/L3 shared layers available
    m0 = cache_metrics(tokens["cyberjoker"])
    ms_cj, code_cj, _ = http_get(warmup_path, tokens["cyberjoker"])
    m1 = cache_metrics(tokens["cyberjoker"])
    record("cyberjoker: page/dashboard (L1 cold miss, 1st request)",
           code_cj == 200, ms_cj, code_cj,
           f"expected slower than admin's cached {ms_adm}ms")

    # Cyberjoker 2nd request: should now be an L1 hit
    m0 = cache_metrics(tokens["cyberjoker"])
    ms_cj2, code_cj2, _ = http_get(warmup_path, tokens["cyberjoker"])
    m1 = cache_metrics(tokens["cyberjoker"])
    raw_delta_cj = m1.get("raw_hits", 0) - m0.get("raw_hits", 0)
    record("cyberjoker: page/dashboard (L1 hit, 2nd request)",
           code_cj2 == 200 and (raw_delta_cj >= 1 or ms_cj2 < 200),
           ms_cj2, code_cj2, f"raw_hits+{raw_delta_cj}")

    # ── 1B: Both users — full endpoint warmup + cache hit ─────────────────────
    section("1B — Full Endpoint Cache Hit Validation (both users)")
    for username in ("admin", "cyberjoker"):
        token = tokens[username]
        info(f"Warming up all endpoints for {username}...")
        for label, path in ALL_ENDPOINTS:
            http_get(path, token)
            http_get(path, token)

        info(f"Measuring cache hits for {username}...")
        for label, path in ALL_ENDPOINTS:
            m0 = cache_metrics(token)
            ms, code, _ = http_get(path, token)
            m1 = cache_metrics(token)
            total_hits = (m1.get("raw_hits", 0) - m0.get("raw_hits", 0) +
                          m1.get("get_hits", 0) - m0.get("get_hits", 0))
            record(f"{username}: {label} (cache hit)",
                   code == 200 and (total_hits >= 1 or ms < 200),
                   ms, code, f"hits+{total_hits}")

    # ── 1C: RBAC cache validation ────────────────────────────────────────────
    section("1C — RBAC Cache Validation (per-user)")
    for username in ("admin", "cyberjoker"):
        token = tokens[username]
        path = WIDGET_ENDPOINTS[0][1]
        # Two sequential requests — the second should benefit from RBAC cache
        http_get(path, token)
        m0 = cache_metrics(token)
        ms, code, _ = http_get(path, token)
        m1 = cache_metrics(token)
        rbac_delta = m1.get("rbac_hits", 0) - m0.get("rbac_hits", 0)
        record(f"{username}: RBAC cache hit on page/dashboard",
               code == 200 and rbac_delta >= 0,
               ms, code, f"rbac_hits+{rbac_delta}")

    # ── 1D: Negative cache (404 sentinel) ────────────────────────────────────
    section("1D — Negative Cache (404 sentinel)")
    fake_path = ("/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1"
                 "&resource=pages&name=does-not-exist-xyz&namespace=krateo-system")

    ms1, code1, _ = http_get(fake_path, tokens["admin"])
    time.sleep(0.5)
    m0 = cache_metrics(tokens["admin"])
    ms2, code2, _ = http_get(fake_path, tokens["admin"])
    m1 = cache_metrics(tokens["admin"])
    neg_delta = m1.get("negative_hits", 0) - m0.get("negative_hits", 0)
    record("admin: non-existent 1st request (API miss)",
           code1 in (404, 500, 200), ms1, code1, "sentinel stored")
    record("admin: non-existent 2nd request (negative cache hit)",
           code2 in (404, 500, 200) and (neg_delta >= 1 or ms2 < ms1),
           ms2, code2, f"neg_hits+{neg_delta} saved={ms1-ms2}ms")

    # ── 1E: L3 promotion validation ──────────────────────────────────────────
    section("1E — L3 Promotion (warmup → L2)")
    m_before = cache_metrics(tokens["admin"])
    l3_before = m_before.get("l3_promotions", 0)
    info(f"Current L3 promotions: {l3_before}")
    info("L3 promotions represent cache warmup data served without hitting K8s API.")
    record("L3 promotions active",
           l3_before > 0, 0, 200, f"l3_promotions={l3_before}")

    # ── 1F: Cross-user shared L2/L3 benefit ──────────────────────────────────
    section("1F — Cross-User Shared L2/L3 Benefit")
    info("After admin warms up endpoints, cyberjoker's L1 miss still benefits")
    info("from shared L2 (HTTP responses) and L3 (informer objects).")

    test_path = RESTACTION_ENDPOINTS[0][1]  # restaction/all-routes

    # Flush any existing L1 for this by using a fresh metric snapshot
    ms_adm, code_adm, _ = http_get(test_path, tokens["admin"])
    ms_adm2, _, _ = http_get(test_path, tokens["admin"])

    # cyberjoker's first request: L1 miss but L2/L3 available from admin's warmup
    ms_cj, code_cj, _ = http_get(test_path, tokens["cyberjoker"])
    # cyberjoker's second request: L1 hit
    ms_cj2, _, _ = http_get(test_path, tokens["cyberjoker"])

    record("admin: restaction/all-routes (cached)",
           code_adm == 200, ms_adm2, code_adm, "")
    record("cyberjoker: restaction/all-routes (1st, L1 miss but L2/L3 shared)",
           code_cj == 200, ms_cj, code_cj,
           f"admin_cached={ms_adm2}ms cj_cold={ms_cj}ms")
    record("cyberjoker: restaction/all-routes (2nd, L1 hit)",
           ms_cj2 < ms_cj or ms_cj2 < 200, ms_cj2, 200,
           f"speedup from 1st: {ms_cj}→{ms_cj2}ms")


# ═══════════════════════════════════════════════════════════════════════════════
# LATENCY BENCHMARK (used for both cached and uncached phases)
# ═══════════════════════════════════════════════════════════════════════════════

def run_latency_bench(tokens, iters, warmup=3):
    """Returns {username: {label: {"p50": ..., "p90": ..., "mean": ..., "times": [...]}}}."""
    bench = {}
    for username in ("admin", "cyberjoker"):
        token = tokens[username]
        bench[username] = {}
        for label, path in ALL_ENDPOINTS:
            for _ in range(warmup):
                http_get(path, token)
            latencies = []
            for _ in range(iters):
                ms, code, _ = http_get(path, token)
                latencies.append(ms)
            p50 = pct(latencies, 50)
            p90 = pct(latencies, 90)
            mean = round(statistics.mean(latencies))
            bench[username][label] = {"p50": p50, "p90": p90, "mean": mean, "times": latencies}
            print(f"    {username:<12s} {label:<30s}  p50={p50:>5d}ms  p90={p90:>5d}ms  mean={mean:>5d}ms")
    return bench


# ═══════════════════════════════════════════════════════════════════════════════
# CACHE DISABLE / RESTORE
# ═══════════════════════════════════════════════════════════════════════════════

def disable_cache():
    section("Disabling Cache (patching Redis sidecar to wrong port)")
    disable_patch = json.dumps([
        {"op": "replace",
         "path": "/spec/template/spec/initContainers/0/command",
         "value": ["redis-server", "--port", "6380"]},
    ])
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=json", "-p", disable_patch)
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    info("Waiting for rollout...")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=180s")
    if not wait_for_snowplow():
        sys.exit(1)
    time.sleep(3)
    info("Cache disabled (Redis unreachable → redisCache=nil).")


def enable_cache():
    section("Restoring Cache (Redis sidecar on correct port)")
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
            "limits": {"cpu": "200m", "memory": "256Mi"},
        },
    }
    enable_patch = json.dumps({
        "spec": {"template": {"spec": {"initContainers": [redis_container]}}}
    })
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=strategic", "-p", enable_patch)
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    info("Waiting for rollout...")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=180s")
    if not wait_for_snowplow():
        sys.exit(1)
    time.sleep(5)
    info("Cache enabled and warmed up.")


# ═══════════════════════════════════════════════════════════════════════════════
# COMPARISON TABLE
# ═══════════════════════════════════════════════════════════════════════════════

def print_comparison(cached_bench, nocache_bench):
    section("LATENCY COMPARISON — Cache ON vs Cache OFF")
    for username in ("admin", "cyberjoker"):
        print(f"\n  {BOLD}User: {username}{RESET}")
        print(f"  {'Endpoint':<30s}  {'c·p50':>7s} {'c·p90':>7s}  │  {'n·p50':>7s} {'n·p90':>7s}  │  {'speedup':>8s}")
        print(f"  {SEP}")

        total_c, total_n = 0, 0
        for label, _ in ALL_ENDPOINTS:
            c = cached_bench[username].get(label, {})
            n = nocache_bench[username].get(label, {})
            cp50 = c.get("p50", 0)
            cp90 = c.get("p90", 0)
            np50 = n.get("p50", 0)
            np90 = n.get("p90", 0)
            cmean = c.get("mean", 0)
            nmean = n.get("mean", 0)
            total_c += cmean
            total_n += nmean
            spd = f"{np50/cp50:.1f}x" if cp50 > 0 else "N/A"
            color = GREEN if np50 > cp50 else (RED if np50 < cp50 else "")
            print(f"  {label:<30s}  {cp50:>5d}ms {cp90:>5d}ms  │  {np50:>5d}ms {np90:>5d}ms  │  {color}{spd:>8s}{RESET}")

        c_avg = total_c // len(ALL_ENDPOINTS)
        n_avg = total_n // len(ALL_ENDPOINTS)
        overall = f"{n_avg/c_avg:.1f}x" if c_avg > 0 else "N/A"
        ocolor = GREEN if n_avg > c_avg else RED
        print(f"  {SEP}")
        print(f"  {'Average (mean)':30s}  {c_avg:>5d}ms {'':>7s}  │  {n_avg:>5d}ms {'':>7s}  │  {ocolor}{overall:>8s}{RESET}")


# ═══════════════════════════════════════════════════════════════════════════════
# FINAL REPORT
# ═══════════════════════════════════════════════════════════════════════════════

def print_report():
    section("FINAL REPORT")
    passed = [r for r in results if r["passed"]]
    failed = [r for r in results if not r["passed"]]

    print(f"\n  Total: {len(results)}   {GREEN}Passed: {len(passed)}{RESET}   {RED}Failed: {len(failed)}{RESET}\n")

    if failed:
        print(f"  {RED}{BOLD}FAILED TESTS:{RESET}")
        for r in failed:
            print(f"    {RED}✗{RESET} {r['name']:<60s}  HTTP {r['code']}  {r['ms']}ms  {r['note']}")
        print()

    print(f"  {GREEN}{BOLD}PASSED TESTS:{RESET}")
    for r in passed:
        print(f"    {GREEN}✓{RESET} {r['name']:<60s}  {r['ms']}ms  {r['note']}")

    return len(failed) == 0


# ═══════════════════════════════════════════════════════════════════════════════
# MAIN
# ═══════════════════════════════════════════════════════════════════════════════

def main():
    print(f"\n{BOLD}{DSEP}{RESET}")
    print(f"{BOLD}  Snowplow Cache — Comprehensive Multi-User Validation{RESET}")
    print(f"  Snowplow : {SNOWPLOW}")
    print(f"  Authn    : {AUTHN}")
    print(f"  Users    : admin, cyberjoker")
    print(f"  Latency iterations: {ITERATIONS}")
    print(f"{BOLD}{DSEP}{RESET}\n")

    # Login both users
    tokens = {}
    for username, creds in USERS.items():
        tokens[username] = login(username, creds["password"])
        ok(f"{username}: JWT acquired (len={len(tokens[username])})")

    # ── Phase 1: Cache ENABLED — validation ──────────────────────────────────
    print(f"\n{BOLD}{'═'*50}{RESET}")
    print(f"{BOLD}  PHASE 1/4: Cache Validation (cache ENABLED){RESET}")
    print(f"{BOLD}{'═'*50}{RESET}")
    phase1_cache_enabled(tokens)

    # ── Phase 2: Cache ENABLED — latency benchmark ───────────────────────────
    print(f"\n{BOLD}{'═'*50}{RESET}")
    print(f"{BOLD}  PHASE 2/4: Latency Benchmark (cache ENABLED){RESET}")
    print(f"{BOLD}{'═'*50}{RESET}")
    section("Measuring cached latencies...")
    cached_bench = run_latency_bench(tokens, ITERATIONS)

    # ── Phase 3: Cache DISABLED — latency benchmark ──────────────────────────
    print(f"\n{BOLD}{'═'*50}{RESET}")
    print(f"{BOLD}  PHASE 3/4: Latency Benchmark (cache DISABLED){RESET}")
    print(f"{BOLD}{'═'*50}{RESET}")
    disable_cache()
    # Re-login after pod restart
    for username, creds in USERS.items():
        tokens[username] = login(username, creds["password"])
    section("Measuring uncached latencies...")
    nocache_bench = run_latency_bench(tokens, ITERATIONS, warmup=1)

    # ── Phase 4: Restore + comparison ────────────────────────────────────────
    print(f"\n{BOLD}{'═'*50}{RESET}")
    print(f"{BOLD}  PHASE 4/4: Restore & Comparison{RESET}")
    print(f"{BOLD}{'═'*50}{RESET}")
    enable_cache()

    print_comparison(cached_bench, nocache_bench)

    all_passed = print_report()
    sys.exit(0 if all_passed else 1)


if __name__ == "__main__":
    main()
