#!/usr/bin/env python3
"""
Snowplow performance benchmark — cache ON vs cache OFF.

Includes both portal widget endpoints AND CompositionDefinition GET/LIST
calls spread across 20 bench namespaces, to produce a realistic and
cache-intensive workload.

Usage:
  python3 perf.py           # full run (cache → disable Redis → nocache → restore)
  python3 perf.py --cache-only
  python3 perf.py --nocache-only
"""
import argparse, base64, json, statistics, subprocess, sys, time
import urllib.request, urllib.error

SNOWPLOW  = "http://34.135.50.203:8081"
AUTHN     = "http://34.136.84.51:8082"
USERNAME  = "admin"
PASSWORD  = "jl1DDPGMFOWw"
ITERS     = 30
WARMUP    = 5
NUM_NS    = 20   # bench-ns-01 … bench-ns-20
CDNAME    = "github-scaffolding-with-composition-page"

# ── Endpoint catalogue ────────────────────────────────────────────────────────

def _cd_get(ns: str) -> tuple[str, str]:
    return (
        f"CompositionDef GET {ns:<10}",
        f"/call?apiVersion=core.krateo.io/v1alpha1&resource=compositiondefinitions"
        f"&name={CDNAME}&namespace={ns}",
    )

def _cd_list(ns: str) -> tuple[str, str]:
    return (
        f"CompositionDef LIST {ns:<9}",
        f"/call?apiVersion=core.krateo.io/v1alpha1&resource=compositiondefinitions"
        f"&namespace={ns}",
    )

PORTAL_ENDPOINTS: list[tuple[str, str]] = [
    ("Page dashboard        ",
     "/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages"
     "&name=dashboard-page&namespace=krateo-system"),
    ("Page blueprints       ",
     "/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages"
     "&name=blueprints-page&namespace=krateo-system"),
    ("Page compositions     ",
     "/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=pages"
     "&name=compositions-page&namespace=krateo-system"),
    ("NavMenu               ",
     "/call?apiVersion=widgets.templates.krateo.io/v1beta1&resource=navmenus"
     "&name=sidebar-nav-menu&namespace=krateo-system"),
    ("RESTAction all-routes ",
     "/call?apiVersion=templates.krateo.io/v1&resource=restactions"
     "&name=all-routes&namespace=krateo-system"),
    ("ConfigMaps list       ",
     "/call?apiVersion=v1&resource=configmaps&namespace=krateo-system"),
]

# Sample CompositionDefinition GETs from 10 spread namespaces
CD_GET_ENDPOINTS = [_cd_get(f"bench-ns-{i:02d}") for i in [1, 3, 5, 7, 9, 11, 13, 15, 17, 19]]

# LIST from 10 namespaces (each list = query for all CDs in that ns)
CD_LIST_ENDPOINTS = [_cd_list(f"bench-ns-{i:02d}") for i in [2, 4, 6, 8, 10, 12, 14, 16, 18, 20]]

ALL_ENDPOINTS = PORTAL_ENDPOINTS + CD_GET_ENDPOINTS + CD_LIST_ENDPOINTS


# ── HTTP helpers ──────────────────────────────────────────────────────────────

def get_token() -> str:
    creds = base64.b64encode(f"{USERNAME}:{PASSWORD}".encode()).decode()
    req = urllib.request.Request(
        f"{AUTHN}/basic/login",
        headers={"Authorization": f"Basic {creds}"},
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.load(resp)
    return data["accessToken"]


def request_ms(url: str, token: str) -> float:
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            r.read()
    except Exception:
        # Count any error (disconnect, timeout, HTTP error) as a slow response
        # so it shows up in p99 rather than crashing the benchmark.
        pass
    elapsed = (time.perf_counter() - t0) * 1000
    time.sleep(0.05)  # 50ms pause to avoid overwhelming GKE LoadBalancer
    return elapsed


# ── Stats ─────────────────────────────────────────────────────────────────────

def pct(sorted_vals: list, p: float) -> int:
    idx = max(0, int(round(p / 100 * len(sorted_vals))) - 1)
    return int(sorted_vals[idx])


def benchmark(token: str, endpoints: list[tuple[str, str]]) -> list[dict]:
    results = []
    for label, path in endpoints:
        url = SNOWPLOW + path
        for _ in range(WARMUP):
            request_ms(url, token)
        samples = [request_ms(url, token) for _ in range(ITERS)]
        s = sorted(samples)
        mean = int(statistics.mean(samples))
        r = {
            "label": label,
            "p50": pct(s, 50), "p90": pct(s, 90), "p99": pct(s, 99),
            "min": int(s[0]),  "max": int(s[-1]),  "mean": mean,
        }
        results.append(r)
        print(f"  {label}  mean={mean:>5}ms  p50={r['p50']:>5}ms  "
              f"p90={r['p90']:>5}ms  p99={r['p99']:>5}ms")
    return results


# ── kubectl helpers ───────────────────────────────────────────────────────────

def run_kubectl(cmd: str):
    subprocess.run(cmd, shell=True, check=True)


_REDIS_WRONG_PORT = (
    """kubectl patch deployment snowplow -n krateo-system --type=strategic -p='{"""
    """"spec":{"template":{"spec":{"initContainers":[{"""
    """"name":"redis","image":"redis:7-alpine","restartPolicy":"Always","""
    """"command":["redis-server","--port","6380"],"""
    """"ports":[{"containerPort":6380}],"""
    """"resources":{"requests":{"cpu":"50m","memory":"64Mi"},"""
    """"limits":{"cpu":"200m","memory":"128Mi"}}}]}}}}'"""
)

_REDIS_CORRECT_PORT = (
    """kubectl patch deployment snowplow -n krateo-system --type=strategic -p='{"""
    """"spec":{"template":{"spec":{"initContainers":[{"""
    """"name":"redis","image":"redis:7-alpine","restartPolicy":"Always","""
    """"command":null,"""
    """"ports":[{"containerPort":6379}],"""
    """"readinessProbe":{"exec":{"command":["redis-cli","ping"]},"""
    """"initialDelaySeconds":2,"periodSeconds":3},"""
    """"resources":{"requests":{"cpu":"50m","memory":"64Mi"},"""
    """"limits":{"cpu":"200m","memory":"256Mi"}}}]}}}}'"""
)


def remove_redis():
    # Make the Redis sidecar listen on 6380 so snowplow (hardcoded to 6379) can't
    # connect and gracefully disables caching. This works even with native sidecars
    # (initContainer + restartPolicy:Always) on GKE 1.29+.
    run_kubectl(_REDIS_WRONG_PORT)
    run_kubectl("kubectl rollout status deployment/snowplow -n krateo-system --timeout=180s")


def restore_redis():
    run_kubectl(_REDIS_CORRECT_PORT)
    run_kubectl("kubectl rollout status deployment/snowplow -n krateo-system --timeout=180s")


def wait_snowplow(timeout=90):
    print("  Waiting for snowplow", end="", flush=True)
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            urllib.request.urlopen(f"{SNOWPLOW}/health", timeout=3)
            print(" ready")
            return
        except Exception:
            print(".", end="", flush=True)
            time.sleep(2)
    print(" TIMEOUT"); sys.exit(1)


# ── Output ────────────────────────────────────────────────────────────────────

def print_section_header(title: str, endpoints: list[tuple[str, str]]):
    print(f"\n  {'─'*90}")
    print(f"  {title}")
    print(f"  {'─'*90}")
    print(f"  {'Endpoint':<30}  {'c·p50':>7} {'c·p90':>7} {'c·p99':>7}  │"
          f"  {'n·p50':>7} {'n·p90':>7} {'n·p99':>7}  │  {'speedup':>8}")
    print(f"  {'·'*90}")


def print_comparison(cache: list[dict], nocache: list[dict]):
    W = 96
    sections = [
        ("PORTAL WIDGETS (6 endpoints)", PORTAL_ENDPOINTS),
        (f"COMPOSITIONDEFINITION GET across {NUM_NS} namespaces (10 endpoints)", CD_GET_ENDPOINTS),
        (f"COMPOSITIONDEFINITION LIST across {NUM_NS} namespaces (10 endpoints)", CD_LIST_ENDPOINTS),
    ]

    print(f"\n{'━'*W}")
    print(f"  FULL BENCHMARK RESULTS — {ITERS} iterations · {WARMUP} warmup discarded · ms")
    print(f"{'━'*W}")

    all_c_means, all_n_means = [], []
    idx = 0
    for section_title, eps in sections:
        n = len(eps)
        c_slice = cache[idx:idx+n]
        n_slice = nocache[idx:idx+n]
        idx += n

        print(f"\n  ▸ {section_title}")
        print(f"  {'Endpoint':<30}  {'c·p50':>7} {'c·p90':>7} {'c·p99':>7}  │"
              f"  {'n·p50':>7} {'n·p90':>7} {'n·p99':>7}  │  {'speedup':>8}")
        print(f"  {'─'*92}")

        c_means, n_means = [], []
        for c, n in zip(c_slice, n_slice):
            sp = f"{n['p50']/c['p50']:.1f}x" if c["p50"] > 0 else "N/A"
            print(
                f"  {c['label']:<30}  {c['p50']:>6}ms {c['p90']:>6}ms {c['p99']:>6}ms  │"
                f"  {n['p50']:>6}ms {n['p90']:>6}ms {n['p99']:>6}ms  │  {sp:>8}"
            )
            c_means.append(c["mean"]); n_means.append(n["mean"])
            all_c_means.append(c["mean"]); all_n_means.append(n["mean"])

        ca = int(statistics.mean(c_means)); na = int(statistics.mean(n_means))
        print(f"  {'─'*92}")
        print(f"  {'Section avg':<30}  {ca:>6}ms {'':>7} {'':>7}  │  {na:>6}ms {'':>7} {'':>7}  │  {na/ca:>7.1f}x")

    # Grand summary
    gca = int(statistics.mean(all_c_means))
    gna = int(statistics.mean(all_n_means))
    print(f"\n{'━'*W}")
    print(f"  GRAND AVERAGE                    cached={gca}ms    no-cache={gna}ms"
          f"    overall speedup={gna/gca:.1f}x")
    print(f"{'━'*W}\n")


# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    parser = argparse.ArgumentParser()
    grp = parser.add_mutually_exclusive_group()
    grp.add_argument("--cache-only",   action="store_true")
    grp.add_argument("--nocache-only", action="store_true")
    args = parser.parse_args()

    print(f"\n{'═'*70}")
    print(f"  Snowplow Performance Benchmark — with 20 bench namespaces")
    print(f"  Snowplow  : {SNOWPLOW}")
    print(f"  Endpoints : {len(ALL_ENDPOINTS)}  Iterations: {ITERS}  Warmup: {WARMUP}")
    print(f"{'═'*70}\n")

    cache_results = nocache_results = None

    if not args.nocache_only:
        print("── Phase 1/3: Cache ENABLED ─────────────────────────────────────────")
        print("  Getting JWT...", end="", flush=True)
        token = get_token(); print(" OK")
        cache_results = benchmark(token, ALL_ENDPOINTS)

    if not args.cache_only and not args.nocache_only:
        print("\n── Phase 2/3: Disabling Redis ───────────────────────────────────────")
        remove_redis()
        wait_snowplow()
        time.sleep(3)

    if not args.cache_only:
        print("\n── Phase 2/3: Cache DISABLED ────────────────────────────────────────")
        print("  Getting JWT...", end="", flush=True)
        token = get_token(); print(" OK")
        nocache_results = benchmark(token, ALL_ENDPOINTS)

    if not args.cache_only and not args.nocache_only:
        print("\n── Phase 3/3: Restoring Redis ───────────────────────────────────────")
        restore_redis()
        print("  Redis sidecar restored (rollout in background)")
        print_comparison(cache_results, nocache_results)


if __name__ == "__main__":
    main()
