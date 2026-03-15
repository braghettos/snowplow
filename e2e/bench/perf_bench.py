#!/usr/bin/env python3
"""
Snowplow Performance Benchmark — Cache ON vs Cache OFF
======================================================
Measures p50/p90/p99/mean latency for key UI endpoints with
cache enabled and disabled, then prints a comparison table.
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

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")
AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")
PASSWORD = os.environ.get("PASSWORD", "jl1DDPGMFOWw")
ITERATIONS = int(os.environ.get("ITERATIONS", "30"))
WARMUP = int(os.environ.get("WARMUP", "5"))

ENDPOINTS = [
    ("Page dashboard",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=dashboard-page&namespace=krateo-system"),
    ("Page blueprints",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=blueprints-page&namespace=krateo-system"),
    ("Page compositions",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=pages&name=compositions-page&namespace=krateo-system"),
    ("NavMenu",
     "/call?apiVersion=widgets.templates.krateo.io%2Fv1beta1&resource=navmenus&name=sidebar-nav-menu&namespace=krateo-system"),
    ("RESTAction all-routes",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=all-routes&namespace=krateo-system"),
    ("RESTAction bp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=blueprints-list&namespace=krateo-system"),
    ("RESTAction comp-list",
     "/call?apiVersion=templates.krateo.io%2Fv1&resource=restactions&name=compositions-list&namespace=krateo-system"),
]

BOLD = "\033[1m"
CYAN = "\033[96m"
GREEN = "\033[92m"
RED = "\033[91m"
RESET = "\033[0m"


def login():
    creds = base64.b64encode(("admin:" + PASSWORD).encode()).decode()
    req = urllib.request.Request(
        AUTHN + "/basic/login",
        headers={"Authorization": "Basic " + creds},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["accessToken"]


def http_get_ms(path, token, timeout=30):
    req = urllib.request.Request(
        SNOWPLOW + path,
        headers={"Authorization": "Bearer " + token},
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            r.read()
    except urllib.error.HTTPError:
        pass
    except Exception:
        pass
    return int((time.perf_counter() - t0) * 1000)


def percentile(data, p):
    s = sorted(data)
    idx = max(0, int(round(p / 100.0 * len(s))) - 1)
    return s[idx]


def benchmark(label, path, token, warmup, iters):
    for _ in range(warmup):
        http_get_ms(path, token)
    latencies = []
    for _ in range(iters):
        latencies.append(http_get_ms(path, token))
    p50 = percentile(latencies, 50)
    p90 = percentile(latencies, 90)
    p99 = percentile(latencies, 99)
    mean = round(statistics.mean(latencies))
    print("  %-26s mean=%dms  p50=%dms  p90=%dms  p99=%dms" % (label, mean, p50, p90, p99))
    return {"label": label, "p50": p50, "p90": p90, "p99": p99, "mean": mean}


def kubectl(*args):
    proc = subprocess.run(["kubectl"] + list(args), capture_output=True)
    return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()


def wait_for_snowplow():
    print("  Waiting for snowplow...")
    for _ in range(30):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                print("  Snowplow ready")
                return
        except Exception:
            time.sleep(2)
    print("  ERROR: snowplow not ready")
    sys.exit(1)


def disable_cache():
    print("\n  Removing Redis sidecar...")
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=json",
            "-p=[{\"op\":\"remove\",\"path\":\"/spec/template/spec/initContainers\"}]")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=120s")
    wait_for_snowplow()
    time.sleep(3)


def restore_cache():
    print("\n  Restoring Redis sidecar...")
    patch = json.dumps({
        "spec": {"template": {"spec": {"initContainers": [{
            "name": "redis",
            "image": "redis:7-alpine",
            "restartPolicy": "Always",
            "ports": [{"containerPort": 6379}],
            "readinessProbe": {
                "exec": {"command": ["redis-cli", "ping"]},
                "initialDelaySeconds": 2, "periodSeconds": 3,
            },
            "resources": {
                "requests": {"cpu": "50m", "memory": "64Mi"},
                "limits": {"cpu": "200m", "memory": "128Mi"},
            },
        }]}}},
    })
    kubectl("patch", "deployment", "snowplow", "-n", "krateo-system",
            "--type=strategic", "-p", patch)
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=120s")
    wait_for_snowplow()
    time.sleep(5)


def main():
    sep = "─" * 100
    double_sep = "━" * 100

    print("\n%s%s%s" % (BOLD, double_sep, RESET))
    print("%s  Snowplow Performance Benchmark%s" % (BOLD, RESET))
    print("  Snowplow  : %s" % SNOWPLOW)
    print("  Iterations: %d  Warmup: %d" % (ITERATIONS, WARMUP))
    print("%s%s%s\n" % (BOLD, double_sep, RESET))

    # Phase 1: Cache ENABLED
    print("%s── Phase 1/3: Benchmark — Cache ENABLED ──%s" % (BOLD + CYAN, RESET))
    token = login()
    print("  JWT acquired")
    cached_results = []
    for label, path in ENDPOINTS:
        cached_results.append(benchmark(label, path, token, WARMUP, ITERATIONS))

    # Phase 2: Cache DISABLED
    print("\n%s── Phase 2/3: Benchmark — Cache DISABLED ──%s" % (BOLD + CYAN, RESET))
    disable_cache()
    token = login()
    print("  JWT acquired")
    nocache_results = []
    for label, path in ENDPOINTS:
        nocache_results.append(benchmark(label, path, token, WARMUP, ITERATIONS))

    # Phase 3: Restore
    print("\n%s── Phase 3/3: Restoring Redis sidecar ──%s" % (BOLD + CYAN, RESET))
    restore_cache()

    # Print comparison table
    print("\n%s%s%s" % (BOLD, double_sep, RESET))
    print("  %sRESULTS%s  (%d iterations, %d warmup, all times in ms)" % (BOLD, RESET, ITERATIONS, WARMUP))
    print("%s%s%s" % (BOLD, double_sep, RESET))
    print("  %-26s  %7s %7s %7s  │  %7s %7s %7s  │  %8s" %
          ("Endpoint", "c·p50", "c·p90", "c·p99", "n·p50", "n·p90", "n·p99", "speedup"))
    print("  " + sep)

    c_total = 0
    n_total = 0
    for cr, nr in zip(cached_results, nocache_results):
        speedup = "%.1fx" % (nr["p50"] / cr["p50"]) if cr["p50"] > 0 else "N/A"
        color = GREEN if nr["p50"] > cr["p50"] else RED
        print("  %-26s  %5dms %5dms %5dms  │  %5dms %5dms %5dms  │  %s%8s%s" %
              (cr["label"], cr["p50"], cr["p90"], cr["p99"],
               nr["p50"], nr["p90"], nr["p99"], color, speedup, RESET))
        c_total += cr["mean"]
        n_total += nr["mean"]

    c_avg = c_total // len(cached_results)
    n_avg = n_total // len(nocache_results)
    overall = "%.1fx" % (n_avg / c_avg) if c_avg > 0 else "N/A"
    overall_color = GREEN if n_avg > c_avg else RED
    print("  " + sep)
    print("  %-26s  %5dms %7s %7s  │  %5dms %7s %7s  │  %s%8s%s" %
          ("Average (mean of means)", c_avg, "", "", n_avg, "", "", overall_color, overall, RESET))
    print("%s%s%s\n" % (BOLD, double_sep, RESET))


if __name__ == "__main__":
    main()
