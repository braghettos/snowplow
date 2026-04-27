#!/usr/bin/env python3
"""
Page Load Test — Cache ON (warm L1) vs Cache OFF
=================================================
Measures the REAL time for the Dashboard page to fully load in a browser
(all cascading /call XHR requests complete) with and without cache.

Prerequisites:
  - Compositions must already exist in the cluster
  - EXPECTED_IMAGE_TAG must match the deployed snowplow version
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

sys.stdout.reconfigure(line_buffering=True)

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
FRONTEND = os.environ.get("FRONTEND_URL", "http://34.46.217.105:8080")
NS = "krateo-system"
ITERS = int(os.environ.get("ITERS", "5"))
USERS = {}  # populated in main() via kubectl

BOLD = "\033[1m"
GREEN = "\033[92m"
RED = "\033[91m"
RESET = "\033[0m"
SEP = "─" * 80


def log(msg):
    ts = time.strftime("%H:%M:%S")
    print(f"  \033[2m[{ts}]\033[0m {msg}", flush=True)


def kubectl(*args, timeout_secs=120):
    try:
        proc = subprocess.run(["kubectl"] + list(args), capture_output=True, timeout=timeout_secs)
        return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()
    except subprocess.TimeoutExpired:
        return 1, "", "timeout"


def redis_cmd(*args):
    rc, out, _ = kubectl("exec", "deployment/snowplow", "-n", NS, "-c", "redis",
                          "--", "redis-cli", *args)
    return out.strip() if rc == 0 else ""


def login(username, password):
    creds = base64.b64encode(f"{username}:{password}".encode()).decode()
    req = urllib.request.Request(AUTHN + "/basic/login",
                                 headers={"Authorization": "Basic " + creds})
    for attempt in range(5):
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                body = json.loads(r.read())
                return body.get("token") or body.get("data", {}).get("token", "")
        except Exception:
            time.sleep(3)
    return ""


def wait_for_snowplow():
    for _ in range(60):
        try:
            urllib.request.urlopen(SNOWPLOW + "/health", timeout=5)
            return True
        except Exception:
            time.sleep(2)
    return False


def wait_for_l1_warmup(timeout=300):
    log("Waiting for L1 warmup ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        resolved = redis_cmd("KEYS", "snowplow:resolved:*")
        if resolved and resolved.strip():
            count = len([k for k in resolved.strip().split("\n") if k.strip()])
            if count > 0:
                log(f"L1 warmup detected ({count} resolved keys)")
                return True
        time.sleep(5)
    log("WARNING: L1 warmup timeout")
    return False


def _wait_old_pods_gone(old_pods_str):
    if not old_pods_str.strip():
        return
    for old_pod in old_pods_str.strip().split():
        log(f"  Waiting for old pod {old_pod} to disappear ...")
        for _ in range(60):
            rc, _, _ = kubectl("get", "pod", old_pod, "-n", NS, "--no-headers")
            if rc != 0:
                log(f"  Old pod {old_pod} gone")
                break
            time.sleep(2)


def enable_cache():
    log("Enabling cache ...")
    _, old_pods, _ = kubectl("get", "pods", "-n", NS, "-l", "app.kubernetes.io/name=snowplow",
                              "-o", "jsonpath={.items[*].metadata.name}")
    kubectl("set", "env", "deployment/snowplow", "-n", NS, "-c", "snowplow", "CACHE_ENABLED=true")
    kubectl("rollout", "status", "deployment/snowplow", "-n", NS, "--timeout=300s")
    _wait_old_pods_gone(old_pods)
    wait_for_snowplow()
    log("Cache enabled")


def disable_cache():
    log("Disabling cache ...")
    _, old_pods, _ = kubectl("get", "pods", "-n", NS, "-l", "app.kubernetes.io/name=snowplow",
                              "-o", "jsonpath={.items[*].metadata.name}")
    kubectl("set", "env", "deployment/snowplow", "-n", NS, "-c", "snowplow", "CACHE_ENABLED=false")
    kubectl("rollout", "status", "deployment/snowplow", "-n", NS, "--timeout=300s")
    _wait_old_pods_gone(old_pods)
    wait_for_snowplow()
    log("Cache disabled")


def browser_login(page, username, password):
    for attempt in range(3):
        try:
            page.goto(f"{FRONTEND}/login", wait_until="networkidle", timeout=30000)
            page.fill('#basic_username', username)
            page.fill('#basic_password', password)
            page.click('button[type="submit"]')
            page.wait_for_timeout(5000)
            token = page.evaluate("() => { try { return JSON.parse(localStorage.getItem('K_user') || '{}').accessToken || '' } catch(e) { return '' } }")
            if token:
                log(f"  Login OK for {username}")
                return True
            log(f"  Login attempt {attempt+1}: no token, URL={page.url}")
        except Exception as e:
            log(f"  Login attempt {attempt+1} failed: {e}")
            time.sleep(3)
    return False


def measure_page_load(page, page_path, page_name, iters=5):
    """Navigate to a page and measure total time until all XHR requests complete.

    Returns dict with load times, XHR count, and waterfall duration.
    """
    results = []
    for i in range(iters):
        page.evaluate("() => performance.clearResourceTimings()")
        t0 = time.perf_counter()
        try:
            # Load the page — don't wait for networkidle (SPA hydration gap)
            page.goto(f"{FRONTEND}{page_path}", wait_until="load", timeout=120000)
            if i == 0:
                page.wait_for_timeout(1000)
                log(f"    URL after nav: {page.url}")
            # Wait for SPA to hydrate and all /call requests to finish.
            # 1. Wait at least 5s for React to mount and start fetching
            # 2. Then poll until no new /call requests for 2 seconds
            page.wait_for_timeout(5000)
            prev_count = page.evaluate("""() => {
                return performance.getEntriesByType('resource')
                    .filter(x => x.name.includes('/call')).length;
            }""")
            stable_since = time.perf_counter()
            while time.perf_counter() - t0 < 120:
                page.wait_for_timeout(500)
                cur_count = page.evaluate("""() => {
                    return performance.getEntriesByType('resource')
                        .filter(x => x.name.includes('/call')).length;
                }""")
                if cur_count > prev_count:
                    prev_count = cur_count
                    stable_since = time.perf_counter()
                elif time.perf_counter() - stable_since > 2:
                    break  # No new /call for 2s — page is loaded
        except Exception:
            pass
        wall_ms = int((time.perf_counter() - t0) * 1000)

        network = page.evaluate("""() => {
            const e = performance.getEntriesByType('resource');
            const xhrs = e.filter(x => x.initiatorType === 'xmlhttprequest' || x.initiatorType === 'fetch');
            const calls = xhrs.filter(x => x.name.includes('/call'));
            // Debug: sample of all fetch URLs to diagnose /call detection
            const sampleUrls = xhrs.slice(0, 5).map(x => x.name);
            return {
                totalRequests: e.length,
                xhrCount: xhrs.length,
                callCount: calls.length,
                sampleUrls: sampleUrls,
                waterfallMs: calls.length > 0 ?
                    Math.round(Math.max(...calls.map(x => x.responseEnd)) - Math.min(...calls.map(x => x.startTime))) : 0,
                totalCallTimeMs: Math.round(calls.reduce((s, x) => s + (x.responseEnd - x.startTime), 0)),
                avgCallMs: calls.length > 0 ?
                    Math.round(calls.reduce((s, x) => s + (x.responseEnd - x.startTime), 0) / calls.length) : 0,
            };
        }""")

        result = {
            "wall_ms": wall_ms,
            "call_count": network.get("callCount", 0),
            "waterfall_ms": network.get("waterfallMs", 0),
            "total_call_ms": network.get("totalCallTimeMs", 0),
            "avg_call_ms": network.get("avgCallMs", 0),
        }
        results.append(result)
        log(f"  [{i+1}/{iters}] {page_name}: wall={wall_ms}ms  /call={result['call_count']}  "
            f"xhr={network.get('xhrCount',0)}  waterfall={result['waterfall_ms']}ms  avg_call={result['avg_call_ms']}ms")
        if i == 0 and result['call_count'] == 0:
            log(f"    DEBUG: total_resources={network.get('totalRequests',0)}  "
                f"xhr_count={network.get('xhrCount',0)}  sample_urls={network.get('sampleUrls', [])}")

    walls = [r["wall_ms"] for r in results]
    waterfalls = [r["waterfall_ms"] for r in results]
    calls = [r["call_count"] for r in results]
    avg_calls = [r["avg_call_ms"] for r in results]
    return {
        "wall_p50": sorted(walls)[len(walls)//2],
        "waterfall_p50": sorted(waterfalls)[len(waterfalls)//2],
        "call_count": sorted(calls)[len(calls)//2],
        "avg_call_p50": sorted(avg_calls)[len(avg_calls)//2],
        "all": results,
    }


def _get_password(secret_name):
    rc, out, _ = kubectl("get", "secret", secret_name, "-n", NS,
                          "-o", "jsonpath={.data.password}")
    if rc == 0 and out.strip():
        return base64.b64decode(out.strip()).decode()
    return "123456"


def main():
    from playwright.sync_api import sync_playwright

    global USERS
    USERS = {"admin": _get_password("admin-password"),
             "cyberjoker": _get_password("cyberjoker-password")}
    log(f"Passwords retrieved: admin={'*'*len(USERS['admin'])}  cyberjoker={'*'*len(USERS['cyberjoker'])}")

    # Check cluster state
    rc, out, _ = kubectl("get", "compositions", "--all-namespaces", "--no-headers")
    comp_count = len([l for l in (out or "").strip().split("\n") if l.strip()]) if rc == 0 and out.strip() else 0
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    ns_count = len([l for l in (out or "").strip().split("\n") if "bench-ns" in l]) if rc == 0 else 0

    print(f"\n{BOLD}{'═'*80}{RESET}")
    print(f"{BOLD}  Page Load Test — Cache ON vs Cache OFF{RESET}")
    print(f"  Compositions: {comp_count}  Bench namespaces: {ns_count}")
    print(f"  Frontend: {FRONTEND}  Snowplow: {SNOWPLOW}")
    print(f"  Iterations: {ITERS}")
    print(f"{BOLD}{'═'*80}{RESET}\n")

    if comp_count == 0:
        print("ERROR: No compositions in cluster. Deploy compositions first.")
        sys.exit(1)

    # ── Step 1: Cache ON (warm L1) ───────────────────────────────────────
    print(f"\n{BOLD}{'─'*80}{RESET}")
    print(f"{BOLD}  CACHE ON — Warm L1{RESET}")
    print(f"{BOLD}{'─'*80}{RESET}")

    enable_cache()
    wait_for_l1_warmup()
    resolved = redis_cmd("KEYS", "snowplow:resolved:*")
    resolved_count = len([k for k in (resolved or "").strip().split("\n") if k.strip()])
    log(f"L1 resolved keys: {resolved_count}")

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        ctx = browser.new_context(viewport={"width": 1280, "height": 900}, ignore_https_errors=True)
        page = ctx.new_page()

        log("Logging in ...")
        browser_login(page, "admin", USERS["admin"])
        auth_token = page.evaluate("() => localStorage.getItem('auth') || ''")
        log(f"  Auth token present: {bool(auth_token)} (len={len(auth_token)})")
        log(f"  Current URL after login: {page.url}")

        log("Measuring Dashboard (cache ON, warm L1) ...")
        on_dashboard = measure_page_load(page, "/dashboard", "Dashboard", iters=ITERS)

        log("Measuring Compositions page (cache ON, warm L1) ...")
        on_compositions = measure_page_load(page, "/compositions", "Compositions", iters=ITERS)

        ctx.close()
        browser.close()

    # ── Step 2: Cache OFF ────────────────────────────────────────────────
    print(f"\n{BOLD}{'─'*80}{RESET}")
    print(f"{BOLD}  CACHE OFF — Direct K8s API{RESET}")
    print(f"{BOLD}{'─'*80}{RESET}")

    disable_cache()

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        ctx = browser.new_context(viewport={"width": 1280, "height": 900}, ignore_https_errors=True)
        page = ctx.new_page()

        log("Logging in ...")
        browser_login(page, "admin", USERS["admin"])

        log("Measuring Dashboard (cache OFF) ...")
        off_dashboard = measure_page_load(page, "/dashboard", "Dashboard", iters=ITERS)

        log("Measuring Compositions page (cache OFF) ...")
        off_compositions = measure_page_load(page, "/compositions", "Compositions", iters=ITERS)

        ctx.close()
        browser.close()

    # ── Step 3: Restore cache ────────────────────────────────────────────
    enable_cache()

    # ── Results ──────────────────────────────────────────────────────────
    print(f"\n{BOLD}{'═'*80}{RESET}")
    print(f"{BOLD}  RESULTS — {comp_count} compositions, {ns_count} namespaces{RESET}")
    print(f"{BOLD}{'═'*80}{RESET}")

    print(f"\n  {'Page':<20s}  {'Metric':<20s}  {'Cache ON':>10s}  {'Cache OFF':>10s}  {'Speedup':>10s}")
    print(f"  {SEP}")

    for page_name, on_data, off_data in [
        ("Dashboard", on_dashboard, off_dashboard),
        ("Compositions", on_compositions, off_compositions),
    ]:
        for metric, key in [
            ("Wall time", "wall_p50"),
            ("Waterfall", "waterfall_p50"),
            ("/call count", "call_count"),
            ("Avg /call time", "avg_call_p50"),
        ]:
            on_val = on_data[key]
            off_val = off_data[key]
            if key == "call_count":
                print(f"  {page_name:<20s}  {metric:<20s}  {on_val:>9d}   {off_val:>9d}   {'':>10s}")
            else:
                spd = off_val / on_val if on_val > 0 else 0
                unit = "ms"
                color = GREEN if spd > 1.5 else RED
                print(f"  {page_name:<20s}  {metric:<20s}  {on_val:>8d}{unit}  {off_val:>8d}{unit}  {color}{spd:>8.1f}x{RESET}")
        print(f"  {'':20s}")

    # Overall speedup
    on_total = on_dashboard["wall_p50"] + on_compositions["wall_p50"]
    off_total = off_dashboard["wall_p50"] + off_compositions["wall_p50"]
    total_spd = off_total / on_total if on_total > 0 else 0
    print(f"  {'TOTAL':<20s}  {'Wall time':<20s}  {on_total:>8d}ms  {off_total:>8d}ms  "
          f"{GREEN if total_spd > 1.5 else RED}{total_spd:>8.1f}x{RESET}")

    print(f"\n{BOLD}{'═'*80}{RESET}")


if __name__ == "__main__":
    main()
