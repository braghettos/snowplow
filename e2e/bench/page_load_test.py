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
        try:
            req = urllib.request.Request(SNOWPLOW + "/metrics/runtime")
            with urllib.request.urlopen(req, timeout=5) as r:
                rt = json.loads(r.read())
                keys = rt.get("cache_key_count", rt.get("redis_key_count", 0))
                if keys > 0:
                    log(f"L1 warmup detected ({keys} cache keys)")
                    return True
        except Exception:
            pass
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


def measure_page_load(page, page_path, page_name, iters=5, target_calls=0, timeout_s=300):
    """Navigate to a page and measure total time until all widget /call
    requests complete.

    The widget tree is discovered progressively: each level's response
    reveals the next level's children.  With cache ON responses arrive
    in ~5ms so all levels are discovered quickly.  With cache OFF each
    level takes ~1-2s, creating long gaps between child discovery.

    To compare apples-to-apples:
      1. First run with cache ON to learn the full widget count
         (target_calls=0 uses a 10s stability window)
      2. Then run with cache OFF using target_calls=<ON count>
         so we wait for the SAME number of widgets.

    Returns dict with load times, /call count, and waterfall duration.
    """
    stability_window = 10  # seconds with no new /call before declaring done
    results = []
    for i in range(iters):
        page.evaluate("() => performance.clearResourceTimings()")
        t0 = time.perf_counter()
        try:
            page.goto(f"{FRONTEND}{page_path}", wait_until="load", timeout=30000)
            if i == 0:
                page.wait_for_timeout(1000)
                log(f"    URL after nav: {page.url}")
            # Wait for React to hydrate
            page.wait_for_timeout(5000)
            prev_count = page.evaluate("""() => {
                return performance.getEntriesByType('resource')
                    .filter(x => x.name.includes('/call')).length;
            }""")
            stable_since = time.perf_counter()
            while time.perf_counter() - t0 < timeout_s:
                page.wait_for_timeout(1000)
                cur_count = page.evaluate("""() => {
                    return performance.getEntriesByType('resource')
                        .filter(x => x.name.includes('/call')).length;
                }""")
                if cur_count > prev_count:
                    prev_count = cur_count
                    stable_since = time.perf_counter()
                # If we have a target, stop as soon as we reach it
                if target_calls > 0 and cur_count >= target_calls:
                    break
                # Otherwise, use the stability window
                if target_calls == 0 and time.perf_counter() - stable_since > stability_window:
                    break
                # With a target, use a longer stability fallback (30s)
                if target_calls > 0 and time.perf_counter() - stable_since > 30:
                    break
        except Exception:
            pass
        wall_ms = int((time.perf_counter() - t0) * 1000)

        network = page.evaluate("""() => {
            const e = performance.getEntriesByType('resource');
            const xhrs = e.filter(x => x.initiatorType === 'xmlhttprequest' || x.initiatorType === 'fetch');
            const calls = xhrs.filter(x => x.name.includes('/call'));
            // Time from first /call start to last /call responseEnd
            // = total page loading time as experienced by the user
            const firstStart = calls.length > 0 ? Math.min(...calls.map(x => x.startTime)) : 0;
            const lastEnd = calls.length > 0 ? Math.max(...calls.map(x => x.responseEnd)) : 0;
            return {
                totalRequests: e.length,
                xhrCount: xhrs.length,
                callCount: calls.length,
                pageLoadMs: Math.round(lastEnd - firstStart),
                avgCallMs: calls.length > 0 ?
                    Math.round(calls.reduce((s, x) => s + (x.responseEnd - x.startTime), 0) / calls.length) : 0,
            };
        }""")

        result = {
            "wall_ms": wall_ms,
            "call_count": network.get("callCount", 0),
            "page_load_ms": network.get("pageLoadMs", 0),
            "avg_call_ms": network.get("avgCallMs", 0),
        }
        results.append(result)
        log(f"  [{i+1}/{iters}] {page_name}: page_load={result['page_load_ms']}ms  "
            f"/call={result['call_count']}  avg_call={result['avg_call_ms']}ms")

    page_loads = [r["page_load_ms"] for r in results]
    calls = [r["call_count"] for r in results]
    avg_calls = [r["avg_call_ms"] for r in results]
    return {
        "page_load_p50": sorted(page_loads)[len(page_loads)//2],
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
    try:
        req = urllib.request.Request(SNOWPLOW + "/metrics/runtime")
        with urllib.request.urlopen(req, timeout=5) as r:
            rt = json.loads(r.read())
            cache_keys = rt.get("cache_key_count", rt.get("redis_key_count", 0))
            log(f"L1 cache keys: {cache_keys}")
    except Exception:
        log("L1 cache key count unavailable")

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
        on_dash_calls = on_dashboard["call_count"]
        log(f"  Dashboard widget tree: {on_dash_calls} /call requests")

        log("Measuring Compositions page (cache ON, warm L1) ...")
        on_compositions = measure_page_load(page, "/compositions", "Compositions", iters=ITERS)
        on_comp_calls = on_compositions["call_count"]
        log(f"  Compositions widget tree: {on_comp_calls} /call requests")

        ctx.close()
        browser.close()

    # ── Step 2: Cache OFF ────────────────────────────────────────────────
    # Use the cache ON widget counts as targets so the test waits for the
    # FULL tree to load (same number of widgets) before declaring done.
    print(f"\n{BOLD}{'─'*80}{RESET}")
    print(f"{BOLD}  CACHE OFF — Direct K8s API  (target: {on_dash_calls} dashboard, {on_comp_calls} compositions){RESET}")
    print(f"{BOLD}{'─'*80}{RESET}")

    disable_cache()

    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        ctx = browser.new_context(viewport={"width": 1280, "height": 900}, ignore_https_errors=True)
        page = ctx.new_page()

        log("Logging in ...")
        browser_login(page, "admin", USERS["admin"])

        log(f"Measuring Dashboard (cache OFF, target={on_dash_calls} calls) ...")
        off_dashboard = measure_page_load(page, "/dashboard", "Dashboard",
                                           iters=ITERS, target_calls=on_dash_calls)

        log(f"Measuring Compositions (cache OFF, target={on_comp_calls} calls) ...")
        off_compositions = measure_page_load(page, "/compositions", "Compositions",
                                              iters=ITERS, target_calls=on_comp_calls)

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
            ("Page load", "page_load_p50"),
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
    on_total = on_dashboard["page_load_p50"] + on_compositions["page_load_p50"]
    off_total = off_dashboard["page_load_p50"] + off_compositions["page_load_p50"]
    total_spd = off_total / on_total if on_total > 0 else 0
    print(f"  {'TOTAL':<20s}  {'Page load':<20s}  {on_total:>8d}ms  {off_total:>8d}ms  "
          f"{GREEN if total_spd > 1.5 else RED}{total_spd:>8.1f}x{RESET}")

    print(f"\n{BOLD}{'═'*80}{RESET}")


if __name__ == "__main__":
    main()
