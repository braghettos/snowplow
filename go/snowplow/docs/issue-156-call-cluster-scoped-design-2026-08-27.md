# Issue #156 — /call cluster-scoped read+write: root-cause + fix design

Repo: `github.com/krateo-platformops/snowplow` (OWNED monorepo, `go/snowplow/`)
Ref: main tip `0149c45` (= `1.10.1-3-g0149c45`). `go build ./...` green.
Author: cache-architect. Date: 2026-08-27.

## Verdict
ENDORSE-WITH-REFINEMENT. The issue's approach (reuse the authoritative
`RESTMapping().Scope` signal, omit `namespaces/<ns>` for cluster-scoped, scope the
validation relaxation to /call only) is correct and matches existing prod prior art
(`internal/dynamic/client.go:137-158`). The refinements are: (a) the mapper is NOT
already reachable at `callHandler` — it must be wired in; use the existing
`dynamic.SharedSADiscoveryClient` SA singleton rather than a fresh per-request mapper;
(b) fail-safe MUST be fail-CLOSED (4xx), not silent fallback to namespaced; (c) the
scope resolution is a two-step GVR→KindFor→RESTMapping (a GVR alone has no Scope).

## Root cause (TRACED)
1. `buildURIPath` (internal/handlers/call.go:231) unconditionally injects
   `namespaces/<ns>` for every resource except `resource=="namespaces"` (line 232).
   No cluster-scoped path is expressible in either direction.
2. `ParseNamespacedName` (internal/handlers/util/nsn.go:17-21) is verb/scope-unaware
   and hard-rejects empty `namespace` (and empty `name`, nsn.go:12-14) before scope is
   ever known — so a cluster-scoped `/call` 400s at validation (call.go:186).
3. Snowplow already KNOWS scope authoritatively but does not consult it on the /call
   path: `internal/dynamic/client.go:146` computes `RESTMapping` and has `.Scope` in
   hand, yet branches on `len(opts.Namespace)==0` (client.go:152) — a caller-supplied
   proxy for scope, not the real signal.

Symptom→cause is direct: the portal access-grant Form calls `/call` POST to create
Role+RoleBinding (namespaced, works) but cannot create ClusterRole+ClusterRoleBinding
because buildURIPath emits `/apis/rbac.authorization.k8s.io/v1/namespaces/<ns>/clusterroles/<n>`
— a 404 path — and ParseNamespacedName rejects the namespace-less request up front.

## Design

### 1. Scope source + mapper wiring
Add a scope resolver seam. Do NOT hardcode a resource set, do NOT add a `scope=` query
param (feedback_no_special_cases).

- `handlers.Call()` (call.go:33) takes no args today and is mounted at 6 sites
  (main.go:1018/1034/1074/1080/1086/1092). The `rc` at main.go:406 and :928 is
  block-scoped inside the cache if/else and is NOT visible at the mount block (1014+).
  So the handler cannot capture an `rc` from there.
- USE the process-wide SA singleton instead: `dynamic.SharedSADiscoveryClient(rc)`
  (cached_client.go:181) already exposes a built, always-non-nil, invalidation-wired
  `DeferredDiscoveryRESTMapper`; `rc` for it is `dynamic.ServiceAccountRESTConfig()`
  (sa_client.go:158, memoized, callable anywhere). Its CRD-lifecycle invalidation
  (InvalidateSADiscovery, cached_client.go:277) already keeps a newly-installed CRD's
  scope fresh — free correctness for the "CRD discovered after boot" corner.
- Scope is resolved GVR→GVK→scope (a GVR has no Scope; mirror client.go:139-146):
  `gvk,_ := mapper.KindFor(gvr)` then `m,_ := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)`
  then `namespaced := m.Scope.Name() == meta.RESTScopeNameNamespace`.
- Wire via a narrow interface field on callHandler (a `scopeResolver` func/iface) defaulting
  to the SA-mapper-backed resolver, so tests inject a fake — do NOT drag a real mapper into
  unit tests. `Call()` sets the default; a `CallWithScopeResolver(...)` test-only constructor
  (or a package-private field set in export_test.go) injects fakes.

rbac.authorization.k8s.io/v1 clusterroles resolves through this mapper as scope=root
(cluster) — validated by the falsifier's fake mapper returning RESTScopeNameRoot.

### 2. buildURIPath change (call.go:225-259)
Thread the resolved `namespaced bool` into buildURIPath (add to `callOptions` or pass as
arg). Change ONLY line 231's guard:

- namespaced==true  → `path.Join(base, "namespaces", ns, resource)` (BYTE-IDENTICAL to today).
- namespaced==false → `path.Join(base, resource)` (no namespaces segment).
- keep the `resource=="namespaces"` special case (232) — namespaces is itself cluster-scoped,
  and the mapper will independently report it root; either branch yields the same path, so
  the existing line is harmless/redundant but leave it (backward-compat, zero churn).

Per-verb walk for cluster-scoped correctness (name append at 236-243 is UNCHANGED):
- GET  → `/apis/G/V/clusterroles/<name>`      ✓ (name appended, AC-1)
- PUT/PATCH → `/apis/G/V/clusterroles/<name>` ✓ (name appended, AC-2)
- DELETE → `/apis/G/V/clusterroles/<name>`    ✓ (name appended, AC-2)
- POST → `/apis/G/V/clusterroles`             ✓ (create already omits name, 236-243 excludes POST, AC-2)
All correct: the name-append block is scope-independent; only the ns segment differs.

### 3. Scoped validation (do NOT touch shared ParseNamespacedName)
ParseNamespacedName (nsn.go) has exactly TWO prod callers: call.go:186 (in scope) and
dispatchers/helpers.go:38 `fetchObject`→`objects.Get` (the cache-dispatch READ path, OUT of
scope). Editing nsn.go in place would change fetchObject. So:

- Leave nsn.go byte-unchanged.
- In validateRequest (call.go:172), parse name+namespace WITHOUT the reject, resolve scope,
  then apply a /call-LOCAL rule:
  - namespaced GVR: require non-empty namespace (preserve today's 400), require name for
    GET/PUT/PATCH/DELETE (POST create allows empty name — apiserver assigns/uses generateName).
  - cluster-scoped GVR: namespace MUST be absent/ignored; require name for GET/PUT/PATCH/DELETE.
  Recommended shape: a new `util.ParseNamespacedNameScoped(req, namespaced, verb)` OR inline the
  three checks in a /call-local helper after scope is known. Either keeps nsn.go untouched.
- PROOF fetchObject unchanged: objects.Get (objects/get.go:34) consumes `ref.Namespace`
  directly and never calls ParseNamespacedName (grep: zero hits in internal/objects/). The
  dispatch path is a different handler (restactions.go:85 / widgets.go:70 call fetchObjectFn),
  never the raw callHandler. Byte-identical.

### 4. Fail-safe on scope-unknown — FAIL-CLOSED (4xx)
If KindFor/RESTMapping errors (CRD not yet discovered, mapper not synced, ambiguous
resource): return 4xx (422 Unprocessable Entity or 400), do NOT silently fall back to
namespaced. Justification: this is an RBAC-grant write path; a silent namespaced fallback
would (a) re-open the exact 404 bug for a legitimately-cluster-scoped write the instant
discovery lags, and (b) for a genuinely namespaced resource whose mapper is cold, produce a
namespaced write under an unverified scope — an unaudited-intent hazard. Fail-closed forces a
retry once discovery settles (the SA mapper's InvalidateSADiscovery wiring guarantees it will).
The mapper is warm within the first Phase-1 walk, so this window is boot-only. NOTE: this is a
behavioral tightening for the (rare) cold-mapper namespaced case — call it out to PM/Diego as
the one non-backward-compatible corner; the alternative (fallback-namespaced) is the
strategic option below.

### 5. Security findings (each proven on THIS repo)
(a) NO snowplow-side namespaced gate in the /call write path — CONFIRMED. call.go:69-121:
    validateRequest → buildURIPath → request.Do runs entirely under `ep` =
    xcontext.UserConfig (call.go:86, the caller's OWN `<user>-clientconfig`). There is NO
    RBAC check, NO namespace allowlist, NO snowplow-side authz anywhere between validate and
    request.Do. The only namespace logic is the URI-path assembly (buildURIPath). The
    apiserver enforces RBAC on the caller's token — for a ClusterRole write the caller needs
    cluster-scoped RBAC (and the escalation check), exactly as intended. The SA mapper is
    used ONLY to read cluster SHAPE (scope), never to perform or authorize the write
    (feedback: SA discovery is shape metadata below the per-user layer, cached_client.go:44-52).
    => No bypass. AC-4 satisfied.
(b) Raw /call stays UNCACHED for cluster-scoped — CONFIRMED. call.go:120 records only the
    `RecordApiserverFallthrough` counter; there is no ResolvedCache Get/Put on this path. No
    namespace-in-key, so no cluster-vs-namespaced key collision is even reachable. AC-3 key-safe.
(c) audit.Emit fires for cluster-scoped writes — CONFIRMED. call.go:128-144 emits for
    POST/PUT/PATCH/DELETE unconditionally; Namespace is just a field. audit.add() skips empty
    values (audit.go:216-217), so an empty Namespace on a ClusterRole write emits a clean
    AuditEvent with the group/resource/name/verb/outcome — the HITL-gated provenance the issue
    wants. AC (provenance) satisfied.
All three PASS — no REWORK finding.

### 6. LIST — OUT of scope (confirmed)
/list (handlers/list.go:33) is ns-OPTIONAL: `ns := query.Get("ns")` (list.go:36) flows into
`dynamic.Options{Namespace: ns}` (list.go:88) → resourceInterfaceFor branches
`len(Namespace)==0 → cluster` (client.go:152). Empty ns ⇒ cluster-scoped LIST works TODAY.
So the cluster-scoped READ-LIST need is already covered; do NOT add LIST-via-/call
(buildURIPath appends name for GET, so /call is by-name only by construction). AC-1's read is
GET-by-name via /call; broad enumeration is /list. Recommend documenting this split, no code.

### 7. Falsifier plan (1:1 to the 5 ACs)
Hermetic white-box `package handlers` tests exist already (export_test.go uses httptest +
package-private access). buildURIPath is package-private ⇒ unit-testable with a fake mapper,
NO kind cluster.

- AC-1 (GET-by-name cluster-scoped): unit. Fake mapper: clusterroles→scope=root. Drive
  validateRequest+buildURIPath (or the handler with a stub roundtripper). ASSERT built URI ==
  `/apis/rbac.authorization.k8s.io/v1/clusterroles/<name>` — NO `namespaces/` segment.
- AC-2 (POST/PATCH/DELETE cluster-scoped): unit, per verb. POST → `.../clusterroles` (no name);
  PATCH/DELETE → `.../clusterroles/<name>`. ASSERT no `namespaces/` for all three.
- AC-3 (namespaced byte-unchanged): unit. Fake mapper: roles→scope=namespace. ASSERT built URI
  BYTE-IDENTICAL to today's `/apis/.../namespaces/<ns>/roles/<name>`. Golden-string equality.
- AC-4 (auth via caller token, no bypass): integration/kind arm (call_test.go already kind-backed).
  Apply RBAC that grants a user cluster-scoped clusterroles create; assert the /call POST creates
  the ClusterRole under the user's token; assert a user WITHOUT cluster RBAC gets 403 from
  apiserver (not a snowplow 200). Pure-unit cannot prove RBAC flows to apiserver.
- AC-5 (tests cover cluster read AND write): the AC-1 (read) + AC-2 (write) units above, plus a
  kind read+write pair in call_test.go for end-to-end.
- RED arm (mandatory): neuter the scope check (force namespaced==true for a cluster GVR) →
  buildURIPath rebuilds `/apis/rbac.authorization.k8s.io/v1/namespaces/<ns>/clusterroles/<name>`
  → the AC-1 assertion FAILS. This proves the scope branch is load-bearing.
- Fail-closed arm: fake mapper returns an error for an unknown GVR → assert 4xx (not a
  namespaced-path 200/404).

Which need kind: AC-4 (RBAC-to-apiserver) + the AC-5 end-to-end pair. AC-1/2/3 + both
adversarial arms are pure-unit (hermetic).

## Strategic option to surface (PM/Diego)
Fail-safe policy on scope-unknown:
- OPTION A (RECOMMENDED): fail-closed 4xx. Safest on an RBAC-grant write path; boot-only
  window; forces retry once the SA mapper warms. One-corner behavioral tightening (cold-mapper
  namespaced write now 4xx-retries instead of proceeding).
- OPTION B: fall back to today's namespaced behavior on mapper miss. Fully backward-compatible
  (zero behavior change for namespaced) but re-opens the 404 for a cluster-scoped write during
  any discovery lag AND writes namespaced under unverified scope. Not recommended for a
  grant-write path.

## LOC bound / file:line targets
- internal/handlers/call.go: buildURIPath ns-guard (~231) + thread `namespaced` + validateRequest
  scope resolution + /call-local ns/name rule + callHandler scopeResolver field + Call()/test ctor.
  ~50-80 LOC.
- internal/handlers/util/nsn.go: UNTOUCHED (or +1 new `ParseNamespacedNameScoped` variant, ~15 LOC).
- internal/dynamic: reuse SharedSADiscoveryClient/ServiceAccountRESTConfig; optionally a tiny
  `ScopeForGVR(mapper, gvr) (namespaced bool, err error)` helper (~12 LOC) so the two-step is
  single-sited and fake-injectable.
- main.go: 0 LOC if Call() self-resolves the SA mapper lazily; else pass a resolver at the 6 mounts.
- Tests: new hermetic cases in call_test-adjacent package-`handlers` file + kind arms. ~120 LOC.

## Falsifier (ship proof)
Green: unit asserts cluster GVR URI has no `namespaces/` and namespaced URI byte-identical;
kind arm shows ClusterRole created under caller token + 403 for unprivileged; audit line emitted
for the cluster write. Red-arm (neuter scope) rebuilds the bad namespaced clusterroles path.
