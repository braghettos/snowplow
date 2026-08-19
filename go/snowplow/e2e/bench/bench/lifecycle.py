"""Cluster lifecycle — namespace + composition + CRD CRUD, cleanup, cache toggle.

Calls `cluster.py` for all kubectl/k8s-client IO. Holds NO harness state;
functions are idempotent.

Block 1 scope (per docs/bench-restructure-path-b-plan-2026-06-02.md §A.3):
this module includes lifecycle orchestration (clean_environment,
assert_clean, cluster_dirty_state, destructive_clean_guard) plus the
namespace/composition/RBAC cleanup helpers that the orchestration calls.
Storm scenarios (Phase 7 user scaling, CRB-delete burst) land in
`bench/storm.py` in Block 2. Browser-side flows + EXPECTED_CALLS land
in Blocks 2 and 3.
"""

from __future__ import annotations

import base64
import concurrent.futures
import json
import os
import subprocess
import time
import urllib.error
import urllib.request

from bench.cluster import (
    COMP_CONTROLLER_DEPLOY,
    COMP_GVR,
    COMP_RES,
    COMPDEF_NAME,
    FINALIZER_PATCH,
    NS,
    helm_context_args,
    kubectl_context_args,
    _count,
    _count_bench_argo,
    _count_match,
    _crd_exists,
    _k8s_gvr_for,
    _k8s_init,
    _parse_ns_name,
    composition_yaml,
    count_bench_ns,
    count_bench_ns_terminating,
    count_compositions,
    count_compositions_in_ns,
    count_compositions_with_panels_ready,
    ensure_composition_controller,
    force_finalize_namespace,
    k8s_bulk_delete_clusterscope,
    k8s_bulk_delete_custom,
    k8s_bulk_delete_namespaced,
    k8s_bulk_patch_finalizers_null_custom,
    k8s_create_namespace,
    k8s_delete_custom,
    k8s_delete_namespace,
    k8s_list_cluster_custom,
    k8s_list_clusterrolebindings_by_prefix,
    k8s_list_clusterroles_by_prefix,
    k8s_list_namespaces_by_prefix,
    k8s_list_rolebindings_all_ns_by_prefix,
    k8s_list_roles_all_ns_by_prefix,
    kubectl,
)


# ─── Logger — sourced from cli.py per PM carry-forward #1 ────────────────────
#
# Block 1 shipped a print()-based shim here. Block 2 replaces it with the
# coloured ANSI logger that lives in cli.py. The import is at module load
# (NOT deferred) so existing callers' bare `log(msg)` references keep
# working without changes. cli.py does NOT import lifecycle at top-level,
# so this does not introduce a cycle.

from bench.cli import log, section  # noqa: E402


# ─── HTTP helpers used by lifecycle (login, snowplow probes) ─────────────────
#
# These are duplicated minimally here so lifecycle.py can call `enable_cache`
# / `disable_cache` (which probe /health post-rollout) without a circular
# dep on bench.browser. The full HTTP layer moves to browser.py in Block 3.

SNOWPLOW = os.environ.get("SNOWPLOW_URL", "http://34.135.50.203:8081")


def _wait_for_snowplow(max_wait=240):
    """Wait until snowplow /health returns 200."""
    for _ in range(max_wait // 2):
        try:
            with urllib.request.urlopen(SNOWPLOW + "/health", timeout=5):
                return True
        except Exception:
            time.sleep(2)
    return False


# ─── Cache toggle (helm-driven) ──────────────────────────────────────────────

_port_forward_proc = None
_PORT_FORWARD_PORT = 18081
HELM_RELEASE = os.environ.get("HELM_RELEASE", "snowplow")
CACHE_ENABLED_VALUES_KEY = os.environ.get(
    "CACHE_ENABLED_VALUES_KEY", "env.CACHE_ENABLED")


def _stop_port_forward():
    """Kill any running port-forward process."""
    global _port_forward_proc
    if _port_forward_proc is not None:
        _port_forward_proc.kill()
        _port_forward_proc.wait()
        _port_forward_proc = None


def _start_port_forward():
    """Start kubectl port-forward to the current snowplow pod.

    Returns the localhost URL that goes directly to the pod, bypassing
    the GKE LoadBalancer. NEVER use this URL for scored measurements
    (see feedback_no_kubectl_in_measurement.md) — diagnostics only.
    """
    global _port_forward_proc
    _stop_port_forward()
    _, pod_name, _ = kubectl("get", "pods", "-n", NS,
                              "-l", "app.kubernetes.io/name=snowplow",
                              "-o", "jsonpath={.items[0].metadata.name}")
    pod_name = pod_name.strip()
    if not pod_name:
        log("WARNING: no snowplow pod found for port-forward")
        return None
    _port_forward_proc = subprocess.Popen(
        ["kubectl", *kubectl_context_args(),
         "port-forward", f"pod/{pod_name}",
         f"{_PORT_FORWARD_PORT}:8081", "-n", NS],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    pf_url = f"http://localhost:{_PORT_FORWARD_PORT}"
    for _ in range(30):
        try:
            urllib.request.urlopen(f"{pf_url}/health", timeout=2)
            break
        except Exception:
            time.sleep(1)
    log(f"  Port-forward to {pod_name} on :{_PORT_FORWARD_PORT}")
    return pf_url


def _wait_old_pods_gone(old_pods_str):
    """Wait until every old pod is fully gone (not just Terminating)."""
    if not old_pods_str.strip():
        return
    for old_pod in old_pods_str.strip().split():
        log(f"  Waiting for old pod {old_pod} to disappear ...")
        for attempt in range(60):  # up to 120s
            rc, _, _ = kubectl("get", "pod", old_pod, "-n", NS, "--no-headers")
            if rc != 0:  # pod no longer exists
                log(f"  Old pod {old_pod} gone")
                break
            time.sleep(2)


def _chart_supports_cache_toggle():
    """Return True iff the deployed chart wires a CACHE_ENABLED env var.

    Detection: jsonpath into the live Deployment's container env list.
    Returns True only if container[0] has an env entry named
    CACHE_ENABLED. False on any error / missing pod / missing env.

    NOTE: chart-agnostic — inspects the deployed manifest, not chart
    values — so it correctly classifies both the rich 0.20.5-style
    chart and the stripped 0.30.0 chart.
    """
    rc, out, _ = kubectl(
        "get", "deploy", HELM_RELEASE, "-n", NS,
        "-o",
        "jsonpath={.spec.template.spec.containers[0]"
        ".env[?(@.name=='CACHE_ENABLED')].name}",
    )
    return rc == 0 and out.strip() == "CACHE_ENABLED"


def _set_cache_via_helm(value):
    """Toggle CACHE_ENABLED through `helm upgrade --reuse-values`.

    Per feedback_chart_only_for_snowplow.md: all snowplow deployment
    config flows through the chart values + ConfigMap, never via
    `kubectl set env deployment/snowplow`. Chart-source dispatch
    handles OCI/HTTP/local-path forms (helm 3.x).
    """
    if not _chart_supports_cache_toggle():
        log("Cache toggle skipped: deployed chart has no CACHE_ENABLED env "
            "(cache subsystem absent at this image)")
        return False

    str_value = "true" if value else "false"
    helm_repo = os.environ.get(
        "SNOWPLOW_HELM_REPO", "oci://ghcr.io/krateo-platformops/charts/snowplow")
    helm_version = os.environ.get("SNOWPLOW_HELM_VERSION")  # optional
    chart_path = os.environ.get("SNOWPLOW_HELM_CHART_PATH")

    if chart_path:
        # Operator-pulled local chart wins — explicit override.
        args = [
            "helm", "upgrade", HELM_RELEASE, chart_path,
            "-n", NS,
            "--reuse-values",
            "--set", f"{CACHE_ENABLED_VALUES_KEY}={str_value}",
        ]
    elif helm_repo.startswith("oci://"):
        args = [
            "helm", "upgrade", HELM_RELEASE, helm_repo,
            "-n", NS,
            "--reuse-values",
            "--set", f"{CACHE_ENABLED_VALUES_KEY}={str_value}",
        ]
        if helm_version:
            args.extend(["--version", helm_version])
    elif helm_repo.startswith("http://") or helm_repo.startswith("https://"):
        args = [
            "helm", "upgrade", HELM_RELEASE, HELM_RELEASE,
            "-n", NS,
            "--repo", helm_repo,
            "--reuse-values",
            "--set", f"{CACHE_ENABLED_VALUES_KEY}={str_value}",
        ]
        if helm_version:
            args.extend(["--version", helm_version])
    else:
        # Local chart path (relative or absolute filesystem dir).
        args = [
            "helm", "upgrade", HELM_RELEASE, helm_repo,
            "-n", NS,
            "--reuse-values",
            "--set", f"{CACHE_ENABLED_VALUES_KEY}={str_value}",
        ]

    # Diego hard rule 2026-06-11: helm always pins the cluster explicitly.
    args.extend(helm_context_args())
    proc = subprocess.run(args, capture_output=True, text=True, timeout=300)
    if proc.returncode != 0:
        raise RuntimeError(
            f"helm upgrade failed (rc={proc.returncode}): "
            f"stdout={proc.stdout.strip()} stderr={proc.stderr.strip()}")
    log(f"helm upgrade OK: {CACHE_ENABLED_VALUES_KEY}={str_value}")
    return True


def enable_cache():
    """Toggle CACHE_ENABLED=true via helm; wait for rollout."""
    log("Enabling cache (CACHE_ENABLED=true) via helm ...")
    if not _chart_supports_cache_toggle():
        log("  (no-op: chart has no cache toggle; cache=on cells will be N/A)")
        return False
    _, old_pods, _ = kubectl("get", "pods", "-n", NS,
                             "-l", "app.kubernetes.io/name=snowplow",
                             "-o", "jsonpath={.items[*].metadata.name}")
    if not _set_cache_via_helm(True):
        return False
    kubectl("rollout", "status", "deployment/snowplow",
            "-n", NS, "--timeout=300s")
    _wait_old_pods_gone(old_pods)
    _wait_for_snowplow()
    log("Cache enabled")
    return True


def disable_cache():
    """Toggle CACHE_ENABLED=false via helm; wait for rollout."""
    log("Disabling cache (CACHE_ENABLED=false) via helm ...")
    if not _chart_supports_cache_toggle():
        log("  (no-op: chart has no cache toggle; cache=off is the only mode)")
        return False
    _, old_pods, _ = kubectl("get", "pods", "-n", NS,
                             "-l", "app.kubernetes.io/name=snowplow",
                             "-o", "jsonpath={.items[*].metadata.name}")
    if not _set_cache_via_helm(False):
        return False
    kubectl("rollout", "status", "deployment/snowplow",
            "-n", NS, "--timeout=300s")
    _wait_old_pods_gone(old_pods)
    _wait_for_snowplow()
    log("Cache disabled")
    return True


# ─── RBAC cleanup (rogue + bench-app-*) ──────────────────────────────────────

def cleanup_rogue_rbac():
    """Remove rogue RBAC left by previous test runs.

    All user/group RBAC is defined by the krateo-platformops/portal helm chart.
    The test must NOT create custom roles — previous versions mistakenly
    created cluster-wide roles that defeated the chart's namespace-scoped
    permissions.
    """
    for cmd in [
        ("delete", "clusterrole", "cyberjoker-viewer", "--ignore-not-found"),
        ("delete", "clusterrole", "krateo-widgets-reader", "--ignore-not-found"),
        ("delete", "clusterrole", "bench-composition-admin", "--ignore-not-found"),
        ("delete", "clusterrolebinding", "cyberjoker-krateo-widgets-reader", "--ignore-not-found"),
        ("delete", "clusterrolebinding", "bench-composition-admin-binding", "--ignore-not-found"),
        ("delete", "rolebinding", "cyberjoker-viewer-binding", "-n", "demo-system", "--ignore-not-found"),
        ("delete", "role", "cyberjoker-viewer", "-n", "demo-system", "--ignore-not-found"),
    ]:
        kubectl(*cmd)
    log("Cleaned up rogue RBAC from previous runs")


def delete_bench_rbac():
    """Delete bench-app-* RBAC across cluster + namespace scope.

    Each composition creates ClusterRoles, ClusterRoleBindings, and
    namespace-scoped Roles and RoleBindings named bench-app-* (usually
    in krateo-system). When namespaces are force-deleted those
    cluster-scoped resources survive. Without cleanup the RBACWatcher
    informer delivers thousands of ADD events on every pod start or
    watch-reconnect, causing a perpetual invalidation storm.

    All 4 resource types are deleted in PARALLEL since they are
    independent. Each kind uses k8s-client when available, kubectl
    fallback otherwise.
    """
    chunk_size = 500

    def _delete_cluster_kind_via_k8s(kind):
        list_fn = {
            "clusterrole": k8s_list_clusterroles_by_prefix,
            "clusterrolebinding": k8s_list_clusterrolebindings_by_prefix,
        }[kind]
        names = list_fn("bench-app-")
        if names is None:
            return None
        if not names:
            log(f"Deleted 0 {kind}s (bench-app-*) [k8s-client]")
            return 0
        deleted = k8s_bulk_delete_clusterscope(kind, names, workers=64)
        log(f"Deleted {deleted}/{len(names)} {kind}s (bench-app-*) "
            f"[k8s-client]")
        return deleted

    def _delete_cluster_kind_via_kubectl(kind):
        rc, out, _ = kubectl("get", kind, "--no-headers", "-o", "name")
        names = [line.split("/", 1)[-1] for line in out.splitlines()
                 if "bench-app" in line]
        if not names:
            return
        for i in range(0, len(names), chunk_size):
            kubectl("delete", kind, *names[i:i + chunk_size],
                    "--ignore-not-found", "--wait=false")
        log(f"Deleted {len(names)} {kind}s (bench-app-*) [kubectl]")

    def _delete_cluster_kind(kind):
        if _k8s_init():
            try:
                if _delete_cluster_kind_via_k8s(kind) is not None:
                    return
            except Exception as e:
                log(f"  k8s-client cluster delete {kind} failed "
                    f"({type(e).__name__}: {e}); falling back to kubectl")
        _delete_cluster_kind_via_kubectl(kind)

    def _delete_namespaced_kind_all_ns_via_k8s(kind):
        list_fn = {
            "role": k8s_list_roles_all_ns_by_prefix,
            "rolebinding": k8s_list_rolebindings_all_ns_by_prefix,
        }[kind]
        a = list_fn("bench-app-")
        b = list_fn("bench-")
        if a is None or b is None:
            return None
        seen = set()
        items = []
        for ns, name in (a + b):
            if (ns, name) in seen:
                continue
            seen.add((ns, name))
            items.append((ns, name))
        if not items:
            log(f"Deleted 0 {kind}s (bench-*) [k8s-client]")
            return 0
        by_ns = {}
        for ns, name in items:
            by_ns.setdefault(ns, []).append(name)
        deleted = k8s_bulk_delete_namespaced(kind, items, workers=64)
        log(f"Deleted {deleted}/{len(items)} {kind}s across "
            f"{len(by_ns)} namespaces (bench-*) [k8s-client]")
        return deleted

    def _delete_namespaced_kind_all_ns_via_kubectl(kind):
        rc, out, _ = kubectl(
            "get", kind, "-A", "--no-headers", "-o",
            "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
        if rc != 0 or not out.strip():
            return
        items = [(ns, name) for ns, name in _parse_ns_name(out)
                 if name.startswith("bench-app-") or name.startswith("bench-")]
        if not items:
            return
        by_ns = {}
        for ns, name in items:
            by_ns.setdefault(ns, []).append(name)
        for ns, names in by_ns.items():
            for i in range(0, len(names), chunk_size):
                kubectl("delete", kind, *names[i:i + chunk_size], "-n", ns,
                        "--ignore-not-found", "--wait=false")
        log(f"Deleted {len(items)} {kind}s across {len(by_ns)} "
            f"namespaces (bench-*) [kubectl]")

    def _delete_namespaced_kind_all_ns(kind):
        if _k8s_init():
            try:
                if _delete_namespaced_kind_all_ns_via_k8s(kind) is not None:
                    return
            except Exception as e:
                log(f"  k8s-client namespaced delete {kind} failed "
                    f"({type(e).__name__}: {e}); falling back to kubectl")
        _delete_namespaced_kind_all_ns_via_kubectl(kind)

    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as ex:
        futures = [
            ex.submit(_delete_cluster_kind, "clusterrolebinding"),
            ex.submit(_delete_cluster_kind, "clusterrole"),
            ex.submit(_delete_namespaced_kind_all_ns, "rolebinding"),
            ex.submit(_delete_namespaced_kind_all_ns, "role"),
        ]
        for f in concurrent.futures.as_completed(futures):
            try:
                f.result()
            except Exception as e:
                log(f"WARNING: RBAC cleanup error: {e}")


# ─── Bench namespace + composition cleanup ───────────────────────────────────

# Widget + RESTAction CRDs that may live inside a bench-ns-XX namespace.
# These MUST be explicitly deleted before the namespace is force-finalized
# via the /finalize subresource — otherwise force-finalize bypasses the
# normal cascade-delete and the CRs survive as ghosts.
WIDGET_KINDS = [
    "panels.widgets.templates.krateo.io",
    "buttons.widgets.templates.krateo.io",
    "tables.widgets.templates.krateo.io",
    "flowcharts.widgets.templates.krateo.io",
    "navmenuitems.widgets.templates.krateo.io",
    "eventlists.widgets.templates.krateo.io",
    "markdowns.widgets.templates.krateo.io",
    "yamlviewers.widgets.templates.krateo.io",
    "tablists.widgets.templates.krateo.io",
    "forms.widgets.templates.krateo.io",
    "filters.widgets.templates.krateo.io",
    "datagrids.widgets.templates.krateo.io",
    "buttongroups.widgets.templates.krateo.io",
    "rows.widgets.templates.krateo.io",
    "columns.widgets.templates.krateo.io",
    "pages.widgets.templates.krateo.io",
    "barcharts.widgets.templates.krateo.io",
    "linecharts.widgets.templates.krateo.io",
    "piecharts.widgets.templates.krateo.io",
    "paragraphs.widgets.templates.krateo.io",
    "navmenus.widgets.templates.krateo.io",
    "routes.widgets.templates.krateo.io",
    "routesloaders.widgets.templates.krateo.io",
    "restactions.templates.krateo.io",
]

FINALIZER_RESOURCES = [
    f"{COMP_RES}.{COMP_GVR}",
    "compositiondefinitions.core.krateo.io",
    "applications.argoproj.io",
    "repoes.git.krateo.io",
    "repoes.github.ogen.krateo.io",
]


def create_bench_namespaces(start, end):
    """Create bench-ns-NN namespaces in batches of 200."""
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
        n = count_bench_ns()
        if n >= expected:
            return True
        time.sleep(5)
    log(f"  WARNING: only {count_bench_ns()}/{expected} bench namespaces Active after {timeout}s")
    return False


def deep_clean_bench_namespace(ns):
    """Delete every widget + RESTAction CR in `ns` BEFORE force-finalize.

    delete_bench_namespaces calls /api/v1/namespaces/<ns>/finalize to
    clear spec.finalizers and delete the namespace immediately. That
    bypasses the normal cascade-delete path: any CR still inside the
    namespace survives as a ghost. Discovered 2026-05-05 when a 2-hour
    old bench-ns-17 contained 908 Panel CRs.

    Strategy: bulk delete the kind with --wait=false + --force +
    --grace-period=0, then re-list; for any survivor, patch
    metadata.finalizers=null and delete again.

    Returns total CR count seen (pre-delete) for reporting.
    """
    total_seen = 0
    for kind in WIDGET_KINDS:
        rc, out, _ = kubectl("get", kind, "-n", ns, "--no-headers",
                             "--ignore-not-found", "-o", "name")
        if rc != 0 or not out.strip():
            continue
        names = [n.strip() for n in out.split("\n") if n.strip()]
        if not names:
            continue
        total_seen += len(names)
        kubectl("delete", kind, "--all", "-n", ns, "--ignore-not-found",
                "--wait=false", "--grace-period=0", "--force")
        time.sleep(1)
        rc2, out2, _ = kubectl("get", kind, "-n", ns, "--no-headers",
                               "--ignore-not-found", "-o", "name")
        if rc2 == 0 and out2.strip():
            stuck = [s.strip() for s in out2.split("\n") if s.strip()]

            def _patch(target):
                kubectl("patch", target, "-n", ns, "--type=merge",
                        f"-p={FINALIZER_PATCH}")
            with concurrent.futures.ThreadPoolExecutor(max_workers=16) as ex:
                list(ex.map(_patch, stuck))
            kubectl("delete", kind, "--all", "-n", ns, "--ignore-not-found",
                    "--wait=false", "--grace-period=0", "--force")
    return total_seen


def delete_all_compositions():
    """Delete all bench compositions via namespace-cascade.

    Cascade-delete bench-ns-NN namespaces in parallel; K8s GC reaps every
    child object (compositions, secrets, configmaps, Argo apps)
    automatically. Falls back to finalizer-patch on any composition that
    blocks namespace termination.

    Uses k8s-client list/delete when available, kubectl fallback
    otherwise. The cascade itself is only ~50 namespace deletes; structural
    pattern stays "get ns" + 64 parallel "delete ns" + "patch ... FINALIZER_PATCH".
    """
    bench_namespaces = None
    used_k8s = False
    if _k8s_init():
        try:
            listed = k8s_list_namespaces_by_prefix("bench-ns-")
            if listed is not None:
                bench_namespaces = [n["name"] for n in listed
                                    if n.get("phase") != "Terminating"]
                used_k8s = True
        except Exception as e:
            log(f"  k8s-client list ns failed "
                f"({type(e).__name__}: {e}); falling back to kubectl")
    if bench_namespaces is None:
        rc, out, _ = kubectl("get", "ns", "--no-headers")
        if rc != 0 or not out.strip():
            log("No namespaces visible; skipping composition cleanup")
            return
        bench_namespaces = [
            line.split()[0] for line in out.strip().split("\n")
            if line.startswith("bench-ns-")
        ]
    if not bench_namespaces:
        log("No bench namespaces present")
        return

    log(f"Cascade-deleting {len(bench_namespaces)} bench namespaces "
        f"[{'k8s-client' if used_k8s else 'kubectl'}] ...")

    # Step 1: parallel namespace delete (--wait=false).
    if used_k8s:
        with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
            list(ex.map(k8s_delete_namespace, bench_namespaces))
    else:
        def delete_ns(ns):
            kubectl("delete", "ns", ns, "--ignore-not-found", "--wait=false")
        with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
            list(ex.map(delete_ns, bench_namespaces))

    # Step 2: poll until namespaces fully terminated OR finalizer-stuck.
    deadline = time.time() + 120
    while time.time() < deadline:
        terminating = []
        polled_via_k8s = False
        if used_k8s:
            try:
                listed = k8s_list_namespaces_by_prefix("bench-ns-")
                if listed is not None:
                    terminating = [n["name"] for n in listed]
                    polled_via_k8s = True
            except Exception:
                polled_via_k8s = False
        if not polled_via_k8s:
            rc, out, _ = kubectl("get", "ns", "--no-headers")
            terminating = []
            if rc == 0:
                for line in (out or "").strip().split("\n"):
                    parts = line.split()
                    if not parts or not parts[0].startswith("bench-ns-"):
                        continue
                    terminating.append(parts[0])
        if not terminating:
            log(f"All {len(bench_namespaces)} bench namespaces deleted")
            return
        log(f"  {len(terminating)} bench namespaces still Terminating ...")
        time.sleep(5)

    # Step 3: finalizer-patch fallback.
    log("Some namespaces stuck; patching composition finalizers ...")
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                         "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    stuck = [(p[0], p[1]) for line in (out or "").strip().split("\n")
             if (p := line.split(None, 1)) and len(p) >= 2
             and p[0].startswith("bench-")]
    if stuck:
        def patch_finalizer(item):
            kubectl("patch", f"{COMP_RES}.{COMP_GVR}", item[1], "-n", item[0],
                    "--type=merge", f"-p={FINALIZER_PATCH}")
        with concurrent.futures.ThreadPoolExecutor(max_workers=64) as ex:
            list(ex.map(patch_finalizer, stuck))
        log(f"Finalizers patched on {len(stuck)} stuck compositions")

    time.sleep(10)
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    final_stuck = []
    if rc == 0:
        for line in (out or "").strip().split("\n"):
            parts = line.split()
            if parts and parts[0].startswith("bench-ns-"):
                final_stuck.append(parts[0])
    if final_stuck:
        log(f"WARNING: {len(final_stuck)} bench namespaces still present "
            f"after finalizer-patch")
    else:
        log("All bench namespaces cleared")


def delete_all_compositiondefinitions():
    """Delete CompositionDefinitions and let the core provider clean up CRD + controller."""
    cd_resource = "compositiondefinitions.core.krateo.io"
    gvr = _k8s_gvr_for(cd_resource)
    used_k8s = False
    items = None
    if gvr and _k8s_init():
        group, version, plural = gvr
        try:
            listed = k8s_list_cluster_custom(group, version, plural)
            if listed is not None:
                items = [(it["namespace"], it["name"]) for it in listed
                         if it.get("namespace") and it.get("name")]
                used_k8s = True
        except Exception as e:
            log(f"  k8s-client list CDs failed "
                f"({type(e).__name__}: {e}); falling back to kubectl")
    if items is None:
        rc, out, _ = kubectl(
            "get", cd_resource, "--all-namespaces",
            "--no-headers", "-o",
            "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
        if rc != 0 or not out.strip():
            log("No CompositionDefinitions to delete")
            return
        items = [(p[0], p[1]) for line in out.strip().split("\n")
                 if (p := line.split(None, 1)) and len(p) >= 2]
    if not items:
        log("No CompositionDefinitions to delete")
        return
    if used_k8s:
        group, version, plural = gvr
        for ns, name in items:
            k8s_delete_custom(group, version, plural, ns, name)
    else:
        for ns, name in items:
            kubectl("delete", cd_resource, name, "-n", ns,
                    "--ignore-not-found", "--wait=false")
    log(f"Triggered deletion of {len(items)} CompositionDefinitions "
        f"[{'k8s-client' if used_k8s else 'kubectl'}]")

    # Wait for CDs to be gone (300s budget).
    deadline = time.time() + 300
    while time.time() < deadline:
        if used_k8s:
            try:
                listed = k8s_list_cluster_custom(*gvr)
                remaining = len(listed) if listed is not None else -1
            except Exception:
                remaining = -1
            if remaining < 0:
                rc, out, _ = kubectl(
                    "get", cd_resource, "--all-namespaces",
                    "--no-headers")
                remaining = len([l for l in (out or "").strip().split("\n")
                                 if l.strip()])
        else:
            rc, out, _ = kubectl(
                "get", cd_resource, "--all-namespaces", "--no-headers")
            remaining = len([l for l in (out or "").strip().split("\n")
                             if l.strip()])
        if remaining == 0:
            log("All CompositionDefinitions deleted")
            break
        log(f"  {remaining} CompositionDefinitions remaining ...")
        time.sleep(10)
    else:
        # Force-patch finalizers on any stuck CompositionDefinitions
        if used_k8s:
            try:
                listed = k8s_list_cluster_custom(*gvr) or []
                stuck = [(it["namespace"], it["name"]) for it in listed
                         if it.get("namespace") and it.get("name")]
            except Exception:
                stuck = []
        else:
            rc, out, _ = kubectl(
                "get", cd_resource, "-A",
                "--no-headers", "-o",
                "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
            stuck = _parse_ns_name(out) if rc == 0 else []
        if stuck:
            log(f"  Patching finalizers off {len(stuck)} stuck "
                f"CompositionDefinitions ...")
            if used_k8s:
                k8s_bulk_patch_finalizers_null_custom(
                    gvr[0], gvr[1], gvr[2], stuck, workers=8)
            else:
                def _patch_cd(item):
                    ns, name = item
                    kubectl("patch", cd_resource, name, "-n", ns,
                            "--type=merge", f"-p={FINALIZER_PATCH}")
                with concurrent.futures.ThreadPoolExecutor(
                        max_workers=8) as ex:
                    list(ex.map(_patch_cd, stuck))
            time.sleep(20)
            if _count(cd_resource) > 0:
                log("WARNING: CompositionDefinitions still remaining "
                    "after force-patch")
            else:
                log("All CompositionDefinitions deleted (after force-patch)")

    # Wait for CRD to disappear (core provider deletes it after CD is gone).
    deadline = time.time() + 120
    while time.time() < deadline:
        rc, _, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}", "--no-headers")
        if rc != 0:
            log("CRD deleted by core provider")
            return
        time.sleep(5)
    log("WARNING: CRD still exists after CD deletion — may need manual cleanup")


def cleanup_orphan_repoes():
    """Clear finalizers on orphan repoes that survive controller restarts.

    The composition.krateo.io/finalizer is set by composition-dynamic-controller
    and only removed when the controller successfully reconciles the parent.
    After crashes or scale-downs, repoes can remain stuck — and patching their
    finalizers fails when their parent namespace is Terminating.

    Pattern: recreate parent namespace (idempotent), patch finalizers null
    in parallel, bulk delete per namespace.
    """
    for resource_kind in ("repoes.github.ogen.krateo.io",
                          "repoes.git.krateo.io"):
        gvr = _k8s_gvr_for(resource_kind)
        used_k8s = False
        items = None
        if gvr and _k8s_init():
            group, version, plural = gvr
            try:
                listed = k8s_list_cluster_custom(group, version, plural)
                if listed is not None:
                    items = [(it["namespace"], it["name"]) for it in listed
                             if it.get("namespace") and it.get("name")]
                    used_k8s = True
            except Exception as e:
                log(f"  k8s-client list {resource_kind} failed "
                    f"({type(e).__name__}: {e}); falling back to kubectl")
        if not used_k8s:
            rc, out, _ = kubectl(
                "get", resource_kind, "-A", "--no-headers",
                "-o",
                "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
            if rc != 0 or not out.strip():
                continue
            items = _parse_ns_name(out)
        if not items:
            continue
        log(f"Clearing finalizers on {len(items)} orphan {resource_kind} "
            f"[{'k8s-client' if used_k8s else 'kubectl'}] ...")
        unique_ns = {ns for ns, _ in items}
        if used_k8s:
            for ns in unique_ns:
                k8s_create_namespace(ns)
        else:
            for ns in unique_ns:
                kubectl("create", "namespace", ns)
        if used_k8s:
            group, version, plural = gvr
            patched = k8s_bulk_patch_finalizers_null_custom(
                group, version, plural, items, workers=32)
            log(f"  Patched finalizers on {patched}/{len(items)} "
                f"{resource_kind}")
            time.sleep(3)
            deleted = k8s_bulk_delete_custom(
                group, version, plural, items, workers=32)
            log(f"  Deleted {deleted}/{len(items)} {resource_kind}")
        else:
            def _patch(item):
                ns, name = item
                kubectl("patch", resource_kind, name, "-n", ns,
                        "--type=merge", f"-p={FINALIZER_PATCH}")
            with concurrent.futures.ThreadPoolExecutor(
                    max_workers=32) as ex:
                list(ex.map(_patch, items))
            time.sleep(3)
            by_ns = {}
            for ns, name in items:
                by_ns.setdefault(ns, []).append(name)
            for ns, names in by_ns.items():
                kubectl("delete", resource_kind, *names, "-n", ns,
                        "--ignore-not-found", "--wait=false")


def _drain_argo_apps(timeout=300):
    """Wait for Argo apps to drain naturally; force-patch+delete stragglers."""
    gvr = _k8s_gvr_for("applications.argoproj.io")

    def _list_argo_apps():
        if gvr and _k8s_init():
            group, version, plural = gvr
            try:
                listed = k8s_list_cluster_custom(group, version, plural)
                if listed is not None:
                    return [(it["namespace"], it["name"]) for it in listed
                            if it.get("namespace") and it.get("name")]
            except Exception as e:
                log(f"  k8s-client list argo failed "
                    f"({type(e).__name__}: {e}); falling back to kubectl")
        rc, out, _ = kubectl(
            "get", "applications.argoproj.io", "-A", "--no-headers",
            "-o",
            "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
        if rc != 0:
            return []
        return _parse_ns_name(out)

    deadline = time.time() + timeout
    while time.time() < deadline:
        items = _list_argo_apps()
        if not items:
            log("Argo applications drained")
            return
        if int(time.time()) % 30 < 10:
            log(f"  Waiting for controllers to clean {len(items)} "
                f"Argo apps ...")
        time.sleep(10)
    items = _list_argo_apps()
    if not items:
        log("Argo applications drained (post-timeout)")
        return
    log(f"  Force-patching {len(items)} stuck Argo apps ...")

    used_k8s = bool(gvr and _k8s_init())
    if used_k8s:
        group, version, plural = gvr
        try:
            patched = k8s_bulk_patch_finalizers_null_custom(
                group, version, plural, items, workers=32)
            deleted = k8s_bulk_delete_custom(
                group, version, plural, items, workers=32)
            log(f"  k8s-client force: patched={patched} deleted={deleted}")
            time.sleep(10)
            log("Argo applications cleaned (forced) [k8s-client]")
            return
        except Exception as e:
            log(f"  k8s-client force path failed "
                f"({type(e).__name__}: {e}); falling back to kubectl")

    def _patch_and_delete(item):
        ns, name = item
        kubectl("patch", "applications.argoproj.io", name, "-n", ns,
                "--type=merge", f"-p={FINALIZER_PATCH}")
        kubectl("delete", "applications.argoproj.io", name, "-n", ns,
                "--ignore-not-found", "--wait=false")
    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as ex:
        list(ex.map(_patch_and_delete, items))
    time.sleep(10)
    log("Argo applications cleaned (forced) [kubectl]")


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

    log(f"Deep-cleaning widget CRs in {len(bench_ns)} bench namespaces ...")

    def _deep_clean(n):
        return n, deep_clean_bench_namespace(n)
    total_purged = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as ex:
        for n, count in ex.map(_deep_clean, bench_ns):
            if count > 0:
                log(f"  deep-clean: {n} purged {count} widget CRs")
                total_purged += count
    log(f"Deep-clean total: purged {total_purged} widget CRs across "
        f"{len(bench_ns)} bench namespaces")
    kubectl("delete", "ns", *bench_ns, "--ignore-not-found", "--wait=false",
            "--force", "--grace-period=0")
    log(f"Triggered deletion of {len(bench_ns)} bench namespaces")

    def force_finalize(ns):
        body = json.dumps({
            'apiVersion': 'v1', 'kind': 'Namespace',
            'metadata': {'name': ns}, 'spec': {'finalizers': []}
        })
        kubectl("replace", "--raw", f"/api/v1/namespaces/{ns}/finalize",
                "-f", "-", input_data=body)

    time.sleep(2)
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


def wait_for_crd(timeout=120):
    """Wait until the bench composition CRD exists and is not deletion-marked."""
    log(f"Waiting for CRD {COMP_RES}.{COMP_GVR} ...")
    deadline = time.time() + timeout
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}",
                             "-o", "jsonpath={.metadata.deletionTimestamp}")
        if rc == 0 and not out.strip():
            log("CRD exists")
            return True
        time.sleep(5)
    return False


def delete_all_clientconfigs():
    """Delete clientconfig secrets in krateo-system."""
    rc, out, _ = kubectl("get", "secrets", "-n", NS, "-o", "name")
    secrets = [s.replace("secret/", "") for s in out.split("\n")
               if s.strip() and "-clientconfig" in s]
    if secrets:
        kubectl("delete", "secret", *secrets, "-n", NS, "--ignore-not-found")
    log(f"Deleted {len(secrets)} clientconfig secrets")


# ─── RESTAction count snapshot (one-shot telemetry) ──────────────────────────
#
# Task #216 (Diego ratified, 2026-06-12): the blocking
# `wait_for_restaction_steady_state` (up to 600s of count-stability polling
# whose return was discarded) is DELETED. The install-settling signal it was a
# blocking proxy for is captured REPORT-ONLY by S6's `conv_settle_s6`
# (cluster_count_stable). The RESTAction-reconcile flavour is preserved as
# telemetry by this single non-blocking count snapshot (the same count logic
# the deleted wait used, with no loop / no timeout / no return-bool semantics).

def restaction_count_snapshot():
    """One-shot cluster-wide RESTAction count. Pure telemetry — no loop, no
    timeout, no stability wait. Returns the int count, or None on kubectl
    failure (so a transient cluster hiccup never blocks or raises into S6)."""
    rc, out, _ = kubectl(
        "get", "restactions.templates.krateo.io",
        "-A", "--no-headers", "-o", "name", timeout_secs=60)
    if rc != 0:
        return None
    count = len([line for line in out.splitlines() if line.strip()])
    log(f"  RESTAction count snapshot (telemetry): {count}")
    return count


# ─── Composition deploy ──────────────────────────────────────────────────────

def deploy_compositions_parallel(ns_start, ns_end, comps_per_ns, max_retries=5, workers=32):
    """Deploy compositions in parallel using a thread pool.

    Batches compositions per namespace into a single multi-doc YAML to
    reduce kubectl invocations from N*M to N (one per namespace).
    """
    total = (ns_end - ns_start + 1) * comps_per_ns
    log(f"Deploying {total} compositions in parallel (workers={workers}, batch-per-ns) ...")
    failed = []
    deployed_count = [0]

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
            err = ""
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


# ─── Single-target composition + namespace delete (S7 / S8 paths) ───────────

def delete_one_composition(ns, name):
    """Delete a single composition, patching its finalizer first."""
    kubectl("patch", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    kubectl("patch", "applications.argoproj.io", name, "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "applications.argoproj.io", name, "-n", ns,
            "--ignore-not-found", "--wait=false")
    log(f"Deleted composition {ns}/{name} (+ Argo app)")


def wait_for_composition_gone(ns, name, timeout=60):
    """Wait until a specific composition no longer exists in K8s."""
    fqn = f"{ns}/{name}"
    deadline = time.time() + timeout
    fallback_applied = False
    while time.time() < deadline:
        rc, _, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", name, "-n", ns,
                           "--no-headers")
        if rc != 0:
            log(f"Composition {fqn} confirmed gone from K8s")
            return True
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
    """Wait until a namespace no longer exists in K8s."""
    deadline = time.time() + timeout
    force_finalized = False
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "ns", ns_name, "--no-headers")
        if rc != 0:
            log(f"Namespace {ns_name} confirmed gone from K8s")
            return True
        elapsed = timeout - (deadline - time.time())
        if elapsed >= 60 and not force_finalized and "Terminating" in (out or ""):
            log(f"Namespace {ns_name} stuck in Terminating after 60s — force-finalizing")
            force_finalize_namespace(ns_name)
            force_finalized = True
        time.sleep(3)
    log(f"WARNING: namespace {ns_name} still exists after {timeout}s")
    return False


def delete_one_bench_namespace(ns_name):
    """Delete a single bench namespace by deleting compositions first
    (controllers clean children), then deleting the namespace.
    """
    ensure_composition_controller("bench-ns-01")

    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name,
                         "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
    if rc == 0 and out.strip():
        comps = [c.strip() for c in out.strip().split("\n") if c.strip()]
        kubectl("delete", f"{COMP_RES}.{COMP_GVR}", "--all", "-n", ns_name,
                "--ignore-not-found", "--wait=false")
        log(f"Triggered deletion of {len(comps)} compositions in {ns_name}")

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

    kubectl("delete", "ns", ns_name, "--ignore-not-found", "--wait=false")
    log(f"Triggered deletion of namespace {ns_name}")


# ─── Top-level lifecycle orchestration ───────────────────────────────────────

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
      8. Deploy image + restart pod (clears in-process cache)
    """
    section("Cleaning environment")

    rc, _, _ = kubectl("get", "crd", f"{COMP_RES}.{COMP_GVR}", "--no-headers")
    if rc == 0:
        # Recreate any missing bench namespaces so kubectl can reach orphans
        rc2, out2, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                               "--no-headers", "-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
        if rc2 == 0 and out2.strip():
            for ns in sorted(set(out2.strip().split("\n"))):
                if ns.startswith("bench-"):
                    kubectl("create", "ns", ns)

        delete_all_compositions()

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

    delete_all_compositiondefinitions()
    _drain_argo_apps(timeout=300)
    cleanup_orphan_repoes()
    delete_bench_namespaces()
    delete_bench_rbac()

    for res in ("roles", "rolebindings"):
        rc, out, _ = kubectl("get", res, "-n", "krateo-system", "--no-headers", "-o", "name")
        if rc == 0 and out.strip():
            bench_items = [l.strip() for l in out.strip().split("\n") if "bench-app-" in l]
            if bench_items:
                kubectl("delete", *bench_items, "-n", "krateo-system",
                        "--ignore-not-found", "--wait=false")
                log(f"Deleted {len(bench_items)} {res} in krateo-system")

    _flush_snowplow_cache()


def _flush_snowplow_cache():
    """Verify the live snowplow image, then restart the pod to clear the
    in-process cache.

    The bench NEVER deploys (Task #320, from the #319 re-roll trace): all
    snowplow deploys flow exclusively through `helm upgrade` with the
    lockstep chart tag (feedback_chart_only_for_snowplow +
    feedback_chart_release_lockstep). The retired `kubectl set image`
    branch here was the chart-vs-live-drift class behind the OBS-1
    incident. On tag mismatch this raises so the operator helm-upgrades
    first — the bench is a verifier, not a deployer.

    The flush-restart itself is deliberate and stays: between runs it is
    the only way to drop stale L3/L1 state (#319 attributed the
    "external re-roll" incidents to exactly this call — correct behavior,
    now also visible per-stage via the phases.py revision-guard).
    """
    tag = (os.environ.get("EXPECTED_IMAGE_TAG") or "").strip()
    if tag:
        rc, out, _ = kubectl(
            "get", "deployment", "snowplow", "-n", "krateo-system", "-o",
            'jsonpath={.spec.template.spec.containers[?(@.name=="snowplow")].image}')
        live = out.strip() if rc == 0 else ""
        live_tag = live.split(":")[-1] if ":" in live else ""
        if live_tag != tag:
            raise RuntimeError(
                f"live snowplow image tag {live_tag or 'unreadable'!r} != "
                f"expected {tag!r} — the bench does not deploy; run "
                f"`helm upgrade` with the lockstep chart first (#320)")
        log(f"  Live snowplow image verified: {live}")
    log("Restarting snowplow pod to clear in-process cache ...")
    kubectl("rollout", "restart", "deployment/snowplow", "-n", "krateo-system")
    kubectl("rollout", "status", "deployment/snowplow", "-n", "krateo-system",
            "--timeout=600s")
    log("  Snowplow pod restarted — in-process cache cleared")


def cluster_dirty_state():
    """Return a dict {category: count} of bench leftovers in the cluster.

    Categories with count > 0 indicate the cluster is not in a state
    suitable for a fresh test run. This is the ground-truth signal used
    by assert_clean to decide whether to call clean_environment().

    Task #275 / #276-B.1: two categories close the masking the
    S3-silent-death trace named (docs/task-275-s3-silent-death-trace):

      - `bench_namespaces_terminating` — the prior run's bench-ns-* go
        Terminating while their child CRs garbage-collect. The
        `bench_namespaces` counter EXCLUDES Terminating (it is the
        "usable bench ns" signal), so without this sibling the dirty
        diagnostic was blind to in-flight namespace GC — the exact
        masking that let phantom composition-panel CRs survive unseen.
      - `comp_panels` — cluster-wide `panels.widgets.templates.krateo.io`
        carrying the `krateo.io/portal-page=compositions` label. These
        are the phantom CRs the admin /compositions datagrid renders as
        cards; residual ones from a prior incomplete clean drove the
        Task #275 F-4-style apparent cache staleness. `None` (transport
        failure) coerces to 0 so a flaky k8s client never false-positives
        the cluster as dirty.
    """
    comp_panels = count_compositions_with_panels_ready(target_ns=None)
    return {
        "bench_namespaces": count_bench_ns(),
        "bench_namespaces_terminating": count_bench_ns_terminating(),
        "comp_panels": comp_panels or 0,
        "compositions": count_compositions() if _crd_exists(f"{COMP_RES}.{COMP_GVR}") else 0,
        "compositiondefinitions": _count("compositiondefinitions.core.krateo.io"),
        "argo_apps_bench": _count_bench_argo(),
        "ogen_repoes": _count("repoes.github.ogen.krateo.io"),
        "git_repoes": _count("repoes.git.krateo.io"),
        "bench_clusterroles": _count_match("clusterrole", name_prefix="bench-"),
        "bench_clusterrolebindings": _count_match("clusterrolebinding", name_prefix="bench-"),
        "bench_roles_namespaced": _count_match("role", name_prefix="bench-"),
        "bench_rolebindings_namespaced": _count_match("rolebinding", name_prefix="bench-"),
    }


# ─── Destructive-clean SCALE guard ───────────────────────────────────────────

# Thresholds for the destructive-clean guard. If a baseline cluster has
# more than this many compositions or bench-namespaces, we assume it is
# the customer-shape Phase-6 baseline (per feedback_test_scale_50k.md)
# and refuse to wipe it. Operators who really want to clean a baseline
# cluster must pass --allow-destructive-clean.
SCALE_GUARD_COMPOSITIONS = 1000
SCALE_GUARD_BENCH_NAMESPACES = 100


def destructive_clean_guard(state, allow_destructive):
    """Return (blocks: bool, reason: str).

    The CRB-delete scenario (and --clean-only) used to call
    clean_environment() unconditionally whenever cluster_dirty_state()
    showed any nonzero counter. That is wrong on a customer-shape Phase-6
    baseline: the bench would happily wipe tens of thousands of
    compositions just to "warm up" a single scenario.

    Guard rules:
      - If allow_destructive is True, never block (operator opted in).
      - Else, if compositions > SCALE_GUARD_COMPOSITIONS, block.
      - Else, if bench_namespaces > SCALE_GUARD_BENCH_NAMESPACES, block.
      - Otherwise, allow.
    """
    if allow_destructive:
        return False, ""
    comps = int(state.get("compositions", 0) or 0)
    nss = int(state.get("bench_namespaces", 0) or 0)
    if comps > SCALE_GUARD_COMPOSITIONS:
        return True, (f"compositions={comps} > {SCALE_GUARD_COMPOSITIONS} "
                      "(looks like the Phase-6 baseline; refusing to wipe)")
    if nss > SCALE_GUARD_BENCH_NAMESPACES:
        return True, (f"bench_namespaces={nss} > {SCALE_GUARD_BENCH_NAMESPACES} "
                      "(looks like the Phase-6 baseline; refusing to wipe)")
    return False, ""


# Back-compat alias for callers in worktree script that import the
# underscored name. Removed in Block 5.
_destructive_clean_guard = destructive_clean_guard


def assert_clean(retry_with_cleanup=True, allow_destructive=False):
    """Assert cluster has no bench leftovers. If dirty and
    retry_with_cleanup, invoke clean_environment() once and re-check;
    raise if still dirty.

    The SCALE guard is consulted before any cleanup so the bench refuses
    to wipe the Phase-6 customer-shape baseline (~50K compositions, ~50
    bench namespaces) on a normal --phases run. Pass
    allow_destructive=True to override.
    """
    section("Pre-flight: cluster state")
    state = cluster_dirty_state()
    dirty = {k: v for k, v in state.items() if v > 0}
    if not dirty:
        log("Pre-flight: cluster clean")
        return
    log(f"Pre-flight DIRTY: {dirty}")
    if not retry_with_cleanup:
        raise RuntimeError(f"Cluster dirty, abort: {dirty}")
    blocks, reason = destructive_clean_guard(state, allow_destructive)
    if blocks:
        log(f"Pre-flight: SCALE guard active — {reason}")
        log("Pre-flight: skipping clean_environment(); proceeding "
            "with phases on the existing cluster. Pass "
            "--allow-destructive-clean to override.")
        return
    log("Pre-flight: invoking clean_environment() ...")
    clean_environment()
    state = cluster_dirty_state()
    dirty = {k: v for k, v in state.items() if v > 0}
    if dirty:
        raise RuntimeError(f"Cluster STILL dirty after cleanup: {dirty}")
    log("Pre-flight: cluster clean after cleanup")


# ─── Synthetic users (Phase 7) ───────────────────────────────────────────────

PHASE7_AUTHN_NS = "krateo-system"
USER_COUNTS = [10, 50, 100, 500, 1000]

AUTHN = os.environ.get("AUTHN_URL", "http://34.136.84.51:8082")


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

    Returns (count_logged_in, dict_of_tokens).
    """
    password = _scaleuser_password()
    total = end - start + 1

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
    time.sleep(5)

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
    """Delete all scaletest-labeled resources: User CRs, password secrets,
    clientconfig secrets.
    """
    log("  Deleting User CRs (authn controller will clean up clientconfig secrets) ...")
    kubectl("delete", "users.basic.authn.krateo.io", "-n", PHASE7_AUTHN_NS,
            "-l", "app=scaletest", "--ignore-not-found", timeout_secs=180)
    log("  Deleting password secrets ...")
    kubectl("delete", "secret", "-n", PHASE7_AUTHN_NS,
            "-l", "app=scaletest", "--ignore-not-found", timeout_secs=180)

    log("  Cleaning up residual clientconfig secrets ...")

    def _del_clientconfig(i):
        name = f"{_scaleuser_name(i)}-clientconfig"
        kubectl("delete", "secret", name, "-n", PHASE7_AUTHN_NS,
                "--ignore-not-found", timeout_secs=10)

    with concurrent.futures.ThreadPoolExecutor(max_workers=32) as ex:
        list(ex.map(_del_clientconfig, range(1, max(USER_COUNTS) + 1)))

    log("  Synthetic users cleaned up (in-process cache clears on secret deletion)")


__all__ = [
    # Lifecycle orchestration
    "clean_environment",
    "assert_clean",
    "cluster_dirty_state",
    "destructive_clean_guard",
    # Cache toggle
    "enable_cache",
    "disable_cache",
    # Namespace + composition CRUD
    "create_bench_namespaces",
    "wait_for_bench_namespaces",
    "deep_clean_bench_namespace",
    "delete_all_compositions",
    "delete_all_compositiondefinitions",
    "cleanup_orphan_repoes",
    "delete_bench_namespaces",
    "delete_bench_rbac",
    "cleanup_rogue_rbac",
    "delete_all_clientconfigs",
    "wait_for_crd",
    "restaction_count_snapshot",
    "deploy_compositions_parallel",
    "delete_one_composition",
    "wait_for_composition_gone",
    "wait_for_namespace_gone",
    "delete_one_bench_namespace",
    # Synthetic users
    "create_synthetic_users",
    "delete_synthetic_users",
    # Constants the operator references
    "WIDGET_KINDS",
    "FINALIZER_RESOURCES",
    "PHASE7_AUTHN_NS",
    "USER_COUNTS",
    "SCALE_GUARD_COMPOSITIONS",
    "SCALE_GUARD_BENCH_NAMESPACES",
]
