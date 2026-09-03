"""Cluster I/O — kubectl wrapper + kubernetes-client helpers.

Pure cluster mutators/readers; no harness state. GKE-context guard lives
here as the module-level `gke_context_guard()`; the full preflight gate
will live in `cli.py:cmd_check` (Block 2).

Why this exists
---------------
At SCALE=50K cleanup work touches 30K+ finalizer-patches, 124K+ RBAC
objects, 29K+ ogen repoes, 17K+ Argo apps. Every kubectl(...) call forks
a subprocess (~30-100 ms wall-clock for binary load + kubeconfig parse +
TLS handshake), so even with 64 workers the bench spends hours on
cleanup. The kubernetes Python client uses ONE TLS connection and emits
direct REST calls (~5-10 ms each), giving ~10-20x speedup on the
bulk-delete hot paths.

Design
------
- Lazy init: kube-config is loaded the first time a k8s_* helper runs.
- Optional dependency: if `kubernetes` is not installed OR the cluster
  is unreachable, K8S_CLIENT_AVAILABLE stays False and callers fall
  back to kubectl(). The bench still works on a stock laptop.
- Helpers swallow 404 (NotFound) as success — bulk delete on a list
  that races with controllers regularly sees "already gone" and the
  contract is "ensure absent", not "you saw it first".
- The original kubectl() wrapper is RETAINED: rollout restart, exec,
  helm, top, etc. stay on subprocess. Only the high-fan-out cleanup
  loops migrate.
"""

from __future__ import annotations

import concurrent.futures
import os
import subprocess
import sys
import threading
import time

# ─── GKE context guard ───────────────────────────────────────────────────────

CANONICAL_GKE_CONTEXT = "gke_neon-481711_us-central1-a_cluster-1"

# Env override for the pinned context. The hard-coded CANONICAL_GKE_CONTEXT
# above is the historical bench cluster, and it is GONE (the neon-481711
# project 403s and no longer lists) — so as shipped the harness pins a
# context that resolves to nothing on every live cluster.
#
# BENCH_GKE_CONTEXT retargets the pin. It does NOT weaken it: the resolved
# context is still injected into every kubectl(), every `helm --kube-context`,
# every direct-Popen `--context=`, and the in-process kubernetes client, and
# gke_context_guard still refuses to run when current-context does not match
# it. That is the whole point of the override — BENCH_ALLOW_NON_GKE=1 is the
# only other way to reach a non-neon cluster today and it REMOVES all pinning,
# which lets a `helm upgrade` land on whatever cluster current-context happens
# to name (gcloud rewrites it; a teammate's kind run flips it). An operator
# who needs a different GKE cluster must be able to say so without giving up
# the safety rail.
#
# Resolved at CALL time, not import time, mirroring
# expected.py:_resolve_overlay_path — so a test (or a caller that sets the
# variable after importing bench.cluster) sees the change.
ENV_GKE_CONTEXT = "BENCH_GKE_CONTEXT"


def canonical_gke_context() -> str:
    """The GKE context every bench kubectl/helm/client call pins.

    Returns $BENCH_GKE_CONTEXT when set and non-empty, else the
    CANONICAL_GKE_CONTEXT fallback.
    """
    return os.environ.get(ENV_GKE_CONTEXT, "").strip() or CANONICAL_GKE_CONTEXT


def gke_context_guard(allow_non_gke: bool | None = None) -> None:
    """Exit non-zero if kubectl current-context is not the canonical GKE.

    Enforces `feedback_kubectl_verify_gke_context` from the cluster module
    boundary so every bench import passes through the gate. The full
    7-item preflight gate lives in `cli.py:cmd_check` (Block 2); this is
    the cluster.py shard.

    Args:
        allow_non_gke: if True, bypass the gate. If None, read
            $BENCH_ALLOW_NON_GKE (truthy values "1", "true", "yes" bypass).

    Exits:
        3 on context mismatch (consistent with the planned CLI exit code).
        Does not exit on kubectl failure (lets callers see the subprocess
        error path); a missing kubectl returns to the caller silently so
        unit tests do not need a real binary.
    """
    if allow_non_gke is None:
        env_flag = os.environ.get("BENCH_ALLOW_NON_GKE", "0").strip().lower()
        allow_non_gke = env_flag in ("1", "true", "yes")
    if allow_non_gke:
        return
    try:
        proc = subprocess.run(
            ["kubectl", "config", "current-context"],
            capture_output=True,
            timeout=10,
        )
    except FileNotFoundError:
        # kubectl not installed → cannot enforce; let caller see this on
        # the next real kubectl call.
        return
    except subprocess.TimeoutExpired:
        return
    if proc.returncode != 0:
        # kubectl reachable but errored (no kubeconfig, etc.) — defer to
        # the caller; we do not want module import to die on a transient.
        return
    ctx = (proc.stdout.decode() or "").strip()
    want = canonical_gke_context()
    if ctx != want:
        sys.stderr.write(
            f"bench: GKE context guard FAIL — current-context={ctx!r} "
            f"(want {want!r}). Set {ENV_GKE_CONTEXT} to pin a different GKE "
            f"cluster, or BENCH_ALLOW_NON_GKE=1 to bypass pinning entirely "
            f"(kind/minikube only).\n"
        )
        sys.exit(3)


# ─── kubectl wrapper ─────────────────────────────────────────────────────────

def kubectl(*args, input_data=None, timeout_secs=120):
    """Run a kubectl invocation. Returns (returncode, stdout, stderr).

    Injects `--context=canonical_gke_context()` UNLESS the caller already
    passed an explicit `--context=...` flag OR `BENCH_ALLOW_NON_GKE=1` is
    set in the environment. This makes the bench hermetic against the
    user's kubectl current-context drifting between Bash invocations
    (per `feedback_bench_tests_hermetic_isolation_all_cluster_paths`).

    stdout/stderr are str (decoded). Timeout returns rc=1 and an error
    message in stderr to keep callers' error-handling uniform.
    """
    arg_list = list(args)
    allow_non_gke = os.environ.get(
        "BENCH_ALLOW_NON_GKE", "0").strip().lower() in ("1", "true", "yes")
    caller_pinned_context = any(
        isinstance(a, str) and a.startswith("--context") for a in arg_list)
    if not allow_non_gke and not caller_pinned_context:
        arg_list = [f"--context={canonical_gke_context()}"] + arg_list
    try:
        proc = subprocess.run(
            ["kubectl"] + arg_list,
            input=input_data.encode() if input_data else None,
            capture_output=True,
            timeout=timeout_secs,
        )
        return proc.returncode, proc.stdout.decode().strip(), proc.stderr.decode().strip()
    except subprocess.TimeoutExpired:
        return 1, "", f"kubectl timed out after {timeout_secs}s"


def _allow_non_gke_env():
    """True when the kind/minikube escape hatch is set (also unit tests)."""
    return os.environ.get(
        "BENCH_ALLOW_NON_GKE", "0").strip().lower() in ("1", "true", "yes")


def helm_context_args():
    """`--kube-context` args every bench helm invocation must append.

    Diego hard rule 2026-06-11: kubectl AND helm always pin the canonical
    GKE context explicitly — current-context drifts (gcloud rewrites it),
    and an unpinned `helm upgrade` would mutate the WRONG cluster. Mirrors
    kubectl()'s injection semantics: empty under BENCH_ALLOW_NON_GKE
    (kind/minikube), pinned otherwise.
    """
    if _allow_non_gke_env():
        return []
    return ["--kube-context", canonical_gke_context()]


def kubectl_context_args():
    """`--context` args for bench code that must shell kubectl DIRECTLY
    (Popen streamers: port-forward, `logs -f`, secret reads) and so cannot
    go through kubectl()'s auto-injection. Same hard-rule semantics.
    """
    if _allow_non_gke_env():
        return []
    return [f"--context={canonical_gke_context()}"]


# ─── Kubernetes-client helper layer (k8s_* prefix) ───────────────────────────

try:
    from kubernetes import client as _k8s_client_mod
    from kubernetes import config as _k8s_config_mod
    _K8S_LIB_AVAILABLE = True
except ImportError:
    _k8s_client_mod = None
    _k8s_config_mod = None
    _K8S_LIB_AVAILABLE = False

K8S_CLIENT_AVAILABLE = False  # flips True after first successful init
_k8s_core = None
_k8s_rbac = None
_k8s_custom = None
_k8s_apiext = None
_k8s_init_lock = threading.Lock()
_k8s_init_attempted = False


def _k8s_init():
    """Lazily load kubeconfig and instantiate API clients.

    Returns True on success, False if the kubernetes lib is missing or
    the cluster is unreachable. Callers can fall back to kubectl() on
    False without surprising the user.
    """
    global K8S_CLIENT_AVAILABLE, _k8s_core, _k8s_rbac, _k8s_custom
    global _k8s_apiext, _k8s_init_attempted
    if K8S_CLIENT_AVAILABLE:
        return True
    if not _K8S_LIB_AVAILABLE:
        return False
    with _k8s_init_lock:
        if K8S_CLIENT_AVAILABLE:
            return True
        if _k8s_init_attempted:
            # one-shot retry budget — don't keep re-trying load_kube_config
            # on every helper call when there is no cluster
            return False
        _k8s_init_attempted = True
        try:
            try:
                # #320 follow-up: pin the canonical GKE context unless the
                # non-GKE escape hatch is set — gcloud rewrites
                # current-context, and a bare load_kube_config() once
                # pointed the in-process client at the wrong cluster
                # (subprocess kubectl self-pins; this client must too).
                env_flag = os.environ.get(
                    "BENCH_ALLOW_NON_GKE", "0").strip().lower()
                pin = (None if env_flag in ("1", "true", "yes")
                       else canonical_gke_context())
                _k8s_config_mod.load_kube_config(context=pin)
            except Exception:
                # Fall back to in-cluster config (when running in a pod)
                _k8s_config_mod.load_incluster_config()
            _k8s_core = _k8s_client_mod.CoreV1Api()
            _k8s_rbac = _k8s_client_mod.RbacAuthorizationV1Api()
            _k8s_custom = _k8s_client_mod.CustomObjectsApi()
            _k8s_apiext = _k8s_client_mod.ApiextensionsV1Api()
            K8S_CLIENT_AVAILABLE = True
            return True
        except Exception:
            return False


def _k8s_is_404(exc):
    """True if a kubernetes ApiException represents NotFound."""
    if not _K8S_LIB_AVAILABLE:
        return False
    return isinstance(exc, _k8s_client_mod.exceptions.ApiException) \
        and getattr(exc, "status", None) == 404


# ── Cluster-scoped RBAC ───────────────────────────────────────────────────

def k8s_list_clusterroles_by_prefix(prefix):
    """Return ClusterRole names whose metadata.name starts with `prefix`.

    Single REST list (no subprocess). Returns None if the kubernetes
    client is unavailable so callers can fall back to kubectl().
    """
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_cluster_role(_request_timeout=300).items
    return [i.metadata.name for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_list_clusterrolebindings_by_prefix(prefix):
    """Return ClusterRoleBinding names starting with `prefix`."""
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_cluster_role_binding(_request_timeout=300).items
    return [i.metadata.name for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_delete_clusterrole(name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_cluster_role(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_clusterrolebinding(name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_cluster_role_binding(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Namespace-scoped RBAC ─────────────────────────────────────────────────

def k8s_list_roles_all_ns_by_prefix(prefix):
    """Return [(ns, name), ...] for Roles cluster-wide whose name starts
    with `prefix`. Single REST list across all namespaces.
    """
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_role_for_all_namespaces(
        _request_timeout=300).items
    return [(i.metadata.namespace, i.metadata.name) for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_list_rolebindings_all_ns_by_prefix(prefix):
    if not _k8s_init():
        return None
    items = _k8s_rbac.list_role_binding_for_all_namespaces(
        _request_timeout=300).items
    return [(i.metadata.namespace, i.metadata.name) for i in items
            if i.metadata.name and i.metadata.name.startswith(prefix)]


def k8s_delete_role(ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_namespaced_role(
            name=name, namespace=ns, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_rolebinding(ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_rbac.delete_namespaced_role_binding(
            name=name, namespace=ns, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Namespace-scoped RBAC: CREATE / READ (Task #250 Phase 6 S8/S9) ────────
#
# Parametric primitives — no cj-hardcoded path per feedback_no_special_cases.
# Phase 6 stages S8 (RB-add) / S9 (RB-remove) bind into pre-populated
# namespaces to exercise the snowplow subject-index propagation path
# (Probe A on /debug/vars/snowplow_rbac_publish_seq + Probe B on the
# per-user /call view). All callers MUST own RBAC layout choices in the
# stage runner; this layer only mediates the apiserver call.

def k8s_create_namespaced_role(ns, name, rules=None, *,
                               api_groups=None, resources=None,
                               verbs=None):
    """Create a namespaced Role with ONE OR MORE PolicyRules.

    Idempotent on AlreadyExists (HTTP 409 → True).

    Args:
        ns:    namespace to create the Role in (str).
        name:  Role metadata.name (str).
        rules: list of (api_groups, resources, verbs) tuples — each
               triplet becomes one V1PolicyRule. PREFERRED shape.
        api_groups / resources / verbs (DEPRECATED keyword): legacy
            single-rule kwargs. When `rules` is None, the function
            falls back to constructing a single-rule list from these.
            Kept for callers that pass exactly one rule. New callers
            MUST pass `rules` (a list).

    Returns:
        (ok, diag) — ok=True on success or AlreadyExists; ok=False with
        a descriptive `diag` string on any failure path (client
        unavailable, ApiException, AttributeError on SDK-symbol drift,
        ...). Stage proofs surface `diag` verbatim.

    Multi-rule shape (Task #250 Block 2b re-gate v3 / 2026-06-05):
    Phase 6 S8 grants cj BOTH composition GET/LIST and widget GVR
    GET/LIST (panels/markdowns/buttons) so the compositions-page
    datagrid renders cards (closes #186 Option (a) per Diego's
    ratification 2026-06-05). Parametric per
    feedback_no_special_cases — callers own the rule list, this
    helper just plumbs to V1PolicyRule.
    """
    if not _k8s_init():
        return False, ("rbac_create_failed: k8s client unavailable "
                       "(_k8s_init returned False)")
    # Normalize rules input.
    if rules is None:
        # Legacy single-rule kwargs path. Reject empty/missing args to
        # surface a callsite bug rather than create an empty-rules Role.
        if api_groups is None and resources is None and verbs is None:
            return False, ("rbac_create_failed: must pass either `rules` "
                           "or legacy api_groups+resources+verbs kwargs")
        rules = [(list(api_groups or []), list(resources or []),
                  list(verbs or []))]
    else:
        # Validate the list-of-triplets shape eagerly so a callsite bug
        # produces a clean diagnostic, not a downstream AttributeError.
        if not isinstance(rules, (list, tuple)) or len(rules) == 0:
            return False, (f"rbac_create_failed: rules must be a non-empty "
                           f"list; got {type(rules).__name__}")
        for i, rule in enumerate(rules):
            if not (isinstance(rule, (list, tuple)) and len(rule) == 3):
                return False, (f"rbac_create_failed: rule #{i} must be a "
                               f"3-tuple (api_groups, resources, verbs); "
                               f"got {type(rule).__name__} of len "
                               f"{len(rule) if hasattr(rule, '__len__') else '?'}")
    try:
        policy_rules = [
            _k8s_client_mod.V1PolicyRule(
                api_groups=list(ag),
                resources=list(res),
                verbs=list(vs),
            )
            for (ag, res, vs) in rules
        ]
        body = _k8s_client_mod.V1Role(
            metadata=_k8s_client_mod.V1ObjectMeta(name=name),
            rules=policy_rules,
        )
        _k8s_rbac.create_namespaced_role(
            namespace=ns, body=body, _request_timeout=30)
        return True, ""
    except Exception as e:
        # ApiException — 409 (AlreadyExists) is success; anything else
        # surfaces with status+reason for diagnostic.
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            status = getattr(e, "status", None)
            if status in (200, 201, 409):
                return True, ""
            reason = getattr(e, "reason", "") or ""
            return False, (f"rbac_create_failed: api_exception "
                           f"status={status} reason={reason}")
        # Non-ApiException (e.g. AttributeError on SDK-symbol drift,
        # connection errors not yet wrapped in ApiException) — surface
        # the type + message so the proof body diagnoses what broke.
        return False, f"rbac_create_failed: {type(e).__name__}: {e}"


def k8s_create_namespaced_role_binding(ns, name, role_ref, subjects):
    """Create a namespaced RoleBinding pointing at `role_ref`.

    Idempotent on AlreadyExists (HTTP 409 → True).

    Args:
        ns:        namespace to create the RoleBinding in (str).
        name:      RoleBinding metadata.name (str).
        role_ref:  2-tuple (kind, name) — kind in {"Role", "ClusterRole"}.
        subjects:  list[dict] — each entry has keys:
                     kind      str  — "Group" / "User" / "ServiceAccount"
                     name      str  — subject name
                     api_group str  — optional; defaults to
                                      "rbac.authorization.k8s.io" for
                                      User/Group kinds, "" for SA
                     namespace str  — optional; required for SA kind only

    Returns:
        (ok, diag) — same shape as `k8s_create_namespaced_role` above.

    Re-gate v2 (2026-06-05):
      - kubernetes==35.0.0 ships `RbacV1Subject`, NOT bare `V1Subject`
        (per empirical `dir(kubernetes.client)` probe — the earlier code
        raised AttributeError before any HTTP call). Fixed at the
        construction site.
      - Return shape `bool → (bool, str)` so the smoke-test outcome that
        flagged the `V1Subject` bug shows up in the proof body next
        time.
    """
    if not _k8s_init():
        return False, ("rbac_create_failed: k8s client unavailable "
                       "(_k8s_init returned False)")
    try:
        kind, role_name = role_ref
        rb_subjects = []
        for s in subjects:
            sub_kind = s["kind"]
            sub_name = s["name"]
            # SA subjects use empty api_group; User/Group default to the
            # rbac.authorization.k8s.io api group per k8s convention.
            default_apigroup = (
                "" if sub_kind == "ServiceAccount"
                else "rbac.authorization.k8s.io"
            )
            sub_apigroup = s.get("api_group", default_apigroup)
            sub_namespace = s.get("namespace")
            # Re-gate v2 fix: use RbacV1Subject (NOT V1Subject — that
            # symbol does not exist in kubernetes>=27.0.0). Empirically
            # falsified against kubernetes==35.0.0 on 2026-06-05 during
            # the live S8 tester run.
            rb_subjects.append(_k8s_client_mod.RbacV1Subject(
                kind=sub_kind,
                name=sub_name,
                api_group=sub_apigroup,
                namespace=sub_namespace,
            ))
        body = _k8s_client_mod.V1RoleBinding(
            metadata=_k8s_client_mod.V1ObjectMeta(name=name),
            role_ref=_k8s_client_mod.V1RoleRef(
                api_group="rbac.authorization.k8s.io",
                kind=kind,
                name=role_name,
            ),
            subjects=rb_subjects,
        )
        _k8s_rbac.create_namespaced_role_binding(
            namespace=ns, body=body, _request_timeout=30)
        return True, ""
    except Exception as e:
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            status = getattr(e, "status", None)
            if status in (200, 201, 409):
                return True, ""
            reason = getattr(e, "reason", "") or ""
            return False, (f"rbac_create_failed: api_exception "
                           f"status={status} reason={reason}")
        return False, f"rbac_create_failed: {type(e).__name__}: {e}"


# ── Pre-check: can the bench actually create RBAC in target_ns? ───────────
#
# Defence-in-depth per re-gate v2: if the bench's kubeconfig identity
# lacks permission to create rolebindings in `target_ns`, the create
# would fail with a 403 ApiException — but the descriptive message
# wouldn't be obvious to a tester reading the proof. A pre-check via
# SelfSubjectAccessReview surfaces the misconfiguration BEFORE the
# create attempt, so the proof body says "rbac_precheck_denied" rather
# than "rbac_create_failed: api_exception status=403".

def k8s_can_i_create_rolebinding(target_ns):
    """Check if the bench's kubeconfig identity can create RoleBindings
    in `target_ns` via Kubernetes SelfSubjectAccessReview.

    Returns:
        (allowed, diag) — allowed=True when the apiserver answers
        `status.allowed == True`. allowed=False with a diagnostic
        string otherwise (client unavailable, transport error, or
        explicit deny).

    Idempotent + read-only — safe to call before any mutation.
    """
    if not _k8s_init():
        return False, ("rbac_precheck_unavailable: k8s client unavailable")
    try:
        authz = _k8s_client_mod.AuthorizationV1Api()
        review = _k8s_client_mod.V1SelfSubjectAccessReview(
            spec=_k8s_client_mod.V1SelfSubjectAccessReviewSpec(
                resource_attributes=_k8s_client_mod.V1ResourceAttributes(
                    namespace=target_ns,
                    verb="create",
                    group="rbac.authorization.k8s.io",
                    resource="rolebindings",
                ),
            ),
        )
        resp = authz.create_self_subject_access_review(
            body=review, _request_timeout=30)
        status = getattr(resp, "status", None)
        allowed = bool(getattr(status, "allowed", False)) if status else False
        if allowed:
            return True, ""
        reason = getattr(status, "reason", "") if status else ""
        return False, (f"rbac_precheck_denied: "
                       f"create rolebindings in {target_ns!r} not "
                       f"allowed for bench identity; reason={reason!r}")
    except Exception as e:
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            status_code = getattr(e, "status", None)
            reason = getattr(e, "reason", "") or ""
            return False, (f"rbac_precheck_error: api_exception "
                           f"status={status_code} reason={reason}")
        return False, f"rbac_precheck_error: {type(e).__name__}: {e}"


def k8s_read_namespaced_role_binding(ns, name):
    """Read a RoleBinding by (ns, name).

    Used by the Phase 6 S8/S9 post-mutation verify loop (R4.1 re-runner)
    to confirm the RB landed in the apiserver before navigating cj's
    browser, and to assert the RB is gone after S9's delete.

    Returns:
        dict on success (apiserver object as returned by the client lib —
        consumers read it as an object, not by-key).
        None on NotFound (404) OR client unavailable. Other exceptions
        propagate (so transport-level failures don't get silently
        swallowed and read as "absent").
    """
    if not _k8s_init():
        return None
    try:
        rb = _k8s_rbac.read_namespaced_role_binding(
            name=name, namespace=ns, _request_timeout=30)
        return rb
    except Exception as e:
        if _k8s_is_404(e):
            return None
        raise


# ── Namespaces ────────────────────────────────────────────────────────────

def k8s_list_namespaces_by_prefix(prefix):
    """Return list of dicts {name, phase} for namespaces starting with
    `prefix`. Phase distinguishes Active from Terminating.
    """
    if not _k8s_init():
        return None
    items = _k8s_core.list_namespace(_request_timeout=300).items
    out = []
    for i in items:
        name = i.metadata.name
        if not name or not name.startswith(prefix):
            continue
        phase = (i.status.phase if i.status else None) or ""
        out.append({"name": name, "phase": phase})
    return out


def k8s_delete_namespace(name):
    if not _k8s_init():
        return False
    try:
        _k8s_core.delete_namespace(name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_create_namespace(name):
    """Idempotent: returns True if namespace exists or was created."""
    if not _k8s_init():
        return False
    try:
        body = _k8s_client_mod.V1Namespace(
            metadata=_k8s_client_mod.V1ObjectMeta(name=name))
        _k8s_core.create_namespace(body=body, _request_timeout=30)
        return True
    except Exception as e:
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            # 409 = AlreadyExists is success
            return getattr(e, "status", None) in (200, 201, 409)
        return False


# ── Custom resources (CRDs) ───────────────────────────────────────────────

def k8s_split_gvr(gvr_string):
    """Parse 'plural.group' into (group, plural). Version unknown — caller
    must supply (commonly 'v1' / 'v1beta1' / 'v1-2-2').
    """
    parts = gvr_string.split(".", 1)
    if len(parts) != 2:
        return (None, gvr_string)
    return (parts[1], parts[0])


def k8s_list_cluster_custom(group, version, plural):
    """Return [{"namespace": ..., "name": ...}, ...] for all instances
    of the given CRD across the cluster. Single REST call.
    """
    if not _k8s_init():
        return None
    try:
        resp = _k8s_custom.list_cluster_custom_object(
            group=group, version=version, plural=plural,
            _request_timeout=300)
    except Exception as e:
        if _k8s_is_404(e):
            return []
        raise
    items = resp.get("items", []) if isinstance(resp, dict) else []
    out = []
    for it in items:
        md = it.get("metadata", {}) if isinstance(it, dict) else {}
        out.append({
            "namespace": md.get("namespace", ""),
            "name": md.get("name", ""),
        })
    return out


def k8s_patch_custom_finalizers_null(group, version, plural, ns, name):
    """JSON Merge Patch metadata.finalizers=null on a custom resource."""
    if not _k8s_init():
        return False
    body = {"metadata": {"finalizers": None}}
    try:
        _k8s_custom.patch_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, body=body,
            _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


def k8s_delete_custom(group, version, plural, ns, name):
    if not _k8s_init():
        return False
    try:
        _k8s_custom.delete_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, _request_timeout=30)
        return True
    except Exception as e:
        return _k8s_is_404(e)


# ── Bench-owned CR apply / patch / read (Task #314 S8b/S8c reconcile) ──────
#
# These three primitives let the S8b (Widget) / S8c (RESTAction) reconcile-
# latency stages create + mutate + (re)read BENCH-OWNED fixture CRs.
# Parametric — the caller owns the group/version/plural and the patch body;
# this layer only mediates the apiserver call (feedback_no_special_cases).
#
# Helm-ownership note: these are used EXCLUSIVELY on `bench-*` fixture CRs in
# a bench namespace. They never touch a portal/helm-owned object. This is the
# bench-internal-kubectl/apiserver path that feedback_never_kubectl_apply
# explicitly exempts ("Bench-internal kubectl is exempt").


def k8s_apply_yaml(yaml_str):
    """Server-side-apply a YAML document (one or many CRs) via kubectl.

    Mirrors the bench's existing `kubectl(..., input_data=...)` prior art
    (composition_yaml / deploy_compositiondefinition use `kubectl apply`).
    `--server-side --force-conflicts` makes the apply idempotent across
    re-runs (the S8b/S8c PRIME step is re-entrant on `--from-stage` resume).

    Returns:
        (ok, diag) — ok=True when kubectl exits 0; ok=False with the
        stderr (truncated) on any failure. Matches the (ok, diag) shape of
        the k8s_create_namespaced_role* helpers so stage proofs surface the
        diagnostic verbatim.
    """
    rc, out, err = kubectl(
        "apply", "--server-side", "--force-conflicts", "-f", "-",
        input_data=yaml_str, timeout_secs=60)
    if rc == 0:
        return True, ""
    return False, (err or out or "kubectl apply failed")[:500]


def k8s_patch_cr_spec(group, version, plural, ns, name, spec_patch):
    """JSON Merge Patch a custom resource's `spec` sub-tree.

    `spec_patch` is the dict written under `{"spec": spec_patch}` — a
    strategic-free JSON merge patch (RFC 7386 semantics via the k8s client's
    default merge-patch content type). Used by S8b/S8c to flip the echo
    field (widget `spec.widgetData.<scalar>` / RESTAction `spec.filter`)
    from its primed value to the mutated value, and to revert it.

    Mirrors k8s_patch_custom_finalizers_null's construction but takes a
    caller-supplied spec sub-tree instead of a fixed finalizers=null body.

    Returns:
        (ok, diag) — ok=True on success or 404 (treated as success: the
        fixture is already gone, so there is nothing to patch/revert).
        ok=False with the exception detail otherwise.
    """
    if not _k8s_init():
        return False, ("patch_failed: k8s client unavailable "
                       "(_k8s_init returned False)")
    body = {"spec": spec_patch}
    try:
        _k8s_custom.patch_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, body=body,
            _request_timeout=30)
        return True, ""
    except Exception as e:
        if _k8s_is_404(e):
            return True, ""
        if _K8S_LIB_AVAILABLE and isinstance(
                e, _k8s_client_mod.exceptions.ApiException):
            status = getattr(e, "status", None)
            reason = getattr(e, "reason", "") or ""
            return False, (f"patch_failed: api_exception "
                           f"status={status} reason={reason}")
        return False, f"patch_failed: {type(e).__name__}: {e}"


def k8s_get_custom(group, version, plural, ns, name):
    """Read one namespaced custom object. Returns the object dict, or None
    on NotFound (404) / client-unavailable.

    Used by the S8b/S8c PRIME pre-check to confirm the fixture landed at the
    apiserver before measurement (and is parametric — no name literal).
    Non-404 transport errors propagate so they are not silently read as
    'absent'.
    """
    if not _k8s_init():
        return None
    try:
        return _k8s_custom.get_namespaced_custom_object(
            group=group, version=version, plural=plural,
            namespace=ns, name=name, _request_timeout=30)
    except Exception as e:
        if _k8s_is_404(e):
            return None
        raise


# ── Bulk parallel helpers ─────────────────────────────────────────────────

def k8s_bulk_delete_clusterscope(kind, names, workers=64):
    """Delete N cluster-scoped objects of `kind` in parallel.

    `kind` is one of "clusterrole" | "clusterrolebinding".
    Returns count successfully deleted. Each delete is ONE REST call;
    no subprocess overhead.
    """
    if not names:
        return 0
    fn = {
        "clusterrole": k8s_delete_clusterrole,
        "clusterrolebinding": k8s_delete_clusterrolebinding,
    }.get(kind)
    if fn is None:
        return 0
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(fn, names))
    return sum(1 for r in results if r)


def k8s_bulk_delete_namespaced(kind, items, workers=64):
    """Delete N namespaced objects in parallel.

    `kind` is one of "role" | "rolebinding".
    `items` is a list of (ns, name).
    """
    if not items:
        return 0
    fn = {
        "role": k8s_delete_role,
        "rolebinding": k8s_delete_rolebinding,
    }.get(kind)
    if fn is None:
        return 0
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(lambda t: fn(t[0], t[1]), items))
    return sum(1 for r in results if r)


def k8s_bulk_patch_finalizers_null_custom(group, version, plural,
                                          items, workers=32):
    """Parallel JSON Merge Patch finalizers=null across a CRD batch.

    `items` is a list of (ns, name).
    """
    if not items:
        return 0
    def _go(t):
        return k8s_patch_custom_finalizers_null(
            group, version, plural, t[0], t[1])
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(_go, items))
    return sum(1 for r in results if r)


def k8s_bulk_delete_custom(group, version, plural, items, workers=32):
    """Parallel delete across a CRD batch. `items` is a list of (ns, name)."""
    if not items:
        return 0
    def _go(t):
        return k8s_delete_custom(group, version, plural, t[0], t[1])
    with concurrent.futures.ThreadPoolExecutor(
            max_workers=workers) as ex:
        results = list(ex.map(_go, items))
    return sum(1 for r in results if r)


# ── GVR table for the CRDs the bench touches ──────────────────────────────

K8S_CRD_GVR = {
    "applications.argoproj.io":
        ("argoproj.io", "v1alpha1", "applications"),
    "repoes.git.krateo.io":
        ("git.krateo.io", "v1alpha1", "repoes"),
    "repoes.github.ogen.krateo.io":
        ("github.ogen.krateo.io", "v1alpha1", "repoes"),
    "compositiondefinitions.core.krateo.io":
        ("core.krateo.io", "v1alpha1", "compositiondefinitions"),
}


def _k8s_gvr_for(resource):
    """Resolve kubectl-style 'plural.group' to (group, version, plural).

    Returns None if unknown — callers fall back to kubectl() rather than
    guessing the version (which can vary per CRD revision).
    """
    return K8S_CRD_GVR.get(resource)


# ─── Composition + bench constants (shared across modules) ───────────────────

NS = "krateo-system"
COMPDEF_NAME = "github-scaffolding-with-composition-page"
COMP_GVR = "composition.krateo.io"
COMP_RES = "githubscaffoldingwithcompositionpages"
COMP_CONTROLLER_DEPLOY = f"{COMP_RES}-v1-2-2-controller"

# Task #348: page the heavy cluster-wide ground-truth count/name probes.
#
# At ~49K compositions a SINGLE unpaged `kubectl get ... --all-namespaces`
# makes the apiserver build one giant response in one shot. Adding
# `--chunk-size` makes the apiserver page it via Limit/Continue and stream
# bounded chunks, ~2× faster wall-clock on the live 49K cluster
# (22.3s → 11.3s for the count; 41.7s → 11.7s for the name-set — see
# /tmp/snowplow-348-347/probe-timing.txt).
#
# CRITICAL — keep the SERVER-SIDE TABLE printer (`--no-headers`, the
# default Table output). It emits ONE lean row per object (client CPU
# stays ~4s). Empirically `-o name` / `-o json` are the variants that
# force the apiserver to serialize FULL objects and kubectl to template
# every one client-side — they are SLOWER (40-50s) and `-o json` moves
# 318 MB over the wire. We need neither extra field: the `--all-namespaces`
# Table for this CRD is `NAMESPACE  NAME  AGE  READY`, so col0=ns / col1=
# name already gives the exact "ns/name" set the CONTENT name-diff needs
# (proven byte-identical to the old `-o jsonpath` oracle via `diff`).
#
# 2000 chosen empirically: it is materially faster than 500/1000 here
# (chunk=500→Table still 22s region via per-request overhead dominance,
# 1000→15.2s, 2000→11.3s) while staying a bounded page the apiserver
# streams comfortably. NOT a sampling cap — the pager walks EVERY page to
# completion (Limit/Continue), so the returned count/name-set is the full
# cluster truth, never truncated.
COMP_PROBE_CHUNK_SIZE = 2000

FINALIZER_PATCH = '{"metadata":{"finalizers":null}}'


# ─── Cluster-state introspection helpers ─────────────────────────────────────

def _parse_ns_name(out):
    """Parse `kubectl ... -o custom-columns=NS:...,NAME:...` output into
    list of (ns, name) tuples.
    """
    return [(p[0], p[1]) for line in (out or "").splitlines()
            if (p := line.split(None, 1)) and len(p) >= 2]


def _count(resource):
    """Count all instances of a resource cluster-wide (or in cluster scope)."""
    rc, out, _ = kubectl("get", resource, "-A", "--no-headers")
    if rc != 0 or not out.strip():
        return 0
    return len([l for l in out.splitlines() if l.strip()])


def _count_match(resource, name_prefix="", ns_prefix=""):
    """Count instances of `resource` whose name and/or namespace match prefixes."""
    rc, out, _ = kubectl("get", resource, "-A", "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return 0
    n = 0
    for ns, name in _parse_ns_name(out):
        if name_prefix and not name.startswith(name_prefix):
            continue
        if ns_prefix and not ns.startswith(ns_prefix):
            continue
        n += 1
    return n


def _crd_exists(crd):
    rc, _, _ = kubectl("get", "crd", crd, "--no-headers")
    return rc == 0


def _count_bench_argo():
    """Argo apps in bench-* namespaces OR with bench-app- name prefix."""
    rc, out, _ = kubectl("get", "applications.argoproj.io", "-A", "--no-headers",
                         "-o", "custom-columns=NS:.metadata.namespace,NAME:.metadata.name")
    if rc != 0 or not out.strip():
        return 0
    return sum(1 for ns, name in _parse_ns_name(out)
               if ns.startswith("bench-") or name.startswith("bench-app-"))


def count_compositions():
    # Task #348: paged Table read (Limit/Continue) — see COMP_PROBE_CHUNK_SIZE.
    # One Table row per composition → line count == composition count, full
    # cluster truth (pager walks every page). Stays an INDEPENDENT kubectl
    # oracle (no snowplow /call), so VERIFY remains non-circular.
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                         "--no-headers",
                         f"--chunk-size={COMP_PROBE_CHUNK_SIZE}")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def count_compositions_in_ns(ns_name):
    # Task #348: same paged Table read, namespace-scoped. A single bench-ns
    # tops out ~999 comps today (one page), so behaviour is identical now;
    # the chunk bound future-proofs a namespace that grows past one page.
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "-n", ns_name,
                         "--no-headers",
                         f"--chunk-size={COMP_PROBE_CHUNK_SIZE}")
    if rc != 0 or not out.strip():
        return 0
    return len(out.strip().split("\n"))


def count_bench_ns():
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    return len([line for line in out.strip().split("\n")
                if "bench-ns-" in line and "Terminating" not in line])


def count_bench_ns_terminating():
    """Count bench-ns-* namespaces in the Terminating phase.

    The sibling `count_bench_ns()` EXCLUDES Terminating namespaces (it is
    the "how many usable bench namespaces exist" signal). That exclusion is
    exactly the masking the Task #275 trace named: between two runs, the
    prior run's bench-ns-* go Terminating while the apiserver garbage-
    collects their child CRs (panels included), but `count_bench_ns()`
    reports 0 → the bench believes the cluster is clean while phantom
    composition-panel CRs are still live.

    This counter makes the Terminating residue VISIBLE to
    `cluster_dirty_state()` so pre-clean verification stops being blind to
    the in-flight namespace GC. `kubectl get ns --no-headers` prints the
    phase in the STATUS column, so a Terminating bench namespace appears as
    `bench-ns-NN   Terminating   <age>`.
    """
    rc, out, _ = kubectl("get", "ns", "--no-headers")
    return len([line for line in out.strip().split("\n")
                if "bench-ns-" in line and "Terminating" in line])


# ── Compositions-page panel widgets (Task #250 Block 2b / re-gate fix) ────
#
# Empirical ground-truth (kubectl probe 2026-06-05 on
# gke_neon-481711_us-central1-a_cluster-1 at 48,999 compositions):
#   - `panels.widgets.templates.krateo.io` cluster-wide: 17,654
#   - filtered by label `krateo.io/portal-page=compositions`: 4,423
#   - the SAME label-filtered set drives the
#     `compositions-page-datagrid` (TRACED at
#     `kubectl get datagrids.widgets.templates.krateo.io
#     compositions-page-datagrid -n krateo-system -o yaml`:
#     `resourcesRefsTemplate.iterator: ${.compositionspanels}` from
#     RESTAction `compositions-panels` which lists panels filtered by
#     `(.metadata.labels // {})["krateo.io/portal-page"] == "compositions"`).
#   - per-ns count varies: bench-ns-01 has 143 comp-panels for 999 comps
#     (ratio 0.14 — controller-dynamic catches up partially).
#
# Why this is the right `n_visible` source for the Task #250 N-aware
# EXPECTED_CALLS formula:
#   - The /compositions datagrid's per-card fan-out (10 + 4×min(N,5))
#     is driven by the COMP-PANELS list returned by `compositions-panels`,
#     NOT the raw compositions LIST. A composition without a corresponding
#     comp-panel does NOT appear as a card on the datagrid → no per-card
#     widget calls.
#   - At small N (S4 with 20 compositions just deployed), the
#     controller-dynamic has materialized only a handful of comp-panels in
#     the measurement window. Empirical S4 = 14 = 10+4×1 maps to "1 card
#     visible" → 1 comp-panel materialized at measure time.
#   - At large N (S6 with 50K compositions), > per_page=5 comp-panels
#     exist → the datagrid caps the visible cards at 5 → 30 calls.
#
# Caller MUST scope appropriately to user RBAC:
#   - admin: target_ns=None → cluster-wide count (UAF returns all panels).
#   - cyberjoker: target_ns=<granted-ns> → narrowed count (UAF returns
#     only panels in granted namespaces).

COMP_PANEL_GROUP = "widgets.templates.krateo.io"
COMP_PANEL_VERSION = "v1beta1"
COMP_PANEL_RESOURCE = "panels"
COMP_PANEL_LABEL_SELECTOR = "krateo.io/portal-page=compositions"


def count_compositions_with_panels_ready(target_ns=None):
    """Count `panels.widgets.templates.krateo.io` carrying the
    `krateo.io/portal-page=compositions` label.

    This is the per-ns / cluster-wide count of compositions that have
    materialized their compositions-page panel widget — i.e. compositions
    visible on the /compositions datagrid. Used as `n_visible` in the
    Task #250 N-aware EXPECTED_CALLS formula.

    Args:
        target_ns: when None, count cluster-wide (admin path); when a
            string, count only in that namespace (cyberjoker / narrowed
            RBAC path).

    Returns:
        int count on success.
        None on transport / client failure (caller falls back to BASE
        expected via `expected_calls(..., n_visible=None)`).
    """
    if not _k8s_init():
        return None
    try:
        if target_ns:
            resp = _k8s_custom.list_namespaced_custom_object(
                group=COMP_PANEL_GROUP,
                version=COMP_PANEL_VERSION,
                namespace=target_ns,
                plural=COMP_PANEL_RESOURCE,
                label_selector=COMP_PANEL_LABEL_SELECTOR,
                _request_timeout=300,
            )
        else:
            resp = _k8s_custom.list_cluster_custom_object(
                group=COMP_PANEL_GROUP,
                version=COMP_PANEL_VERSION,
                plural=COMP_PANEL_RESOURCE,
                label_selector=COMP_PANEL_LABEL_SELECTOR,
                _request_timeout=300,
            )
    except Exception as e:
        if _k8s_is_404(e):
            return 0
        return None
    items = resp.get("items", []) if isinstance(resp, dict) else []
    return len(items)


def list_composition_names():
    """Return a set of "ns/name" strings for all compositions in the
    cluster via kubectl. Returns None on failure (so callers can
    distinguish from 'legitimately empty').

    Task #348: switched from `-o jsonpath` (which forces the apiserver to
    serialize FULL objects + kubectl to template every one: 41.7s, ~24s
    client CPU at 49K) to the SERVER-SIDE Table printer paged via
    `--chunk-size` (11.7s, ~4s client CPU). The `--all-namespaces` Table
    for this CRD is `NAMESPACE  NAME  AGE  READY`, so the first two
    whitespace columns are ns/name — the SAME cheap request that powers
    count_compositions(). The resulting "ns/name" set is byte-identical to
    the old jsonpath oracle (proven via `diff` on the live 49K cluster —
    see /tmp/snowplow-348-347/probe-timing.txt), so the CONTENT name-diff
    semantics are preserved exactly. Stays an INDEPENDENT kubectl oracle.
    """
    rc, out, _ = kubectl("get", f"{COMP_RES}.{COMP_GVR}", "--all-namespaces",
                         "--no-headers",
                         f"--chunk-size={COMP_PROBE_CHUNK_SIZE}")
    if rc != 0:
        return None
    names = set()
    for line in out.strip().split("\n"):
        parts = line.split()
        if len(parts) >= 2:
            names.add(f"{parts[0]}/{parts[1]}")
    return names


# ─── Composition deploy + YAML helpers ───────────────────────────────────────

def composition_yaml(ns, name):
    """Return the YAML body for a single bench composition."""
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


def deploy_compositiondefinition(ns="bench-ns-01"):
    """Apply the bench CompositionDefinition into `ns` and wait for Ready.

    Returns True if the CD becomes Ready within 300s, False otherwise.
    """
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
    deadline = time.time() + 300
    while time.time() < deadline:
        rc, out, _ = kubectl("get", "compositiondefinitions.core.krateo.io",
                             COMPDEF_NAME, "-n", ns,
                             "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
        if rc == 0 and out.strip() == "True":
            return True
        time.sleep(5)
    return False


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
        rc2, _, _ = kubectl("get", f"deployment/{deploy}", "-n", ns, "--no-headers",
                            "--ignore-not-found")
        if rc2 != 0:
            return
        kubectl("rollout", "restart", f"deployment/{deploy}", "-n", ns)
        kubectl("rollout", "status", f"deployment/{deploy}", "-n", ns,
                "--timeout=60s")
        return

    ready = int(out.strip()) if out.strip().isdigit() else 0
    if ready <= 0:
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
    kubectl("patch", "applications.argoproj.io", "--all", "-n", ns,
            "--type=merge", f"-p={FINALIZER_PATCH}")
    kubectl("delete", "applications.argoproj.io", "--all", "-n", ns,
            "--ignore-not-found", "--wait=false")


def force_finalize_namespace(ns_name):
    """Force-finalize a stuck Terminating namespace via /finalize subresource."""
    import json as _json
    body = _json.dumps({
        'apiVersion': 'v1', 'kind': 'Namespace',
        'metadata': {'name': ns_name}, 'spec': {'finalizers': []}
    })
    kubectl("replace", "--raw", f"/api/v1/namespaces/{ns_name}/finalize",
            "-f", "-", input_data=body)


# ─── Statistics helper ───────────────────────────────────────────────────────

def pct(data, p):
    """Return the p-th percentile of `data` (a sorted-or-not list).

    Uses the nearest-rank method. Returns None for empty input.
    """
    if not data:
        return None
    s = sorted(data)
    return s[max(0, int(round(p / 100.0 * len(s))) - 1)]


def call_url(ns, name=""):
    """Build a /call URL for the bench composition GVR.

    Used by browser flows + cache invalidation probes. Keeping the URL
    template in cluster.py keeps "what /call URL do I hit" answerable
    without crossing the browser.py boundary.
    """
    url = f"/call?apiVersion={COMP_GVR}%2Fv1-2-2&resource={COMP_RES}&namespace={ns}"
    if name:
        url += f"&name={name}"
    return url


# ─── Module-import guard (idempotent) ────────────────────────────────────────

# Run the GKE guard ONCE at import time so all callers downstream are
# safe. Tests + CLI bypass via BENCH_ALLOW_NON_GKE=1.
gke_context_guard()


__all__ = [
    # Constants
    "CANONICAL_GKE_CONTEXT",
    "ENV_GKE_CONTEXT",
    "canonical_gke_context",
    "NS",
    "COMPDEF_NAME",
    "COMP_GVR",
    "COMP_RES",
    "COMP_CONTROLLER_DEPLOY",
    "FINALIZER_PATCH",
    "K8S_CRD_GVR",
    # Guards + wrappers
    "gke_context_guard",
    "kubectl",
    # k8s_client helpers
    "k8s_list_clusterroles_by_prefix",
    "k8s_list_clusterrolebindings_by_prefix",
    "k8s_delete_clusterrole",
    "k8s_delete_clusterrolebinding",
    "k8s_list_roles_all_ns_by_prefix",
    "k8s_list_rolebindings_all_ns_by_prefix",
    "k8s_delete_role",
    "k8s_delete_rolebinding",
    "k8s_create_namespaced_role",
    "k8s_create_namespaced_role_binding",
    "k8s_read_namespaced_role_binding",
    "k8s_can_i_create_rolebinding",
    "k8s_list_namespaces_by_prefix",
    "k8s_delete_namespace",
    "k8s_create_namespace",
    "k8s_split_gvr",
    "k8s_list_cluster_custom",
    "k8s_patch_custom_finalizers_null",
    "k8s_delete_custom",
    # Task #314 — bench-owned CR apply / patch / read (S8b/S8c reconcile).
    "k8s_apply_yaml",
    "k8s_patch_cr_spec",
    "k8s_get_custom",
    "k8s_bulk_delete_clusterscope",
    "k8s_bulk_delete_namespaced",
    "k8s_bulk_patch_finalizers_null_custom",
    "k8s_bulk_delete_custom",
    # Counts + introspection
    "count_compositions",
    "count_compositions_in_ns",
    "count_bench_ns",
    "count_compositions_with_panels_ready",
    "list_composition_names",
    # Task #250 Block 2b — compositions-page panel widget probe constants
    "COMP_PANEL_GROUP",
    "COMP_PANEL_VERSION",
    "COMP_PANEL_RESOURCE",
    "COMP_PANEL_LABEL_SELECTOR",
    # Composition + namespace ops
    "deploy_compositiondefinition",
    "composition_yaml",
    "ensure_composition_controller",
    "delete_argo_apps_in_ns",
    "force_finalize_namespace",
    # Statistics
    "pct",
    "call_url",
    # Module-level state accessor (read-only flag)
    "K8S_CLIENT_AVAILABLE",
]
