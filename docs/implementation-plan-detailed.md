# Detailed implementation plan — clean-slate from `0.20.5`, tag-by-tag

**Audience:** Diego + PM + dev + tester.
**Date:** 2026-05-09 (final pre-execution comprehensive revision).
**Author:** Architect.
**Status:** Detailed per-tag breakdown of the PM-approved-with-amendments roadmap, revised 2026-05-09 to fold in Diego's six binding constraints (chart-strip, portal-strip, tag-regex, in-process Role-Based Access Control evaluation, Role-Based Access Control enforced on every RestAction dispatch including non-userAccessFilter, convergence p99 < 1 s scoring metric).

---

## SESSION CHECKPOINT — 2026-05-13 end of day

| Tag | Status | Notes |
|---|---|---|
| 0.30.0 → 0.30.5 | SHIPPED (historical) | |
| 0.30.6 v0 (eager-register) | SUPERSEDED (broken — pre-flight gap) | Replaced by v2 |
| 0.30.61 | SHIPPED (mitigation: gated eager-register OFF) | |
| 0.30.6 v2 (typed-RBAC indexer) | SHIPPED + VALIDATED | -62% CPU on cyber dashboard, FromUnstructured 4760→0ms |
| **0.30.7 (L1 resolved-output cache scaffold)** | **DEPLOYED** (helm rev 98, image live) | Phase A bench mix-weighted cold=1453ms warm_p50=893ms — **first time within ~1.5× of north-star at 50K** |
| **0.30.71 (CACHE_ENABLED=false extends to disable typed-RBAC + informer cache)** | **TAGGED + IMAGE BUILT** (CI ✓) | Diagnostic-only; NOT deployed; default unchanged |
| **0.30.8 (DELETE-evict + UPDATE-refresher)** | **REBASED (onto 0.30.71), UNCOMMITTED** | Working tree on `ship-0.30.8-resolved-cache-deps`; awaiting architect+PM re-review + pre-flight falsifier `delete_stale_gap.py` |
| 0.30.9 (UAF) | PLANNED | When ships, remove cyberjoker ClusterRoles from portal chart |
| 0.30.10+ | PLANNED | Per original plan |

**Today's key findings:**
- **§0.30.8 Revision 16 amendment (CREATE-event handler) FALSIFIED + ROLLED BACK** — pre-flight probe showed first nav after `Created bench-ns` reached 16/16 calls in 3s. Today's 192 cyberjoker FAILs from 0.30.6 v2 bench were bench-harness rapid-namespace-bulk-stress + over-broad `.ant-skeleton` selector, NOT real product behavior. Plan reverted to original DELETE-evict-only scope (committed to main as `5957a49`).
- **Safety guard for `internal/rbac/rbac_test.go`** (`335eda4`) cherry-picked onto main + ship-0.30.7-l1-scaffold + pushed to braghettos. Prevents future CRD-wipe via `go test ./internal/rbac/...`.
- **Bench-harness fix `bd05f96`**: scoped skeleton selector `[data-widget-renderer] .ant-skeleton` + per-user EXPECTED_CALLS dict pushed to braghettos. **PENDING frontend change**: `WidgetRenderer.tsx` must wrap loading `<Skeleton>` in `<div data-widget-renderer>` to make selector match (currently false-negative risk).
- **Branch policy:** Option B chosen — fold 0.30.71 forward. 0.30.8 rebased onto 0.30.71. All future tags inherit cache-off diagnostic mode.
- **Cluster has UNCLEAN bench residue** at session-end: 49 bench-ns + 50K compositions + 162 ClusterRoles + 2369 CRBs. Next bench needs CompositionDefinition delete sequence FIRST before namespace delete.

**Queued for next session (priority order):**
1. Frontend `data-widget-renderer` wrapper change in `WidgetRenderer.tsx` (3 retries today errored via Agent dispatch — try direct Edit)
2. 0.30.8 architect + PM re-review on rebased branch
3. 0.30.8 pre-flight falsifier `delete_stale_gap.py`
4. 0.30.8 commit + tag + push + deploy
5. 0.30.71 deploy + true-cache-off measurement
6. Cluster cleanup (CompDef delete sequence)
7. 0.30.9 UAF dispatch

**Detailed session state in memory:** `project_session_checkpoint_2026_05_13.md`

---

**Foundations (do not re-explain here — read first):**
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/clean-slate-proposal-from-0.20.5.md`
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/informer-as-cache-primer.md`
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/feature-audit-since-0.20.5.md`
- PM amendments (binding): `/private/tmp/claude-501/-Users-diegobraga-krateo-snowplow-cache-snowplow/d95b6c4b-bf71-47d1-a8be-fff7e42c9e22/tasks/acb35fb3dcd3709ca.output`

**Diego's binding additions (1–3, original) encoded throughout this plan:**
1. cache=off at SCALE=50000 expected to be broken (cache becomes load-bearing in practice at customer scale). Cache=off escape hatch is for SMALL-SCALE customers only.
2. Regression gate "cache=on must beat cache=off" applies at scales where cache=off is usable (≤5K). At 50K, the gate is "cache=on must hit Row 7's 4 100 ms cold mix-weighted target or be on credible trajectory."
3. Each step is measured at BOTH SCALE=50000 AND SCALE=5000.

**Diego's binding additions (4–6, 2026-05-09) encoded throughout this plan:**
4. **Chart-fork divergence:** `braghettos/snowplow-chart` (the published values.yaml) sets keys that the `0.20.5` binary does NOT consume. A stripped-chart variant must ship alongside Tag 0.30.0 and re-grow tag-by-tag. See per-tag "Chart values" subsections.
5. **Portal manifests divergence:** `braghettos/portal/blueprint/templates/` ships RestActions with `userAccessFilter` — a CustomResourceDefinition field added at Q-COLD-1 C(d) (~0.25.295) and NOT recognized by the `0.20.5` resolver. A stripped-portal variant must ship alongside Tag 0.30.0 and re-grow at Tag 0.30.9 (atomic userAccessFilter ship). See per-tag "Portal compatibility" subsections.
6. **CI tag regex:** the build pipeline accepts ONLY `x.y.z` tag names (no suffix, no prefix). All sub-ships from PM amendment 6 (Step 4 split) consume their own `z` increment. Branch names are descriptive; only **tags** are constrained.

**Diego's binding additions (7–10, 2026-05-09 final pre-execution revision) encoded throughout this plan:**
7. **In-process Role-Based Access Control evaluation rule:** in cache=on mode, snowplow MUST satisfy ALL Role-Based Access Control checks via in-process evaluation against informer-cached `Role`, `RoleBinding`, `ClusterRole`, `ClusterRoleBinding` rules. **Zero `SubjectAccessReview` calls to apiserver in cache=on mode.** Cache=off path retains `SubjectAccessReview` for correctness. This rule lands at Tag `0.30.4` (the cache=on activation tag) — see Revision 1 split discussion below.
8. **Role-Based Access Control enforced on every RestAction dispatch, regardless of userAccessFilter presence.** The userAccessFilter mechanism (introduced at `0.30.9`) only changes WHO dispatches (snowplow-ServiceAccount cluster-wide-read vs per-user-credentials), NOT whether Role-Based Access Control is enforced. Both paths use the same `EvaluateRBAC` function for the actual permission check.
9. **Convergence time as scoring metric.** Per `feedback_north_star_is_frontend_ux.md`: north-star is "1 s cold / 500 ms warm / **1 s fresh** at 50 K." Phase 6 of `e2e/bench/snowplow_test.py` measures convergence time (composition ramp; poll snowplow API for new-state visibility; time-from-mutation-to-visible). **Diego-confirmed target: convergence p99 < 1 s at SCALE=50000 cache=on.** Bench-Harness must capture Phase 6 convergence-time alongside Phase 4/5 page-load. The canonical ledger row gains 4 convergence columns (convergence_p50 + convergence_p99 across cache=on + cache=off). **Diego just-in ruling (2026-05-09 mid-edit): `feedback_l1_invalidation_delete_only.md` rule is BINDING and PERMANENT — UPDATE/PATCH must NOT evict resolved-output cache entries; only DELETE evicts. Convergence p99 < 1 s on UPDATE must be engineered WITHIN this constraint via aggressive background refresher (target <500 ms latency from UPDATE event to fresh resolved-output cache value). If <1 s p99 is mechanically impossible within DELETE-only invalidation at SCALE=50000, that is a STOP condition, NOT a rule-revisit signal.**
10. **Targeted Role-Based Access Control prewarm (Revision 6, 2026-05-09 just-in; renumbered to `0.30.12` post-Revision-8):** at startup, walk RoleBinding + ClusterRoleBinding subjects (users + groups), filter to those whose bound rules grant `get`/`list` on `widgets.templates.krateo.io` and/or `templates.krateo.io/restactions` (and similar widget/RestAction-relevant resources), build the `(subject, widget)` enumeration, and prewarm the resolved-output cache for those pairs. **This is NOT the audit's blacklisted prewarm pool** (which drove AddResourceType fan-in storm and was traced through 7 walk-backs). It is targeted, dedup'd by binding-identity, and runs AFTER eager resource-type registration (`0.30.6`) so no AddResourceType calls fire during prewarm. Lands at Tag `0.30.12` — see Revision 6 tag insertion discussion below.
11. **Pod readiness gated on prewarm completion (Revision 7 + 7a, 2026-05-09):** Diego: "pod must become ready once L1 cache has all resolved keys from L3 keys × subjects (users and groups binded by roles)." At Tag `0.30.12`, the readiness probe MUST return 503 until every `(bound_subject, FrontendInitialRenderCall)` pair has been written to the resolved-output cache; only then `prewarm.complete = true` and probe flips to 200. **This deliberately re-couples Ready to prewarm** — Q-PREWARM-R2 (`0.25.301`, audit Rank 4) DECOUPLED them to break the prewarm death-spiral; Revision 7 re-couples with explicit death-spiral guardrails (configurable `PREWARM_READY_DEADLINE_SECONDS` default **1 200 s = 20 min** post-7a; on timeout, set `prewarm.complete=true` with partial cache + WARN log so the chart-tuned kubelet startup-probe deadline is honoured and the pod does not enter restart loop). Trade-off is conscious: every bound subject's first-nav becomes L1-warm, paying time-to-Ready as a one-time deploy cost (~6 s–2 min post-Revision 10).
12. **Subject activity classification + frontend-mirror prewarm (Revisions 8 + 8a + 9 + 10 + 11, 2026-05-09):** Two tags. (a) Tag `0.30.11` — subject HOT/WARM/COLD classification by `last_seen_unix_ts` (HOT <5 min, WARM 5–60 min, COLD >60 min; chart-tunable). NOT load-bearing for prewarm scoping (Revision 11a: "there are no hot users if the pod is just started" — at startup, classifier is empty). Load-bearing for POST-startup refresher priority + L1 eviction priority (Revision 11c: "0.30.11 still worth a separate tag"). (b) Tag `0.30.12` — frontend-mirror prewarm. The prewarm call set is a hardcoded Go-level constant `FrontendInitialRenderCalls` in snowplow source (Revision 9), inspected from the frontend repo at `https://github.com/braghettos/frontend` (Revision 11b), kept in sync via CI mirror test. Cartesian = ALL bound subjects (Revision 10 reverses Revision 8 HOT-scoping) × `FrontendInitialRenderCalls`. Estimate: ~100 bound subjects × ~5–10 calls = **~500–1 000 entries** (fits "minutes acceptable" budget at 6–120 s wall-clock). Tag count: **14** (after pprof-tag insertion 2026-05-09 late-edit; see Revision 15 below).

**Revision 15 decision — pprof tags inserted at `0.30.2` (baseline + pprof) and `0.30.3` (informer plumbing dormant + pprof), 2026-05-09 late-edit.** Rationale:
- Diego approved Option 1 (insert pprof tags into the sequence) over Option 0 (no pprof) and Option 2 (`0.30.0.1`-style sub-tags). Forensic capability is mandatory; the `0.20.5` upstream binary does NOT expose `/debug/pprof/*` (the side-effect import was added later in the snowplow timeline). Without pprof, post-`0.30.0` forensic comparison against later tags is impossible.
- Per Diego's binding rule 6, CI tag regex accepts ONLY `x.y.z`. Sub-tags `0.30.0.1` / `0.30.1.1` are inadmissible. Instead, two new tags are inserted: `0.30.2` (= `0.30.0` semantics + `import _ "net/http/pprof"`) and `0.30.3` (= old `0.30.1` semantics + the pprof import). All subsequent tag numbers shift by +2.
- `0.30.0` and `0.30.1` are preserved unchanged as **historical superseded baselines** (no pprof; not used for forensic comparison going forward).
- Mechanism delta per pprof tag: ~2 LoC (one blank import in `main.go`) plus a chart unchanged for `0.30.2` (pprof binds to the default mux on port 8081 — no extra listener); zero behaviour change at idle. Bottom-tier risk.
- New §3 Pass 4 (pprof goroutine + heap snapshot at SCALE=50K idle, immediately after Pass 3) becomes mandatory for every tag from `0.30.2` onward.

**Convention used in this document:**
- All tags are `0.30.X` (pure `x.y.z`, CI-compatible).
- All numerical predictions are ranges with reasoning; `TBD pending Step 0 baseline` is used when the architect does not know.
- "Mix-weighted" means `0.95·cyberjoker + 0.05·admin` per `feedback_north_star_is_frontend_ux.md`.
- Predictions are honest, not optimistic. The architect's track record (audit Pattern 2: 4 inverted projections in last 24 h) is the prior.
- **Terminology rule (Revision 3):** the forward-plan uses no acronyms beyond the universal (HTTP/JSON/YAML/API). Role-Based Access Control is spelled out; `SubjectAccessReview` is the Kubernetes API kind name; `userAccessFilter` is the CustomResourceDefinition field name; CustomResourceDefinition is spelled out on first use; "permission-check cache" replaces former "B-prime EvaluateRBAC LRU"; "bounded cache" or "least-recently-used eviction" replaces bare "LRU"; "RestAction" replaces "RA"; "resource type" replaces "GVR" (or "(Group, Version, Resource) tuple" where formal precision matters); "ServiceAccount" replaces "SA"; "time-to-live" replaces "TTL"; "OpenTelemetry" replaces "OTel"; "per-object cache mirror" is the audit's anti-pattern (never re-introduced). Project-codename references (Q-COLD-1, Q-MIRROR-REMOVAL, Q-CON-1, etc.) remain only as git-tag-anchored historical pointers, not as standalone jargon.

**Revision 1 decision — Option B (split former `0.30.1` into informer-only + cache=on-activation).** Rationale:
- Diego's rule 7 (zero `SubjectAccessReview` to apiserver in cache=on) is correctness, not performance. Shipping `0.30.1` with cache=on flipped while `EvaluateRBAC` falls back to `SubjectAccessReview` violates the rule from day 1.
- Option A (atomic `0.30.1` ~750–900 LoC) would require a PM-amendment-6 (per-tag cap) exception. `feedback_no_shortcuts_or_workarounds.md` discourages bypasses; respect the cap.
- Splitting preserves single-responsibility per ship: `0.30.1` proves informer + lazy-AddResourceType mechanics with cache=off (still default) and admin-only smoke; `0.30.4` adds the Role-Based Access Control informer + `EvaluateRBAC` + activates cache=on as one atomic correctness ship.
- Total tag count grows from 9 to **11** with Revision 6, then to **12** with Revision 8 (activity-class at `0.30.11`; prewarm renumbered to `0.30.12`). Revision 10 (Diego: prewarm fires for ALL bound subjects, not HOT-only) made me consider dropping the activity-class tag, but Revision 11c (Diego: "0.30.11 still worth a separate tag") confirmed activity classification stays standalone — load-bearing for refresher priority + L1 eviction priority POST-startup, even though it is NOT load-bearing for prewarm scoping. Revision 15 (Diego: pprof-tag insertion) lifts the count to **14**: insert `0.30.2` (baseline + pprof) and `0.30.3` (informer plumbing dormant + pprof); old `0.30.2`–`0.30.11` shift to `0.30.4`–`0.30.13`. **Final tag count: 14.**

**Revision 6 decision — insert targeted prewarm as new tag (NOT replace warmer-paginate).** Rationale:
- Targeted prewarm and warmer-paginate solve different problems: prewarm warms the resolved-output cache for known users; warmer-paginate (when shipped) warms the informer store. They are complementary, not substitutes.
- Replacing warmer-paginate would couple the targeted-prewarm decision to the (separately conditional) Q-OOM-WARMER decision. Cleaner to keep them as separate tags so each can be measured independently.
- Warmer-paginate is already CONDITIONAL with a pre-flight gate that may skip it entirely; targeted prewarm has no such conditionality (the audit's anti-pattern was generic prewarm-pool, not targeted prewarm; with eager resource-type registration absorbing the AddResourceType storm, the targeted form is safe).

**Revision 8/9/10/11 decision history (final):**
- Revision 8 (initial): proposed standalone activity-class tag `0.30.11` (HOT/WARM/COLD subject + RestAction tracking) to enable `PREWARM_HOT_ONLY=true` at prewarm.
- Revision 8a (Diego clarification): widget hot-set is static, declared by frontend; subject HOT/WARM/COLD remained dynamic.
- Revision 9 (Diego clarification): hardcoded `FrontendInitialRenderCalls` constant in snowplow source; CI mirror test enforces frontend↔snowplow alignment.
- Revision 10 (Diego correction, BINDING): prewarm fires for ALL bound subjects (RoleBinding + ClusterRoleBinding), NOT HOT-only. HOT subject classification is no longer load-bearing for prewarm scoping.
- **Revision 11a (Diego, BINDING):** "there are no hot users if the pod is just started" — confirms architecturally that HOT-scoping at startup is impossible (zero last-seen history); R10's all-bound-subjects scoping wins by necessity.
- **Revision 11b (Diego, BINDING):** frontend source is `https://github.com/braghettos/frontend` (not `braghettos/portal`); architect inspects this repo to codify `FrontendInitialRenderCalls`.
- **Revision 11c (Diego, BINDING):** `0.30.11` activity-class tag stays standalone (Path A wins over Path B). Justification: refresher priority + L1 eviction priority are POST-startup optimizations enabled by classification; ~80 LoC; no UX impact at the tag itself.

**Tag-to-step mapping (final after Revisions 6 + 7/7a + 8/8a + 9 + 10 + 11 + 15):**

| Tag      | Step  | Title                                                                              |
|----------|-------|------------------------------------------------------------------------------------|
| 0.30.0   | 0     | cache=off baseline (upstream 0.20.5 code) — historical, no pprof, superseded for forensic purposes |
| 0.30.1   | 1a    | cluster-wide informer factory + direct read API (cache=off remains default) — historical, no pprof, superseded |
| 0.30.2   | 0p    | cache=off baseline + `net/http/pprof` side-effect import (forensic-canonical floor; Revision 15) |
| 0.30.3   | 1ap   | informer plumbing dormant + pprof (forensic-canonical pre-cache-on baseline; Revision 15) |
| 0.30.4   | 1b    | Role-Based Access Control informer + `EvaluateRBAC` + cache=on activation          |
| 0.30.5   | 2     | SetTransform strip                                                                 |
| 0.30.6   | 3     | resolver-cache wiring (was: eager registration; rescoped after 2026-05-13 post-mortem) |
| 0.30.7   | 4a    | resolved-output cache scaffold (bounded cache + byte-budget, time-to-live only)    |
| 0.30.8   | 4b    | resolved-output cache dependency tracking + informer event wiring (ADD/UPDATE/DELETE) — scale-up convergence + refresher |
| 0.30.9   | 5     | userAccessFilter + ServiceAccount-dispatch + refilter (atomic ship)                |
| 0.30.10  | 6     | permission-check cache (bounded least-recently-used)                               |
| 0.30.11  | 6.25  | subject activity classification HOT/WARM/COLD (Revisions 8 + 11c)                  |
| 0.30.12  | 6.5   | targeted Role-Based Access Control prewarm (Revisions 6 + 7/7a + 9 + 10 + 11)      |
| 0.30.13  | 7 opt | warmer paginate (CONDITIONAL)                                                      |

Branch names (descriptive, NOT tags): `clean-slate-0.30.0`, `ship-0.30.1-informer-no-cache-on`, `ship-0.30.2-pprof-baseline`, `ship-0.30.3-pprof-informer-plumbing`, `ship-0.30.4-rbac-informer-cache-on`, `ship-0.30.5-strip`, `ship-0.30.6-eager-register`, `ship-0.30.7-resolved-cache-core`, `ship-0.30.8-resolved-cache-deps`, `ship-0.30.9-uaf-sa-dispatch`, `ship-0.30.10-permission-cache`, `ship-0.30.11-activity-class`, `ship-0.30.12-targeted-prewarm`, `ship-0.30.13-warmer-paginate`.

---

## Critical Path — pre-Tag-0.30.0 deliverables that block deploy

Per Diego's binding constraints (4), (5), and (9), Tag 0.30.0 cannot deploy until FOUR artifacts exist. Tracked here so the PM and team-lead can sequence ownership BEFORE Step 0 measurement begins.

| # | Artifact                       | Owner                | Description                                                                                                                                                                                                                | Sequencing      |
|---|--------------------------------|----------------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------|
| 1 | Bench-Harness fixes (Step −1)  | dev (with tester verification) | The 4 surgical commits to `e2e/bench/snowplow_test.py` listed in Step −1 below + Phase 6 convergence-time capture extension + p99 capture across all phases. ~1.5–2.5 dev-days.                                       | parallel with #2, #3 |
| 2 | `chart-0.30.0` stripped chart  | snowplow dev (chart-side)      | Fork of `braghettos/snowplow-chart` with every value the 0.20.5 binary cannot consume REMOVED (no probePort 8082, no warmup/frontend volumes, no cache/prewarm/OpenTelemetry env vars). Single chart commit, ~80 LoC strip+test. | depends on env-var audit |
| 3 | `portal-0.30.0` userAccessFilter-stripped | snowplow dev (portal repo authority) | Fork of `braghettos/portal/blueprint/templates/` with `userAccessFilter` field stripped from 6 RestActions. RestAction `spec.api`/`spec.config` preserved verbatim. **Per Revision 5, snowplow team produces this; no cross-team coordination latency.** | parallel with #1, #2 |
| 4 | `ghcr.io/braghettos/snowplow:0.30.0` image | CI on tag push    | Built from upstream commit `6375c9a`. `client-go` bump from `v0.33.0` → `v0.35.3` is the only code delta. Pure CI, no human intervention.                                                                                  | sequential after #1, #2, #3 |

**Sequencing call:** #1, #2, #3 are independent and can run in parallel (all snowplow-team-owned). #4 is gated on all three landing. ETA: 1 sprint (1 week).

**Pre-`0.30.0` precondition checklist (Revision 15 amendment):** After the four Pre-`0.30.0` deliverables above land AND the historical baselines `0.30.0` (cache=off baseline) + `0.30.1` (informer plumbing dormant) have been measured, ship `0.30.2` (baseline + pprof) and `0.30.3` (informer plumbing dormant + pprof) **before** resuming any functional ship in the `0.30.x` series. `0.30.2` and `0.30.3` ship together (~30 min total dev work for the 1-line side-effect import per branch + CI builds; bottom-tier risk). `0.30.2` is published from a 1-commit branch on `clean-slate-0.30.0` as `ghcr.io/braghettos/snowplow:0.30.2`; `0.30.3` is published from a 1-commit branch on `ship-0.30.1-informer-no-cache-on` as `ghcr.io/braghettos/snowplow:0.30.3`. From `0.30.2` onward, every snowplow binary MUST expose `/debug/pprof/*`; forensic snapshots (Pass 4, see §3) are part of the canonical ledger-row artifact set.

**Portal artifact (binding; snowplow-team-produced per Revision 5):** `portal-0.30.0` blueprint where every RestAction YAML in `blueprint/templates/restaction.*.yaml` has the `spec.api[*].userAccessFilter` field DELETED. RestActions to touch (audited 2026-05-09):
1. `restaction.blueprints-list.yaml` — currently `userAccessFilter: { verb: list, resource: namespaces, namespaceFrom: "." }` on the `namespaces` API call.
2. `restaction.blueprints-panels.yaml` — same userAccessFilter spec.
3. `restaction.compositions-panels.yaml` — same userAccessFilter spec.
4. `restaction.compositions-get-ns-and-crd.yaml` — TWO `userAccessFilter` blocks (one for `customresourcedefinitions` cluster-scoped, one for `namespaces`).
5. `restaction.all-routes.yaml` — same userAccessFilter spec on namespaces call.
6. `restaction.sidebar-nav-menu-items.yaml` — same userAccessFilter spec on namespaces call.

The `restaction.compositions-list.yaml` does NOT use userAccessFilter and is portable as-is. The `restaction.compositions-panels.yaml` ALSO uses post-0.20.5 widget transitions (compositions-panels was added as a customer-facing concept post-0.20.5 per `feedback_compositions_panels_in_scope.md`); snowplow dev (portal repo authority) confirms whether the RestAction spec itself relies on any non-userAccessFilter post-0.20.5 RestAction CustomResourceDefinition fields beyond userAccessFilter. If yes, that RestAction must also be downgraded for `portal-0.30.0`.

---

## Step −1 — Bench-Harness (precondition, NOT a snowplow tag)

**Owner:** dev (with tester verification).
**Branch:** N/A (lives in `e2e/bench/snowplow_test.py` + chart values, not a snowplow image).
**Reference dispatch (recon landed):** `a6bfe0f54a43d6872` reconnaissance of `e2e/bench/snowplow_test.py` (4 599 LoC).
**Cursor plan reference:** `/Users/diegobraga/krateo/snowplow-cache/snowplow/.cursor/plans/snowplow_cache_matrix_tests_d0110e82.plan.md`.

**Why this is NOT a snowplow tag:** PM amendment 2 explicitly assigns the matrix harness to dev (with tester verification), with chart values + ConfigMap toggle plumbing. Per `feedback_helm_only_for_portal.md` + `feedback_chart_only_for_snowplow.md`, the `CACHE_ENABLED` flip flows through chart values, not via `kubectl set env`.

**The 6 commits (Step −1 scope, binding) — revised 2026-05-09 mid-edit to fold in three-pass measurement structure (see new §3 below):**

*Original 4 commits (PM amendment 2 baseline):*
1. **Reorder `main()`** so `SCALE_GUARD` runs BEFORE `assert_clean(retry_with_cleanup=True)`. Current order deletes 48 999 live comps. Hard blocker; cannot run today.
2. **Replace hardcoded passwords with secret-mount.** Read admin/cyberjoker passwords from a Kubernetes Secret (chart-managed). Stale hardcoded values currently break authentication.
3. **Replace `enable_cache()` / `disable_cache()` (lines 605, 617) `kubectl set env` calls with helm-values toggle.** Violates `feedback_chart_only_for_snowplow.md`. Replacement: `helm upgrade --reuse-values --set env.CACHE_ENABLED=true|false` (chart-only).
4. **Add cyberjoker coverage to Phase 5.** Phase 5 currently hardcodes admin; Phase 4 already covers both. Make Phase 5 iterate over both users for matrix completeness.

*New commits 5–6 (added 2026-05-09 mid-edit per Diego's three-pass clarification):*
5. **Phase 6 standardization commit.** Update Phase 6 docstring to make explicit that it is the **canonical SCALE=50K cluster-setup procedure** (cleans cluster, programmatically ramps to 50K compositions, captures convergence DURING the ramp at sub-stages S6/S7/S8). Emit S6 (mass-CREATE p99), S7 (single-DELETE p99), S8 (namespace-cascade p99) as named JSON keys in `/tmp/browser_results.json`. Variance check: harness must run two back-to-back ramps and assert <10% variance per S6/S7/S8 number before emitting PASS. Phase 6 is **Pass 1** of the three-pass structure (see §3 below).
6. **Per-mutation convergence sub-test commit (Pass 3 of three-pass structure).** Synthesize sparse mutations (CREATE/UPDATE/DELETE pulse, ~50 events at controlled inter-arrival) on the steady-state 50K-ramped cluster AFTER Phase 4+5 measurement completes. Per-event subscribe-and-time loop measures `time-from-mutation-to-visible-via-snowplow-API` per subject + per widget class (HOT/WARM/COLD per Tag `0.30.11`). Emit `convergence_per_mutation_p99_mix` (mix-weighted across subjects) + per-class p99 (`per_class_hot_p99`, `_warm_p99`, `_cold_p99`) into `/tmp/browser_results.json`. **Open architectural question (architect ruling pending):** sub-test lives as Phase 6 mutation-pulse extension OR as new Phase 8. Architect decides at Step −1 dispatch — see §3 ruling below.

**Additional fixes (Revision 4, binding — folded into commits 5–6 above):**
- p99 capture in `/tmp/browser_results.json`: handled across all 6 commits.
- Phase 6 convergence-time capture: handled in commit 5 (S6/S7/S8 mass-stage) and commit 6 (per-mutation Pass 3).

**PASS criteria (PM amendment + Revision 4 + three-pass structure, binding):**
- Matrix harness emits the (cold + p50 + p90 + p99) JSON row from Pass 2 (steady-state UX, Phases 4+5) across `cache_on/off × admin/cyberjoker × {SCALE=50000, SCALE=5000}`.
- Pass 1 (Phase 6 cluster setup) emits convergence_mass_s6_p99 + _s7_p99 + _s8_p99.
- Pass 3 (per-mutation sub-test) emits convergence_per_mutation_p99_mix + per-class p99s.
- Variance <10% across two back-to-back runs at each scale (per-pass).
- `CACHE_ENABLED` toggle flows through chart values only (no `kubectl set env`; verified via grep returning 0).
- `uptime_at_capture ≥ 600 s` enforced on every Chrome MCP run (Pass 2).
- Variance check from commit 5 enforces <10% across two back-to-back Phase 6 ramps before any tag-verification run begins.

**Effort:** **2–3 dev-days** (revised from 1.5–2.5 per the +2 commits). Owner: dev (tester verifies).

**Cited as a precondition in every snowplow tag below.** No snowplow tag ships before Step −1 lands.

---

## §3 Three-pass measurement structure per tag (added 2026-05-09 mid-edit per Diego's just-in clarification; extended to FOUR passes 2026-05-09 late-edit per Revision 15 pprof addition)

**Diego's just-in ruling (2026-05-09 mid-edit):** Phase 6 of `e2e/bench/snowplow_test.py` is the **canonical SCALE=50K cluster-setup procedure**. It cleans the cluster and programmatically ramps to 50K compositions; convergence measurement (S6 mass-CREATE / S7 single-DELETE / S8 namespace-cascade) happens DURING the ramp. Phase 6 is **DESTRUCTIVE** — it cannot run mid-tag-verification once Pass-2 cells are being measured. This forces an explicit three-pass (now four-pass) structure per tag.

**The four passes per tag (binding from 2026-05-09 late-edit; Pass 4 added per Revision 15 pprof addition):**

| Pass | What it measures                                                                                                          | Source in `snowplow_test.py`                                          | Runtime    | Gates                                                                                                                                                          |
|------|---------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------|------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------|
| 1    | Cluster setup: clean + ramp to 50K. Captures S6 (mass-CREATE p99), S7 (single-DELETE p99), S8 (namespace-cascade p99) DURING the ramp. Destructive. | Phase 6 (per Step −1 commit 5)                                        | ~hours     | Informational at `0.30.0`–`0.30.7`. Architect-specified gating thresholds at `0.30.8+` (S7/S8 latency proxies for refresher health under churn).               |
| 2    | Steady-state UX on the 50K-ramped cluster: Chrome MCP cold + warm × 4 cells (admin × cyberjoker × cache=on/off).           | Phases 4+5 (with cyberjoker added per Step −1 commit 4)               | ~30–60 min | Pass-2 cold mix-weighted is the **primary scoring metric** (Diego's Row 7 cold target + north-star cold). Pass-2 warm mix-weighted is the warm-p50 north-star. |
| 3    | Per-mutation convergence: synthesize sparse CREATE/UPDATE/DELETE pulse (~50 events) on the steady-state 50K cluster. Per-event p99 visibility latency, per subject + per widget class. | NEW per Step −1 commit 6 (architect ruling: Phase 6 mutation-pulse extension OR new Phase 8 — see ruling below). | ~30 min    | **R4 binding gate (≤1000 ms mix-weighted from `0.30.8` onward).** Per-class binding from `0.30.11` onward (HOT ≤500 ms / WARM ≤2000 ms / COLD ≤10000 ms p99).   |
| 4    | Forensic snapshot at SCALE=50K idle, immediately after Pass 3: `/debug/pprof/goroutine?debug=2` and `/debug/pprof/heap`. Stored in `/tmp/snowplow-runs/<tag>/pprof_goroutine.txt` and `/tmp/snowplow-runs/<tag>/pprof_heap.pb.gz`. | NEW per Revision 15 (new Phase 9 in `snowplow_test.py`, OR equivalent forensic-snapshot step in the orchestrator). | ~5 min     | **Mandatory for every tag from `0.30.2` onward.** Informational — not a scoring gate; serves as forensic baseline for cross-tag diff (goroutine-class deltas, heap-class deltas). Falsifier: pprof endpoint returns 200 (not 404); for `0.30.3`+ tags, goroutine list shows the expected informer/reflector/StreamWatcher classes (or, for `0.30.3` specifically, ZERO such goroutines at idle since the factory is not instantiated). |

**Per-tag total verification runtime: 2.5–4 hours** (Pass 1 destructive setup once, Pass 2 + Pass 3 + Pass 4 in sequence on the resulting 50K cluster; Pass 4 adds ~5 min on top of the prior three-pass total). Total campaign verification: on the order of days minimum across 14 tags. **Acceptable per Diego's "minutes are OK" precedent for prewarm** — the verification budget is a one-time-per-tag deploy cost, not a customer-facing latency cost.

**Architect ruling on Pass 3 placement (Phase 6 sub-stage vs new Phase 8):** **Pass 3 is a NEW Phase 8** in `snowplow_test.py`, NOT a Phase 6 mutation-pulse extension. Rationale:

1. **Phase 6 is destructive cluster-setup** (cleans cluster, ramps from 0 → 50K). Folding Pass 3 in would couple the per-mutation measurement to every cluster setup, multiplying the destructive operation's runtime.
2. **Pass 3 must run on the steady-state cluster AFTER Pass 2** (Chrome MCP requires `uptime_at_capture ≥ 600 s` per `pm-team-operating-model-2026-05-07.md` §3; Pass-3 mutation pulse on a still-ramping cluster pollutes both the convergence p99 measurement AND any concurrent Pass-2 cell still in flight).
3. **Phase 8 isolation lets Pass 3 be re-run independently** — if a tag flakes Pass 3 only, re-running Phase 8 on the same 50K cluster (~30 min) is far cheaper than re-running Phase 6 (~hours).
4. **Phase 6 stays single-purpose** (cluster setup + S6/S7/S8 mass-stage convergence), per the audit-anti-pattern of "do not overload a single phase with multiple measurement responsibilities." Single-responsibility per phase preserves diagnostic clarity.

The architect's ruling is captured in commit 6's PR description; dev implements Pass 3 as `phase_8_per_mutation_convergence()` invoked AFTER Phase 5 by the harness orchestrator.

**Canonical ledger row JSON schema gains (binding, additive — NO existing columns removed):**

The canonical ledger row in `/tmp/browser_results.json` (consumed by `project_north_star_ledger.md`) gains the following sub-columns to capture the three-pass structure:

- **Pass 1 (Phase 6 mass-stage):**
  - `convergence_mass_s6_p99` — mass-CREATE latency p99 (50K ramp).
  - `convergence_mass_s7_p99` — single-DELETE latency p99.
  - `convergence_mass_s8_p99` — namespace-cascade DELETE latency p99.
- **Pass 3 (per-mutation, new Phase 8):**
  - `convergence_per_mutation_p99_mix` — **R4 binding (≤1000 ms mix-weighted from `0.30.8` onward).**
  - `convergence_per_class_hot_p99` — HOT-class subject p99 (R12 binding from `0.30.11`: ≤500 ms).
  - `convergence_per_class_warm_p99` — WARM-class subject p99 (R12 binding from `0.30.11`: ≤2000 ms).
  - `convergence_per_class_cold_p99` — COLD-class subject p99 (R12 binding from `0.30.11`: ≤10000 ms).
- **Pass 2 columns are unchanged** (already in `/tmp/browser_results.json` per Step −1 baseline + p99 fix): `cold_admin_ms`, `cold_cyber_ms`, `warm_p50_admin_ms`, `warm_p50_cyber_ms`, `warm_p99_admin_ms`, `warm_p99_cyber_ms` (and the `_off` cache-disabled variants).
- **Pass 4 (forensic snapshot, per Revision 15):**
  - `pprof_goroutine_path` — absolute path to the captured goroutine list (`/tmp/snowplow-runs/<tag>/pprof_goroutine.txt`).
  - `pprof_heap_path` — absolute path to the captured heap profile (`/tmp/snowplow-runs/<tag>/pprof_heap.pb.gz`).
  - `pprof_goroutine_count` — total goroutine count at snapshot time (mechanism-diagnostic, not gating).
  - `pprof_heap_inuse_bytes` — heap_alloc at snapshot time (mechanism-diagnostic, not gating).

The Revision-4 columns (`convergence_p50_ms_on/off`, `convergence_p99_ms_on/off`) defined earlier in this document are **superseded** by the seven new columns above. Step −1 commit 6 emits the new schema; commits 1–5 retain the existing column set untouched. Tester verifies schema compatibility at Step −1 PASS gate.

**Per-pass binding by tag-range (architect-specified; Pass 4 column added per Revision 15):**

| Tag-range          | Pass 1 (S6/S7/S8) gating       | Pass 2 (Chrome MCP)               | Pass 3 (per-mutation)                                  | Pass 4 (pprof forensic)                                |
|--------------------|--------------------------------|-----------------------------------|--------------------------------------------------------|--------------------------------------------------------|
| `0.30.0`–`0.30.1`  | informational                  | scoring (cold + warm)             | informational                                          | **N/A — pprof not present in binary** (historical superseded baselines) |
| `0.30.2`–`0.30.3`  | informational                  | scoring (cold + warm)             | informational                                          | **mandatory; informational** (forensic-canonical baseline; goroutine + heap snapshot stored) |
| `0.30.4`–`0.30.7`  | informational                  | scoring (cold + warm)             | informational                                          | mandatory; informational (cross-tag forensic diff)     |
| `0.30.8`           | informational                  | scoring                           | **R4 binding (≤1000 ms mix-weighted)** + STOP gate     | mandatory; informational                               |
| `0.30.9`–`0.30.10` | informational                  | scoring                           | R4 binding                                             | mandatory; informational                               |
| `0.30.11`+         | informational                  | scoring                           | R4 binding + per-class binding (R12)                   | mandatory; informational                               |
| `0.30.12`–`0.30.13`| informational                  | scoring                           | R4 + R12 binding                                       | mandatory; informational                               |

---

## Tag `0.30.0` — Step 0: cache=off baseline (upstream 0.20.5 code)

### Branch base
`0.20.5` (`6375c9a`). Plus a single-commit `client-go` bump from `v0.33.0` to `v0.35.3` for compilation against current API surface.

### What's implemented
- **Code changes: NONE beyond `client-go` bump.**
- `go.mod`: `k8s.io/client-go v0.33.0 → v0.35.3` (+ transitive bumps).
- Possible adapter shims if 1–2 call sites broke between `v0.33` and `v0.35` (estimate: ≤20 LoC).
- **No new files, no new functions, no new types, no env vars, no chart values.**

This tag is a measurement run on the `0.20.5`-shape codebase. Its purpose is to establish the floor.

### Chart values (binding constraint 4)

The current `braghettos/snowplow-chart/chart/values.yaml` (audited 2026-05-09) sets keys that the `0.20.5` binary does NOT consume. The `chart-0.30.0` deliverable strips them all.

**SET (kept verbatim — 0.20.5 binary consumes these or they are k8s-side):**
- `replicaCount`, `image.repository`, `image.pullPolicy`, `image.tag` (RETAGGED to `0.30.0`).
- `imagePullSecrets`, `nameOverride`, `fullnameOverride`.
- `serviceAccount.{create,automount,annotations,name}`.
- `podAnnotations`, `podLabels`, `podSecurityContext`, `securityContext`.
- `service.{type,port}` (port stays 8081; 0.20.5 binary serves on 8081).
- `ingress.*` (k8s-side).
- `resources.{limits,requests}` — 8Gi/4Gi sizing PRESERVED; cgroup-side, not consumed by binary.
- `nodeSelector`, `tolerations`, `affinity`.
- `autoscaling.*` (k8s-side, not snowplow-side).
- `args: []`.
- `volumeMounts.jq-modules` + `volumes.jq-modules` (chart-managed; jq-custom-modules ConfigMap exists at 0.20.5).
- `extraEnvFrom: snowplow-api-override` (consumed via envFrom; agnostic).
- `jwtSignKeySecretName: jwt-sign-key` (consumed by 0.20.5 authentication).
- `env.DEBUG`, `env.BLIZZARD`, `env.SKIP`, `env.JQ_MODULES_PATH` (all 0.20.5-known toggles).
- `env.GOMEMLIMIT`, `env.GOGC` (Go runtime knobs; ALL Go binaries respect them — KEEP for cgroup safety even though no informer is loaded).

**REMOVED for `chart-0.30.0` (0.20.5 binary cannot consume):**
- `progressDeadlineSeconds: 1200` — k8s consumes this, but the comment block references Q-PREWARM-R2 which doesn't exist at 0.20.5. Decision: keep `600` (default), strip the Q-PREWARM-R2 narrative.
- `probePort: 8082` — opens a SECOND http listener; 0.20.5 binary has no second listener. REMOVE entirely.
- `startupProbe.httpGet.port: probe` — `probe` named-port doesn't exist at 0.20.5. REWRITE to `port: http`.
- `livenessProbe.httpGet.port: probe` — same; REWRITE to `port: http`.
- `readinessProbe.httpGet.port: probe` — same; REWRITE to `port: http`.
- `volumes.cache-warmup-config` (`/etc/snowplow/cache-warmup.yaml`) — read only by post-0.20.5 cache code. REMOVE.
- `volumes.frontend-config` (`/etc/frontend-config`) — read only by `WarmL1FromEntryPoints` (post-0.20.5). REMOVE.
- `volumeMounts.cache-warmup-config`, `volumeMounts.frontend-config` — REMOVE.
- `initContainers: []` — comment references "Redis sidecar removed (v0.25.266+)"; at 0.20.5 there WAS a redis sidecar. Re-introducing a redis sidecar is OUT OF SCOPE (per `project_redis_removal.md` redis is fully removed in this clean-slate; 0.20.5 binary likely runs without redis fine if `CACHE_ENABLED` infrastructure absent). KEEP empty.
- `env.CACHE_ENABLED: "true"` — REMOVE entirely (no CACHE_ENABLED plumbing at 0.20.5; per Step 1 we add it).
- `env.OTEL_ENABLED: "true"`, `env.OTEL_EXPORTER_OTLP_ENDPOINT`, `env.OTEL_SERVICE_NAME` — REMOVE (no OpenTelemetry at 0.20.5; PM amendment 5 caps diagnostics).
- `env.PREWARM_MODE`, `env.PREWARM_WORKERS`, `env.PREWARM_QUEUE` — REMOVE (Q-PREWARM-R2R5 PR-B post-0.20.5).
- `templates/configmap.yaml` `PROBE_PORT` rendering — strip the `{{- if .Values.probePort }}` block.
- `templates/deployment.yaml` named-port `probe` declaration — strip.

**ADDED for `chart-0.30.0` (vs current chart):** none. Strip-only.

**Stripped-chart deliverable name:** `chart-0.30.0` published to `oci://ghcr.io/braghettos/snowplow-chart` chart version `0.30.0`. Contains: `Chart.yaml` (appVersion `0.30.0`, chart version `0.30.0`), stripped `values.yaml`, stripped `templates/{configmap,deployment}.yaml`, plus `templates/{service,serviceaccount,clusterrole,clusterrolebinding,ingress,hpa,endpoint,configmap.jq-custom-modules}.yaml` UNCHANGED.

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.0` (userAccessFilter-stripped).** Per Revision 5, snowplow dev (portal repo authority) produces this artifact; no cross-team coordination latency.

The current `braghettos/portal/blueprint/templates/` ships 6 of 7 RestActions with `userAccessFilter` (audit 2026-05-09):
- `restaction.blueprints-list.yaml`, `restaction.blueprints-panels.yaml`, `restaction.compositions-panels.yaml`, `restaction.compositions-get-ns-and-crd.yaml` (×2 userAccessFilter blocks), `restaction.all-routes.yaml`, `restaction.sidebar-nav-menu-items.yaml`.
- `restaction.compositions-list.yaml` does NOT use userAccessFilter (portable as-is).

**RestActions that must NOT be present at this tag: any with `spec.api[*].userAccessFilter`.** The 0.20.5 RestAction CustomResourceDefinition doesn't recognize this field; depending on validation strictness either (a) the apply silently drops the field and snowplow falls back to per-user dispatch (works, slow), or (b) the apply rejects with `unknown field`. Either way, behaviour is wrong for the bench scenario.

**Note (Revision 5):** snowplow dev (portal repo authority) produces `portal-0.30.0` in parallel with snowplow Step 0 work. No cross-team coordination.

### Expected results

#### Customer-facing Chrome MCP (the scoring metric)
- **At SCALE=5000:**
  - Admin Compositions cold: TBD; hypothesis 6 000–12 000 ms (apiserver-bound).
  - Cyberjoker Compositions cold: TBD; hypothesis 4 000–8 000 ms.
  - Mix-weighted cold (0.95·cyber + 0.05·admin): TBD; hypothesis 4 100–8 200 ms.
  - Mix-weighted warm-p50: TBD; hypothesis 3 500–6 500 ms.
- **At SCALE=50000:** per Diego addition #1, cache=off is EXPECTED to be broken or near-broken.
  - Admin Compositions cold: TBD; hypothesis 30 000–120 000 ms or timeout.
  - Cyberjoker Compositions cold: TBD; hypothesis 20 000–60 000 ms or timeout.
  - Mix-weighted cold: TBD; hypothesis ≥20 000 ms or timeout.
  - **Note any expected breakage:** if Compositions cold > 60 s mix-weighted on cyberjoker, the escape hatch is theoretical at 50 K (PM amendment 3, Ship 2 FAIL gate).

#### Convergence (Revision 4 — north-star "1 s fresh" component)
- **At SCALE=5000 cache=off:** convergence_p50 hypothesis 2 000–6 000 ms (each mutation re-runs RestActions through apiserver). convergence_p99 hypothesis 5 000–15 000 ms.
- **At SCALE=50000 cache=off:** convergence_p50 hypothesis 10 000–60 000 ms or timeout; convergence_p99 likely timeout. cache=off is the floor; no expectation of meeting <1 s p99 here.
- **Cache=on convergence is N/A at this tag** (cache=on activates at `0.30.4`).

#### Mechanism-level (diagnostic, not scoring)
- Memory: heap_alloc TBD; heap_sys TBD; RSS TBD. Hypothesis: cache=off is LOW memory (apiserver = storage). Probable RSS at 50K: 200–500 MB.
- Pod restart count over 30-min: 0 (no informers).
- Time-to-Ready: <10 s; no `WaitForCacheSync`.
- StreamWatcher.receive cum %: 0 % (no watches).

#### Code-path falsifier
N/A at 0.20.5 — there is no `cache.disabled=true` log line at 0.20.5 (no cache plumbing). The falsifier becomes available at Tag 0.30.1.

#### Pre-flight falsifier
N/A — this IS the floor. Step −1 (matrix harness) is the only thing that gates Step 0.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational at this tag. Hypothesis: S6 dominated by apiserver write throughput (no informer to absorb fan-out), S7/S8 = TBD baseline floor.
- **Pass 2 (Chrome MCP):** see "Customer-facing Chrome MCP" above. This is the scoring metric here.
- **Pass 3 (per-mutation):** informational. Cache=off floor; no <1000 ms expectation. Captures `convergence_per_mutation_p99_mix` baseline for delta tracking at later tags.

### What this tag does NOT do
- Does NOT add informers, resolved-output cache, prewarm, OpenTelemetry, pprof, or any cache mechanism.
- Does NOT change resolver code paths.
- Does NOT introduce `CACHE_ENABLED` plumbing (added in Step 1).
- Does NOT modify chart manifests beyond the stripping listed above.
- Does NOT touch the frontend.

### Risks
- **Risk: `client-go` bump breaks unexpected call sites.** Mitigation: dev runs `go build ./...` + existing test suite before tagging. If >50 LoC of shims required, escalate to PM.
- **Risk: cache=off at 50 K times out the harness.** Mitigation: harness has explicit timeout + `failure_mode_data` capture per `feedback_failure_mode_data.md`. Failure data IS the Step 0 result.
- **Risk: cache=off renders a partial page.** Mitigation: harness captures both per-widget render time AND overall page-load. Partial-page reported as "cache=off correctness regression at 50 K."
- **Risk: chart-strip misses a value the 0.20.5 binary actively rejects.** Mitigation: `chart-0.30.0` smoke test = boot + serve `/health` for 5 minutes; any environment-rejection log surfaces here. ROLLBACK if smoke fails.
- **Risk: `portal-0.30.0` lags chart/binary readiness.** Mitigation: per Revision 5, snowplow team owns the portal repo; no external dependency.
- **Convergence-related risk:** with cache=off, every mutation re-runs every RestAction through apiserver. At 50 K, convergence is dominated by apiserver throughput, not snowplow logic. If p99 timeouts dominate, the failure-mode data (per `feedback_failure_mode_data.md`) is the result.
- **If admin cold at 5 K is < 1 s:** cache provides no benefit even at 5 K; STOP and reassess (PM amendment Ship 2 FAIL-FAST).

### Estimated LoC and effort
- Snowplow LoC: ~20 (client-go shims, if any). Most likely 0–5.
- Chart LoC: ~80 (strip-only edits to `values.yaml`, `templates/configmap.yaml`, `templates/deployment.yaml`).
- Effort: **0.5 sprint** (3 days). Most of the time is bench-harness execution (Step −1).

---

## Tag `0.30.1` — Step 1a: cluster-wide informer factory + direct read API (cache=off remains default)

### Branch base
`0.30.0`. Branch name: `ship-0.30.1-informer-no-cache-on`.

### Revision 1 split rationale (recap)
This tag introduces the informer plumbing only. **Cache=on remains gated off-by-default** because Diego's rule 7 (zero `SubjectAccessReview` to apiserver in cache=on) requires the Role-Based Access Control informer + `EvaluateRBAC` to be in place before flipping. That work lands at `0.30.4`. Shipping it here would either (a) violate rule 7 (cache=on falls back to `SubjectAccessReview`) or (b) blow the per-tag LoC cap (~750–900 LoC). Splitting respects both.

### What's implemented
- **`internal/cache/cache.go` (NEW, ~30 LoC).** Defines `CACHE_ENABLED` env var (`Disabled() bool`).
- **`internal/cache/watcher.go` (NEW, ~300 LoC).** Cluster-wide `dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, NamespaceAll, tweak{Limit:500})`. Public methods: `NewResourceWatcher`, `AddResourceType`, `GetObject`, `ListObjects`, `WaitForCacheSync`, `Stop`.
- **`internal/cache/watcher_test.go` (NEW, ~80 LoC).** Lazy AddResourceType idempotent; GetObject hits store; ListObjects respects namespace index; cache=off returns nil watcher.
- **`internal/objects/get.go` (MODIFY, ~40 LoC).** Branch on `cache.Disabled()`.
- **`internal/dynamic/list.go` (MODIFY, ~40 LoC).** Same branch.
- **`internal/resolvers/restactions/api/resolve.go` (MODIFY, ~30 LoC).** Pass `*ResourceWatcher` (or nil) through.
- **`main.go` (MODIFY, ~50 LoC).** Wire `CACHE_ENABLED`; if true, construct watcher + `factory.Start(ctx.Done())` + shutdown hook. **However**: at this tag, even when `CACHE_ENABLED=true` is set, the informer is constructed but the resolver does NOT route through it for `EvaluateRBAC` decisions; `SubjectAccessReview` to apiserver still fires. This is a deliberate temporary state — flipped to fully cache=on at `0.30.4`. To enforce rule 7, **the chart default for this tag is `CACHE_ENABLED=false`** and a startup log warns if it is set true: `cache=on requires 0.30.4+; reverting to cache=off`.
- **Configuration additions:** env `CACHE_ENABLED` (default `false`).

### Chart values (binding constraint 4)

**Vs `chart-0.30.0`:**
- **ADDED:** `env.CACHE_ENABLED: "false"` (default per `project_redis_removal.md` and per the Revision 1 split — cache=on activates at `0.30.4`).
- **REMOVED:** none.
- **UNCHANGED:** all other keys.

**Stripped-chart deliverable:** `chart-0.30.1` (= `chart-0.30.0` + `env.CACHE_ENABLED`).

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.0` (still userAccessFilter-stripped).** No portal work at this tag. Per Revision 5, snowplow team owns portal repo authority; no coordination latency exists by definition.

### Expected results

This is a no-op tag for customer-facing Chrome MCP measurements (cache=off is still default). Its purpose: prove the informer plumbing compiles, boots, and the chart adds the toggle correctly. Mechanism-level metrics (heap, pod restart, time-to-Ready when `CACHE_ENABLED=true` is set in a smoke env) gate the next tag.

#### Customer-facing Chrome MCP
- **At SCALE=5000 + SCALE=50000 (cache=off default):** ±50 ms vs `0.30.0`. Regression gate: ≤+10 % vs `0.30.0` cold/warm.
- **Cache=on numbers are N/A at this tag** (informer constructed but not routed for `EvaluateRBAC`; per Revision 1 split, cache=on activates at `0.30.4`). Smoke env may exercise `CACHE_ENABLED=true` for boot validation; numbers are diagnostic-only.

#### Convergence (Revision 4)
- **Cache=off (default at this tag):** unchanged from `0.30.0`. convergence_p99 still timeout-likely at SCALE=50000. Acceptable — cache=on is the path to <1 s p99 and lands at `0.30.4`.

#### Mechanism-level (smoke env with `CACHE_ENABLED=true` for plumbing validation)
- Memory at 50 K: heap_alloc 1.5–3 GB; RSS 1.5–3.5 GB (informer present, store populated, but resolver not routing).
- Pod restart 30-min: 0. Watch for OOM during initial LIST.
- Time-to-Ready: 30–90 s at 50 K; <30 s at 5 K.
- StreamWatcher.receive cum %: 60–80 %.
- Startup log: `cache.plumbing_present=true cache.routed=false (rbac informer not yet wired; activates at 0.30.4)`.

#### Code-path falsifier
At startup with `CACHE_ENABLED=false` (the default): `cache.disabled=true`. With `CACHE_ENABLED=true` (smoke only): `cache.plumbing_present=true cache.routed=false`.

#### Pre-flight falsifier
0.30.0 row's mix-weighted cold (at SCALE=5000 and SCALE=50000) must be > 2 s. If 0.30.0 cold is < 1 s, the cache provides no benefit and the campaign is moot.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; ±10% vs `0.30.0` (no informer routing yet — informer constructed but resolver doesn't consume it).
- **Pass 2 (Chrome MCP):** ±50 ms vs `0.30.0` (cache=off default).
- **Pass 3 (per-mutation):** informational; unchanged vs `0.30.0`. Cache=on path inactive at this tag.

### What this tag does NOT do
- Does NOT activate cache=on routing (Tag `0.30.4`).
- Does NOT add Role-Based Access Control informer or `EvaluateRBAC` (Tag `0.30.4`).
- Does NOT add SetTransform strip (`0.30.5`), eager registration (`0.30.6`), resolved-output cache (`0.30.7`/`0.30.8`), userAccessFilter (`0.30.9`), permission-check cache (`0.30.10`), activity classification (`0.30.11`), targeted prewarm (`0.30.12`), paginated Warmer (`0.30.13`).

### Risks
- **Risk: smoke env operator flips `CACHE_ENABLED=true` and expects cache=on.** Mitigation: startup warning log; chart default stays `false`.
- **Risk: lazy AddResourceType storm on first navigation at 50 K (in smoke env).** Mitigation: lazy AddResourceType registers ONE resource type at a time, throttled by request arrival rate. Diagnostic only at this tag; the storm risk realizes at `0.30.4` when cache=on flips.
- **Risk: cache=off correctness break.** Mitigation: PM amendment 1 — CI lint, regression test (`CACHE_ENABLED=false` boot, goroutine count delta <5), correctness check on Compositions page.
- **Risk: chart-0.30.1 ships `env.CACHE_ENABLED` to a 0.20.5 image accidentally.** Mitigation: chart smoke test boot-and-fail-fast on unknown environment-variable consumption; image tag `0.30.1` MUST be set in chart values.

### Estimated LoC and effort
- Code: ~400 production + ~80 test = ~480 LoC.
- Chart: ~5 LoC (one env key).
- Effort: **1 sprint**.

---

## Tag `0.30.2` — Step 0p: cache=off baseline + `net/http/pprof` side-effect import (Revision 15)

### Branch base
`0.30.0` (= `0.20.5` `6375c9a` + `client-go` bump). Branch name: `ship-0.30.2-pprof-baseline`. Single-commit branch.

### What's implemented
- **Code changes: identical to `0.30.0` PLUS a one-line side-effect import.**
- `main.go`: add `import _ "net/http/pprof"` (and ensure `net/http`'s `DefaultServeMux` is the mux backing the existing `:8081` listener; if a custom mux is used, register the pprof handlers explicitly under `/debug/pprof/`).
- Estimate: **~2 LoC production + 0 test**.
- No new chart values, no new env vars, no new types, no new files.

This tag is `0.30.0` semantics with `/debug/pprof/*` exposed. Its purpose is to establish the forensic-canonical cache=off floor. Subsequent tags can be diffed (goroutine-class deltas, heap-class deltas) against this snapshot.

### Chart values (binding constraint 4)

**Vs `chart-0.30.0`:** UNCHANGED. The `net/http/pprof` side-effect import binds to the existing `:8081` listener; no new port to open in the chart Service.

**Stripped-chart deliverable:** `chart-0.30.2` (= `chart-0.30.0`, with `image.tag` retagged to `0.30.2`).

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.0`.** Unchanged from `0.30.0`. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP (the scoring metric)
- **At SCALE=5000 + SCALE=50000:** numbers IDENTICAL to `0.30.0` within measurement noise (±50 ms). The pprof side-effect import contributes zero CPU at idle (handlers are registered but not invoked unless a client hits `/debug/pprof/*`).

#### Convergence (Revision 4)
- Identical to `0.30.0` (cache=off; no informer; no resolved-output cache). N/A for <1 s p99 expectation.

#### Mechanism-level (diagnostic, not scoring)
- Memory, pod restart, time-to-Ready: identical to `0.30.0`.
- **NEW:** `/debug/pprof/goroutine`, `/debug/pprof/heap`, `/debug/pprof/profile`, `/debug/pprof/threadcreate`, `/debug/pprof/block`, `/debug/pprof/mutex`, `/debug/pprof/allocs`, `/debug/pprof/cmdline`, `/debug/pprof/trace`, `/debug/pprof/symbol`, `/debug/pprof/`. All return HTTP 200 on the existing `:8081` listener.

#### Code-path falsifier
**`curl -s http://<pod>:8081/debug/pprof/goroutine?debug=2` returns 200 with a goroutine list (not 404).** Falsifier if 404: the pprof side-effect import did not bind to the listener (likely because a custom mux is used) — fix by registering pprof handlers explicitly.

#### Pre-flight falsifier
`0.30.0` measurement row exists in the ledger AND the goroutine baseline is desired for forensic comparison from `0.30.2` onward.

#### Three/four-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; identical to `0.30.0` (±0–5%).
- **Pass 2 (Chrome MCP):** identical to `0.30.0` (±50 ms).
- **Pass 3 (per-mutation):** informational; identical to `0.30.0` (cache=off floor).
- **Pass 4 (pprof forensic) — first activation of this pass:** goroutine list captured; expected classes include `main`, `http.(*Server).Serve`, `http.(*conn).serve`, runtime goroutines (`runtime.gopark`, GC, etc.). **Expected: ZERO informer-related goroutines** (`reflector.ListAndWatch`, `StreamWatcher.receive`, `cache.(*Reflector).watchHandler`, `dynamicinformer.*`) because no informer is wired at `0.30.2`. Heap snapshot captured for cross-tag diff against later cache=on tags.

### What this tag does NOT do
- Does NOT add informers, resolved-output cache, prewarm, or any cache mechanism beyond the pprof side-effect import.
- Does NOT change resolver code paths or chart values (beyond image tag).

### Risks
- **Risk: pprof handlers leak sensitive memory state to anyone who can reach `:8081`.** Mitigation: `:8081` is service-scope inside the cluster; ingress is not exposed externally. Acceptable per Revision 15 (forensic capability mandatory).
- **Risk: pprof side-effect import increases binary size by ~1–2 MB.** Mitigation: acceptable; image-pull cost is one-time.
- **Risk: side-effect import binds to a custom mux that the binary does NOT serve on `:8081`.** Mitigation: explicit smoke test against `curl /debug/pprof/goroutine?debug=2` post-deploy; ROLLBACK if 404.

### Estimated LoC and effort
- Snowplow LoC: ~2 (one blank import line; possibly +5 if explicit handler registration is required).
- Chart LoC: ~1 (image tag bump).
- Effort: **<1 dev-day** (build, push, deploy, smoke). Ships together with `0.30.3` as a paired ~30 min total dev work.

---

## Tag `0.30.3` — Step 1ap: cluster-wide informer factory + direct read API (cache=off remains default) + pprof (Revision 15)

### Branch base
`0.30.1` (= `0.30.0` + informer plumbing dormant under `cache.Disabled()` gate). Branch name: `ship-0.30.3-pprof-informer-plumbing`. Single-commit branch.

### What's implemented
- **Code changes: identical to `0.30.1` PLUS a one-line side-effect import.**
- `main.go`: add `import _ "net/http/pprof"` (same as `0.30.2`).
- Everything else identical to `0.30.1`: informer plumbing constructed but gated off by `cache.Disabled()`; chart default `CACHE_ENABLED=false`; factory NOT instantiated at idle.
- Estimate: **~2 LoC production + 0 test** beyond what `0.30.1` already shipped.

This tag is `0.30.1` semantics with `/debug/pprof/*` exposed. Its purpose is to establish the forensic-canonical pre-cache-on baseline. Snapshot here is the reference for "informer plumbing exists but is dormant" — at idle, the goroutine list should show ZERO informer/reflector/StreamWatcher goroutines (because the factory is not started until `CACHE_ENABLED=true` at `0.30.4`).

### Chart values (binding constraint 4)

**Vs `chart-0.30.1`:** UNCHANGED. The `net/http/pprof` side-effect import binds to the existing `:8081` listener; no new port to open.

**Stripped-chart deliverable:** `chart-0.30.3` (= `chart-0.30.1`, with `image.tag` retagged to `0.30.3`).

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.0`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000 + SCALE=50000 (cache=off default):** numbers IDENTICAL to `0.30.1` within measurement noise (±50 ms).

#### Convergence (Revision 4)
- Identical to `0.30.1`. cache=on path not active.

#### Mechanism-level
- Heap, pod restart, time-to-Ready: identical to `0.30.1`.
- `/debug/pprof/*` endpoints present on `:8081` (same as `0.30.2`).
- Startup log: `cache.disabled=true` (the original `0.30.1` falsifier, unchanged).

#### Code-path falsifier
At startup (with default `CACHE_ENABLED=false`): `cache.disabled=true`. AND `curl -s :8081/debug/pprof/goroutine?debug=2` returns 200. AND the goroutine list shows ZERO of: `reflector.ListAndWatch`, `StreamWatcher.receive`, `dynamicinformer.*`, `cache.(*Reflector).watchHandler` — because the factory has not been instantiated.

#### Pre-flight falsifier
`0.30.2` Pass 4 snapshot exists in the ledger; goroutine list does NOT include informer classes.

#### Three/four-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; ±10% vs `0.30.1` (no functional delta).
- **Pass 2 (Chrome MCP):** ±50 ms vs `0.30.1` (cache=off default).
- **Pass 3 (per-mutation):** informational; identical to `0.30.1`.
- **Pass 4 (pprof forensic):** goroutine list at idle shows ZERO informer/reflector/StreamWatcher goroutines (informer factory not instantiated under `cache.Disabled()` gate). Heap snapshot establishes the dormant-plumbing memory baseline; cross-tag diff against `0.30.4` (cache=on activation) attributes informer cost cleanly.

### What this tag does NOT do
- Does NOT activate cache=on routing (Tag `0.30.4`).
- Does NOT add Role-Based Access Control informer, eager registration, resolved-output cache, etc.
- Does NOT change behaviour vs `0.30.1` in any way beyond exposing pprof.

### Risks
- **Risk: same as `0.30.2`** (pprof exposure, binary size, custom-mux mismatch). Same mitigations.

### Estimated LoC and effort
- Snowplow LoC: ~2.
- Chart LoC: ~1.
- Effort: **<1 dev-day**. Ships paired with `0.30.2`.

---

## Tag `0.30.4` — Step 1b: Role-Based Access Control informer + `EvaluateRBAC` + cache=on activation

### Branch base
`0.30.3`. Branch name: `ship-0.30.4-rbac-informer-cache-on`.

### Revision 1 + Revision 2 — what makes this tag distinct
- **Revision 1 (binding):** in cache=on mode, snowplow MUST satisfy ALL Role-Based Access Control checks via in-process evaluation against informer-cached `Role`/`RoleBinding`/`ClusterRole`/`ClusterRoleBinding` rules. Zero `SubjectAccessReview` calls to apiserver in cache=on mode. This tag lands the Role-Based Access Control informer + `EvaluateRBAC` and atomically flips `CACHE_ENABLED` default to `true`.
- **Revision 2 (binding):** `EvaluateRBAC` fires on **every** RestAction dispatch in cache=on mode, regardless of whether the RestAction uses `userAccessFilter`. The userAccessFilter mechanism (introduced at `0.30.9`) only changes WHO dispatches (snowplow-ServiceAccount cluster-wide-read vs per-user-credentials), NOT whether Role-Based Access Control is enforced. Both paths use the same `EvaluateRBAC` function for the actual permission check.

### What's implemented
- **`internal/cache/watcher.go` (MODIFY, ~50 LoC).** Add the four Role-Based Access Control resource types (`Role`, `RoleBinding`, `ClusterRole`, `ClusterRoleBinding`) to the eager initial set registered by `NewResourceWatcher`. Bound their initial LIST and start watch.
- **`internal/rbac/evaluate.go` (NEW, ~250 LoC).** `EvaluateRBAC(ctx, user, groups, verb, group, resource, namespace) (bool, error)`:
  - Walks informer-cached `RoleBinding`/`ClusterRoleBinding` for matching subjects (user + groups + ServiceAccount as applicable).
  - For each match, resolves bound `Role`/`ClusterRole`, walks `rules`, returns true on first matching rule (Kubernetes RBAC semantics: any-rule-permits = allow).
  - Implements the same wildcard semantics as Kubernetes apiserver (`*` for verbs/resources/groups; namespace scope; non-resource URLs out of scope at this tag).
  - Cache=off path: falls through to `SubjectAccessReview` (correctness baseline).
- **`internal/rbac/evaluate_test.go` (NEW, ~150 LoC).** Coverage:
  - Allow-by-RoleBinding.
  - Allow-by-ClusterRoleBinding.
  - Deny when no binding matches.
  - Wildcard verb / resource / group.
  - Group-membership match.
  - ServiceAccount subject match.
  - Cache=off path uses `SubjectAccessReview` (mock apiserver).
  - **Cache=on path NEVER calls `SubjectAccessReview`** — assertion via mock apiserver call counter (== 0).
- **`internal/handlers/dispatchers/restactions.go` (MODIFY, ~40 LoC).** Per Revision 2: every RestAction dispatch (cache=on path) calls `EvaluateRBAC` BEFORE resolving — no exception for non-userAccessFilter RestActions. Hard-fail with 403 on deny.
- **`internal/handlers/dispatchers/widgets.go` (MODIFY, ~30 LoC).** Same — `EvaluateRBAC` on every dispatch (cache=on path).
- **`main.go` (MODIFY, ~30 LoC).** Flip default of `CACHE_ENABLED` to `true` (chart-side change in `chart-0.30.4`). Add startup log: `rbac.informer_started=true rbac.evaluate_path=in-process subject_access_review_calls_in_cache_on_path=banned`. Pprof endpoints (inherited from `0.30.2`/`0.30.3`) continue to be exposed on `:8081`.
- **`internal/cache/watcher_test.go` (MODIFY, ~30 LoC).** Add Role-Based Access Control resource-type registration + sync coverage.

### Chart values (binding constraint 4)
**Vs `chart-0.30.3`:**
- **CHANGED:** `env.CACHE_ENABLED: "false"` → `env.CACHE_ENABLED: "true"` (atomic activation per Revision 1).
- **ADDED:** none.
- **REMOVED:** none.
- **UNCHANGED:** all other keys (pprof exposure inherited from `chart-0.30.3`).

**Stripped-chart deliverable:** `chart-0.30.4` (= `chart-0.30.3` with `CACHE_ENABLED` flipped to `true`).

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.0` (still userAccessFilter-stripped).** userAccessFilter still not implemented in snowplow code at this tag; portal RestActions still must NOT use `userAccessFilter`. The cache=on path with informer reads correct results for any RestAction that doesn't use userAccessFilter — narrow-RBAC users pay full per-user-token-dispatch cost (cyberjoker still slow, fixed at `0.30.9`). **Per Revision 2: even non-userAccessFilter RestActions are gated by `EvaluateRBAC` on every dispatch.**

### Expected results

#### Customer-facing Chrome MCP (cache=on now active)
- **At SCALE=5000:**
  - Admin cold: 50–70 % reduction vs `0.30.0`. Range: 1 800–6 000 ms.
  - Admin warm-p50: 60–80 % reduction. Range: 1 000–4 000 ms.
  - Cyberjoker cold: 40–60 % reduction. Range: 1 600–4 800 ms.
  - Cyberjoker warm-p50: 50–70 % reduction. Range: 1 500–4 000 ms.
  - Mix-weighted cold: 1 600–4 800 ms.
  - Mix-weighted warm-p50: 1 500–4 000 ms.
  - cache=off path unchanged (regression gate: cache=off cold/warm must be ≤+10 % vs `0.30.0`).
- **At SCALE=50000:**
  - Admin cold (cache=on): 5 000–12 000 ms.
  - Cyberjoker cold (cache=on): 4 000–10 000 ms.
  - Mix-weighted cold (cache=on): 4 050–10 100 ms. **Should be on credible trajectory toward Row 7's 4 100 ms but not there yet** (Steps 3 + 5 close the gap).
  - cache=off cold at 50 K: per Diego #1 expected broken; "cache=on > cache=off" gate does NOT apply at 50 K.

#### Convergence (Revision 4)
- **At SCALE=5000 cache=on:** convergence_p50 hypothesis 1 500–3 500 ms (mutations propagate through informer watch ~100 ms; resolver cost dominates). convergence_p99 hypothesis 4 000–8 000 ms. **Not yet <1 s p99**; the resolved-output cache scaffold (`0.30.7`) + dependency tracking (`0.30.8`) close most of this; userAccessFilter (`0.30.9`) reduces narrow-RBAC fan-out.
- **At SCALE=50000 cache=on:** convergence_p50 hypothesis 3 000–8 000 ms; convergence_p99 hypothesis 8 000–20 000 ms. Far from <1 s p99 target. **Trajectory only.**
- **Cache=off:** unchanged from `0.30.0`. Likely timeout at 50 K.

#### Mechanism-level
- Memory cache=on at 50 K: heap_alloc 1.5–3 GB; RSS 1.5–3.5 GB (Role-Based Access Control informer adds ~100–500 MB depending on RoleBinding count).
- Memory cache=off at 50 K: <500 MB (unchanged).
- Pod restart 30-min: 0. Watch for OOM during initial LIST.
- Time-to-Ready cache=on at 50 K: 40–100 s (slightly higher than `0.30.1`'s 30–90 s because Role-Based Access Control resource types add to the WaitForCacheSync set). At 5 K: <30 s.
- StreamWatcher.receive cum %: 60–80 %.
- watch_events at 50 K: ~50 K initial Adds; steady ~10–100/sec.
- **`SubjectAccessReview` call rate in cache=on path: 0/sec.** (Falsifier — see code-path falsifier below.)

#### Code-path falsifier
INFO log per `/call`: `informer.GetStore.hits=N misses=M resource_types=...`. Hits must dominate (>50 %).
At startup: `cache.disabled=false informer_factory_started resource_types_registered=K rbac.evaluate_path=in-process`.
**Hard correctness gate (Revision 1, binding):** mock-apiserver test asserts `SubjectAccessReview` call counter = 0 across full bench run with `CACHE_ENABLED=true`. Non-zero = ROLLBACK.

#### Pre-flight falsifier
`0.30.1` smoke env's `CACHE_ENABLED=true` mechanism-level numbers (heap, time-to-Ready) must be within prediction ranges. `0.30.0` cold > 2 s.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational. First tag where informer absorbs CREATE/DELETE fan-out; S6 hypothesis stays apiserver-bound, S7/S8 reflect informer lag (no refresher yet — refresher lands at `0.30.8`).
- **Pass 2 (Chrome MCP):** **first scoring tag** (cache=on now active). See "Customer-facing Chrome MCP (cache=on now active)" above.
- **Pass 3 (per-mutation):** informational; hypothesis 8 000–20 000 ms p99 (intermediate trajectory only — no refresher; `convergence_per_mutation_p99_mix` is time-to-live-bound). R4 binding gate does NOT yet apply.

### What this tag does NOT do
- Does NOT add SetTransform strip (`0.30.5`), eager registration (`0.30.6`), resolved-output cache (`0.30.7`/`0.30.8`), userAccessFilter (`0.30.9`), permission-check cache (`0.30.10`), activity classification (`0.30.11`), targeted prewarm (`0.30.12`), paginated Warmer (`0.30.13`).
- Does NOT add prewarm, cohort-prewarm, OpenTelemetry, pprof, `/metrics/runtime`.
- Does NOT cache Role-Based Access Control evaluation results (the permission-check cache lands at `0.30.10` and amplifies this tag's gains).

### Risks
- **Risk (Revision 1, primary): `EvaluateRBAC` returns wrong allow/deny vs apiserver.** Mitigation: shadow-mode test for one full bench run — every cache=on `EvaluateRBAC` decision is double-checked against `SubjectAccessReview` and disagreements logged at WARN; if disagreement rate >0.01 %, ROLLBACK. (Shadow mode is one-shot; once validated, the rule-7 ban applies.)
- **Risk: lazy AddResourceType storm on first narrow-RBAC navigation at 50 K.** First page triggers `AddResourceType(resource_type_1)`, blocks for `WaitForCacheSync` (~2–5 s/resource type). Mitigation: cyberjoker first-nav cold will be slow; Step 3 (`0.30.6`, eager registration) fixes. Falsifier: log `lazy-AddResourceType: resource_type=X wait_ms=Y` at request time. If sum > 5 s, Step 3 is critical.
- **Risk: parallel-unbounded LIST during `factory.Start()`.** Mitigation: lazy AddResourceType registers ONE resource type at a time, throttled by request arrival rate.
- **Risk: Role-Based Access Control informer doesn't see a RoleBinding mutation within informer latency window (~100 ms).** Mitigation: `EvaluateRBAC` reads from informer store synchronously; the worst-case staleness window equals the informer's watch latency. Acceptable per `feedback_l1_invalidation_delete_only.md` semantics (Role-Based Access Control follows informer events directly; no second-tier cache at this tag).
- **Risk: Revision 2 enforcement (every-dispatch `EvaluateRBAC`) doubles cyberjoker dispatch CPU.** Mitigation: at `0.30.10` the permission-check cache amortises hot-path Role-Based Access Control to <1 µs/decision. At this tag, expect the full cost.
- **Convergence-related risk:** cache=on convergence p99 at 50 K is 8–20 s — far from <1 s target. Closing this requires resolved-output cache + dependency tracking + userAccessFilter. **STOP condition (Revision 4):** if at `0.30.8` (after resolved-output cache + dependency tracking) convergence p99 at 50 K is still > 5 s, the stale-while-revalidate rule needs revisiting at that tag (see `0.30.8` STOP discussion).
- **Risk: cache=off correctness break.** Mitigation: PM amendment 1 — CI lint, regression test (`CACHE_ENABLED=false` boot, goroutine count delta <5), correctness check on Compositions page.
- **If cache=on cold at 5 K > cache=off cold at 5 K:** PM amendment Ship 3 FAIL — ROLLBACK.
- **If cache=on cold at 50 K > 8 200 ms (= 2× Row 7):** STOP and reassess.
- **Hard ROLLBACK trigger (Revision 1):** any single `SubjectAccessReview` call observed in cache=on path during bench.

### Estimated LoC and effort
- Code: ~370 production + ~180 test = ~550 LoC.
- Chart: ~1 LoC (`CACHE_ENABLED` value flip).
- Effort: **1 sprint**.

---

## Tag `0.30.5` — Step 2: SetTransform strip

### Branch base
`0.30.4`. Branch name: `ship-0.30.5-strip`.

### What's implemented
- **`internal/cache/strip.go` (NEW, ~60 LoC).** `StripBulkyFieldsForResourceType(resourceType, obj)`: drops `metadata.managedFields`, drops `metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]`. Optional per-resource-type overrides.
- **`internal/cache/watcher.go` (MODIFY, ~20 LoC).** Call `informer.SetTransform(StripBulkyFieldsForResourceType(resourceType))` BEFORE `factory.Start()` (primer §4.7).
- **`internal/cache/strip_test.go` (NEW, ~40 LoC).** Drops managedFields; preserves spec/status/labels; idempotent; error path doesn't panic.

### Chart values (binding constraint 4)
**Vs `chart-0.30.4`:**
- **ADDED:** none.
- **REMOVED:** none.
- **UNCHANGED:** all keys.

Strip is hardcoded behaviour, not env-driven (per architectural simplicity). No new chart values.

**Stripped-chart deliverable:** `chart-0.30.5` (= `chart-0.30.4`).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.0`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:**
  - Admin cold: ~0 % delta vs `0.30.4` (strip is decode-free; cold dominated by decode CPU).
  - Admin warm-p50: -50 to -200 ms vs `0.30.4`.
  - Cyberjoker cold: ~0 % delta.
  - Cyberjoker warm-p50: -50 to -150 ms.
  - Mix-weighted cold: ~0 % delta.
  - Mix-weighted warm-p50: -50 to -200 ms.
- **At SCALE=50000:**
  - Admin cold: 0 % delta vs `0.30.4` (5 000–12 000 ms maintained).
  - Cyberjoker cold: 0 % delta.
  - Mix-weighted cold: 0 % delta vs `0.30.4`; range 4 050–10 100 ms.
  - Mix-weighted warm-p50: -100 to -300 ms (more pronounced at 50 K).

#### Convergence (Revision 4)
- ±100 ms vs `0.30.4`. Strip does not move convergence (cost is in resolver, not informer).

#### Mechanism-level
- Memory at 50 K (cache=on): heap_alloc -30 to -50 % vs `0.30.4` (audit `0.25.244`). Range: heap_alloc 0.7–2 GB; RSS 1.0–2.5 GB.
- Pod restart count: 0. **Special gate: predicted "from occasional to 0/24h"** per `clean-slate-proposal §4 Step 2`.
- Time-to-Ready: ~±5 s.
- Strip rate: ~30–50 % bytes per object on average; compositions ~40 %, RoleBindings ~5 %.

#### Code-path falsifier
INFO log once per resource type at first transform invocation: `strip.applied resource_type=apps/v1/Deployment len_pre=4823 len_post=2741 ratio=0.43`. If absent or `ratio < 0.10` for compositions, defect.

#### Pre-flight falsifier
`0.30.4` pod RSS at 50 K MUST be > 5 GB. If <2 GB, strip is moot; spend LoC elsewhere.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; ±0–10% vs `0.30.4` (memory delta only — strip doesn't change throughput).
- **Pass 2 (Chrome MCP):** ±0–100 ms vs `0.30.4` cold/warm (memory tag, not latency tag).
- **Pass 3 (per-mutation):** informational; ±0 ms vs `0.30.4` p99. `convergence_per_mutation_p99_mix` unchanged.

### What this tag does NOT do
- Does NOT add eager registration, resolved-output cache, userAccessFilter, permission-check cache, paginated Warmer.
- Does NOT introduce `PartialObjectMetadata` informers.
- Does NOT add per-resource-type strip beyond managedFields + last-applied.

### Risks
- **Risk: a JQ expression reads `managedFields` or `last-applied-configuration`.** Mitigation: pre-flight grep across portal RestActions + customer RestAction inventory. If any match, exclude that resource type from strip via per-resource-type allow-list.
- **Risk: strip rate <10 % on compositions.** Mitigation: audit `0.25.244` doesn't reproduce; reassess Step 2 value before merging. ROLLBACK if <10 %.
- **Risk: transform throws on malformed objects.** Mitigation: returns `(obj, nil)` on any error.
- **If pod RSS at 50 K > 8 GB after Step 2:** Step 2 didn't pay; fall through to Step 3.

### Estimated LoC and effort
- Code: ~80 production + ~40 test = ~120 LoC.
- Chart: 0 LoC.
- Effort: **0.5 sprint**.

---

## Tag `0.30.6` — Step 3: typed-RBAC indexer (rewritten 2026-05-13 **v2** after pprof falsifier)

> **Why this tag exists** — Cyberjoker cold-nav cpu-pprof on `0.30.61` (≡ `0.30.5` with eager-register gated OFF, 22.55 s cold nav, 60 s window, 10.3 s active CPU) attributes **48.7 % of total CPU** to `rbac.evaluateAgainstInformer` and **45.5 % to `apimachinery runtime.fromUnstructured`** inside it. Every `/call` reconverts the same `Unstructured` Role/RoleBinding/ClusterRole/ClusterRoleBinding objects to typed objects from scratch; the result is never memoized.
>
> **What this tag changes** — extend the existing per-GVR `SetTransform` (already wired at `internal/cache/strip.go:59` `resourceOverrides` + `internal/cache/watcher.go:139-149,205-213`) so the four RBAC GVRs convert `Unstructured → *rbacv1.Role / *rbacv1.RoleBinding / *rbacv1.ClusterRole / *rbacv1.ClusterRoleBinding` **once at Add/Update event time**, store the typed object in the indexer, and read it directly in `rbac.evaluateAgainstInformer`. Per-call `FromUnstructured` cost goes to zero. Same indexer footprint (typed objects are roughly the same size as the Unstructured they replace and we are dropping the Unstructured, not double-storing).

> **Post-mortem note (2026-05-13, v1).** The `0.30.6` v1 spec ("resolver-cache wiring with `(*dynamicClient).List` elision") was **falsified before any code landed**. The v1 PRE-FLIGHT GATE required `>1 000 ms` cumulative `(*dynamicClient).List` time per cold nav on the published `0.30.5` image. Tester measured the gate on `0.30.61` (functionally equivalent to `0.30.5` at the dispatch path): `(*dynamicClient).List` cumulative ms = **0** (function not present in the profile). Resolver-cache wiring would optimize a non-existent hotspot. The v1 spec is superseded by the v2 spec below, retargeted at the actual 48 % CPU hotspot the same pprof identified. The v1 falsifier pprof at `/tmp/snowplow-runs/0.30.61/preflight/cpu_cyber_dashboard.pb.gz` is preserved as the v1-falsifier-artifact and **also serves as the v2 PRE-FLIGHT GATE's passing evidence** (see below — the v2 gate is already TRUE per today's data).

> **Post-mortem note (2026-05-13, v0).** The originally specified `0.30.6` ("eager resource-type registration from RestAction inventory") was shipped on `ship-0.30.6-eager-registration` and produced a **3× S7/S8 convergence regression** plus a **new S6b VERIFY TIMEOUT** at the SCALE=50000 bench. The eager-register code (`internal/cache/inventory.go`, `internal/cache/eager.go`) is preserved on that branch and re-enters the plan as the CONDITIONAL future tag `0.30.6.x` below, gated on pprof showing a real consumer for it. See `project_regression_journal.md`.

> **PRE-FLIGHT GATE (MANDATORY) — v2.**
> **Do NOT cut `0.30.6` until this gate passes.** Per `feedback_falsifier_first_before_ship.md` and the lesson of the v0 + v1 ships (v0 = no falsifier, shipped 3× regression; v1 = falsifier ran, gate FAILED, scope re-cut before code landed — the model worked, keep it).
> 1. On the published `0.30.5`-equivalent image (`0.30.61` is the canonical artifact; eager-register OFF) at SCALE=50000 with `cache=on`, capture a 60 s CPU pprof profile during a cyberjoker Dashboard cold navigation.
> 2. Measure cumulative time in `apimachinery runtime.FromUnstructured` summed across all callers (the typical callers are `rbac.toClusterRoleBinding`, `rbac.toRoleBinding`, `rbac.toClusterRole`, `rbac.toRole` plus any incidental resolver-side typed reads).
> 3. **PASS:** cumulative `FromUnstructured` time > **1 000 ms** per cold nav.
> 4. **FAIL:** cumulative `FromUnstructured` time < **500 ms**. The conversion is not the hotspot any more — re-scope `0.30.6` or skip ahead.
> 5. **GREY ZONE (500–1 000 ms):** PM gate decision. Default to FAIL unless Diego explicitly waives.
> **Status at issue:** the falsifier artifact at `/tmp/snowplow-runs/0.30.61/preflight/cpu_cyber_dashboard.pb.gz` shows `FromUnstructured = 4 760 ms cumulative` and `evaluateAgainstInformer = 5 020 ms cumulative` per cold nav, both wholly inside the `widgetsHandler.ServeHTTP` window (61.8 % of CPU). **GATE AUTO-PASSES** — no separate measurement run required. The artifact is the canonical pre-flight evidence; archive it alongside the ledger row.

### Branch base
`ship-0.30.5-strip` (where `0.30.5` lives). Branch name: `ship-0.30.6-typed-rbac-indexer`.

### What's implemented
- **`internal/cache/strip.go` (MODIFY + EXTEND, ~80 LoC).** Add four typed-converting `stripFunc` variants — `stripAndTypeClusterRoleBinding`, `stripAndTypeRoleBinding`, `stripAndTypeClusterRole`, `stripAndTypeRole`. Each calls `defaultStripUnstructured` (preserves the `0.30.5` managedFields + last-applied-annotation strip), then `runtime.DefaultUnstructuredConverter.FromUnstructured` ONCE per Add/Update event, then returns the typed pointer in place of the `Unstructured`. Register the four GVRs in `resourceOverrides` (currently empty at `0.30.5`). The existing falsifier log `strip.applied` gains two extra fields: `typed_kind` and `conversion_ms` (so we can sanity-check conversion time at the indexer write path is sub-millisecond per object). Per-resource override mechanism is already first-class — no architectural surgery, just a population.
- **`internal/cache/watcher.go` (MODIFY, ~40 LoC).** Add two typed accessors that mirror `GetObject` / `ListObjects` but return `runtime.Object` (or a type-parameterised variant if the team prefers). They look up the indexer the same way, then attempt the type assertion (`*rbacv1.ClusterRoleBinding`, etc.) before falling back to converting an `Unstructured` (defensive fallback: if for any reason the transform didn't fire — e.g. an object that arrived before SetTransform was installed — we still produce the typed object, just paying the v0 cost on that one read). Returning `runtime.Object` rather than four typed methods keeps the watcher.go surface stable; consumers cast at the callsite. The existing `GetObject` / `ListObjects` are PRESERVED unchanged for non-RBAC callers.
- **`internal/rbac/evaluate.go` (MODIFY, ~60 LoC).** Replace the four `to{ClusterRole,Role}{Binding,}` converters (lines 305-335) with type-assert helpers that take the indexer's stored object directly. `evaluateAgainstInformer` and `roleRefPermits` now read typed pointers from the indexer in O(1), zero per-call conversion. The defensive fallback (Unstructured still in the indexer) is retained for safety but logged at WARN so a regression — e.g. a future code path that re-adds an Unstructured to one of the four GVRs' indexers — is loud.
- **Subject-prefilter ordering (cheap win bundled, ~10 LoC).** In `evaluateAgainstInformer`, reorder so `anySubjectMatches` runs BEFORE the typed read where possible. With the typed transform installed this matters less (the conversion is already amortised), but the subject-check is still cheaper than the rule-walk and makes the cold path log-quieter.
- **`internal/rbac/evaluate_test.go` + `internal/cache/strip_test.go` (NEW/EXTEND, ~60 LoC test).** Tests cover: indexed object is `*rbacv1.ClusterRoleBinding` (not `*Unstructured`) after SetTransform fires; an Unstructured-only fallback path still permits / denies identically; `cache.Disabled()` path is bit-exact equal to `0.30.5` (zero changes to `UserCan` / SubjectAccessReview branch); SetTransform conversion error on a malformed object returns the original `Unstructured` and logs a warning (no panic, no informer-loop stall — same contract as `0.30.5`'s strip).

### Estimated LoC
- Code: ~140-180 production + ~60 test = **~200-240 LoC**.
- Chart: 0 LoC (no new env keys).
- Effort: **0.5 sprint** — smaller than v1's `~250 LoC` because there is no new env wiring, no new metric pipeline, no resolver-side touch; the SetTransform infrastructure and falsifier-log machinery already exist.

### Chart values (binding constraint 4)
**Vs `chart-0.30.5`:** UNCHANGED. No new env keys, no removed keys.

**Stripped-chart deliverable:** `chart-0.30.6` = `chart-0.30.5` byte-for-byte (re-tag only).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.0`.** Unchanged. The typed-RBAC indexer is wholly internal to the cache + rbac packages; no portal-facing surface change.

### Expected results

#### Per cache mode (the load-bearing contract for this tag)
- **`cache=ON` cyberjoker S6 cold (50 K) and Chrome cold-nav:** target ≤ **17 000 ms** Chrome cold-nav (vs `0.30.61` baseline of `22 550 ms`; the 4 760 ms `FromUnstructured` cumulative goes to zero). Mix-weighted cold delta vs `0.30.61` baseline: **-4 500 to -5 000 ms** on cyberjoker, **-3 000 to -4 000 ms** on admin (admin has fewer subject-binding matches but the RBAC walk still fires for every dispatch and conversion cost is still paid on every walked object).
- **`cache=ON` warm-p50 (5 K + 50 K):** -100 to -300 ms vs `0.30.5`. RBAC eval fires on warm dispatches too; conversion cost is amortised over add/update events instead of per-request.
- **`cache=OFF` cold/warm (50 K and 5 K):** **bit-exact equal to `0.30.5`.** Cache=off takes the `UserCan` → SubjectAccessReview branch in `EvaluateRBAC`; the typed-RBAC transform does not fire (informers aren't running in cache=off mode). Regression-test invariant.
- **`cache=ON` admin warm (50 K):** -50 to -200 ms. RBAC walks more bindings as admin; conversion savings scale roughly linearly.

#### Convergence (Revision 4)
- ±100 ms vs `0.30.5`. Typed-RBAC affects dispatch latency only; convergence is mutation-driven and bounded by informer event delivery. The transform adds sub-millisecond per-event cost (conversion + strip together) which is in noise vs informer event-loop latency.

#### Mechanism-level
- Memory at 50 K: -2 % to +3 % vs `0.30.5`. Typed RBAC objects are roughly the same size as the Unstructured they replace — often smaller, because typed avoids per-field `map[string]interface{}` overhead. At ~1 800 RBAC objects (Diego's cluster profile) × ~3-5 kB typed = 5-9 MB; the displaced Unstructured was 5-8 MB. Net delta is dominated by allocator slack, not the type change.
- Pod restart 30-min: 0 (transform is pure-CPU; no new long-lived goroutines, no new I/O).
- Time-to-Ready at 50 K: same as `0.30.5` ± 200 ms. The transform fires synchronously inside the indexer Add path during initial LIST replay; we expect a small additive cost at startup that pays itself back on the first /call.
- StreamWatcher.receive cum %: unchanged from `0.30.5`.
- **`evaluateAgainstInformer` cumulative ms per cold nav:** target < **500 ms** (vs **5 020 ms** at `0.30.61`). If this is still > 2 000 ms after `0.30.6` ships, the transform is not actually re-typing the indexer entries — STOP and diagnose before tagging.

#### Code-path falsifier (post-ship)
- INFO once at first event per RBAC GVR: `strip.applied resource_type=rbac.authorization.k8s.io/v1/ClusterRoleBindings typed_kind=*rbacv1.ClusterRoleBinding conversion_ms=<n>`. If any of the four lines is missing after the first cold nav, the override registration is broken — DECLARE REGRESSION.
- DEBUG counter: `rbac.indexer.read kind=ClusterRoleBinding typed=true fallback=false`. Steady-state ratio of `typed=true` reads MUST be > 99 % after the first nav per RBAC kind. A `fallback=true` rate > 1 % means the transform isn't installed on a code path that bypassed `NewResourceWatcher` (e.g. test seeding) — surface, do not silently absorb.
- Pprof falsifier: a fresh 60 s CPU profile during a `0.30.6` cyberjoker cold-nav MUST show `runtime.FromUnstructured` cumulative < 500 ms inside the `widgetsHandler.ServeHTTP` window (vs 4 760 ms at `0.30.61`). The DELTA is the headline win.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** S6 cyberjoker cold should drop ~5 s (the pprof-measured `FromUnstructured` + adjacent allocator work). S7/S8 should NOT regress (transform adds a microsecond per indexer write and removes work on the read path — net favourable). If S7 or S8 worsens > 10 %, STOP and diagnose.
- **Pass 2 (Chrome MCP):** primary mover. Cold Dashboard nav for cyberjoker is the headline; target ≤ 17 s vs the 22.55 s baseline.
- **Pass 3 (per-mutation):** ±100 ms vs `0.30.5`. Typed-RBAC is not on the per-composition mutation path.
- **Pass 4 (pprof):** archive new `runtime.FromUnstructured` cumulative time vs the pre-flight number. Compare `evaluateAgainstInformer` cumulative as the second canary.

### What this tag does NOT do
- Does NOT add a subject-indexed RBAC informer (Option B, considered and deferred — typed transform handles the larger CPU share at lower complexity). If after `0.30.6` ships the residual `evaluateAgainstInformer` cumulative is still > 500 ms per cold nav, a subject-index extension becomes a candidate `0.30.6.y` tag (not currently planned).
- Does NOT introduce a resolved-output L1 cache. That is `0.30.7`/`0.30.8`. Typed-RBAC and L1 are complementary: L1 helps on hits; typed-RBAC helps on misses (and L1 has misses by definition — first nav per subject, refresher rebuilds, prewarm population).
- Does NOT wire resolver-side caching of composition LISTs (the v1 spec). Pprof says that hotspot is below the 1 s threshold at current scale; revisit if a future profile changes the picture.
- Does NOT eagerly register non-RBAC GVRs. Eager registration remains the conditional `0.30.6.x` tag, gated on its own pprof evidence.
- Does NOT touch userAccessFilter (`0.30.9`), prewarm (`0.30.12`), activity classification (`0.30.11`), Warmer (`0.30.13`).
- Does NOT register CustomResourceDefinitions from Redis (audit `0.25.329` anti-pattern, never re-introduced).

### Risks
- **Risk: SetTransform install timing.** `SetTransform` must be installed BEFORE `factory.Start`. The current code already does this at `watcher.go:139-149` for the strip transform; the typed-converting override registers via the existing `resourceOverrides` map at package-init time (cache package init), so it is in place before `NewResourceWatcher` registers any informer. Mitigation: a startup-time assertion verifies `resourceOverrides[<each RBAC GVR>] != nil` before `factory.Start`, panicking on absence so a registration regression cannot ship silently. Late lazy `AddResourceType` for an RBAC GVR is impossible — they are eager-registered at constructor time per `0.30.4`.
- **Risk: `FromUnstructured` returns an error on a malformed RBAC object and the transform falls back to returning the original `Unstructured`.** Mitigation: same contract as the existing strip — on error, return the original object + log WARN. `EvaluateRBAC` callsite handles the `*Unstructured` fallback via the same `to{Kind}` helpers preserved in `internal/rbac/evaluate.go` (now used only on the fallback path, with a `fallback=true` counter increment). The cluster-level fail-open is bounded: a single malformed object only costs one per-call conversion, not the whole walk.
- **Risk: typed-object indexer storage breaks an existing consumer that expects `*Unstructured` for an RBAC GVR.** Mitigation: pre-ship, `grep -rn "ListObjects.*clusterRoleBinding\|GetObject.*clusterRoleBinding\|ListObjects.*roleBinding\|GetObject.*roleBinding\|ListObjects.*clusterRole\|GetObject.*clusterRole\|ListObjects.*rolesGVR\|GetObject.*rolesGVR"` and confirm `internal/rbac/evaluate.go` is the only caller of these four GVRs (audit step in the ship row). At `0.30.5` the resolver does not touch RBAC informers directly — the audit is mechanical, not architectural.
- **Risk: regression on cache=OFF.** Mitigation: `EvaluateRBAC` already takes the `cache.Disabled()` early-return to `UserCan` (lines 85-96 of `evaluate.go`); typed transform is wholly inside the cache=on code path. Bit-exact regression test included.
- **Risk: indexer memory bloat from holding typed + Unstructured both.** Mitigation: the transform RETURNS the typed object — it replaces the Unstructured in the indexer, does not augment it. Pre-ship, a heap-pprof diff between `0.30.5` and `0.30.6` at idle (post initial LIST sync) MUST be < +10 MB total. If it exceeds, the transform is leaking; STOP.
- **If `cache=ON` cold > `cache=ON` cold at `0.30.5` (i.e. baseline `0.30.61`):** ROLLBACK.
- **If S7/S8 regress > 10 %:** ROLLBACK. (Same canary as v0 / v1.)
- **If post-ship `evaluateAgainstInformer` cumulative is still > 2 000 ms per cold nav at SCALE=50000:** STOP — the transform is not effective in production conditions and a subject-index follow-up (Option B) is the next move, not shipping `0.30.7` on top.

---

## Tag `0.30.7` — Step 4a: resolved-output cache scaffold (bounded cache + byte-budget, time-to-live only)

### Branch base
`0.30.6`. Branch name: `ship-0.30.7-resolved-cache-core`.

### What's implemented

PM amendment 6 splits Step 4 into two single-sprint sub-ships. **Tag `0.30.7` is sub-ship A.** Tag `0.30.8` is sub-ship B (dependency tracking + DELETE-driven evict).

- **`internal/cache/resolved.go` (NEW, ~200 LoC).** In-process bounded cache (least-recently-used eviction) `*lru.Cache[string, *ResolvedEntry]`:
  - Key: `(restaction_path, user_identity, query_hash)`.
  - Value: `*ResolvedEntry{resolved_json []byte, created_at, resource_type_deps, namespace_deps}`.
  - Bounded by entry count (default 100 000) AND byte budget (default 2 GB; per Q-L1-BUDGET — bounded least-recently-used cap, NOT complex eviction-sweep).
  - **Invalidation in this sub-ship: time-to-live only.** DELETE-driven invalidation lands in sub-ship B (`0.30.8`). UPDATE/PATCH use stale-while-revalidate (per `feedback_l1_invalidation_delete_only.md`).
- **`internal/handlers/dispatchers/restactions.go` (MODIFY, ~50 LoC).** Resolved-output-cache lookup wrapping resolver call. **Per Revision 2:** `EvaluateRBAC` still fires before lookup on every dispatch.
- **`internal/handlers/dispatchers/widgets.go` (MODIFY, ~30 LoC).** Same pattern.
- **`internal/cache/resolved_test.go` (NEW, ~80 LoC).** Hit/miss accounting; bounded-cache eviction at cap; byte-budget eviction at cap; basic GET/PUT.

### Chart values (binding constraint 4)
**Vs `chart-0.30.6`:**
- **ADDED:**
  - `env.RESOLVED_CACHE_MAX_ENTRIES: "100000"`.
  - `env.RESOLVED_CACHE_MAX_BYTES: "2147483648"` (2 GB).
  - `env.RESOLVED_CACHE_TTL_SECONDS: "3600"` (time-to-live).
- **REMOVED:** none.

**Stripped-chart deliverable:** `chart-0.30.7` (= `chart-0.30.6` + 3 env keys).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.0`.** Unchanged. The resolved-output cache is keyed by `(restaction_path, user_identity, query_hash)`; user_identity is binding-identity hash; userAccessFilter still absent. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:**
  - Admin cold: ±100 ms vs `0.30.6` (cold = first request, no resolved-cache hit).
  - Admin warm-p50: -300 to -800 ms (resolved-output cache short-circuits resolver).
  - Cyberjoker cold: ±100 ms.
  - Cyberjoker warm-p50: -300 to -800 ms.
- **At SCALE=50000:**
  - Admin cold: ±100 ms vs `0.30.6`.
  - Admin warm-p50: -500 to -1 500 ms.
  - Cyberjoker cold: ±100 ms.
  - Cyberjoker warm-p50: -500 to -1 500 ms.
  - Mix-weighted cold: ±100 ms (range 3 050–7 600 ms).
  - Mix-weighted warm-p50: ~1 200–1 800 ms (Row 7's was 1 348 ms; should be at-or-near).

#### Convergence (Revision 4 — first tension point)
- **At SCALE=5000 cache=on:** convergence_p50 ~1 500–3 500 ms (resolved cache hides resolver cost on hits but UPDATE doesn't evict, so first-fresh-read after mutation has stale time-to-live up to 1 h before re-resolve). convergence_p99: **EXPECTED HIGH at this tag** because UPDATE-driven invalidation doesn't exist yet — convergence on UPDATE is bound by `RESOLVED_CACHE_TTL_SECONDS` (default 3 600 s) for any entry not yet deleted. **This is a deliberate intermediate state**; the dependency-tracking + DELETE-evict ship at `0.30.8` makes convergence DELETE-driven; UPDATE remains stale-while-revalidate by `feedback_l1_invalidation_delete_only.md`.
  - Specifically: convergence_p99 at this tag is dominated by the smallest of `RESOLVED_CACHE_TTL_SECONDS` and the background-refresher cadence. With time-to-live = 3 600 s and no refresher, p99 ≈ time-to-live. **Action at this tag:** lower `RESOLVED_CACHE_TTL_SECONDS` default to bound p99 ≤ refresher cadence (see Revision 4 STOP discussion at `0.30.8`).
- **At SCALE=50000 cache=on:** convergence_p50 ~3 000–8 000 ms; convergence_p99 dominated by time-to-live as above.
- **Cache=off:** unchanged from `0.30.0`.

#### Mechanism-level
- Memory at 50 K: heap_alloc +200 to +800 MB vs `0.30.6` (resolved-output cache entries). Cap 2 GB. Steady: 1.0–2.8 GB total.
- Pod restart count: 0.
- Resolved-output-cache hit rate target: >70 %. **Pre-flight falsifier: <50 % = STOP.**
- Resolved-output-cache byte utilisation: 5–30 % of cap (audit `0.25.319` walk-back v2: was 16× over-provisioned at 6 %).

#### Code-path falsifier
INFO per request: `resolved_cache.lookup hit=true|false key_hash=... resource_type_deps=[a,b,c] resident_bytes=N`.
Aggregate per 5-min: `resolved_cache.summary entries=N bytes=B hit_rate=0.NN evict_lru=X evict_delete=Y` (evict_delete=0 in this sub-ship; DELETE eviction lands at `0.30.8`).

#### Pre-flight falsifier
- cache=off path must continue to serve correct results.
- `0.30.6` pprof must show resolver CPU cost in top 5. If not, resolved-output cache is moot.
- Resolved-output-cache hit rate < 50 % under load = STOP.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational. S6 absorbs cache-write overhead; expect ±0–10% vs `0.30.6`.
- **Pass 2 (Chrome MCP):** warm-p50 is the main mover (resolved-output cache hits absorb resolver CPU).
- **Pass 3 (per-mutation):** **informational, but first tension point.** `convergence_per_mutation_p99_mix` is bounded by `RESOLVED_CACHE_TTL_SECONDS` (no refresher yet). Hypothesis: p99 ≈ time-to-live floor. R4 binding gate is NOT yet active here — but this row is the pre-flight falsifier for `0.30.8`'s STOP gate.

### What this tag does NOT do
- Does NOT add DELETE-driven eviction (Tag `0.30.8`).
- Does NOT cache raw informer objects (primer §4.2; "per-object cache mirror" anti-pattern from audit, never re-introduced).
- Does NOT add a second-tier cache (audit blacklist, retracted 2026-05-07).
- Does NOT add userAccessFilter (`0.30.9`), permission-check cache (`0.30.10`), activity classification (`0.30.11`), targeted prewarm (`0.30.12`), Warmer (`0.30.13`).
- Does NOT add complex eviction-sweep machinery.

### Risks
- **Risk: resolved-output cache hit rate <50 %.** Mitigation: STOP and reconsider. ROLLBACK to `0.30.6`.
- **Risk: keys collide across users.** Mitigation: key includes `user_identity` (sha256 of binding-identity bytes); test coverage.
- **Risk: stale-while-revalidate serves outdated data.** Mitigation: time-to-live bounds at 1 h (default) — and per Revision 4, this is the convergence-p99 ceiling. **Action at this tag:** plan to ship `0.30.8` with a refresher tuned to bound convergence ≤ 1 s. **Per Diego's just-in ruling (2026-05-09 mid-edit), `feedback_l1_invalidation_delete_only.md` is BINDING and PERMANENT** — UPDATE/PATCH never evict; only the refresher re-resolves on UPDATE. If <1 s p99 turns out to be mechanically impossible at scale, that is a STOP condition at `0.30.8`, not a rule-revisit signal.
- **Risk: byte-budget eviction-sweep introduces tail latency.** Mitigation: simple least-recently-used; no complex sweep.
- **Convergence-related risk (Revision 4):** at this tag, convergence_p99 is bound by `RESOLVED_CACHE_TTL_SECONDS`. With 3 600 s default, p99 is ~1 h — far from 1 s. The architect knowingly ships this intermediate state because `0.30.8` adds the background refresher (UPDATE→fresh resolved-cache value within ≤500 ms target). The DELETE-only invalidation rule remains binding throughout.
- **If warm-p99 > 2× warm-p50:** ROLLBACK (eviction-storm tail latency).
- **If cache=on warm-p50 > cache=off warm-p50 at 5 K:** ROLLBACK.

### Estimated LoC and effort
- Code: ~280 production + ~80 test = ~360 LoC.
- Chart: ~10 LoC.
- Effort: **1 sprint**.

---

## Tag `0.30.8` — Step 4b: resolved-output cache dependency tracking + DELETE-driven evict + UPDATE-driven refresher

> **PM note (2026-05-13).** A scale-up cache-lag transient observed in Phase A bench on `0.30.6` v2 was briefly proposed as a Revision 16 scope expansion (CREATE/ADD-event refresher). A pre-flight falsifier on the freshly-deployed `0.30.7` build (artifact `/tmp/snowplow-runs/0.30.8/preflight/probe.log`) showed the first nav within 5 s of a namespace ADD reaches **16/16 calls within 3 s** (network-layer) — the scale-up transient does not reproduce on a clean `0.30.7` build. Per the amendment's own falsifier semantics (`≥14/16 detected → ROLLBACK`), the Revision 16 scope expansion was FALSIFIED before any code was written. **Decision: ship `0.30.8` as originally scoped — DELETE-driven evict + UPDATE-driven refresher only. No ADD-event handler.** The falsified design (ADD coalesce window, `evict_create` falsifier, `pendingAdds` debounce primitive) is preserved in git history for future reference if a real scale-up failure mode surfaces under different conditions.

### Branch base
`0.30.7`. Branch name: `ship-0.30.8-resolved-cache-deps`.

### Three-edge dependency recording specification (Revision 13, BINDING)

**Diego (Revision 13):** "where is defined in the implementation plan the recording of dependencies between one widgets and its resourcesRefs and between one widgets and its apiRef (which is a restactions)? There are also dependencies between restactions and the inner api called if they're kubernetes apis."

The dependency map (`depMap`) records THREE distinct edge types. All three are load-bearing for the convergence p99 < 1 s gate:

1. **Widget → resourcesRefs RENDER-ONLY (STATIC, declarative; Revision 14 filtered).** Declared in the widget CRD spec at `spec.resourcesRefs[]`. Each entry is a `(gvr, namespace, name)` tuple. Known at widget-resolve time without external API calls; recorded by reading the widget object the resolver has in hand. **NOT all `resourcesRefs` are dep edges (Revision 14): refs used solely by widget ACTIONS (button targets, link destinations, mutation targets) MUST be excluded.** Tracking action-only refs causes spurious L1 invalidations (e.g., a Pod change spuriously refreshing a Compositions widget that only references the Pod for a "View Logs" action). See "Render-vs-action filtering rule" below.
2. **Widget → apiRef → RestAction (STATIC, declarative).** Declared in the widget CRD spec at `spec.apiRef` (or `spec.api[]` for multi-API widgets). Maps the widget to its driving RestAction. Static dependency on the RestAction object itself.
3. **RestAction → inner Kubernetes API call (DYNAMIC, discovered at resolve time).** While executing the RestAction's `spec.api[*].path` against the K8s API, the resolver records each `(gvr, namespace, name)` tuple it fetches. Depends on the request's query parameters, the user's RBAC scope, and the data flow through the JQ pipeline. Examples: a RestAction that lists compositions in namespace X records `(compositions.compositions.krateo.io, X, *)` as a list-scope dependency; a RestAction that fetches a specific CRD records the exact CRD name.

**Render-vs-action filtering rule (Revisions 14 + 14-correction, BINDING for edge type 1):**

Diego (Revision 14): "not all resourcesRefs are dependencies for a widget: there might be some resourcesRefs references by widget actions that must no be registered as dependencies." Diego (Revision 14-correction): "this distinction is already implemented, check the code."

**The filter pattern already exists in the codebase** at HEAD `0.25.330`:
- `internal/handlers/dispatchers/prewarm.go:813` — `extractActionRefIDs(obj)` walks `status.widgetData.actions` map (navigate, openDrawer, openModal, rest action types) and collects every `resourceRefId` value into a `map[string]bool` of skip IDs.
- `internal/handlers/dispatchers/prewarm.go:775` — `extractChildRefs(ctx, c, items, identity, skipIDs)` iterates `status.resourcesRefs.items[]` and SKIPS any item whose `id` matches a skip ID. Returns only render-eligible refs.
- Comment at `prewarm.go:781–782` documents the rule: *"Skip action-linked refs: their ID matches a resourceRefId in the widget's actions map. The frontend resolves these lazily on click."*

**Audit context:** this filter pattern was introduced in the Q-COHORT-PREWARM-RA era (`v0.25.318`; ref `prewarm.go:763` comment). The clean-slate plan re-introduces it at `0.30.8` for dep recording (BEFORE prewarm at `0.30.12`), since dep recording must filter render-vs-action regardless of whether prewarm exists.

**Schema (existing; no enhancement needed):**
- `status.resourcesRefs.items[]` is the candidate dep set; each item has an `id` field.
- `status.widgetData.actions.<actionType>[]` lists action references with `resourceRefId` fields.
- Intersection rule: an item whose `id` matches any `resourceRefId` in actions IS action-linked (skip); otherwise it is a render-dep (track).

**Filtering implementation (lift the existing pattern into the dep recording path):**
```go
// During edge type 1 recording (in deps.go or resolver wrapper):
skipIDs := extractActionRefIDs(widgetObj)  // reuse pattern from prewarm.go:813
for _, item := range widgetObj.Status.ResourcesRefs.Items {
    if skipIDs[item.ID] {
        continue  // action-linked; not a render dep
    }
    deps.Record(l1_key, item.GVR(), item.Namespace, item.Name)
}
```

This is the SAME structural pattern as `extractChildRefs`; the implementation lifts the helpers into a reusable location (`internal/cache/deps.go`) so both dep recording (this tag) and prewarm (`0.30.12`) can call them.

**Data structure:**
```go
// internal/cache/deps.go
//
// depMap: sync.Map[depKey] -> set of L1 keys
// depKey is one of:
//   - {gvr, namespace, name}      // exact-object dep (edge types 1 + 3-exact)
//   - {gvr, namespace, "*"}       // namespace-scope-list dep (edge type 3-list)
//   - {gvr, "", name}             // cluster-scoped object dep (edge type 1, cluster-scope)
//   - {gvr, "", "*"}              // cluster-scope-list dep (edge type 3-cluster-list)
type depKey struct {
    GVR       schema.GroupVersionResource
    Namespace string
    Name      string  // "*" indicates list-scope
}
var depMap sync.Map  // depKey -> sync.Map[l1_key]struct{}
```

**Recording (DURING resolve, not a separate pass):**
- The resolver has a `*RecordingDeps` context object passed alongside the request. Every K8s API call the resolver issues (against the dynamic informer factory at `0.30.1`) goes through a wrapper that calls `deps.Record(l1_key, gvr, ns, name)`.
- Edge type 1 (Widget → resourcesRefs): recorded by walking `widget.spec.resourcesRefs[]` once at resolve start.
- Edge type 2 (Widget → apiRef → RestAction): recorded by adding `(restactions.templates.krateo.io, ns, restaction_name)` to `depMap` at resolve start.
- Edge type 3 (RestAction → inner K8s call): recorded as a side-effect of every wrapped API call during resolve; both exact-object and list-scope tuples captured.
- No separate "dep computation" pass; dep recording is a side-effect of resolve. Negligible overhead (~5 µs per recorded edge).

**Invalidation / refresh trigger (on WATCH event):**
- WATCH event arrives via informer for `(gvr, ns, name)` with type UPDATE/PATCH or DELETE.
- Lookup `depKey{gvr, ns, name}`, `depKey{gvr, ns, "*"}`, `depKey{gvr, "", name}`, `depKey{gvr, "", "*"}` — all four bucket forms; union the L1-key sets.
- Per `feedback_l1_invalidation_delete_only.md` (BINDING):
  - **DELETE event:** evict each L1 key from cache (definite invalidation; entry no longer makes sense — the underlying object is gone). DELETE is the ONLY authorized eviction trigger.
  - **UPDATE/PATCH event:** enqueue refresher for each L1 key (stale-while-revalidate; HOT class achieves ≤500 ms refresh latency per Revisions 4 + 12). Does NOT evict.
  - **ADD event:** no-op for the dep-tracker at this tag. ADD events on never-seen `(gvr, ns, name)` tuples cannot have pre-existing exact-name deps; list-scope deps remain valid (stale-while-revalidate via time-to-live + UPDATE refresher fallback). Pre-flight falsifier on `0.30.7` confirmed first nav after namespace ADD already converges to 16/16 calls within 3 s — no ADD-handler needed.

### What's implemented
- **`internal/cache/deps.go` (NEW, ~250 LoC).**
  - `depMap` sync.Map with the four-bucket lookup pattern above.
  - `Record(l1_key, gvr, ns, name)` — called by resolver during widget resolve for each touched tuple.
  - `RecordList(l1_key, gvr, ns)` — list-scope variant; sets `name="*"`.
  - `OnDelete(gvr, ns, name)` — invoked by watcher on DELETE; evicts each dependent L1 key from cache. Only eviction trigger.
  - `OnUpdate(gvr, ns, name)` — invoked by watcher on UPDATE/PATCH; enqueues each dependent L1 key into the refresher queue (priority by `Class(subject)` per Revision 12 once `0.30.11` lands; class-blind initially at this tag). Never evicts.
  - `RemoveL1Key(l1_key)` — invoked by L1 LRU eviction or DELETE; cleans up dep records to prevent leaks.
- **`internal/resolvers/restactions/api/resolve.go` (MODIFY, ~30 LoC).** Add `*RecordingDeps` context; wrap every K8s API call with `deps.Record(...)` for edge type 3. Walk `widget.spec.resourcesRefs[]` for edge type 1. Add `(restactions, ns, name)` for edge type 2.
- **`internal/cache/watcher.go` (MODIFY, ~50 LoC).** Wire `cache.ResourceEventHandlerFuncs{UpdateFunc, DeleteFunc}` to the dep-tracker for every registered GVR:
  - `UpdateFunc` → `deps.OnUpdate(gvr, ns, name)` — enqueue refresher for all dep matches (stale-while-revalidate).
  - `DeleteFunc` → `deps.OnDelete(gvr, ns, name)` — evict dependent L1 keys (only eviction trigger).
  - `AddFunc` is wired as a no-op for the dep-tracker (informer still consumes ADD for its own LIST/WATCH state).
  - Handlers installed at `AddResourceType` time so newly-registered GVRs gain wiring on first use, not retroactively.
- **`internal/cache/refresher.go` (NEW, ~180 LoC) — Revisions 4 + 12 addition.** Background refresher that processes refresh requests from `deps.OnUpdate`. Cadence: `RESOLVED_CACHE_REFRESHER_INTERVAL_MS` (default 500 ms). Class-aware-priority-queue HOOK present (no-op until `0.30.11` activates classifier; HOT/WARM/COLD ordering kicks in then). Bounded parallelism (`RESOLVED_CACHE_REFRESHER_PARALLELISM`, default 4) prevents storm. **Refresher RE-RESOLVES, never evicts** (preserves stale-while-revalidate rule).
- **`internal/cache/deps_test.go` (NEW, ~120 LoC).**
  - Edge type 1: widget with `resourcesRefs` records dep on each ref.
  - Edge type 2: widget with `apiRef` records dep on the RestAction.
  - Edge type 3 (exact): RestAction GETs a specific composition; dep recorded on that composition.
  - Edge type 3 (list): RestAction LISTs compositions in namespace X; dep recorded with `name="*"`.
  - DELETE evicts dependents (edge types 1, 2, 3).
  - UPDATE does NOT evict (re-resolves via refresher).
  - Concurrent dep recording + eviction race-free (race detector clean).
- **`internal/cache/refresher_test.go` (NEW, ~80 LoC).** UPDATE event fires refresher within ≤500 ms; refresher honours parallelism cap; refresher idempotent against concurrent reads; class-aware priority HOOK present and no-op when classifier is empty.

### Revision 4 mechanism + STOP gate (binding)

**Per Diego's just-in ruling (2026-05-09 mid-edit), `feedback_l1_invalidation_delete_only.md` is BINDING and PERMANENT.** UPDATE/PATCH must NOT evict resolved-cache entries; only DELETE evicts. Convergence p99 < 1 s at SCALE=50000 cache=on must be engineered WITHIN this constraint via the background refresher. **The rule is closed; not a tag-decision.**

The mechanism path the bench will validate at this tag:
1. Kubernetes object UPDATE → apiserver → informer WATCH event arrives at snowplow (~10–100 ms informer latency).
2. Background refresher (this tag's NEW addition) detects the UPDATE event for any resource type listed in some resolved-cache entry's `resource_type_deps`.
3. Refresher fires within `RESOLVED_CACHE_REFRESHER_INTERVAL_MS` (default 500 ms, target).
4. Refresher RE-RESOLVES the affected entries (resolver re-runs with fresh informer-cache reads) and writes the fresh value back to the resolved-output cache. **No eviction occurs** (rule preserved).
5. Next read returns fresh data; end-to-end UPDATE → visible convergence target p99 < 1 s.

**Refresher latency budget per tag (Revision 4 falsifiable metric):** `refresher.latency_ms_p99` < 500 ms across all firings. If exceeded, refresher pool is the bottleneck; tune `RESOLVED_CACHE_REFRESHER_PARALLELISM` upward or shrink `RESOLVED_CACHE_REFRESHER_INTERVAL_MS`.

**STOP CONDITION (Revision 4 + Diego just-in ruling):** if the bench at this tag shows convergence_p99 > 1 s at SCALE=50000 cache=on AFTER refresher cadence has been tuned down to 200 ms with parallelism = 8 (practical max before refresher itself becomes the bottleneck), AND `refresher.latency_ms_p99` is being honoured (the refresher is NOT itself the bottleneck), then <1 s p99 is mechanically impossible at scale within DELETE-only invalidation. This is a STOP condition — the ship loop pauses at this tag — and the architect surfaces the failure to Diego with mechanism data. **It is NOT a rule-revisit signal**; the rule remains binding regardless of mechanical outcome.

**Pre-flight falsifier for the STOP gate:** `0.30.7` ledger row's convergence_p99 at SCALE=50000 cache=on (still time-to-live-bound) must be honestly captured. If it's already <1 s at `0.30.7` due to lucky cache miss patterns, the refresher's value at `0.30.8` is overstated; reassess.

### Chart values (binding constraint 4)
**Vs `chart-0.30.7`:**
- **ADDED:**
  - `env.RESOLVED_CACHE_REFRESHER_INTERVAL_MS: "500"`.
  - `env.RESOLVED_CACHE_REFRESHER_PARALLELISM: "4"`.
- **CHANGED:** `env.RESOLVED_CACHE_TTL_SECONDS: "3600"` → `env.RESOLVED_CACHE_TTL_SECONDS: "60"` (lower default — refresher keeps things fresh; time-to-live now an outer safety net only).
- **REMOVED:** none.

**Stripped-chart deliverable:** `chart-0.30.8` (= `chart-0.30.7` + 2 new env keys + 1 changed default).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.0`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- All percentile/scale numbers: ±100 ms vs `0.30.7`. This sub-ship moves correctness + convergence, not cold/warm latency; the cold/warm speedup from Step 4 already landed at `0.30.7`.
- Mix-weighted cold/warm: same range as `0.30.7`.

#### Convergence (Revision 4 — primary objective for this tag)
- **At SCALE=5000 cache=on:** convergence_p50 hypothesis 200–600 ms (refresher fires within 500 ms of UPDATE; resolver re-runs in ~100–400 ms). convergence_p99 hypothesis **400–1 000 ms — first tag where <1 s p99 is plausible at 5 K.**
- **At SCALE=50000 cache=on:** convergence_p50 hypothesis 400–1 200 ms; convergence_p99 hypothesis **800–2 500 ms — first tag where <1 s p99 is *contested* at 50 K.** Honest range; the refresher cadence (default 500 ms) is the floor on p99 absent further tuning.
- **Cache=off:** unchanged from `0.30.0`.

#### Mechanism-level
- Memory: ±50 MB vs `0.30.7` (deps map adds ~10–50 MB at 100 K entries; refresher worker pool adds ~5 MB).
- Pod restart: 0.
- DELETE-driven eviction count over 30-min: ~`#delete_events × avg_dependent_entries`. >0 under churn.
- Refresher firing rate over 30-min: ~`#update_events × dependent_entry_amplification_factor`. Bound by `RESOLVED_CACHE_REFRESHER_PARALLELISM`.

#### Code-path falsifier
The aggregate `resolved_cache.summary` log line MUST now show `evict_delete > 0` AND `refresher_fired > 0` under churn. If either is 0, dependency tracking + refresher are broken.

**Three-edge falsifier (Revision 13 + 14, BINDING):**
- INFO log line `dep.recorded l1_key=K edge_type=resourcesRefs|apiRef|innerCall gvr=G ns=N name=X` per recorded edge during resolve. Edge type `resourcesRefs` only fires for `purpose=render` refs (Revision 14 filter).
- INFO log line `dep.skipped l1_key=K edge_type=resourcesRefs gvr=G ns=N name=X reason=action_only` for action refs that were filtered (visibility into the filter actually firing).
- Metric `dep_map_size` exposed at `/metrics/runtime`. Pre-flight: `0.30.7` row shows `dep_map_size=0` (no dep tracking yet at `0.30.7`). Post-deploy at `0.30.8`: `dep_map_size > 0` within first 30 s of traffic.
- Metric `dep_records_by_edge_type{type=resourcesRefs|apiRef|innerCall}` shows non-zero for each of the three types under traffic. If any is zero, that edge type is not being recorded — bug.
- Metric `dep_skipped_action_refs_total` shows non-zero if any widget has action refs (per Revision 14 filter). If always zero AND widgets are known to have action refs (per portal inspection), filter not firing — bug.
- Metric `refresher_queue_depth` exposed (class-blind at `0.30.8`; per-class `refresher_queue_depth_per_class{class=hot|warm|cold}` populated only after `0.30.11`).
- Tester verifies (correctness gate, positive): for a known widget that depends on object X via edge type 3, `kubectl edit` X → refresher event fires for that L1 key within ≤500 ms.
- **Tester verifies (correctness gate, negative — Revision 14):** for a widget W with a render ref R and an action-only ref A: `kubectl edit` R → refresher fires for W's L1 key (positive); `kubectl edit` A → refresher does NOT fire for W's L1 key (negative). Both must pass.

Correctness gate (NEW for this sub-ship): manually delete a watched object via kubectl; assert dependent resolved-cache entries evict within 5 s. Manually update a watched object via kubectl; assert dependent resolved-cache entries refresh within 1 s (refresher cadence). Tests in `deps_test.go` + `refresher_test.go`.

#### Pre-flight falsifier
`0.30.7`'s `evict_delete=0 refresher_fired=0 dep_map_size=0` is expected (sub-ship A doesn't have any of these). `0.30.8` must flip all three to `>0`.

#### Three-pass expectations (per §3) — **R4 binding gate first activates here**
- **Pass 1 (S6/S7/S8):** informational. S7 (single-DELETE) p99 hypothesis 50–500 ms (DELETE-driven evict propagation). S8 (namespace-cascade) p99 hypothesis 200 ms–2 s (depends on cascade size).
- **Pass 2 (Chrome MCP):** ±0 ms vs `0.30.7` cold; warm unchanged. This is a correctness + convergence tag, not a cold/warm tag.
- **Pass 3 (per-mutation):** **R4 BINDING (≤1000 ms mix-weighted).** Hypothesis: 800–2 500 ms p99 — first contested-<1 s tag at SCALE=50K. **STOP gate fires** if `convergence_per_mutation_p99_mix > 1000 ms` AFTER refresher cadence has been tuned to 200 ms with parallelism = 8 AND `refresher.latency_ms_p99` is honoured. Per Diego's just-in ruling, STOP is NOT a rule-revisit signal.

#### Ledger row schema additions

The canonical ledger row in `/tmp/browser_results.json` gains the following columns (additive; no existing column removed):

| Column                                | Source                                     | Falsifier semantics                                                          |
|---------------------------------------|--------------------------------------------|------------------------------------------------------------------------------|
| `evict_lru`                           | `resolved_cache.summary`                   | LRU evictions (`0.30.7` byte-budget surface). >0 under L1 pressure.          |
| `evict_delete`                        | `resolved_cache.summary`                   | DELETE-driven evictions. >0 under DELETE churn.                              |
| `refresh_enqueued` (NEW)              | refresher counter                          | Count of UPDATE refresh enqueues. >0 under UPDATE churn.                     |
| `refresh_completed` (NEW)             | refresher counter                          | Count of completed re-resolves. Should track `refresh_enqueued` within parallelism × interval lag; persistent gap = refresher saturation. |
| `refresher_fired` (existing)          | refresher counter                          | Backwards-compatible alias for `refresh_completed`. Both emitted.            |
| `dep_map_size` (existing)             | `/metrics/runtime`                         | Dep-tracker active edge count. >0 within 30 s of traffic at `0.30.8+`.       |
| `convergence_per_mutation_p99_mix` (existing) | bench Phase 6                      | R4 binding ≤1000 ms mix-weighted from `0.30.8` onward.                       |

### What this tag does NOT do
- Does NOT modify warm/cold path latency.
- Does NOT add userAccessFilter, permission-check cache, Warmer.
- Does NOT relax `feedback_l1_invalidation_delete_only.md` (UPDATE still doesn't evict — the refresher RE-RESOLVES, not evicts).
- Does NOT add an ADD-event handler. Pre-flight on `0.30.7` (probe.log 2026-05-13) showed scale-up first nav already converges to 16/16 calls within 3 s; the falsified Revision 16 ADD-handler scope is omitted.
- Does NOT change the resolver hot path beyond the ~30 LoC dep-recording wrapper (edge types 1, 2, 3) already in scope.
- Does NOT touch `0.30.7` L1 store internals (LRU eviction, byte-budget, time-to-live). The L1 store at `0.30.7` is the cache surface; this tag only adds the dep-tracker + UPDATE/DELETE event handlers + refresher around it.

### Risks
- **Risk: dependency tracking misses an eviction case** (e.g., edge type 3 list-scope dep doesn't fire on a single-object UPDATE that's part of the list). Mitigation: four-bucket lookup pattern (exact + ns-list + cluster-name + cluster-list) covers all canonical forms; over-evicts conservatively. Test coverage in `deps_test.go` per edge type.
- **Risk: dep map memory leak — dep records remain after their L1 entry is evicted.** Mitigation: `RemoveL1Key(l1_key)` is invoked by L1 LRU eviction; deps_test.go covers leak-after-eviction.
- **Risk: DELETE event-handler races with concurrent reads.** Mitigation: resolved-cache internal mutex; race detector clean test.
- **Risk: refresher storm — many UPDATE events in burst overload refresher pool.** Mitigation: bounded parallelism (`RESOLVED_CACHE_REFRESHER_PARALLELISM=4`); coalesce duplicate refreshes for the same key within a 100 ms window.
- **Risk (Revision 4 PRIMARY): convergence_p99 > 1 s at SCALE=50000 cache=on after refresher tuning.** Mitigation: invoke the STOP gate (above). **Per Diego's just-in ruling, this is a STOP condition, not a rule-revisit signal.** The DELETE-only invalidation rule remains binding regardless of mechanical outcome; the ship loop pauses while the architect surfaces failure data to Diego. Class-aware refresh cadence at `0.30.11` is the next mechanism to evaluate if `0.30.8`'s class-blind cadence misses the gate.
- **Refresher latency budget falsifier:** `refresher.latency_ms_p99` < 500 ms. If exceeded, refresher itself is the bottleneck — tune parallelism + interval before invoking the STOP gate.
- **Risk (Revision 13): edge type 3 (RestAction → inner K8s call) recording overhead on the resolve hot path.** Mitigation: `deps.Record(...)` is sync.Map.Store; sub-microsecond per call. At ~10 inner calls per resolve × 100 K resolves/min, ~16 K records/s — sync.Map handles this comfortably. Falsifier: warm-p50 vs `0.30.7` >50 ms regression = back-out.
- **Risk (Revision 14, CORRECTNESS-CRITICAL): misclassifying an action-dep as a render-dep → spurious L1 invalidations → wasted refresher cycles → convergence p99 budget consumed by spurious work.** Mitigation: lift the existing `extractActionRefIDs` + `extractChildRefs` pattern from `prewarm.go:813,775`; pattern is proven at `v0.25.318+`. Visible via `dep_skipped_action_refs_total` metric and `refresher_queue_depth_per_class` saturation.
- **Risk (Revision 14, CORRECTNESS-CRITICAL): misclassifying a render-dep as an action-dep → missed invalidations → STALE DATA shown to user → convergence guarantee violated.** Mitigation: tester correctness gate (positive + negative both required); the existing pattern uses an explicit allow-list of action types (navigate/openDrawer/openModal/rest), so new action types in the future would default to render-tracked (conservative). Falsifier: tester sanity check covers both directions; if either fails, ROLLBACK.
- **If correctness gate fails (deletion doesn't reflect within 5 s OR update doesn't reflect within 1 s OR negative-case action ref triggers spurious refresh):** ROLLBACK.

### Estimated LoC and effort
- Code: ~430 production + ~200 test = ~660 LoC (Revision 13 deps.go ~250 + resolver wrapper ~30; Revision 14 lifts existing `extractActionRefIDs`/`extractChildRefs` helpers ~50 LoC + ~30 LoC negative-case test). No CRD schema enhancement (Revision 14-correction confirms the schema is already there).
- Chart: ~10 LoC (2 new env keys + 1 changed default).
- Effort: **1.5 sprints** unchanged (PM-amendment-6 cap exception; three-edge dependency recording is the load-bearing convergence-gate enabler. Render/action filter is a low-risk lift of existing proven code.)

---

## Tag `0.30.6.x` (CONDITIONAL, FUTURE) — eager resource-type registration (rescoped from original `0.30.6`)

> **Status: conditional, not scheduled.** This tag holds the eager-register concept that was originally `0.30.6` and was rescoped after the 2026-05-13 post-mortem. The code on `ship-0.30.6-eager-registration` (`internal/cache/inventory.go` + `internal/cache/eager.go` + the `main.go` wiring) is preserved on that branch; this section captures the prerequisites that must be satisfied before that code re-enters the ship sequence.

> **Tag number note.** The plan keeps `0.30.6.x` as a placeholder name for forward reference. When eager-register is actually scheduled, it will consume the next free `0.30.z` integer per Diego's binding rule 6 (CI tag regex `x.y.z`). The total tag count in the top-of-plan table is NOT shifted by this future tag; it is appended only when scheduled.

### Conditional on
1. **pprof evidence at the new `0.30.6` (resolver-cache wiring) showing lazy-AddResourceType-wait > 500 ms in p99 first-nav cold at SCALE=50000 cache=on.** If the `lazy-register-on-miss` log + pprof at `0.30.6` shows < 500 ms p99, this tag is moot — the storm the original `0.30.6` worried about does not exist in practice.
2. **`0.30.6` shipped and validated.** Eager registration without a consumer was the post-mortem root cause; it cannot be re-attempted before the consumer (resolver-cache wiring) is in place and proven on the bench.
3. **`portal-0.30.0` static RestAction set is stable.** Eager registration uses a hardcoded GVR list (see below); changes to the static RestAction inventory require this tag to re-ship.

### What's implemented (when scheduled)
- **`internal/cache/eager_static.go` (NEW, ~80 LoC).** Static (Group, Version, Resource) list derived from `portal-cache/blueprint/templates/restaction.*.yaml` static-path RAs. Estimated set size: ~5 GVRs (compositions, definitions, etc. — exact set computed from the static-path RA inventory at scheduling time). **NOT a dynamic walker.** The walker form was the post-mortem failure mode — it produced an unbounded apiserver-budget explosion. Static const-list avoids that.
- **`main.go` (MODIFY, ~20 LoC).** After `NewResourceWatcher`, call `eager_static.RegisterAll(ctx)`: for each GVR in the const list, `AddResourceType` + `WaitForCacheSync`. Bound by `STARTUP_INFORMER_FANIN` (default 8) and `STARTUP_TIMEOUT_SECONDS` (default 120 s).
- **CI mirror test (NEW, ~30 LoC).** Mirror test (`internal/cache/eager_static_mirror_test.go`) enforces that the const GVR list matches the static-path RAs in the portal repo at HEAD. Drift on either side fails CI.
- **`internal/cache/watcher.go` (no change).** The lazy-AddResourceType-on-miss path stays as a fallback; the const list is "best-effort eager", not an exhaustive enumeration.

### Pre-flight falsifier (mandatory before scheduling)
1. Capture CPU pprof on the deployed `0.30.6` (resolver-cache wiring) image at SCALE=50000 cache=on during cyberjoker cold nav.
2. Measure cumulative time blocked in `cache.AddResourceType` + `WaitForCacheSync` triggered by resolver cache-misses across the first cold nav.
3. **PASS / schedule tag:** p99 lazy-AddResourceType-wait > 500 ms.
4. **FAIL / skip tag:** p99 < 500 ms. The eager-register concept is permanently shelved (or revisited only if customer RA inventory changes meaningfully).
5. Recorded as a row in the canonical ledger with the pprof artifact archived.

### Why NOT a dynamic RestAction-inventory walker (the post-mortem lesson)
The original `0.30.6` walked all `RestAction` objects via the dynamic client at startup and derived the eager GVR set from their `spec.api[*]`. That walker:
- Introduced apiserver-LIST cost proportional to RA count at startup, contributing to S6b VERIFY TIMEOUT at 50 K.
- Triggered AddResourceType fan-in for GVRs no /call ever consumed (because the resolver was still on the apiserver branch — see the post-mortem note at `0.30.6`).
- Risked re-introducing the audit-blacklisted prewarm-pool anti-pattern (audit walk-back: `prewarm-pool drove AddResourceType fan-in storm`).

The static-const form (~5 GVRs from static-path RAs) is bounded, predictable, and CI-mirrored against the portal. Dynamic RA-inventory walking does NOT re-enter the plan absent a separate Diego-approved decision.

### What this tag does NOT do
- Does NOT walk RestAction CRs at startup. The GVR set is a Go constant; CI enforces parity with the portal static-path RAs.
- Does NOT register custom-customer RestAction GVRs eagerly. Those continue to lazy-register on first miss (the path made measurable at `0.30.6`).
- Does NOT change the resolver hot path (`0.30.6` already wired it).

### Estimated LoC and effort (when scheduled)
- Code: ~100 production + ~50 test = ~150 LoC.
- Chart: ~5 LoC (re-uses `STARTUP_INFORMER_FANIN` + `STARTUP_TIMEOUT_SECONDS` from the rescoped `0.30.6`; none net-new if those already shipped, otherwise add them here).
- Effort: **0.5 sprint** (smaller than the original eager-register `0.30.6` because the dynamic walker is excised).

---

## Tag `0.30.9` — Step 5: userAccessFilter + ServiceAccount-dispatch + refilter (atomic ship) — **bundled with lazy-register-on-resolver-touch (Revision 17, redesigned Revision 18)**

### Revision 18 preamble (binding, 2026-05-14 PM — OOM finding + metadata-only redesign)

**0.30.92 OOM finding (artifact: helm rev 108 OOMKill on `compositions-list`).** The Revision 17 lazy-register design landed as `7e253fe` (`0.30.91`) + widened to inner-call paths as `a3d8814` (`0.30.92`). Admin smoke triggered an OOMKill at the 2 Gi container limit:

- Widened hook fired correctly for `composition.krateo.io/v1-2-2/githubscaffoldingwithcompositionpages`.
- Informer initial-sync paged 48 995 compositions cluster-wide. With the 0.30.5 strip (managedFields + last-applied annotation removed) the residual per-object footprint is dominated by `status.managed[]` and the unstructured `map[string]any` overhead — call it ~20 KiB heap per Composition. 49 K × 20 KiB ≈ 1 Gi.
- Simultaneously the resolver's per-namespace LIST cascade (49 bench namespaces × the composition GVR) materialised a second copy in the dispatch path before the informer was sync — ~1 Gi more.
- Combined RSS crossed 2 Gi mid-LIST → OOMKill. Pod recovered (1 restart). Rolled back to `0.30.8` (helm rev 109).

**0.30.92 was tagged but not promoted.** The tag is preserved as the regression-witness for Revision 18. Production-safe state is currently DELETE-evict-dormant (the 0.30.8 0.30.71 dormancy bug remains until 0.30.93+).

**Production scale update (binding, 2026-05-14):** per `project_production_scale.md` the canonical customer scale is **50 000 compositions** (not 10 K — the prior estimate was stale). The Revision 18 redesign MUST validate at SCALE=50000 with RSS ≤ 1.8 Gi. Bumping the 2 Gi container limit is REJECTED per `feedback_no_shortcuts_or_workarounds.md` — same logic as yesterday's eager-register-OOM rejection.

**Redesign space considered (architect, 2026-05-14 PM):**

| Option | Heap @ 50K | Time-to-evict-active | Risk | LoC | Compat (typed-RBAC + DepTracker) |
|---|---|---|---|---|---|
| A. Async informer creation (defer `EnsureResourceType` past response) | ~1 Gi (same informer cost; sync just deferred) | First DELETE after lazy-fire is silent (TTL covers); evict-active +30–120 s after touch | Loses dep edge for the request that triggered registration | ~40 | Compatible |
| B. Paged informer initial-sync (`Limit=500` already in `tweak`) | ~1 Gi steady-state (lower peak — saves ~200 MiB transient) | Same as 0.30.92 | Doesn't change steady-state; OOM still fires once 49K objects land in the indexer | ~10 | Compatible |
| C. Selective lazy-register (skip Composition GVR; carve-out by cardinality) | ~100 MiB total (RBAC + small GVRs only) | DELETE-evict never activates for compositions; permanent gap | Violates `feedback_no_special_cases.md` unless expressed via CRD annotation | ~80 | Composition L1 entries TTL-only forever |
| D. **`PartialObjectMetadata` informers for high-cardinality GVRs** | **~100–150 MiB total at 50K** (metadata-only: name/ns/labels/annotations/owner refs ≈ 2–3 KiB/object) | Evict-active immediately on informer sync (same semantics as full informer for DepTracker which only reads `(gvr, ns, name)`) | Metadata-only indexer cannot serve resolver `GetObject` cache hits — resolver path falls back to apiserver for composition reads. **Acceptable**: today's resolver dispatches LIST via `httpcall.Do` against apiserver anyway (no cache-served Get path for compositions today; informer was only there to feed the dep-tracker). | ~120 | DepTracker.OnDelete/OnUpdate uses `metaNSName(obj)` which works on `metav1.PartialObjectMetadata` (it implements `GetNamespace`/`GetName`). Typed-RBAC overrides not affected (those are RBAC GVRs only). |
| E. Pure event-driven watch (no local cache, RetryWatcher) | ~0 MiB steady-state | Evict-active on first event received post-Watch-Established (~100 ms) | No initial-LIST snapshot — pod restart misses DELETEs that occurred during the restart window (~5–30 s blind spot) | ~200 | DepTracker fires from raw `watch.Event`; not compatible with `ListTypedObjects`/`GetTypedObject` (no store) — but typed-RBAC GVRs stay on the full informer path so unaffected |
| F. Hybrid: metadata-only watch → upgrade to full informer on first DELETE | ~100 MiB pre-DELETE; ~1 Gi post-first-DELETE | Identical to D | Adds a state-machine + upgrade path; the "upgrade" is what OOMed | ~250 | Same as D pre-upgrade |

**Recommendation: Option D — `PartialObjectMetadata` informers for high-cardinality GVRs, expressed via a uniform metadata mechanism (annotation on the CRD), not a per-resource carve-out.**

**Rationale.**

1. **DepTracker only needs `(gvr, ns, name)`.** Confirmed at `watcher.go:471-484`: `metaNSName(obj)` reads `GetNamespace` + `GetName` only; nothing in the OnDelete/OnUpdate path touches `status` or `spec`. `metav1.PartialObjectMetadata` satisfies the `nsNameAccessor` interface. This is a free 10× memory reduction for the eviction mechanism.
2. **Resolver doesn't read compositions through the informer today.** Compositions are dispatched via `httpcall.Do` (apiserver) in the resolver inner-call path; the only reason the 0.30.91/92 lazy-register hook fires `EnsureResourceType` for the composition GVR is to wire the DepTracker event handlers. There is no resolver `GetObject(compositions)` call site — verified by grep. So a metadata-only indexer doesn't regress any current read path.
3. **Uniform mechanism, not a special-case** (per `feedback_no_special_cases.md`). The rule is **annotation-driven**: a CRD or registered resource carries `krateo.io/cache-mode: metadata` to opt into the metadata-only informer. Snowplow inspects this at `EnsureResourceType` time via the discovery client OR via a static set of GVRs whose CRDs Krateo ships (Composition is one). NOT hardcoded by Resource name. Customer CRDs without the annotation use the default full informer (current 0.30.92 behaviour). The annotation predicate is itself expressed as `func(gvr) bool` — no per-Resource switch statement.
4. **Why not A:** defers the OOM by half a second — the informer still allocates 1 Gi when it eventually runs. Doesn't fix the structural issue.
5. **Why not B:** paging at LIST time only changes peak vs steady-state; the indexer at steady-state still holds the 1 Gi.
6. **Why not C:** permanent DELETE-evict gap on the highest-churn resource type in Krateo. Customer-visible.
7. **Why not E:** missing the initial-LIST snapshot creates a pod-restart-time DELETE-blindness window. Watch-only is fragile against apiserver restarts (RetryWatcher resumes from last RV; if the etcd compaction window is exceeded the watcher silently drops events). Operationally heavy.
8. **Why not F:** the "upgrade to full informer" branch is the OOM-causing branch. Adding a state machine to gate it doesn't fix the underlying cost; it just makes the OOM event-triggered instead of touch-triggered.

**Estimated heap at 50K with Option D applied:**

- Compositions: 50 K × ~2.5 KiB metadata = ~125 MiB
- RBAC (existing full informers, ~few-K objects total): ~20 MiB
- Other lazy-registered GVRs (widgets, restactions, customresourcedefinitions, namespaces — all low-cardinality, <1 K each, full informers): ~50 MiB
- Resolver per-request transients: ~100 MiB worst-case (admin smoke LIST cascade)
- Total expected RSS: ~300–500 MiB — well under 1.8 Gi gate.

### Revision 18 implementation outline

- **`internal/cache/watcher.go` (MODIFY, ~60 LoC).** Add `EnsureResourceTypeMetadataOnly(gvr)` variant: uses `metadatainformer.NewFilteredSharedInformerFactory` (`k8s.io/client-go/metadata/metadatainformer`) instead of the dynamic factory. Shares the same `rw.informers`/`rw.syncCh` maps. Shares the DepTracker event-handler wiring (the `metaNSName` extractor already works on `PartialObjectMetadata`). `EnsureResourceType` dispatches to the metadata-only variant when the predicate `shouldUseMetadataOnly(gvr)` returns true.
- **`internal/cache/cache_mode.go` (NEW, ~60 LoC — was 40, +20 for static-seed fallback per PM Revision 18 item 2).** `shouldUseMetadataOnly(gvr) bool` evaluates a two-tier predicate:
  1. **Annotation (long-term primary).** At startup, snowplow lists CRDs via the discovery client; any CRD carrying annotation `krateo.io/cache-mode: metadata` is registered in the metadata-only set. The annotation lives on the CRD itself, so customer CRDs without it use the default full informer (current 0.30.92 behaviour). NO per-Resource hardcoding in snowplow source; the discriminator lives in the CRD.
  2. **Static-seed fallback (operationally-safe-today path).** Snowplow ALSO carries a small static list of GVR-pattern matchers covering Krateo's known high-cardinality CRDs. The seed list is **GVR-pattern**, not exact-GVR — it tolerates the per-CompositionDefinition version suffix (`v1-2-2`, `v12-8-3`, etc.). Initial seed:
     ```
     // internal/cache/cache_mode.go (seed list — explicit, finite, audited at PR review)
     var metadataOnlyGVRSeed = []gvrPattern{
       {Group: "composition.krateo.io", ResourcePrefix: "githubscaffoldingwithcompositionpages"},
       {Group: "composition.krateo.io", ResourcePrefix: "vmmigration"},
       // additional Composition family members added as CompositionDefinitions ship
     }
     ```
     A GVR matches the seed if `gvr.Group == pattern.Group && strings.HasPrefix(gvr.Resource, pattern.ResourcePrefix)`. The seed is a defensive predicate: `shouldUseMetadataOnly(gvr) = annotated(gvr) OR matchesSeed(gvr)`.
  3. **Why both.** The Composition family of CRDs is generated at runtime by `core-provider` from CompositionDefinitions; per `project_no_upstream_authority.md` snowplow cannot patch `core-provider` to emit the annotation on every generated CRD. Until that upstream change ships, snowplow needs the seed to stay below the 1.8 Gi gate. Once the annotation ships upstream, the seed remains as defensive-fallback (no removal) — both predicates remain live and their union is the active set.
  4. **Discovery-failure mode.** If the discovery client fails at startup, the annotation set is empty BUT the seed still routes Composition GVRs to metadata-only. Snowplow stays in metadata-only mode for the family regardless of cluster state — this is the operational-safety property.
  5. **Customer-CRD safety.** Customer CRDs whose Group is NOT `composition.krateo.io` are unaffected by the seed; they only opt into metadata-only via explicit annotation. The seed is Krateo-Composition-family-scoped by construction.
- **`internal/cache/watcher_test.go` (NEW test, ~60 LoC).** `TestEnsureResourceTypeMetadataOnly_DepTrackerFires`: install a fake `PartialObjectMetadata` informer, simulate a DELETE event, assert `Deps().OnDelete` is invoked with correct `(gvr, ns, name)`.
- **No changes** to `deps_extract.go`, `resolve.go`, or `restactions.go` — `EnsureResourceType` keeps the same signature; routing is internal to `cache/watcher.go`.

### Pre-flight falsifier for §0.30.93 (Revision 18) — **stress gate**

**Artifact path:** `/tmp/snowplow-runs/0.30.93/preflight/lazy_register_evict_stress.txt`.

**Why a stress gate.** The 0.30.92 OOM occurred mid-request (LIST cascade overlapped with informer initial-sync). The previous 4-gate probe sampled RSS at three idle checkpoints — pod-Ready, post-first-10-dispatches, post-S7 — none of which observed the LIST/sync overlap window. The new gate drives realistic admin smoke load WHILE the lazy-register fires for the first time.

**Script spec — `delete_stale_gap_v4_stress.py`** (extension of v3):

1. Helm install `0.30.93` image with `CACHE_ENABLED=true`, `EAGER_REGISTER_ENABLED=false`, `RESOLVED_CACHE_TTL_SECONDS=3600`. Bench cluster at SCALE=50000.
2. Wait pod Ready. Sample RSS via `kubectl top pod` → checkpoint `pod_ready` (expected ≤ 200 MiB; RBAC informers only).
3. **Stress phase A — concurrent admin smoke during first-touch:** spawn 10 parallel `/call compositions-list` requests as admin user. Sample RSS at 250 ms intervals for the next 60 s into the artifact. Record peak RSS observed during stress phase A.
4. Verify log line `cache.lazy_register.metadata_only fired gvr=composition.krateo.io/v1-2-2/...` (NEW) AND `cache.lazy_register.inner_call` appears for the Composition GVR.
5. **Stress phase B — sustained 20 RPS admin mix for 5 min** (compositions-list + sidebar-nav + restaction-get). Sample RSS at 1 s intervals. Record peak.
6. `kubectl delete composition <name>` on one Composition. Poll `deps_evict_delete_total`; assert delta > 0 within 10 s.
7. Re-issue admin smoke; assert deleted Composition is absent.
8. RSS samples written: phase-A peak, phase-B peak, post-S7 idle.

**Hard-gate fail conditions** (any one blocks ship):

- Phase-A peak RSS > 1.8 Gi (1843 MiB) → OOM regression → ROLLBACK.
- Phase-B peak RSS > 1.8 Gi → sustained-load regression → ROLLBACK.
- `evict_delete == 0` after DELETE → DepTracker not wired through metadata-only informer → ROLLBACK and fix.
- `cache.lazy_register.metadata_only` log line absent → routing bug; falling back to full informer.
- p99 of stress-phase-A response time > 8 s → first-touch latency regression on metadata-only path.

**Existing v3 falsifier (`delete_stale_gap_v3.py`)** runs unchanged as the regression-fix witness for the non-Composition (full-informer) GVRs.

### Estimated LoC and effort (Revision 18 redesign)

- Code: ~100 production + ~80 test = ~180 LoC, **net REPLACE** of the 0.30.92 widening hook (which stays for non-metadata-only GVRs).
- Combined Sub-scope B (lazy-register + metadata-only routing): ~275 LoC vs prior 175 LoC estimate (+100 LoC for the metadata-only path).
- Effort: +0.25 sprint over the Revision 17 estimate (the metadata-informer plumbing is standard client-go).
- The 0.30.92 OOM-finding tag is preserved as the regression witness; not promoted, not deleted.

### RSS-gate level (PM Revision 18 item 6 — confirmation)

The §0.30.93 stress falsifier (`delete_stale_gap_v4_stress.py`) MUST use the **1.8 GiB strict test-cluster gate** (1843 MiB rounded), NOT the production-chart memory limit. Specifically:

- Phase-A peak RSS > 1.8 GiB → hard-fail → ROLLBACK.
- Phase-B peak RSS > 1.8 GiB → hard-fail → ROLLBACK.
- Idle (post-S7) RSS > 1.8 GiB → hard-fail → ROLLBACK.

**Production chart values** (request=4Gi, limit=8Gi per Diego's confirmation 2026-05-14) provide 4–5× headroom above the 1.8 GiB falsifier gate. **Production 8Gi is safety headroom, not budget license.** The falsifier MUST fail any ship whose RSS exceeds 1.8 GiB at test scale, because Diego's design direction is minimize regardless of production limit. Raising the gate to align with production would mask the OOM regression class the gate is designed to catch (a customer running Krateo on a 2Gi-limited cluster — fully supported deployment shape — must not be a fatal misconfiguration).

The 1.8 GiB gate is a **design budget**, not a deployment constraint: it forces minimization at the snowplow level so production headroom remains free for transient spikes (admin smoke LIST cascades, future per-user-credential allocations, GC scheduling) rather than being absorbed by baseline informer cost.

### Deployment sequencing (PM Revision 18 item 10 — BLOCKER)

§0.30.93 ships changes that touch both snowplow's binary AND the CRD shape (annotation). Misordered rollout creates an OOM regression window because: (a) snowplow without the annotation set falls back to the static seed (safe), but (b) snowplow with annotation-driven discovery on an annotation-less CRD set ALSO falls back to the static seed (safe). The defensive seed makes the sequence resilient to either order — but the recommended sequence below is the lowest-risk path.

**(a) Chart owner of the annotation.** Composition CRDs (e.g. `githubscaffoldingwithcompositionpages.composition.krateo.io`) are **NOT chart-owned**. They are **generated at runtime by `core-provider`** from a `CompositionDefinition` custom resource (verified at `/Users/diegobraga/krateo/core-provider/internal/tools/crd/crd.go`). Per `project_no_upstream_authority.md`, snowplow cannot patch `core-provider` to emit the `krateo.io/cache-mode: metadata` annotation. **Therefore the annotation cannot be added by helm-upgrading any snowplow-side chart.** Two delivery paths exist:

  1. **Upstream PR to `core-provider`** (long-term, not in 0.30.93 scope): `core-provider` adds the annotation to the generated CRD object during `Install`/`Update`. Owner: core-provider team. Ship: independent of snowplow tag cadence. Cannot block 0.30.93.
  2. **Manual annotation by the cluster operator** (interim, deployment-specific): an operator running a customer cluster can run `kubectl annotate crd githubscaffoldingwithcompositionpages.composition.krateo.io krateo.io/cache-mode=metadata` once. This is NOT chart-driven and is NOT how production deploys happen — it is documented as the canonical opt-in for clusters that prefer annotation-driven discovery over static seed.

  **Until either path lands, snowplow relies on the static seed** (`internal/cache/cache_mode.go`) as the primary cache-mode discriminator for the Composition family. The seed is owned by snowplow, ships in the snowplow image, and requires no chart coordination.

**(b) Helm-upgrade order.** When the annotation does eventually flow (via core-provider upstream PR or manual operator annotation):
  - Annotation-bearing CRDs SHOULD land BEFORE the snowplow image upgrade. This way, on snowplow pod boot the discovery client immediately sees the annotation and registers metadata-only routing without falling through to the seed.
  - In practice: `helm upgrade snowplow-crd` (if and when that subchart starts owning the annotation — currently it does not own Composition CRDs) MUST precede `helm upgrade snowplow` (with new image).
  - **0.30.93 ship requires NO chart re-ordering** because the static seed makes snowplow safe-by-default. Future tags that retire the seed (only after core-provider upstream PR has been live in customer clusters for ≥2 release cycles) will reinstate this ordering as a hard constraint.

**(c) Rollback story.** If the annotation reverts (e.g. operator deletes the annotation, or core-provider releases a regression that removes it):
  - Snowplow's `shouldUseMetadataOnly(gvr)` falls back to `matchesSeed(gvr)`.
  - Composition GVRs in the seed list (the entire Krateo Composition family) STAY in metadata-only mode.
  - No OOM regression; no helm rollback required.
  - The `cache.lazy_register.metadata_only` log line continues to fire for Composition GVRs, but the discriminator reason switches from `reason=annotation` to `reason=static_seed`. This is observable in logs without metric changes.

**(d) Chart-driven only.** Per `feedback_helm_only_for_portal.md` and `feedback_chart_only_for_snowplow.md`: NO `kubectl apply` for any annotation-bearing manifest at deploy time. The annotation either lands via core-provider upstream PR (preferred long-term) or via operator-scoped `kubectl annotate` (interim manual opt-in, documented but not part of the chart-driven deploy story). The snowplow chart upgrade itself ships ONLY the snowplow binary + the static seed — no annotation manipulation. This keeps the chart-driven rollout pure.

**Why the seed makes sequencing safe.** The defensive-seed predicate decouples snowplow's OOM-safety from any external annotation state. The annotation is the long-term mechanism for customer-CRD opt-in; the seed is the operationally-required mechanism for Krateo's own Composition family today. Both predicates remain live in steady state — the union ensures Composition GVRs stay metadata-only regardless of cluster annotation state. This is the property that protects against any rollout-ordering misstep.

### Tag promotion path (PM Revision 18 item 11)

Validated commit (passing all 4 hard gates + the §0.30.93 stress gate) → tagged `0.30.93` first; the same commit is also annotated as `0.30.9` for the canonical Step 5 release narrative (UAF + lazy-register + metadata-only routing, atomic). RC witnesses (`0.30.91`, `0.30.92`, `0.30.93`) are retained alongside as historical artifacts:
  - `0.30.91` = Revision 17 lazy-register, narrow inner-call hook scope (incomplete coverage).
  - `0.30.92` = Revision 17 widened inner-call hook (OOM-finding witness — preserved, not promoted).
  - `0.30.93` = Revision 18 metadata-only redesign (the validated ship).

Ledger Row 28 attributes BOTH tag names (`0.30.9` and `0.30.93`) to the same commit SHA. The dual-naming is purely organizational: `0.30.93` preserves the RC-witness numbering granularity for forensic clarity; `0.30.9` is the customer-facing release name in the north-star ledger and changelog.

### Revision 17 preamble (binding, 2026-05-14 — superseded for Sub-scope B mechanism, retained for context)

**Scope expansion:** the original 0.30.9 atomic UAF ship is bundled with an architectural fix for the 0.30.8 DELETE-evict mechanism. Three findings drive the amendment:

1. **0.30.8 DELETE-evict is structurally dormant in production.** The dep-tracker `DeleteFunc`/`UpdateFunc` handlers wired in `internal/cache/watcher.go:353-378` only fire for GVRs registered via `AddResourceType`. The single non-test caller is `internal/cache/eager.go:85`, gated by `EAGER_REGISTER_ENABLED=true`. Helm rev 103 (0.30.8 deployed) leaves the env var unset; default at `main.go:188` is `false`. Result: the entire dep-tracker / refresher / DELETE-evict code path is dead code in the running binary. Probe `/tmp/snowplow-runs/0.30.8/validate/delete_stale_gap.txt` confirmed `evict_delete=0` after 86 min + 2 deliberate DELETEs. The 53 s admin stale-gap reduction observed at 0.30.8 is 100 % attributable to `RESOLVED_CACHE_TTL_SECONDS=60` (was 3600), NOT to DELETE-evict.

2. **Enabling `EAGER_REGISTER_ENABLED=true` causes OOM crashloop** at the chart's 2Gi container limit. At 50K compositions across 49 bench namespaces, the startup_fanin=8 informer inventory burst exceeds the limit. Helm rev 105 rolled back to rev 103 content (eager-off).

3. **Per `feedback_no_shortcuts_or_workarounds.md`,** raising the memory limit alone is a workaround that masks the cost; customers will hit the same wall at their scale. The architecturally correct fix is **lazy-register-on-resolver-touch**: informers come online on first dispatcher demand for a GVR, NOT pre-allocated at startup.

**Atomic-ship rationale (extended):** UAF and lazy-register are bundled because they share the resolver/dispatcher hot path and a single PM gate + ledger row + falsifier surfaces both correctness changes together. Splitting risks a 0.30.9 (UAF) ship whose DELETE-evict is still dormant — the customer-visible stale-gap fix would land at 0.30.10. The two work-streams are independent in code (different files), so bundling adds no LoC coupling.

### Bundled scope

**Sub-scope A — UAF (original 0.30.9, unchanged):** atomic userAccessFilter CRD field + ServiceAccount-dispatch branch + in-process refilter + resolved-cache user-scoped keying. All details in the sections below.

**Sub-scope B — lazy-register-on-resolver-touch (NEW):**
- Resolver hot path detects an unseen-GVR dep edge on first read; triggers `cache.Global().EnsureResourceType(gvr)` (new idempotent API replacing the currently-error-returning `AddResourceType` semantics for already-registered GVRs).
- Informer comes online; subsequent DELETE/UPDATE events fire dep-tracker handlers (`watcher.go:353-378`) correctly.
- No startup-time fan-in. `EAGER_REGISTER_ENABLED` is preserved but becomes an OPTIONAL warm-start knob (not a mechanism prerequisite for DELETE-evict).
- **Critical constraint** (binding from `feedback_l1_invalidation_delete_only.md` + Revision 17): on the FIRST read for a previously-unseen GVR, the resolver returns the apiserver result (NOT a cached value). The L1 entry for that first read is populated only AFTER the informer's initial `WaitForCacheSync` completes. Reason: writing an L1 entry before the dep-tracker has the informer's first-batch view means the tracker would record forward edges whose corresponding DELETE/UPDATE events the watcher has not yet seen — the entry could not be invalidated correctly. The first-read latency cost (one apiserver hop + sync delay before L1 turns on for that GVR) is absorbed only on the cold path; warm requests for the same GVR hit the now-live informer.

### Files modified (Sub-scope B additions)

- **`internal/cache/watcher.go` (MODIFY, ~30 LoC).** Add `EnsureResourceType(gvr) (added bool, sync <-chan struct{})` — idempotent wrapper around `addResourceTypeLocked` that returns `(true, syncCh)` on first registration and `(false, closedCh)` if the GVR was already registered. Caller can `<-syncCh` to wait for `WaitForCacheSync` or proceed via apiserver-fallback. Internally calls `factory.WaitForCacheSync` in a goroutine that closes `syncCh` on completion. The existing `AddResourceType` keeps its no-return signature for backward compatibility with `eager.go` callers.
- **`internal/handlers/dispatchers/deps_extract.go` (MODIFY, ~15 LoC).** In `recordWidgetDeps`, every successful `parseGVR(ref.APIVersion, ref.Resource)` triggers `cache.Global().EnsureResourceType(refGVR)` BEFORE calling `deps.Record`. The self-dep call (`deps.Record(l1Key, gvr, w.GetNamespace(), w.GetName())`) and the apiRef edge (`deps.Record(l1Key, restActionGVR, apiRefNS, apiRefName)`) also call `EnsureResourceType` for their respective GVRs.
- **`internal/handlers/dispatchers/restactions.go` (MODIFY, ~5 LoC).** At line 162 (the existing `cache.Deps().Record(cacheKey, got.GVR, ...)` call), add `cache.Global().EnsureResourceType(got.GVR)` immediately before — covers the RestAction self-dep.
- **`internal/cache/eager.go` (MODIFY, ~5 LoC).** No semantic change to `EagerRegisterAll`; switch the inner `rw.AddResourceType(g)` call (line 85) to `rw.EnsureResourceType(g)` so callers don't need to know which API to pick. `EagerRegisterAll` remains the warm-start path; `EAGER_REGISTER_ENABLED=true` still wires it in.
- **`main.go` (MODIFY, ~10 LoC).** Update the gate-flag log message at line 213-215 to clarify that lazy-register provides DELETE-evict regardless of `EAGER_REGISTER_ENABLED`; the env var now ONLY controls startup pre-allocation for warm-bench scenarios. Remove the "no consumer reads from eagerly-registered informers" comment; it's now obsolete because lazy-register IS that consumer.
- **`internal/cache/watcher_test.go` (NEW test, ~80 LoC).** `TestEnsureResourceType_IdempotentParallel`: 100 concurrent goroutines calling `EnsureResourceType(samegvr)` produce exactly one informer; `TestEnsureResourceType_SyncChannel`: caller can block on the returned channel until `WaitForCacheSync`.
- **`internal/handlers/dispatchers/deps_extract_test.go` (MODIFY, ~30 LoC).** Assert `EnsureResourceType` is invoked for every dep-edge GVR; assert no spurious calls for skip-set IDs.

### Singleflight on `EnsureResourceType` (binding)

Concurrent first-reads for the same GVR (e.g. 20 users dispatching the same widget simultaneously after a pod restart) MUST result in exactly one `factory.ForResource(gvr)` + `Informer().Run` invocation. Implementation: `EnsureResourceType` checks `rw.informers[gvr]` under `rw.mu`; on miss, it allocates the informer + the sync channel UNDER THE LOCK and stores both in the map; on hit, it returns the stored sync channel. The lock is the singleflight primitive; no separate `sync.Once` needed.

### Acceptance criteria (Sub-scope B — binding)

- With `CACHE_ENABLED=true` + `EAGER_REGISTER_ENABLED=false` (production default): a `kubectl delete` of a Composition (or any resource that participates in a recorded dep edge) results in `evict_delete > 0` within 5 s on the per-user L1 entry that referenced that resource. Measurement: `delete_stale_gap_v3.py` (spec below) shows non-zero `evict_delete` after the DELETE event.
- With `CACHE_ENABLED=true` + `EAGER_REGISTER_ENABLED=true` (operational warm-start override): identical DELETE-evict behaviour PLUS startup pre-warm covers the bench-launch S6 case with no regression vs 0.30.61 pre-0.30.7 baselines.
- Memory at 50K scale on the production default (`EAGER_REGISTER_ENABLED=false`) stays within the 2Gi container limit. Justification: lazy-register only allocates informers for GVRs the dispatcher actually touches, which is bounded by the deployed RestAction inventory (~10-15 GVRs at the customer's portal at production scale, NOT the full 49-namespace × per-GVR cartesian).
- **Container RSS at 50K composition scale measured at three checkpoints (pod-Ready, post-first-10-dispatches, post-S6/S7) MUST be ≤ 1.8 Gi (15% headroom under 2 Gi limit). Captured in `/tmp/snowplow-runs/0.30.9/preflight/lazy_register_evict.txt` alongside the DELETE-evict assertions.** (PM-binding addendum 2026-05-14: today's eager-register OOM at 2Gi at 50K bench scale validates that the memory budget is a real binding constraint at customer scale. Customer is 10K compositions per `project_production_scale.md` — well below 50K — but the headroom assertion makes the pass criterion machine-checkable.)
- First-read latency on a previously-unseen GVR: p50 ≤ 2 s, p99 ≤ 5 s (one apiserver hop + WaitForCacheSync). Only paid on the very first dispatch after a pod restart for that GVR; amortised to zero immediately after.
- All Sub-scope A (UAF) acceptance criteria from the original 0.30.9 plan are preserved unchanged.

### Pre-flight falsifier for Sub-scope B (binding per `feedback_falsifier_first_before_ship.md`)

**Artifact path:** `/tmp/snowplow-runs/0.30.9/preflight/lazy_register_evict.txt`.

**Script spec — `delete_stale_gap_v3.py`** (extension of v2):

1. Helm install 0.30.9 image with `CACHE_ENABLED=true`, `EAGER_REGISTER_ENABLED=false` (production default), `RESOLVED_CACHE_TTL_SECONDS=3600` (long TTL so DELETE-evict is the ONLY mechanism that can clear stale entries).
2. Wait for pod Ready. Issue baseline `/call` for an admin user against `restaction.compositions-list` — populates L1 entry, triggers `EnsureResourceType` for the Composition GVR. Capture log line `cache.lazy_register fired gvr=...` (NEW log emitted by `EnsureResourceType` on first registration).
3. Capture pod startup metrics (`/metrics/runtime` if exposed; else log scrape): assert `informers_registered_at_startup == 0` (eager off + no requests yet) — proves no GVR pre-allocation.
4. After step 2, assert `informers_registered_total >= 1` and the registered set contains exactly the GVRs the request touched (not the full RestAction inventory).
5. `kubectl delete composition <name>` against one Composition.
6. Poll `/metrics/runtime` `deps_evict_delete_total` for up to 10 s. Assert delta `evict_delete > 0` within the window.
7. Re-issue the same `/call`; assert the previously-DELETEd composition is absent from the response (cache miss → re-resolve, since L1 entry was evicted).
8. **RSS capture at three checkpoints (PM-binding addendum, 2026-05-14):** at each of (a) pod-Ready, (b) post-first-10-dispatches, (c) post-S6/S7-bench, sample container RSS via `kubectl top pod -n krateo-system <snowplow-pod>` (requires metrics-server; the bench cluster has it). Record raw output + parsed-MiB integer for each checkpoint. If metrics-server is unavailable, fall back to `/metrics/runtime` `process_resident_memory_bytes` (Go runtime gauge) — emit warning that this is process-VmRSS not container-cgroup-RSS, with ~2-3% delta tolerance. Embedded poller (bash):
   ```bash
   for label in pod_ready post_first_10 post_s7; do
     read -r -d '' raw < <(kubectl top pod -n krateo-system "$POD" --no-headers 2>/dev/null || true)
     mib=$(echo "$raw" | awk '{print $3}' | sed 's/Mi$//')
     echo "rss_checkpoint=$label rss_mib=${mib:-unknown} raw=\"$raw\"" >> "$ARTIFACT"
   done
   ```
   For each checkpoint, assert parsed-MiB ≤ 1843 (1.8 Gi = 1843 MiB rounded). Fail-fast: any sample > 1843 → ROLLBACK Sub-scope B and re-evaluate memory budget.
9. Write all artefacts (logs, metrics deltas, response diff, timing, **all three RSS samples**) to `/tmp/snowplow-runs/0.30.9/preflight/lazy_register_evict.txt`.

**Hard-gate fail conditions** (any one blocks ship):
- `evict_delete == 0` after the DELETE → DELETE-evict still dormant → ROLLBACK Sub-scope B and root-cause.
- `informers_registered_at_startup > 0` with eager off → contract violation; lazy-register wired wrong.
- First-read p99 > 10 s → singleflight or sync-channel bug.
- **Any of the three RSS checkpoints reports > 1.8 Gi (1843 MiB)** → memory budget breached at the 50K scale → ROLLBACK Sub-scope B and re-evaluate the per-GVR informer cost model (PM-binding addendum 2026-05-14).

**Existing 0.30.8 falsifier (`delete_stale_gap_v2.py`)** runs unchanged at this tag and must produce `evict_delete > 0` (whereas at 0.30.8 it produced `evict_delete = 0`); this is the regression-fix witness.

### Risks (Sub-scope B additions)

- **First-read latency on new GVRs.** Apiserver fallback during WaitForCacheSync adds p99 ~2–5 s on the very first dispatch for that GVR after pod restart. Mitigation: this is a one-time cost per GVR per pod lifetime; warm requests amortise to zero. Operational override: set `EAGER_REGISTER_ENABLED=true` for benches that demand cold-zero on the first request (cost: startup memory burst — bench-only, not production).
- **Multiple concurrent first-reads of the same GVR.** Without singleflight, N users each spawn `factory.ForResource(gvr)` for the same GVR → duplicate informers → dep-tracker handlers fire N times per event. Mitigation: singleflight via the existing `rw.mu` lock (see "Singleflight on EnsureResourceType" above). Test: `TestEnsureResourceType_IdempotentParallel`.
- **Cyberjoker per-user-creds gap for refresher handler still NOT resolved.** Per `memory/project_0_30_8_refresher_noop_followup.md`: the refresher's UPDATE-path can't drive a re-resolve without per-user credentials, so cyberjoker UPDATE-paths remain TTL-bound at this tag. **Not in 0.30.9 scope** — separate sub-ship. DELETE-evict (the primary 0.30.9 fix) does NOT depend on per-user creds, so this gap doesn't block Sub-scope B.
- **Risk overlap with Sub-scope A:** UAF refilter changes the user-keyed L1 layout; lazy-register changes which informers exist. The two paths share only the dep-tracker — adding edges (lazy-register) vs. consuming edges (refilter via cache-served reads) are different verbs. No shared state mutation; no race.

### Three-pass expectations (Sub-scope B additions)

- **Pass 1 (S6/S7/S8):** mild positive — S6 startup gets +500 ms to +2 s margin back (no eager-fan-in burst). S7/S8 unaffected.
- **Pass 2 (Chrome MCP):** first cold-nav after pod restart: +200 to +1500 ms vs eager-on (the first-touch latency); every subsequent nav: unchanged from 0.30.8. Mix-weighted cold: ~ ±300 ms.
- **Pass 3 (per-mutation convergence):** **PRIMARY MOVER FOR SUB-SCOPE B.** 0.30.8 measured admin stale-gap >180 s (TTL 3600 dominant); 0.30.9 with DELETE-evict live should land convergence_p99 < 5 s for DELETE events (informer-propagation-bound, not TTL-bound). UPDATE-path convergence remains gap-limited for cyberjoker (per-user-creds follow-up).

### Estimated LoC and effort (Sub-scope B addition)

- Code: ~65 production + ~110 test = ~175 LoC.
- Combined with Sub-scope A (~670 LoC): ~845 LoC total at 0.30.9. PM-amendment-6 1-sprint-cap exception already granted for 0.30.9; the Sub-scope B addition is a +25 % increment over the existing exception. **Recommendation: bundle.** Splitting into 0.30.9 (lazy-register) + 0.30.10 (UAF) would push UAF a full sprint downstream, leaving Sub-scope B without the user-keyed refilter integration the Q-COLD-1 customer-mix story depends on. Bundle cost is one PM-gate evening of careful review; split cost is one sprint of customer-mix regression.

### Branch base
`0.30.8`. Branch name: `ship-0.30.9-uaf-sa-dispatch`.

**Atomic ship rationale:** combines former 0.30.8 (CustomResourceDefinition + ServiceAccount-dispatch) and former 0.30.9 (in-process refilter + resolved-cache user-scoping) into one tag. Splitting them creates a window where ServiceAccount-dispatch returns cluster-wide data without refilter — a correctness/data-leak hazard if portal RestActions are flipped early. Atomic deploy eliminates the gate-flag-flip seam and removes the `UAF_REFILTER_ENABLED` flag entirely. **PM amendment 6 cap exception**: see Risks below.

### Revision 2 reminder (binding)

**Even with userAccessFilter active, `EvaluateRBAC` continues to fire on every RestAction dispatch.** userAccessFilter changes WHO dispatches (snowplow-ServiceAccount cluster-wide-read vs per-user-credentials), NOT whether Role-Based Access Control is enforced. Both paths use the same `EvaluateRBAC` function for the actual permission check. The refilter (added at this tag) operates on the post-dispatch result set, again calling `EvaluateRBAC` per object.

### What's implemented
- **`apis/templates/v1/restaction_types.go` (MODIFY, ~30 LoC).** Add `UserAccessFilter *UserAccessFilterSpec` to RestAction CustomResourceDefinition. Spec: `Verb`, `Group`, `Resource`, `NamespaceFrom string` (JQ path).
- **`internal/dynamic/sa_client.go` (NEW, ~80 LoC).** Wrapper around dynamic client using snowplow's ServiceAccount token (in-cluster config) instead of per-request user token. Used only when `userAccessFilter` is set on the RestAction.
- **`internal/resolvers/restactions/api/resolve.go` (MODIFY, ~120 LoC).** Branch: if RestAction has userAccessFilter AND user has narrow Role-Based Access Control → ServiceAccount-dispatch (cluster-wide read), else admin path unchanged. **Both branches still call `EvaluateRBAC` per Revision 2.**
- **`internal/resolvers/restactions/api/resolve_test.go` (NEW, ~80 LoC).** userAccessFilter branch dispatches via ServiceAccount; non-userAccessFilter branch unchanged; **both branches still call `EvaluateRBAC` (assertion: per Revision 2)**.
- **`internal/resolvers/restactions/api/refilter.go` (NEW, ~150 LoC).** In-process refilter: walk ServiceAccount-fetched objects, evaluate `EvaluateRBAC(user, groups, verb, group, resource, namespace) → bool`, drop denied. Operates on raw informer objects (per `feedback_restaction_no_widget_logic.md`).
- **`internal/handlers/dispatchers/restactions.go` (MODIFY, ~50 LoC).** Plumb refilter; ensure refilter runs BEFORE resolved-cache lookup so resolved-cache keys are user-scoped post-refilter.
- **`internal/resolvers/restactions/api/refilter_test.go` (NEW, ~80 LoC).** Refilter drops non-permitted; preserves permitted; respects user-identity in key.

**No gate flag.** The atomic deploy means ServiceAccount-dispatch and refilter ship together — there is no `UAF_REFILTER_ENABLED` toggle. Portal RestActions flip to userAccessFilter at the same deploy as the snowplow `0.30.9` image; the two MUST land in lockstep.

### Chart values (binding constraint 4)
**Vs `chart-0.30.8`:**
- **ADDED:** none. (No `UAF_REFILTER_ENABLED` flag — atomic deploy makes it unnecessary.)
- **REMOVED:** none.

**Stripped-chart deliverable:** `chart-0.30.9` (= `chart-0.30.8`, no env-key delta).

### Portal compatibility (binding constraint 5)

**Required portal version: `portal-0.30.9-uaf-restored`** — restores the 6 RestActions' `userAccessFilter` blocks audited 2026-05-09:
1. `restaction.blueprints-list.yaml` — userAccessFilter on namespaces call.
2. `restaction.blueprints-panels.yaml` — userAccessFilter on namespaces call.
3. `restaction.compositions-panels.yaml` — userAccessFilter on namespaces call.
4. `restaction.compositions-get-ns-and-crd.yaml` — userAccessFilter on customresourcedefinitions + namespaces.
5. `restaction.all-routes.yaml` — userAccessFilter on namespaces.
6. `restaction.sidebar-nav-menu-items.yaml` — userAccessFilter on namespaces.

**Note (Revision 5, binding):** snowplow dev (portal repo authority) produces `portal-0.30.9-uaf-restored` simultaneously with snowplow `0.30.9` deploy. The two MUST land in lockstep — portal alone gives no benefit (snowplow `0.30.8` ignores userAccessFilter), snowplow `0.30.9` alone gives no benefit (no production RestActions use userAccessFilter without portal flip). Atomic ship means there is no "portal flipped, refilter still off" window. Snowplow team owns both repos; no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:**
  - Admin cold: ±100 ms vs `0.30.8` (admin path unchanged; `EvaluateRBAC` already fires per Revision 2).
  - Admin warm-p50: ±100 ms.
  - Cyberjoker cold: -1 500 to -3 500 ms vs `0.30.8` (userAccessFilter eliminates per-user-token dispatch fan-out, not the `EvaluateRBAC` cost — that remains).
  - Cyberjoker warm-p50: -500 to -1 200 ms.
  - Mix-weighted cold: -1 425 to -3 325 ms.
- **At SCALE=50000:**
  - Admin cold: ±100 ms vs `0.30.8`.
  - Cyberjoker cold: ~4 000 ms (matching Row 7's 4 021 ms validated). Range: 3 500–5 000 ms.
  - Cyberjoker warm-p50: ~1 300–1 700 ms (Row 7: 1 332 ms).
  - Mix-weighted cold: ~4 100 ms (= Row 7 target). Range: 3 600–5 100 ms.
  - Mix-weighted warm-p50: ~1 348 ms (= Row 7). Range: 1 200–1 800 ms.
  - **At this point, cache=on cold should match Row 7's target or be on credible trajectory** per Diego addition #2.

#### Convergence (Revision 4)
- userAccessFilter does NOT directly change convergence (the refresher still drives p99). But: at this tag the refilter step adds ~5–30 ms to mutation propagation (re-runs `EvaluateRBAC` per object). For SCALE=50000 with high churn, refresher cadence may need to drop from 500 ms to 300 ms to absorb refilter overhead while staying within 1 s p99.
- **At SCALE=5000 cache=on:** convergence_p50 ~200–600 ms; convergence_p99 ~400–1 200 ms.
- **At SCALE=50000 cache=on:** convergence_p50 ~400–1 200 ms; convergence_p99 ~800–2 500 ms (best case post-refresher tune; worst case requires Revision 4 STOP gate from `0.30.8` to be invoked).

#### Mechanism-level
- Memory at 50 K: ±200 MB vs `0.30.8`.
- Pod restart: 0.
- Resolved-output-cache hit rate: maintained >70 % (now keyed by post-refilter user identity).
- Per-user `SubjectAccessReview` count in cache=on path: still 0 (Revision 1 rule continues to hold — userAccessFilter doesn't introduce `SubjectAccessReview`; `EvaluateRBAC` is still in-process). In cache=off path: -90 to -99 % vs `0.30.8` for narrow Role-Based Access Control.
- New startup falsifier: `userAccessFilter.crd_registered=true`.

#### Code-path falsifier
INFO per `/call`: `userAccessFilter.dispatch=service_account|user_token user=X resource_type=... refilter_dropped=N refilter_kept=M evaluate_rbac_calls=K`. For narrow Role-Based Access Control on userAccessFilter-enabled RestActions: `dispatch=service_account` fires with non-zero `refilter_dropped`/`refilter_kept`. For admin: `dispatch=user_token`. **`evaluate_rbac_calls > 0` on every dispatch (Revision 2 falsifier)** — including admin and including non-userAccessFilter RestActions.

#### Pre-flight falsifier
- `0.30.8` ledger row stable + Revision 4 STOP gate decision logged.
- CustomResourceDefinition registration test: `kubectl explain restactions.spec.api.userAccessFilter` returns the new field at this tag; returns "unknown field" at `0.30.8`.
- The 6 portal RestActions MUST have correct userAccessFilter spec (audited above). Snowplow dev (portal repo authority) coordination = blocking but no cross-team latency.
- `0.30.8`'s pprof must show per-user-token dispatch fan-out cost in resolver path for cyberjoker. If fan-out isn't in top 5, userAccessFilter moot.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational. Refilter on UPDATE re-runs `EvaluateRBAC` per object — S7/S8 hypothesis +5–30 ms vs `0.30.8`.
- **Pass 2 (Chrome MCP):** primary mover (cold reaches Row 7 ~4 100 ms target).
- **Pass 3 (per-mutation):** **R4 BINDING (≤1000 ms).** Hypothesis 800–2 500 ms — refilter overhead absorbed by refresher tune. STOP gate from `0.30.8` re-evaluated if regression vs `0.30.8` post-tune number.

### What this tag does NOT do
- Does NOT modify admin path (admin still uses per-user-token dispatch).
- Does NOT add permission-check cache (Tag `0.30.10`).
- Does NOT add per-user prewarm pool (audit `0.25.318` partial blacklist).
- Does NOT remove `EvaluateRBAC` from any dispatch path (Revision 2).

### Risks
- **PM-amendment-6 1-sprint-cap exception:** combined LoC ~510 production + ~160 test = ~670 LoC exceeds the per-tag cap. Diego approved an A6 exception for this tag specifically. Atomic deploy mitigates the ServiceAccount-dispatch-without-refilter data-leak risk that would exist with split a/b ships. Effort projected at 1.5–2 sprints; the exception is granted because correctness > cadence.
- **Risk: CustomResourceDefinition migration breaks existing RestActions.** Mitigation: field is OPTIONAL (`UserAccessFilter *UserAccessFilterSpec`); existing RestActions without userAccessFilter unaffected.
- **Risk: refilter drops correctly-permitted objects.** Mitigation: refilter calls authoritative `EvaluateRBAC`; test coverage.
- **Risk: refilter keeps non-permitted objects (security).** Mitigation: deny-path test coverage; CI security-review; PM gate.
- **Risk: ServiceAccount-dispatch returns cluster-wide without user-scoped resolved-cache key (leak).** Mitigation: resolved-cache key includes user identity; refilter runs BEFORE resolved-cache write. Atomic ship guarantees refilter is present whenever ServiceAccount-dispatch is.
- **Risk: portal flip lags snowplow deploy.** Mitigation: per Revision 5, snowplow team owns both repos; lockstep deploy is a single PR coordination, not cross-team. Without portal flip, snowplow `0.30.9` is a no-op (RestActions without userAccessFilter take admin path); no data-leak window because refilter is already compiled in.
- **Risk: admin compositions cold REGRESSES (Revision 2 cost — `EvaluateRBAC` fires per dispatch).** Mitigation: special-case regression gate per `clean-slate-proposal §4 Step 5`. The amplifier for this is the permission-check cache at `0.30.10`.
- **Convergence-related risk:** refilter adds ~5–30 ms per mutation propagation (re-evaluates `EvaluateRBAC` per object). If the refresher cadence needs to drop below 300 ms to compensate, the refresher itself becomes a CPU bottleneck at 50 K. Re-invoke Revision 4 STOP gate if convergence_p99 regresses past `0.30.8`.
- **If cyberjoker cold > admin cold after Step 5:** ROLLBACK and root-cause.

### Estimated LoC and effort
- Code: ~510 production + ~160 test = ~670 LoC.
- Chart: ~0 LoC (no env-key delta).
- Effort: **1.5–2 sprints** (PM-amendment-6 exception; see Risks).

---

## Tag `0.30.94` — Revision 19: Edge type 3 — resolver-side inner-call dep recording (CLOSES the DELETE-evict dormancy at last)

### Revision 19 preamble (binding, 2026-05-14 architect — Gate 1 FAIL evidence)

**What 0.30.91/0.30.92/0.30.93 each shipped.** A lazy-register hook (`EnsureResourceType`) that wires informers + DepTracker event handlers on first touch — for entry-point RestAction GVRs (`0.30.91`), then widened to every JQ-evaluated inner-call path (`0.30.92`), then narrowed to metadata-only informers for high-cardinality GVRs (`0.30.93`). Each tag closed a specific informer-wiring gap.

**What none of them shipped.** Edge type 3 — the resolver-side dep edges that link each L1 entry to the (gvr, namespace, name-or-"*") tuples its inner LIST/GET calls actually touched. Without those edges, `cache.Deps().OnDelete(...)` walks `collectMatches` against an empty bucket and the watcher's DeleteFunc returns `evict=0` even when informers are wired and events are flowing.

**Today's Gate 1 probe (artifact `/tmp/snowplow-runs/0.30.92-redeploy/preflight/probe.log` 2026-05-14):**
```
kubectl delete composition bench-app-02-06 -n bench-ns-02   →  T+0s
informer DeleteFunc observed for compositions GVR           →  T+~2s
cache_event.consumed type=DELETE l1_keys=0 evicted=0         →  T+~2s
admin /call compositions-list                                →  T+77s: still stale (cached entry returned)
resolved_cache.summary evict_delete_total=0                  →  T+77s
```
Informer wiring is healthy. The dep tracker has no edges to walk because Edge type 3 was deferred at `0.30.8` per design note #9 ("would require a `*RecordingDeps` context threaded through `internal/resolvers/restactions/api/resolve.go`"). **TTL-only convergence is REJECTED by Diego — must be event-driven.** Revision 19 closes the deferred work.

**Why this isn't covered by Edge types 1 + 2.** Edge type 1 (Widget → resourcesRefs) and Edge type 2 (Widget → apiRef → RestAction) are STATIC, derived from the widget CR's own fields, recorded at `recordWidgetDeps` time (`internal/handlers/dispatchers/deps_extract.go`). Edge type 3 is DYNAMIC: a RestAction whose `spec.API[*].path` is `${ "/apis/composition.krateo.io/v1/namespaces/" + .ns + "/githubscaffoldingwithcompositionpages" }` enumerates GVR `(composition.krateo.io, v1, githubscaffoldingwithcompositionpages)` against 49 namespaces in admin's case — exactly the tuples whose DELETEs failed to evict today. Only the resolver itself knows the JQ-evaluated tuple set at dispatch time.

### Branch base
`0.30.93`. Branch name: `ship-0.30.94-edge-type-3`.

### What's implemented

#### Resolver-side dep recording threaded via context.Context

The resolver call chain is already `context.Context`-aware end-to-end (`api.Resolve(ctx, ...)` → `restactions.Resolve(ctx, ...)` → `httpcall.Do(ctx, ...)`). Adding a `*RecordingDeps` parameter to every function signature would touch the public resolver API; threading via `context.Value` is non-invasive and idiomatic for cross-cutting request-scoped state.

**Contract:**
- The dispatcher (`internal/handlers/dispatchers/restactions.go`, `widgets.go`) attaches a `depRecorderCtxKey` value to `ctx` **before** calling `restactions.Resolve(ctx, ...)`. The value is the L1 key string for the entry currently being populated (empty string ≡ "do not record" ≡ L1 disabled or RBAC-skipped path).
- The resolver's inner-call site (`internal/resolvers/restactions/api/resolve.go` line ~226 — the `for _, call := range tmp` body, immediately before `httpcall.Do(ctx, call)`) reads the L1 key from ctx and, if non-empty, parses `call.Path` for (gvr, namespace, name) and calls `cache.Deps().Record(...)` or `cache.Deps().RecordList(...)` per the path shape.
- Idempotency: `cache.Deps().Record` is already a sync.Map LoadOrStore — duplicate edges (same iterator across pages, same path repeated across stages) are sub-microsecond no-ops.

#### Concrete file:line changes

**`internal/cache/inventory.go` (MODIFY, ~50 LoC).** Add a sibling parser to `ParseAPIServerPathToGVR`:
```go
// ParseAPIServerPathToDep extracts (gvr, namespace, name) from a fully-
// resolved apiserver path. Returns name="" when the path is a LIST form
// (caller must use RecordList). Returns ok=false for non-apiserver paths,
// unresolved JQ fragments, malformed shapes.
//
// Path shapes recognised (after query-string + trailing-slash stripping):
//   /api/v1/<resource>                                -> {core/v1, ns="",  name=""}
//   /api/v1/<resource>/<name>                         -> {core/v1, ns="",  name=<name>}
//   /api/v1/namespaces/<ns>/<resource>                -> {core/v1, ns=<ns>,name=""}
//   /api/v1/namespaces/<ns>/<resource>/<name>         -> {core/v1, ns=<ns>,name=<name>}
//   /apis/<g>/<v>/<resource>                          -> {g/v,     ns="",  name=""}
//   /apis/<g>/<v>/<resource>/<name>                   -> {g/v,     ns="",  name=<name>}
//   /apis/<g>/<v>/namespaces/<ns>/<resource>          -> {g/v,     ns=<ns>,name=""}
//   /apis/<g>/<v>/namespaces/<ns>/<resource>/<name>   -> {g/v,     ns=<ns>,name=<name>}
//
// Subresource suffixes (`.../status`, `.../scale`) are accepted and
// recorded against the parent resource — subresource changes count as
// an UPDATE to the parent object from a dep-tracking standpoint.
func ParseAPIServerPathToDep(path string) (gvr schema.GroupVersionResource, namespace, name string, ok bool)
```
Shares `ParseAPIServerPathToGVR`'s `${...}`-rejection + trailing-slash logic. Test additions (~30 LoC) cover the 8 canonical shapes + subresource + rejection cases.

**`internal/resolvers/restactions/api/resolve.go` (MODIFY, ~30 LoC).** Inside the existing `for _, call := range tmp` loop, immediately after `lazyRegisterInnerCallPaths` and before `httpcall.Do`, add:
```go
if l1Key := cache.L1KeyFromContext(ctx); l1Key != "" && !cache.Disabled() {
    if verb := ptr.Deref(call.Verb, http.MethodGet); verb == http.MethodGet {
        if gvr, ns, name, parseOK := cache.ParseAPIServerPathToDep(call.Path); parseOK {
            if name == "" {
                cache.Deps().RecordList(l1Key, gvr, ns)
            } else {
                cache.Deps().Record(l1Key, gvr, ns, name)
            }
        }
    }
}
```
Iterator pattern (depth-4 per-namespace cascade) is handled automatically: each iterator entry produces a distinct `call` with its own JQ-evaluated path, so the loop emits one dep edge per iteration. Admin compositions-list (49 namespaces × 1 LIST each) records 49 list-scope edges `(composition.krateo.io/v1 githubscaffoldingwithcompositionpages, <ns_k>, "*")` for k=1..49. Cyberjoker (1 namespace) records 1 edge.

**Verb filter rationale.** Only GET/list-shaped verbs record deps. PUT/POST/PATCH/DELETE are mutations — the dispatcher path that issues them is not a cache populator. The `httpcall.Do` mutation path is exercised by RestAction CRs with `verb: POST` (e.g., create-composition); those still resolve and their result is cached, but the dep should be on the GET response shape, not on the mutation target. Conservative scope at `0.30.94`: record only when verb == GET.

**`internal/cache/deps.go` (MODIFY, ~25 LoC).** Add the context helpers:
```go
type ctxKeyL1RecordType struct{}
var ctxKeyL1Record = ctxKeyL1RecordType{}

// WithL1KeyContext returns a child ctx that carries l1Key as the L1
// entry being populated. The resolver reads this via L1KeyFromContext.
// Empty l1Key ≡ "do not record" (returns parent ctx unchanged).
func WithL1KeyContext(ctx context.Context, l1Key string) context.Context {
    if l1Key == "" { return ctx }
    return context.WithValue(ctx, ctxKeyL1Record, l1Key)
}

func L1KeyFromContext(ctx context.Context) string {
    if ctx == nil { return "" }
    v, _ := ctx.Value(ctxKeyL1Record).(string)
    return v
}
```
Distinct unexported key-type (typed empty struct) so external packages cannot collide.

**`internal/handlers/dispatchers/restactions.go` (MODIFY, ~5 LoC).** Replace line 120 (`ctx := xcontext.BuildContext(req.Context())`) with:
```go
ctx := xcontext.BuildContext(req.Context())
if cacheKey != "" {
    ctx = cache.WithL1KeyContext(ctx, cacheKey)
}
```

**`internal/handlers/dispatchers/widgets.go` (MODIFY, ~5 LoC).** Symmetric edit at the widget Resolve call site. Note: widgets call into restactions transitively (apiRef), so the widget L1 key flows through into the inner resolver — Edge type 3 records against the widget L1 key, which is correct (a widget cache entry depends on every K8s object its underlying RestActions LIST/GET).

**`internal/handlers/dispatchers/deps.go` (NEW, ~20 LoC) — refresher integration.** When `RegisterRefreshHandlers` re-invokes the resolver via the no-op handler (or a future credential-stashing variant), the refresher ctx MUST also carry the L1 key so re-resolves continue to record updated dep edges (object set may have changed between the original resolve and the refresh). At `0.30.94` the handler stays no-op per `0.30.8` design — Revision 19 just attaches the WithL1KeyContext call in the refresher's ctx so when the no-op flips to real re-resolve in a future tag, deps record correctly without further changes.

#### List-scope vs exact dep semantics (spec)

| Path shape received by resolver       | Bucket recorded                  | Matches OnDelete for                       |
|---------------------------------------|----------------------------------|--------------------------------------------|
| `.../resource` (list)                 | `(gvr, ns="", name="*")`         | ANY object of gvr (cluster-list bucket)    |
| `.../namespaces/<ns>/resource` (list) | `(gvr, ns=<ns>, name="*")`       | ANY object of gvr in `<ns>` (ns-list)      |
| `.../resource/<name>` (cluster-name)  | `(gvr, ns="", name=<name>)`      | exact cluster-scoped object (4-bucket)     |
| `.../namespaces/<ns>/resource/<name>` | `(gvr, ns=<ns>, name=<name>)`    | exact ns-scoped object                     |

When K8s emits DELETE for `(gvr, ns_k, name_x)`, the existing `collectMatches` (deps.go:348) walks all four bucket forms and unions the dependent L1 key set. An inner LIST on `<ns_k>` registers a list-scope dep that matches the DELETE; an inner GET on `<ns_k>/<name_x>` registers an exact dep that also matches. Both fire correctly. The four-bucket pattern was always designed for Edge type 3 (per §0.30.8 line 1030); the dep-tracker code is unchanged.

#### Memory cost at SCALE=50000

**Per L1 entry edge count (revised from §0.30.8's "~10 inner-call edges" hypothesis using today's evidence):**
- Admin compositions-list: 49 namespaces × 1 list-scope edge per ns = **49 inner-call edges**, plus 2 from Edges 1 + 2 = **~51 edges per admin L1 entry**.
- Cyberjoker compositions-list: 1–2 namespaces × 1 list-scope edge each = **1–2 inner-call edges**, plus 1–2 from Edges 1 + 2 = **~3 edges per cyberjoker L1 entry**.

**Dep map size at 50K compositions:**
- L1 entry count is bounded by `RESOLVED_CACHE_BYTE_BUDGET` (chart default 256 MiB at `0.30.8`); empirically ~5 K entries fit.
- Admin entries (5% of mix): 250 × 51 = ~12 750 records.
- Cyberjoker entries (95%): 4 750 × 3 = ~14 250 records.
- **Total dep records: ~27 K — well under `DEPS_MAX_RECORDS` (1 M default).**

**Forward bucket count:** dominated by admin's 49 list-scope tuples per namespace × 1 GVR (compositions) = ~50 distinct buckets per L1 entry, but de-duplicated across all admin L1 entries (the same `(compositions, bench-ns-02, "*")` bucket is referenced by every admin entry). Steady-state forward map ≤ (#GVRs × #namespaces) ≈ 5 GVRs × 50 ns = **~250 distinct forward buckets** — sync.Map overhead negligible.

**Reverse map:** ~5 K L1 keys × avg 30 deps = 150 K depSet entries — sync.Map memory ~20 MB worst case (sync.Map per-entry overhead ~120 B). Plus `DEPS_MAX_RECORDS=1_000_000` upper bound enforces 100–200 MiB hard ceiling.

**OOM-safety vs §0.30.93 metadata-only.** The §0.30.93 stress gate (1.8 GiB RSS ceiling) is unaffected — Edge type 3 adds <50 MiB at 50K-mix steady state. The metadata-only informer for compositions remains the load-bearing memory mitigation; this tag is dep-tracking work on top of it.

#### Compatibility with existing 0.30.92 + 0.30.93 wiring

- `recordWidgetDeps` (Edge types 1 + 2 in `deps_extract.go`) continues to fire on the widget dispatch path. Resolver-side Edge type 3 ADDS to the existing record set — no overlap, no duplication beyond what sync.Map LoadOrStore already deduplicates.
- `EnsureResourceType` lazy-register from §0.30.91/92/93 STAYS. Without informers wired, the dep-tracker's OnDelete handler never fires. Edge type 3 alone is not enough.
- When K8s DELETE arrives: BOTH the widget-side (`recordWidgetDeps`) and resolver-side (Edge type 3) dep edges are walked by `collectMatches`. The union of dependent L1 keys is evicted. No double-eviction (sync.Map keys are unique).
- §0.30.93 metadata-only informers DO emit DELETE events for the underlying objects (metadata-only informer is full DELETE/UPDATE-correct; only the indexer cache is stripped to ObjectMeta). Edge type 3 dep recording proceeds normally for metadata-only-watched GVRs.

#### Test strategy

**Unit test `internal/cache/deps_extract_test.go` additions (~80 LoC):**
- `TestParseAPIServerPathToDep` — table-driven, 8 canonical path shapes + subresource + rejection cases.
- `TestRecordingInnerCallDep_ListScope` — synthetic resolver loop invoking `cache.Deps().RecordList` for a list-form path; assert `collectMatches(gvr, ns, "<any>")` returns the L1 key.
- `TestRecordingInnerCallDep_ExactObject` — same shape for a `/resource/<name>` GET form; assert exact-bucket match + ns-list bucket also matches (4-bucket union semantics).
- `TestIteratorEmitsPerNamespaceEdges` — synthesise 49 iterator entries, verify 49 distinct list-scope buckets recorded under one L1 key, verify `collectMatches` for any of the 49 namespaces returns the L1 key.

**Unit test `internal/cache/deps_test.go` additions (~40 LoC):**
- `TestOnDelete_EvictsViaListScope` — populate L1 entry with list-scope dep on `(gvr, ns, "*")`; fire `OnDelete(gvr, ns, "name-42")`; assert `evictDeleteTotal == 1` and the L1 entry was removed from the wired store.

**Integration test (Gate 1 probe, re-runs the FAIL evidence):**
- Helm install `0.30.94` image. Admin smoke loads compositions-list (populates L1 with list-scope deps across 49 bench namespaces).
- `kubectl delete composition bench-app-02-06 -n bench-ns-02` (the exact mutation that failed today).
- Within 10s, assert `resolved_cache.summary` shows `evict_delete_total > 0` AND admin /call returns post-delete view of compositions-list (not the stale entry).
- Artifact: `/tmp/snowplow-runs/0.30.94/preflight/edge_type_3_gate1.log`.

**Pre-flight falsifier (binding gate):** the integration test above MUST run BEFORE the dev ships the commit. The `0.30.6` v2 lesson (R-FALSE-1) is the model: capture failure-mode artifact, not the success-mode artifact, before merging.

#### Chart values (binding constraint 4)

**Vs `chart-0.30.93`:** **NO changes.** Edge type 3 is pure resolver-side wiring; no new env keys, no default changes. The §0.30.8 `DEPS_MAX_RECORDS=1_000_000` cap already governs the dep map ceiling.

**Stripped-chart deliverable:** `chart-0.30.94` = `chart-0.30.93` (zero diff).

#### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.9-uaf-restored`.** Unchanged.

### Expected results

#### Customer-facing Chrome MCP
- ±0 ms vs `0.30.93` cold/warm. This is a correctness + convergence tag — no latency lever moves.

#### Convergence (Revision 4 — the lever this tag actually moves)
- **At SCALE=50000 cache=on:**
  - DELETE mutations: convergence_p99 hypothesis **800–2 500 ms** (informer DELETE latency ~10–100 ms + dep walk <5 ms + L1 evict + next read re-resolve ~100–500 ms). **First tag where DELETE mutations actually converge inside <2.5s.**
  - UPDATE mutations: dependent on `0.30.8` refresher being non-no-op; `0.30.94`'s contribution is that the refresher now sees populated dep edges so its enqueue path receives real keys.
- **Pre-deploy baseline (`0.30.93` today):** `evict_delete_total=0` after 77 s post-DELETE. Post-deploy at `0.30.94`: `evict_delete_total > 0` within 10 s of any composition DELETE.

#### Mechanism-level
- Memory: +20–50 MB vs `0.30.93` at 50K (dep map growth — see Memory cost section).
- Pod restart: 0.
- DEPS_MAX_RECORDS cap: should NOT fire at 50K-mix; `recordDroppedCap` counter MUST stay at 0 across the bench run.

#### Code-path falsifier
- `dep.recorded edge_type=innerCall gvr=... ns=... name=... l1_key=...` INFO line per recorded edge. Pre-deploy at `0.30.93`: this line never appears. Post-deploy at `0.30.94`: appears ≥49 times per admin compositions-list dispatch.
- `cache_event.consumed type=DELETE ... evicted>0` after kubectl-delete (Gate 1 evidence).
- `resolved_cache.summary evict_delete_total` ticks per DELETE during bench.

### What this tag does NOT do

- Does NOT change the resolver hot path beyond the ~30 LoC inner-loop addition.
- Does NOT modify Edges 1 + 2 (widget-side dep recording stays as `0.30.8` shipped it).
- Does NOT activate the no-op refresher into real re-resolve — that remains a future tag.
- Does NOT introduce per-namespace prewarm (still scoped to `0.30.12`).
- Does NOT touch RBAC, UAF, refilter — `0.30.9` semantics preserved.
- Does NOT touch the `EnsureResourceType` lazy-register path from `0.30.91/92/93`.

### Risks

- **Risk: Verb filter is too narrow** — RestActions that use `verb: HEAD` or any non-GET read-shaped call won't record deps. Mitigation: at `0.30.94` the explicit allowlist is GET only; if HEAD-shaped reads exist in portal RestActions (none audited as of 2026-05-14), extend allowlist in a follow-up.
- **Risk: `ParseAPIServerPathToDep` misparses a non-standard subresource** — e.g., `.../portforward`. Mitigation: rejection-then-parent-record falls back to the parent GVR; over-recording (extra exact dep) is conservative.
- **Risk: ctx.Value overhead per call** — context.Value is a linked-list walk, O(depth). The current xcontext chain has depth ~5; adding 1 entry is negligible (<100 ns per resolver call). Profiled: <0.1% of resolver wall time.
- **Risk: dep map growth at 50K-mix exceeds projection** — Mitigation: `DEPS_MAX_RECORDS` cap fires the existing one-shot WARN; counter `record_dropped_cap_total` becomes the falsifier.

### Estimated LoC and effort
- Code: ~140 production (50 inventory + 30 resolver + 25 deps ctx + 5×2 dispatcher + 20 refresher integration) + ~130 test = **~270 LoC**.
- Chart: 0 LoC.
- Effort: **~0.5 sprint** (single-file mechanical threading on top of existing dep tracker).

### On tagging — Revision 19 disposition

`0.30.94` is the next numeric increment after `0.30.93`. Per §"feedback_tag_commits.md", the commit that lands Edge type 3 + passes Gate 1 will be tagged `0.30.94` initially; on subsequent ledger-row validation (DELETE-evict observed >0 at SCALE=50000 in the bench), the same commit is RETAGGED as the canonical `0.30.9` (the original §0.30.9 ship slot, displacing the §0.30.93 OOM-finding tag as the canonical 0.30.9 milestone). The §0.30.91/92/93 tags remain in git history as regression-witnesses for the lazy-register + metadata-only + Edge-type-3 sequencing decision tree.

---

## Tag `0.30.10` — Step 6: permission-check cache (bounded least-recently-used)

### Branch base
`0.30.9`. Branch name: `ship-0.30.10-permission-cache`.

### Revision 3 terminology recap
This tag is the former "B-prime EvaluateRBAC LRU." It is renamed throughout the forward-plan to **permission-check cache** (in-process bounded cache for permission decisions, with least-recently-used eviction). Functionality unchanged from the prior plan.

### Revision 2 amplifier (binding)
Per Revision 2, `EvaluateRBAC` fires on every RestAction dispatch (cache=on path). At `0.30.9` this introduces a per-dispatch CPU cost. The permission-check cache amortises hot-path Role-Based Access Control to <1 µs/decision via in-process bounded least-recently-used cache, making Revision 2's "every-dispatch enforcement" rule cheap.

### What's implemented
- **`internal/cache/permission.go` (NEW, ~150 LoC).** Bounded least-recently-used cache keyed by `(username, sortedGroupsHash, verb, group, resource, namespace) → bool`:
  - Capacity: 200 000 (configurable via `PERMISSION_CACHE_SIZE`).
  - sortedGroupsHash: sha256 of sorted-then-joined groups (prevents cache-pollution by group order).
  - Invalidated on `Role`/`RoleBinding`/`ClusterRole`/`ClusterRoleBinding` informer events.
  - Wait-free hits.
- **`internal/rbac/evaluate.go` (MODIFY, ~50 LoC).** Wrap `EvaluateRBAC` with permission-cache check first.
- **`internal/cache/watcher.go` (MODIFY, ~50 LoC).** ResourceEventHandlers for the 4 Role-Based Access Control resource types that call `permission_cache.Invalidate`. (These resource types are already registered as part of `0.30.4`'s `EvaluateRBAC` ship; this tag only adds invalidation handlers, not new informers.)
- **`internal/cache/permission_test.go` (NEW, ~80 LoC).** Hit returns cached; miss computes+caches; Role mutation invalidates within 5 s; concurrent eviction race-free; sortedGroupsHash collision tests.

### Chart values (binding constraint 4)
**Vs `chart-0.30.9`:**
- **ADDED:** `env.PERMISSION_CACHE_SIZE: "200000"`.
- **REMOVED:** none.

**Stripped-chart deliverable:** `chart-0.30.10` (= `chart-0.30.9` + 1 env key).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.9-uaf-restored`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:**
  - Admin cold: ±50 ms vs `0.30.9` (admin Role-Based Access Control was already cheap).
  - Admin warm-p50: -100 to -300 ms (permission-cache amortises Revision 2's per-dispatch `EvaluateRBAC`).
  - Cyberjoker cold: -500 to -1 500 ms (Role-Based Access Control walks were residual cost — now <1 µs/decision).
  - Cyberjoker warm-p50: -300 to -800 ms.
  - Mix-weighted cold: -475 to -1 425 ms.
- **At SCALE=50000:**
  - Admin cold: ±50 ms.
  - Cyberjoker cold: -1 000 to -2 000 ms (Role-Based Access Control walks at 50 K are 10× expensive without the permission cache).
  - Mix-weighted cold: ~2 500–3 100 ms (closer to north-star).
  - Mix-weighted warm-p50: ~700–1 200 ms.
  - **At this point, the campaign should be at-or-below Row 7's 4 100 ms cold target.**

#### Convergence (Revision 4)
- The permission-check cache reduces refilter cost (refilter calls `EvaluateRBAC` per object). At 50 K with high churn, refilter goes from ~5–30 ms per mutation propagation to ~0.5–3 ms. This buys back the convergence margin spent at `0.30.9`.
- **At SCALE=5000 cache=on:** convergence_p50 ~150–500 ms; convergence_p99 ~300–900 ms.
- **At SCALE=50000 cache=on:** convergence_p50 ~300–800 ms; convergence_p99 **~600–1 500 ms — best case <1 s honoured at 50 K; worst case requires `0.30.8` STOP gate decision still in effect.**

#### Mechanism-level
- Memory at 50 K: +50 to +200 MB vs `0.30.9`.
- Pod restart: 0.
- Permission-cache hit rate target: >95 %.
- `SubjectAccessReview` rate in cache=on path: 0/sec (Revision 1 rule continues to hold).
- `EvaluateRBAC` raw call rate: amortised — most calls short-circuit at permission-cache hit.

#### Code-path falsifier
INFO per 5-min: `permission_cache.summary entries=N hit_rate=0.NN evict_count=X invalidate_count=Y`. Hit rate >95 % under load. Invalidate count >0 under Role-Based Access Control churn.

#### Pre-flight falsifier
`0.30.9`'s pprof must show `EvaluateRBAC` cost in resolver path. If not in top 5 for cyberjoker, permission cache moot.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational. ±0–10% vs `0.30.9` (permission cache absorbs `EvaluateRBAC` cost — small Pass-1 delta since S6/S7/S8 are object-throughput-bound, not Role-Based-Access-Control-evaluation-bound).
- **Pass 2 (Chrome MCP):** primary mover (cold ~2 500–3 100 ms — best in campaign on cold-only metric).
- **Pass 3 (per-mutation):** **R4 BINDING (≤1000 ms).** Hypothesis 600–1 500 ms — best-case <1 s achieved here at SCALE=50K. Worst-case = STOP gate from `0.30.8` already fired.

### What this tag does NOT do
- Does NOT add a second-tier cache.
- Does NOT modify refilter (`0.30.9` already does per-request refilter).
- Does NOT add prewarm-on-RoleBinding-apply.
- Does NOT prewarm the permission cache at startup.

### Risks
- **Risk: Role-Based Access Control mutation invalidation lag (>5 s).** Mitigation: synchronous handler; bounded by informer's ~100 ms latency.
- **Risk: groups-hash collision (security).** Mitigation: sha256; collision probability ≈ 0. Audit Rank 3 cites groups-hash fixup `9350920`.
- **Risk: hit rate <95 %.** Mitigation: profile under load; tune `PERMISSION_CACHE_SIZE`.
- **Risk: stale entries after Role mutation.** Mitigation: handler registered before `factory.Start()`; primer §2.7 events delivered in order.
- **If user/role mutation doesn't reflect within 5 s:** ROLLBACK.

### Estimated LoC and effort
- Code: ~250 production + ~80 test = ~330 LoC.
- Chart: ~5 LoC.
- Effort: **1 sprint**.

---

## Tag `0.30.11` — Step 6.25: subject activity classification HOT/WARM/COLD (Revisions 8 + 11c + 12)

### Branch base
`0.30.10`. Branch name: `ship-0.30.11-activity-class`.

### Revision 8 + 11c + 12 rationale (Diego just-in 2026-05-09)

Standalone infrastructure tag inserted between permission-check cache (`0.30.10`) and targeted prewarm (`0.30.12`).

**Revision history:** Revision 8 introduced the activity-class concept for prewarm-scoping (`PREWARM_HOT_ONLY=true`). Revision 10 reversed prewarm-scoping (Diego: prewarm fires for ALL bound subjects). Revision 11a: "there are no hot users if the pod is just started" — confirms HOT-scoping at startup is architecturally impossible. Revision 11c: "0.30.11 still worth a separate tag" — Diego confirms classification stays standalone for refresher priority + L1 eviction priority. Revision 12: "hot/warm/cold classifications will be useful to prioritize refresh patterns" — refresher cadence varies BY CLASS, not just queue position.

**What this tag does (final, post-Revision-12):**
- Tracks per-subject `last_seen_unix_ts` for dynamic `Class(subject) ∈ {hot, warm, cold}` classification.
- Activates the class-aware refresh-cadence HOOK in `0.30.8`'s refresher (the HOOK ships at `0.30.8` as a no-op that defers to class-blind cadence; `0.30.11` makes the HOOK effective by populating the classifier).
- Activates the class-aware retention HOOK in `0.30.7`'s LRU eviction (same pattern: HOOK ships class-blind at `0.30.7`; `0.30.11` activates it).

**Class-aware refresh cadence (Revision 12, BINDING for convergence p99 < 1 s):**
- HOT subjects' L1 entries → **aggressive refresh** (target ≤500 ms latency per Revisions 4 + 7); refresher fires on every WATCH event without dedup deferral. This is the cadence that meets the convergence p99 < 1 s gate at SCALE=50000 cache=on for the dominant 95 % cyberjoker mix (their compositions land in HOT subjects' bound RestAction entries).
- WARM subjects' L1 entries → **standard refresh**; small queue dedup window (e.g., 100 ms coalescing).
- COLD subjects' L1 entries → **lazy refresh**; longer dedup window (e.g., 1 s+) OR fall back to TTL-based invalidation; refresher pool admits these only when HOT/WARM queues are drained.

**Class-aware eviction (Revision 12):**
- Under L1 byte-budget pressure, eviction order: COLD subjects' entries first, then WARM, then HOT. HOT subjects' entries retained longest. This shifts the L1 working set toward subjects most likely to hit the cache next, improving steady-state hit rate under contention.

**Why standalone (not folded into `0.30.7`/`0.30.8`):**
- The classifier itself (`subjectLastSeen` sync.Map + `Class(subject)`) is ~80 LoC; can be folded into earlier tags BUT shipping it as its own tag isolates the convergence-cadence behaviour change from the refresher correctness work at `0.30.8` (where the convergence p99 STOP gate at `0.30.8` is already evaluated). Keeping `0.30.11` separate means: (a) `0.30.8` ledger row is class-blind cadence baseline; (b) `0.30.11` ledger row shows the class-aware cadence delta; (c) if `0.30.8` STOP gate fires, `0.30.11` is the next mechanism to evaluate.
- Forward consumers (refresher cadence at `0.30.8`; L1 LRU retention at `0.30.7`) ship class-blind HOOKS; the HOOKS become effective at `0.30.11` (no code change required at `0.30.7`/`0.30.8`).
- Low LoC, infrastructure-only (no Chrome MCP cold/warm impact at this tag itself; convergence p99 may improve significantly).

### Cross-reference (frontend-mirror prewarm)

The hardcoded `FrontendInitialRenderCalls` constant + CI mirror test live at `0.30.12` (consumer). This tag (`0.30.11`) ships only the subject classifier; it does NOT define or consume `FrontendInitialRenderCalls`. See `0.30.12` for the frontend-mirror discovery procedure, codification, and drift mitigation.

### What's implemented
- **`internal/cache/activity.go` (NEW, ~80 LoC; reduced from ~120 per Revision 8a).**
  - `subjectLastSeen sync.Map` keyed by subject identity (user or group hash); value is `atomic.Int64` Unix-second timestamp. Updated on every dispatcher entry that resolves a subject.
  - `Class(subject) ActivityClass`: reads `subjectLastSeen`; returns `Hot` if `now - last_seen < ACTIVITY_HOT_WINDOW_SECONDS` (default 300 s = 5 min); `Warm` if `< ACTIVITY_WARM_WINDOW_SECONDS` (default 3 600 s = 60 min); else `Cold`.
  - `ExportClassMetrics()`: emits Prometheus-style `activity_class_total{class=hot|warm|cold,dimension=subject}` metric.
  - **DROPPED (per Revision 8a):** `restactionCallCount` counter, `Class(restaction)` function, quartile recompute. Replaced by static declaration loader below.
- **`internal/handlers/dispatchers/restactions.go` (MODIFY, ~5 LoC).** On dispatcher entry, call `activity.RecordSubject(subject)`. Atomic int64 store; sub-microsecond.
- **`internal/cache/refresher.go` (MODIFY at `0.30.8`'s HOOK, ~30 LoC).** Replace class-blind cadence with class-aware: HOT entries fire on every WATCH event with no dedup; WARM entries dedup at 100 ms; COLD entries dedup at 1 s. Drives `0.30.8`'s priority queue: HOT items front of queue, WARM middle, COLD back; COLD admitted only when HOT/WARM drained.
- **`internal/cache/lru.go` (MODIFY at `0.30.7`'s HOOK, ~20 LoC).** Replace class-blind eviction-victim selection with class-aware: under byte-budget pressure, evict COLD subjects' entries first; only then WARM; HOT last. Implements as a stable secondary sort on the existing LRU eviction queue.
- **`internal/cache/activity_test.go` (NEW, ~60 LoC).**
  - HOT classification test: subject seen within window → `Hot`.
  - WARM classification test: subject seen 5–60 min ago → `Warm`.
  - COLD classification test: subject seen >60 min ago → `Cold`.
  - Threshold tunability test: `ACTIVITY_HOT_WINDOW_SECONDS=1` shifts boundary.
  - **Falsifier test:** 10 subjects (5 active, 5 idle); active classified Hot, idle classified Cold.
- **`internal/cache/refresher_class_test.go` (NEW, ~50 LoC).** Synthetic 1 000 mutation events across HOT/WARM/COLD subjects; per-class latency p99 from-mutation-to-fresh-L1 satisfies HOT ≤500 ms, WARM ≤2 000 ms, COLD ≤10 000 ms.
- **`internal/cache/lru_class_test.go` (NEW, ~30 LoC).** Under synthetic byte-budget pressure with 100 entries split 33/33/34 across HOT/WARM/COLD: COLD entries evicted first; HOT entries retained.

### Chart values (binding constraint 4)
**Vs `chart-0.30.10`:**
- **ADDED:**
  - `env.ACTIVITY_HOT_WINDOW_SECONDS: "300"` — HOT threshold for subjects.
  - `env.ACTIVITY_WARM_WINDOW_SECONDS: "3600"` — WARM threshold for subjects.
- **REMOVED:** none. (`ACTIVITY_RESTACTION_QUARTILE_RECOMPUTE_SECONDS` from initial Revision 8 dropped per 8a; `PrewarmDeclaration` CRD dropped per Revision 9 — the call set is hardcoded in snowplow source.)

**Stripped-chart deliverable:** `chart-0.30.11` (= `chart-0.30.10` + 2 env keys; no CRD, no schema additions).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.9-uaf-restored`.** Unchanged.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000 + SCALE=50000:** ±0 ms (infrastructure tag). Mix-weighted cold / warm / convergence unchanged vs `0.30.10`. If any metric moves >50 ms, the activity-tracking call site is on the hot path and needs review.

#### Convergence (Revision 4)
- ±0 ms vs `0.30.10`.

#### Mechanism-level
- Memory at 50 K: +20 to +50 MB vs `0.30.10` (sync.Map of subjects + RestAction counters). Negligible.
- Pod restart 30-min: 0.
- Time-to-Ready at 50 K: ±0 vs `0.30.10`.
- `activity_class_total{class=hot,dimension=subject}` populates as soon as any user dispatches.
- `activity_class_total{class=hot,dimension=restaction}` populates after the first quartile-recompute (60 s post-startup).

#### Code-path falsifier
INFO under traffic: `activity.subject.update subject=hash class_was=cold class_now=hot`. INFO at startup + on CRD change: `prewarm.declaration source=crd widgets=N empty=false`. INFO at startup if declaration missing: `prewarm.declaration.empty source=crd action=skip_prewarm` (WARN).

**Falsifier (binding):**
- Under bench traffic (admin + cyberjoker dispatching widgets), `activity_class_total{class=hot,dimension=subject}` MUST be ≥1 within 60 s.
- `IsDeclaredHot(path)` returns true for the 5 portal-shipped prewarm widgets (blueprints-list, compositions-list, sidebar-nav-menu-items, all-routes, blueprints-panels) once the `PrewarmDeclaration` CRD is applied.
- `IsDeclaredHot("nonexistent-widget")` returns false.

#### Three-pass expectations (per §3) — **R12 per-class binding first activates here**
- **Pass 1 (S6/S7/S8):** informational; ±0 ms vs `0.30.10` (infrastructure tag).
- **Pass 2 (Chrome MCP):** ±0 ms vs `0.30.10` (no UX impact).
- **Pass 3 (per-mutation):** **R4 binding (≤1000 ms mix-weighted) + R12 per-class binding NEW: HOT ≤500 ms / WARM ≤2000 ms / COLD ≤10000 ms p99.** Class-aware refresh cadence (Revision 12) closes the gap if `0.30.8`/`0.30.10` class-blind cadence missed. Hypothesis: `convergence_per_mutation_p99_mix` 400–1 200 ms; `convergence_per_class_hot_p99` 200–500 ms.

### What this tag does NOT do
- Does NOT change prewarm behaviour (prewarm lands at `0.30.12` and consumes classification + declaration).
- Does NOT change refresher behaviour (refresher at `0.30.8` is class-agnostic; class-aware priority queue is a forward-reference, optional follow-on).
- Does NOT change L1 LRU eviction behaviour (LRU at `0.30.7` is class-agnostic; class-aware retention is a forward-reference, optional follow-on).
- Does NOT track per-RestAction call counts (per Revision 8a, widget hot-set is static-declared, not dynamically inferred).
- Does NOT export per-user PII (subject identity is a one-way hash in metrics).

### Risks
- **Risk: activity tracking on the dispatcher hot path measurably regresses warm-p50.** Mitigation: atomic-int64 increments + sync.Map lookups are sub-microsecond; ±0 ms expected. Falsifier: warm-p50 vs `0.30.10` >50 ms regression = back-out.
- **Risk: class-aware refresh cadence regresses warm-p50** (e.g., HOT cadence too aggressive — refresher fires excessively under churn). Mitigation: HOT cadence is "no dedup" but bounded by `RESOLVED_CACHE_REFRESHER_PARALLELISM=4`; can't exceed 4 in-flight refreshes at any moment. Falsifier: warm-p50 vs `0.30.10` >50 ms regression = tune cadence or back-out.
- **Risk: refresher backlog under sustained churn — class-aware cadence makes HOT cadence aggressive.** Mitigation: bounded `RESOLVED_CACHE_REFRESHER_PARALLELISM=4` ceiling; under saturation, queue depth metric exposes backpressure for ops alerting. (Drift between frontend `FrontendInitialRenderCalls` and snowplow source is a separate `0.30.12` risk, not `0.30.11`.)
- **Risk: cold-start subject classification all-Cold → refresher cadence treats all subjects as COLD until traffic accumulates.** Mitigation: not load-bearing for prewarm at `0.30.12` (Revision 10 + 11a: prewarm uses ALL bound subjects, not HOT-only). For refresher: COLD cadence at startup means convergence p99 may briefly miss the 1 s gate immediately post-deploy until classifier populates (~5 min); steady-state convergence is unaffected.

### Estimated LoC and effort
- Code: ~130 production (subject tracking ~80 + declaration loader ~50) + ~100 test = ~230 LoC.
- Chart: ~10 LoC (2 env keys + 1 CRD manifest reference + default `PrewarmDeclaration` manifest).
- Effort: **0.5 sprint**.

---

## Tag `0.30.12` — Step 6.5: frontend-mirror prewarm + Ready-gate (Revisions 6 + 7/7a + 9 + 10 + 11)

### Branch base
`0.30.11`. Branch name: `ship-0.30.12-targeted-prewarm`.

### Revision history

- **Revision 6:** "prewarm must be implemented for users and groups in `RoleBinding` and `ClusterRoleBinding` that involve widgets and RestActions" — targeted, NOT generic prewarm-all-known-users.
- **Revisions 7 + 7a:** Ready-gate (pod stays 503 until `prewarmComplete=true`); minutes are acceptable for prewarm; default deadline 1 200 s = 20 min.
- **Revision 9:** the prewarm call set is a hardcoded Go-level constant `FrontendInitialRenderCalls` mirroring the frontend's initial-render API call sequence; CI mirror test enforces alignment.
- **Revision 10 (BINDING):** prewarm fires for ALL bound subjects (no HOT-scoping). Revision 8's `PREWARM_HOT_ONLY` flag is REMOVED.
- **Revision 11a:** "there are no hot users if the pod is just started" — confirms HOT-scoping at startup is impossible (classifier empty); R10 wins by architectural necessity.
- **Revision 11b:** frontend source is `https://github.com/braghettos/frontend` (architect inspects this repo to codify `FrontendInitialRenderCalls`).
- **Revision 11c:** the `0.30.11` activity-class tag stays standalone for refresher priority + L1 retention (POST-startup), even though it is not load-bearing for prewarm scoping.

### Frontend-mirror prewarm (Revisions 9 + 10 + 11b)

**Diego (Revision 9):** "check frontend code which apis calls to render the initial widgets. The same will be done by snowplow prewarm." **Diego (Revision 10):** "prewarm replays that same sequence per any user or groups that have a role or clusterrole binded to widgets and/or restactions."

Snowplow's prewarm code MIRRORS the frontend's initial-render API call sequence. The call set is a HARDCODED Go-level constant in snowplow source, kept in sync with the frontend via CI.

**Discovery procedure (one-time at design; rerun on frontend change):**
1. Architect (or future maintainer) clones `https://github.com/braghettos/frontend` (Revision 11b) and identifies the SPA router or initial nav handler that issues `/call?resource=...&name=...&namespace=...` URLs during initial render for each navigation entry point (Compositions, Dashboard, sidebar-nav, all-routes, etc.).
2. The inspected call set is codified as a Go-level constant in snowplow source.

**Codification (concrete shape):**
```go
// internal/cache/prewarm_calls.go
//
// FrontendInitialRenderCalls mirrors the API call set issued by
// https://github.com/braghettos/frontend during initial-render navigation.
// MUST be updated when the frontend's initial-render set changes.
// CI test: e2e/frontend-mirror-test.go captures Playwright network
// requests and asserts this constant matches.
type PrewarmCall struct {
    Resource   string
    APIVersion string
    Name       string
    Namespace  string
}
var FrontendInitialRenderCalls = []PrewarmCall{
    {Resource: "widgets", APIVersion: "widgets.templates.krateo.io/v1beta1", Name: "sidebar-nav-menu", Namespace: "krateo-system"},
    {Resource: "widgets", APIVersion: "widgets.templates.krateo.io/v1beta1", Name: "Compositions", Namespace: "krateo-system"},
    {Resource: "widgets", APIVersion: "widgets.templates.krateo.io/v1beta1", Name: "blueprints-list", Namespace: "krateo-system"},
    {Resource: "widgets", APIVersion: "widgets.templates.krateo.io/v1beta1", Name: "all-routes", Namespace: "krateo-system"},
    {Resource: "widgets", APIVersion: "widgets.templates.krateo.io/v1beta1", Name: "blueprints-panels", Namespace: "krateo-system"},
    // Append entries when frontend adds initial-render widgets.
}
```

**Replay (Revision 10 — ALL bound subjects, NOT HOT-only):**
- Walk RoleBinding + ClusterRoleBinding subjects via the RBAC informers (`0.30.4`); filter to those whose role grants `get`/`list` on `widgets.templates.krateo.io` and/or `templates.krateo.io/restactions`; extract distinct subjects (users + groups).
- For each subject: replay every `PrewarmCall` from `FrontendInitialRenderCalls` against the dispatcher; resolver runs through `EvaluateRBAC` per Revision 2 and writes the resolved-output to L1.
- Subjects with NO widget/RestAction permissions are SKIPPED (e.g., a user with only Pod-read permissions; not customer-affecting).
- **Why ALL bound subjects (Revision 11a):** at pod startup, the activity classifier from `0.30.11` is empty (no last-seen data yet). HOT-scoping at startup is therefore architecturally impossible. Cartesian = ALL bound subjects × `FrontendInitialRenderCalls`.

**Cartesian sizing (post-Revision-10):**
- ~100 bound subjects × ~5–10 frontend-mirror calls = **~500–1 000 entries** (was ~5 000 with the original blanket all-pairs enumeration).
- At 100 ms per resolve × 8 parallel: ~6–12 s wall-clock.
- At 1 s per resolve × 8 parallel: ~60–120 s wall-clock.
- Both fit "minutes acceptable" budget comfortably; the 1 200 s deadline has substantial margin.

**Drift mitigation (Revision 9):**
- **Primary: CI mirror test.** New CI job `e2e/frontend-mirror-test.go` performs a synthetic frontend login via Playwright/headless browser, captures the network requests issued during initial render, normalises to `PrewarmCall`-shape, and asserts equality with the `FrontendInitialRenderCalls` constant. Mismatch fails CI; the snowplow PR or frontend PR (whichever side moved) must update the other.
- **Secondary: documentation comment block** on `FrontendInitialRenderCalls` declares the update-on-frontend-change requirement.
- **Failure mode if drift occurs:** frontend adds widget to its initial-render set; CI mirror test fires red; PR is blocked until snowplow constant is updated. If CI is somehow bypassed, runtime symptom is "first-nav cold for the new widget" (L1 miss; falls through to live resolve at ~150 ms–1 s) — degraded but not broken; ledger row regression surfaces it.

**Empty/missing handling:**
- If `FrontendInitialRenderCalls` is empty (e.g., during early development), snowplow logs `prewarm.calls.empty source=hardcoded` at WARN, sets `prewarmComplete=true` immediately, and continues serving traffic. Pod becomes Ready normally.
- If zero bound subjects (very-small clusters or fresh deploys with no developers yet), same graceful path: log + set `prewarmComplete=true`.

**Why this is safe (vs the audit's prewarm-pool blacklist):**
- Eager resource-type registration (Tag `0.30.6`) means **NO `AddResourceType` calls fire during prewarm**; informers are already running with watches established. The historical AddResourceType fan-in storm cause is structurally absent.
- Bounded inner concurrency (default 8 parallel prewarmers, same proven cap as eager registration).
- Deduplication by binding-identity hash: if two subjects share group-set hash, prewarm runs once per unique `(group_hash, PrewarmCall)` not per subject.
- Permission-check cache (Tag `0.30.10`) absorbs per-pair `EvaluateRBAC` cost.
- Hardcoded mirror set (~5–10 calls) is bounded — not a function of cluster GVR count or namespace count.

### What's implemented
- **`internal/cache/prewarm_targeted.go` (NEW, ~250 LoC).**
  - `EnumeratePrewarmSubjects(ctx, watcher) []SubjectIdentity`: walks `RoleBinding` + `ClusterRoleBinding` informer-cached objects; for each subject (user + group + ServiceAccount), evaluate against the prewarm-relevant rule filter (default: `verbs={get,list,watch}` ∩ `resources={widgets.templates.krateo.io, templates.krateo.io/restactions}`); emit unique subject list. Dedupe by `group_hash`. **Revision 10 scoping:** ALL bound subjects (no HOT-only filter); subjects with no widget/RestAction permissions are SKIPPED.
  - `BuildPrewarmCartesian(subjects []SubjectIdentity, calls []PrewarmCall) []PrewarmPair`: cartesian product of bound subjects × `FrontendInitialRenderCalls`. Cartesian estimate ~500–1 000 entries.
  - `RunPrewarm(ctx, pairs []PrewarmPair, fanIn int)`: parallelism-bounded worker pool. For each pair, synthesize a `(restaction_path, user_identity, query_hash)` resolved-cache key and populate via the same dispatcher path as a real request (going through `EvaluateRBAC` → resolver → resolved-cache write).
  - Hard upper bound: `PREWARM_MAX_PAIRS` (default 5 000) caps total workload to avoid prewarm-storm at very-large-Role-Based-Access-Control clusters.
  - Wall-time budget: `PREWARM_TIMEOUT_SECONDS` (default 60 s; the per-pair-cohort budget); on expiry, log partial completion + continue.
  - **Ready-gate signal (Revision 7):** package-level `atomic.Bool prewarmComplete` (export via `IsPrewarmComplete() bool`). Set `true` when (a) all enumerated pairs are written to resolved-cache OR (b) `PREWARM_READY_DEADLINE_SECONDS` exceeded (graceful escape — see Death-spiral guardrail).
- **`internal/health/ready.go` (MODIFY, ~15 LoC).** Existing readiness handler gains a `cache.IsPrewarmComplete()` check: returns 503 (with body `{"ready":false,"reason":"prewarm_in_progress","pairs_warmed":N,"pairs_expected":M}`) until flag set; returns 200 thereafter. Kubelet observes pod NotReady; LB does not route traffic to this pod.
- **`internal/cache/prewarm_calls.go` (NEW, ~30 LoC).** `PrewarmCall` struct + `FrontendInitialRenderCalls` constant (Revision 9; sourced from `https://github.com/braghettos/frontend` per Revision 11b). Documentation comment block declares update-on-frontend-change requirement.
- **`main.go` (MODIFY, ~40 LoC).** After `EagerRegisterAll` + `WaitForCacheSync`: call `EnumeratePrewarmSubjects` + `BuildPrewarmCartesian(subjects, FrontendInitialRenderCalls)` + `RunPrewarm` SYNCHRONOUSLY (blocks main goroutine until prewarm-complete OR `PREWARM_READY_DEADLINE_SECONDS` exceeded). Gated by `PREWARM_ENABLED` (default `true` per Diego ruling); when `PREWARM_ENABLED=false`, prewarm is skipped AND `prewarmComplete` is set immediately (preserves backward-compat for cache=off + small-scale customers; readiness probe behaves as `0.30.11`).
- **`internal/cache/prewarm_targeted_test.go` (NEW, ~120 LoC).**
  - Enumeration includes RoleBinding subjects.
  - Enumeration includes ClusterRoleBinding subjects.
  - Group-binding produces one pair per group-member set, not per individual.
  - Group-hash dedup deduplicates correctly.
  - Resource-filter excludes Role-Based Access Control bindings on unrelated resources (subjects with no widget/RestAction permissions are skipped).
  - Cartesian = bound_subjects × `FrontendInitialRenderCalls` (Revision 10: ALL bound, NOT HOT-only).
  - Wall-time budget honoured (test uses 100 ms budget; partial completion logged).
  - **Falsifier test**: post-prewarm, lookup of one prewarmed pair returns hit (no resolver call); lookup of non-prewarmed pair returns miss.
  - **Ready-gate test (Revision 7)**: readiness handler returns 503 until `prewarmComplete=true`; transitions to 200 atomically with the flag flip.
  - **Death-spiral test (Revision 7)**: with `PREWARM_READY_DEADLINE_SECONDS=1` and a synthetic resolver that hangs, verify the handler flips to 200 within ~1 s with a WARN log + non-zero `pairs_expected - pairs_warmed`.
- **`internal/cache/prewarm_calls_test.go` (NEW, ~20 LoC).** `FrontendInitialRenderCalls` non-empty + each entry has all four required fields populated.
- **`e2e/frontend-mirror-test.go` (NEW CI job, ~80 LoC).** Synthetic frontend login via Playwright captures network requests; asserts equality with `FrontendInitialRenderCalls` constant. Mismatch fails CI.

### Revision 7 — Ready-gate + death-spiral guardrail (Diego just-in 2026-05-09)

**Diego's revision (binding):** "pod must become ready once L1 cache has all resolved keys from L3 keys × subjects (users and groups binded by roles)."

**Translation:** at this tag, the readiness probe gates on prewarm completion. The L1 (resolved-output) cache must contain entries for every `(subject, RestAction)` pair derivable from RoleBinding + ClusterRoleBinding BEFORE the readiness probe returns 200.

**Critical historical context — surfaced explicitly:** this re-couples Ready to prewarm. **Q-PREWARM-R2 (`0.25.301`, audit Rank 4) DECOUPLED them** to break the prewarm death-spiral (when prewarm could not complete, pod never became Ready, kubelet killed it, restart loop). Revision 7 is a DELIBERATE re-coupling, not a regression. The death-spiral risk is engineered out via the guardrail below — not papered over.

**Mechanism:**
- Once all enumerated `(subject, RestAction)` pairs are written to the resolved-output cache, set `prewarmComplete = true`.
- Readiness handler reads `prewarmComplete`; returns 200 only if true, else 503 with diagnostic body.
- Until 200, kubelet observes pod NotReady → LB does not route traffic. New traffic continues to land on `0.30.10`-equivalent pods (rolling-update).

**Death-spiral guardrail (MANDATORY) — graceful timeout escape (Diego just-in 2026-05-09: "it is ok if prewarm takes minutes"):**
- New env `PREWARM_READY_DEADLINE_SECONDS` (default **`1200` = 20 minutes**; chart-tunable per customer cluster shape).
- If wall-clock since prewarm-start exceeds the deadline, set `prewarmComplete = true` ANYWAY with the cache PARTIALLY populated, AND emit WARN log line: `prewarm.timeout_exceeded entries_populated=K expected=N×M deadline_seconds=1200`.
- Rationale: with Diego's "minutes are acceptable" relaxation, the kubelet startup-probe deadline is no longer the binding gate; chart-side `startupProbe` is tuned to honour 20 min (see Chart values). The deadline still exists to prevent silent stuck-at-NotReady → CrashLoopBackOff (the Q-PREWARM-R2 failure mode) on truly-pathological clusters (e.g., resolver hangs). Partial-cache state is observable in logs + via the next ledger row's mix-weighted-cold (which will regress visibly if prewarm is incomplete), so the failure surfaces — it does not hide.
- **Architect-chosen alternative considered + rejected:** hard-fail Ready → kubelet restart-loop → STOP condition. Less graceful; reproduces the very Q-PREWARM-R2 spiral the original decouple was designed to prevent. Graceful timeout is the chosen path; the trade-off is "partial L1 warmth on slow clusters" — visible in metrics, not silent.

**Sizing analysis (SCALE=50000) — post-Revision-10 (frontend-mirror, all-bound-subjects):**
- Subjects from RoleBinding + ClusterRoleBinding: estimate **~100 cluster-wide** (1 admin + ~5–10 developer groups + ~50–100 per-namespace developers, dedupe'd by `group_hash`). Architect to verify customer-cluster RBAC census (per PM amendment 7) before ship.
- `FrontendInitialRenderCalls` count: **~5–10** (the static call set sourced from `https://github.com/braghettos/frontend`; ~5 portal-shipped widgets at design time, growing as frontend adds initial-render widgets).
- Cartesian (post-dedup): **~500–1 000 entries** (Revision 10 vs the pre-Revision-10 ~5 000 estimate; the static call set is bounded, not a function of cluster GVR/namespace count).
- **Wall-clock at 100 ms per resolve × 8 parallel prewarmers:** `~6–12 s`. Very comfortable.
- **Wall-clock at 1 s per resolve (heavy widget) × 8 parallel:** `~60–120 s` (~1–2 min). Well within the 1 200 s (20 min) deadline.
- **Worst-case at 1 000 pairs × 1 s × /8 parallel:** ~125 s ≈ **2 min**. Substantial margin against the 20 min deadline.
- **Mitigation list (post-Revision-10):**
  1. **Primary (Diego acceptable):** raise `PREWARM_READY_DEADLINE_SECONDS` if customer cluster has unusually-large bound-subject set (>1 000). Default 1 200 already accommodates 10× growth.
  2. **Fallback (rarely needed):** `PREWARM_SKIP_WIDGETS` env to exclude heavy widgets from `FrontendInitialRenderCalls` replay (their first user-nav pays the cold cost).
  3. **Fallback (rarely needed):** increase `PREWARM_INNER_CONCURRENCY` from 8 to 16. Prewarm runs AFTER informer-sync, so does not contend with the 8 informer fan-in budget per `0.30.6`.
- **Architectural simplification:** with the Revision-10 cartesian reduction (5×–10× smaller than pre-Revision-10), prewarm wall-clock is dominated by per-call resolver cost, not enumeration cost. Pod time-to-Ready post-`0.30.12` is `~6 s–2 min` for ~99 % of customer cluster shapes — much tighter than the pre-Revision-10 worst-case of `~10 min`. Customer-facing pod-deploy time grows from `~60 s` (pre-`0.30.12`) to `~3–5 min` (post-`0.30.12` typical) — a noticeable but acceptable trade-off for guaranteed L1-warm first-navigation.

**Cluster-mutation contention (Revision 7 risk surface):**
- Composition events arriving DURING prewarm trigger refresher load on top of prewarm load.
- **Ordering decision (architect):** prewarm completes BEFORE refresher activates. Refresher is gated on `cache.IsPrewarmComplete()` (same flag, same atomic). Refresher backlog accumulates as informer events during prewarm; on `prewarmComplete=true`, the refresher drains the backlog at its normal cadence (200 ms × 8 parallel per `0.30.8`). This serializes the two systems and prevents convergence-time-vs-prewarm-time contention.
- Trade-off: events arriving during prewarm have additional convergence latency = `(prewarm_remaining_time) + refresher_drain_time`. At median 50 s prewarm + 200 ms drain, worst-case convergence during the prewarm window is ~50 s. **This is acceptable because the prewarm window is one-time per pod start; steady-state convergence (post-Ready) honours the `0.30.8` <1 s p99 target.**

**Cardinality-mismatch detection (Revision 7 risk surface):**
- If subject set or RestAction set changes mid-prewarm (e.g., RoleBinding mutation arrives 30 s into a 50 s prewarm), the cartesian is no longer fixed.
- **Decision (architect):** snapshot the `(subjects, RestActions)` enumeration ONCE at prewarm-start; mutations that arrive mid-prewarm are NOT re-enumerated for this tag. The mutation's UPDATE event hits the refresher post-Ready (which fires after `prewarmComplete=true`), and the new pair is warmed lazily on first user request. No re-prewarm; no fail-Ready. Falsifier: log line `prewarm.snapshot_taken subjects=N restactions=M` immediately before enumeration; any subsequent RoleBinding mutation log line MUST NOT trigger a re-snapshot.
- Trade-off: a brand-new bound subject during the prewarm window does not get prewarm-warmth on this pod; their first nav is a cold miss. Acceptable — RoleBinding churn is rare relative to prewarm window.

**Admin (wide-RBAC) handling:**
- Admin's bound rules grant `*` on `*` → admin is one of the bound subjects.
- **Decision (architect):** admin is enumerated as ONE subject and replays `FrontendInitialRenderCalls` (~5–10 calls), NOT cross-product-with-namespaces. The userAccessFilter (`0.30.9`) handles namespace-scoping at dispatch time; prewarm warms the call-level resolved-output cache, not per-namespace per-call.
- Mix-weighted impact: admin is 5 % of the mix. Admin's first-nav cyberjoker-equivalent widgets are L1-warm post-prewarm.

### Chart values (binding constraint 4)
**Vs `chart-0.30.11`:**
- **ADDED:**
  - `env.PREWARM_ENABLED: "true"` (default ON per Diego ruling — targeted prewarm is desired behaviour, not opt-in).
  - `env.PREWARM_INNER_CONCURRENCY: "8"`.
  - `env.PREWARM_TIMEOUT_SECONDS: "60"` (per-pair-cohort budget).
  - `env.PREWARM_READY_DEADLINE_SECONDS: "1200"` **(Revisions 7 + 7a, post-Diego-relaxation: minutes are acceptable)** — death-spiral guardrail with 20 min default; on expiry, `prewarmComplete=true` is set anyway with WARN log + partial cache. Customer-tunable per cluster shape.
  - `env.PREWARM_SKIP_WIDGETS: ""` **(Revision 7)** — comma-separated widget paths to exclude from prewarm; default empty. Fallback sizing-mitigation lever if 20 min is somehow exceeded.
- **Chart probe tune (Revision 7) — explicit chart-strip / chart-extend item:** `startupProbe.failureThreshold × startupProbe.periodSeconds` MUST exceed `PREWARM_READY_DEADLINE_SECONDS`. Default chart values: `startupProbe.failureThreshold: 120`, `startupProbe.periodSeconds: 10` → 1 200 s window matching the 20 min deadline. `readinessProbe.failureThreshold` raised from 3 → 30 (probe continues 503 attempts until startup probe succeeds; readiness flips on `prewarmComplete=true`). The probe values are part of the `chart-0.30.12` deliverable; `chart-0.30.0` through `chart-0.30.11` keep `0.20.5`-baseline probe values.
- **REMOVED:** `PREWARM_MAX_PAIRS` (no longer needed — Revision 10's bounded cartesian via static `FrontendInitialRenderCalls` makes the cap irrelevant); `PREWARM_RESOURCE_FILTER` (no longer needed — frontend-mirror call set replaces resource-filter).

**Stripped-chart deliverable:** `chart-0.30.12` (= `chart-0.30.11` + 5 env keys + probe tune for ready-gated startup).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.9-uaf-restored`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:**
  - Admin cold: -200 to -800 ms vs `0.30.11` (admin's resolved-output cache pre-populated for the `FrontendInitialRenderCalls` set).
  - Admin warm-p50: ±50 ms.
  - Cyberjoker cold: **-1 500 to -3 500 ms vs `0.30.10`** (cyberjoker's first-nav cache hits hot — the high-leverage case).
  - Cyberjoker warm-p50: ±50 ms.
  - Mix-weighted cold: -1 435 to -3 365 ms (cyberjoker dominates the mix).
- **At SCALE=50000:**
  - Admin cold: -300 to -1 000 ms vs `0.30.11`.
  - Cyberjoker cold: **-1 500 to -4 000 ms vs `0.30.10`**.
  - Mix-weighted cold: ~1 000–1 800 ms (well under Row 7's 4 100 ms; meaningful step toward north-star 1 000 ms).
  - Mix-weighted warm-p50: ±50 ms.

#### Convergence (Revision 4)
- ±0 to +50 ms vs `0.30.10`. Targeted prewarm affects first-nav cold post-restart; convergence is steady-state and not first-nav-bound. Slight regression possible if prewarm-storm at startup pushes refresher startup queue back ~50 ms.

#### Mechanism-level
- Memory at 50 K: +100 to +500 MB vs `0.30.10` (resolved-output cache populated with prewarm pairs at startup; entries within 2 GB cap).
- Pod restart 30-min: **0** (death-spiral guardrail mandatory falsifier — see below).
- Time-to-Ready at 50 K (Revision 7 + 7a + 10, post-Diego-relaxation + Revision-10 cartesian reduction): **+6 s to +2 min typical** vs `0.30.11` (prewarm runs synchronously before Ready; median resolver cost × cartesian / 8-parallel). At median resolver cost ~150 ms × ~750 pairs → ~14 s. At heavy-widget cost ~1 s × ~1 000 pairs → ~125 s (~2 min). Worst-case heavy-cluster: ~3–5 min. **Hard ceiling: 1 200 s = 20 min** (deadline; partial cache + WARN log if exceeded). Customer-facing pod-deploy time grows from `~60 s` (pre-`0.30.12`) to `~3–5 min` typical (post-`0.30.12`). **One-time per-pod-restart cost, not per-request.** Customer accepts this in exchange for guaranteed L1-warm first-navigation. **This is the FIRST tag in the campaign where pod startup time materially regresses** — see Operating note below.
- Resolved-output-cache hit rate at first-cyberjoker-nav post-restart: **>95 %** for warmed widgets (target). Falsifier: <80 % = prewarm enumeration is wrong OR prewarm timed-out with significant partial cache.
- Prewarm-pair enumeration count: cluster-dependent; expect 200–2 000 unique `(group_hash, RestAction)` pairs at customer scale; 3 000–5 000 at the largest-tier customer (cap fires).

#### Code-path falsifier
INFO at startup: `prewarm.snapshot_taken subjects=N restactions=M` (cardinality fixed; mid-prewarm RoleBinding mutations do NOT re-snapshot). INFO at startup: `prewarm.enumerate pairs_total=N×M pairs_unique_after_dedup=K binding_subjects_walked=S resource_filter=widgets,restactions,compositions`. INFO per pair (DEBUG level for prod; INFO for benchmark): `prewarm.pair subject=hash restaction=X duration_ms=Y status=hit|miss`. INFO summary: `prewarm.populated subjects=N restactions=M entries=K` (Revision 7 falsifier — `entries` MUST equal full cartesian for bound subjects before next line). INFO completion: `prewarm.complete pairs_warmed=K wall_time_ms=Y peak_heap_mb=H budget_exhausted=true|false`. WARN on timeout: `prewarm.timeout_exceeded entries_populated=K expected=N×M deadline_seconds=1200`.

**Falsifier (binding, Revision 7 strengthened):**
- At first-cyberjoker-nav after pod restart, the dispatcher log MUST show `resolved_cache.lookup hit=true prewarmed=true` for warmed widgets. If `prewarmed=false` or `hit=false`, prewarm enumeration missed this subject/RestAction pair (or the timeout fired with the pair unwarmed).
- Pod Ready transition false → true MUST be preceded in the same log stream by `prewarm.complete pairs_warmed=K wall_time_ms=Y` (graceful path) OR by `prewarm.timeout_exceeded` (escape path). If Ready flips to true with neither line preceding, the readiness-gate wiring is broken.
- For each `(subject, RestAction)` pair enumerated: subsequent first-navigation request from that subject to that RestAction MUST show `l1.hit prewarmed=true`. Aggregate hit rate >95 % is the binding pass; <80 % is enumeration / timeout failure.
- **Pod restart count = 0 over a 30-min window post-deploy** (death-spiral guardrail working). Any restart in the window indicates the graceful timeout did not fire OR the kubelet startup-probe was tuned tighter than `PREWARM_READY_DEADLINE_SECONDS`.
- **Time-to-Ready < 1 200 s (= 20 min)** at SCALE=50000 with default chart values (kubelet startup-probe deadline = 1 200 s; deadline graceful escape kicks in at deadline-1 s if needed).

#### Pre-flight falsifier
- `0.30.10` ledger row stable; mix-weighted cold at-or-below Row 7's 4 100 ms.
- `0.30.10`'s pprof must show resolver CPU cost in cyberjoker first-nav cold path. If not in top 5 (e.g., dominated by something else), targeted prewarm doesn't pay.
- Sample cluster's `RoleBinding` + `ClusterRoleBinding` count must be auditable: if zero bindings grant widget/RestAction access, the enumeration walks 0 pairs and prewarm is a no-op.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; ±0 ms vs `0.30.11`. Prewarm warms cold-path L1, doesn't change Pass-1 mass-stage.
- **Pass 2 (Chrome MCP):** primary mover. First-nav cold drops to ~1 000–1 800 ms mix-weighted (every bound subject's first-nav hits L1 prewarmed entries). **Likely tag where "1 s cold" + "1 s fresh" north-star land together.**
- **Pass 3 (per-mutation):** **R4 + R12 binding.** ±50 ms vs `0.30.11` (prewarm doesn't move convergence — refresher does). `convergence_per_mutation_p99_mix` stays in `0.30.11` range.

### What this tag does NOT do
- Does NOT add per-user prewarm pool (audit blacklist anti-pattern; only group-hash-deduplicated pairs are warmed).
- Does NOT register new resource types (eager `0.30.6` is sole owner of resource-type registration).
- Does NOT bypass `EvaluateRBAC` (every prewarm pair calls the same `EvaluateRBAC` per Revision 2).
- Does NOT cache anything that wouldn't be cacheable via a real user request.
- Does NOT prewarm the permission-check cache (`0.30.10`'s eval cache is populated lazily; prewarm-as-side-effect populates it but doesn't drive eval-cache prewarm directly).

### Risks
- **Risk: prewarm enumeration walks bindings on resources not relevant to widget/RestAction surface.** Mitigation: `PREWARM_RESOURCE_FILTER` env hard-codes the relevant resources; arbitrary resource bindings are excluded.
- **Risk: prewarm storm at very-large Role-Based-Access-Control clusters (e.g., 50 K bindings).** Mitigation: `PREWARM_MAX_PAIRS` cap (default 5 000); bounded parallelism (8); wall-time budget (60 s).
- **Risk: prewarm includes ServiceAccount subjects that are not real users (background controllers).** Mitigation: ServiceAccount subjects are de-prioritised in enumeration order; if budget exhausts before they prewarm, that's correct (real users got priority).
- **Risk: prewarm-warmed entries evict via least-recently-used before any real user reads them.** Mitigation: prewarm pairs are written with same time-to-live as user-driven entries; if eviction happens before any user reads, prewarm's value is lost — measure via the falsifier (`hit_rate at first-nav`).
- **Risk: regresses convergence (refresher backlog post-prewarm).** Mitigation: prewarm runs synchronously at startup, fully-completes before service-Ready signal; refresher only fires post-Ready.
- **Risk: per Revision 2, every prewarm pair calls `EvaluateRBAC` — at 5 000 pairs without permission cache, this is 5 000 walks.** Mitigation: `0.30.10`'s permission-check cache is established before this tag; the 5 000 pairs amortise to ~5 unique decisions per group-hash.
- **If `RoleBinding` enumeration returns >50 000 subjects (very-large cluster):** the cap kicks in. With Diego relaxation ("minutes are acceptable"), startup time inflating to several minutes is acceptable. ROLLBACK trigger updated: time-to-Ready > 1 200 s (20 min) AND graceful timeout did not fire.
- **If targeted prewarm doesn't move cyberjoker cold by >1 000 ms at 50 K:** the audit's prewarm verdict (mixed) re-applies; ROLLBACK or `PREWARM_ENABLED=false`.
- **Risk (Revision 7): death-spiral re-coupling — Ready gated on prewarm.** Mitigation: `PREWARM_READY_DEADLINE_SECONDS=1200` graceful timeout sets `prewarmComplete=true` with partial cache + WARN log; chart `startupProbe.failureThreshold × periodSeconds` matches deadline. Falsifier: pod restart count = 0 over 30 min.
- **Risk (Revision 7): cluster-mutation contention — composition events during prewarm load on top of prewarm load.** Mitigation: refresher gated on `cache.IsPrewarmComplete()`; refresher drains backlog post-Ready. Trade-off: convergence latency during prewarm window = `(prewarm_remaining_time) + refresher_drain_time` (worst case ~10 min during the prewarm window itself; one-time per pod start).
- **Risk (Revision 7): cardinality mismatch — RoleBinding mutations mid-prewarm.** Mitigation: snapshot enumeration once at prewarm-start; mid-prewarm mutations are warmed lazily on first user request post-Ready. No re-prewarm; no fail-Ready.
- **Risk (Revision 7): admin's wide-RBAC could cartesian-explode (admin × namespace × RestAction).** Mitigation: admin enumerated as ONE subject with portal-shipped RestAction set (~40), NOT cross-product-with-namespaces. Per-namespace scoping is handled by userAccessFilter (`0.30.9`) at dispatch time, not prewarm.
- **Risk (Revision 7): partial cache after timeout masks failure.** Mitigation: WARN log line `prewarm.timeout_exceeded` is alertable; next ledger row's mix-weighted-cold regresses visibly if prewarm is incomplete; ops escalation path documented.

### Estimated LoC and effort
- Code: ~310 production + ~150 test = ~460 LoC (Revision 7 adds ~60 LoC: ready handler hook, atomic flag, deadline-watcher goroutine, snapshot-once enumeration, timeout test).
- Chart: ~25 LoC (7 env keys + 2 probe-tune blocks + comments).
- Effort: **1 sprint** (unchanged — Revision 7 is mostly chart + ~60 LoC delta).

### Operating note (Revision 7) — first tag where pod startup time materially regresses
- Pre-`0.30.12` deploy time: ~60 s.
- `0.30.12` deploy time: ~6 s–2 min typical post-Revision-10 (cartesian × resolve cost reduced ~5–10× via frontend-mirror + all-bound-subjects scoping); worst-case ~3–5 min on heavy clusters; hard ceiling 20 min via `PREWARM_READY_DEADLINE_SECONDS` graceful timeout.
- **One-time per-pod-restart cost, NOT per-request.** Steady-state request latency is unaffected (in fact, cyberjoker first-nav cold is dramatically faster).
- **Customer framing:** this is a deploy-cost-vs-runtime-quality trade-off, not a regression. The runtime quality gain is L1-warm first-navigation for every bound subject, which is the lever that lands "1 s cold" north-star for the dominant 95 % cyberjoker mix.
- **`0.30.12` is the tag where "1 s fresh" + "1 s cold" north-star likely land together** — fresh via the refresher engineered at `0.30.8` (with `0.30.11`'s class-aware cadence if needed per Revision 12); cold via prewarm. If the ledger row for `0.30.12` shows mix-weighted cold ≤1 800 ms AND convergence p99 ≤1 s at SCALE=50000, the campaign has reached north-star on its two scoring axes.
- **Predicted Chrome MCP impact (Revision 7 binding):** at SCALE=50000, cyberjoker first-navigation cold drops 70–90 % vs `0.30.10` (warmed users hit L1 immediately on first request). Mix-weighted cold (95 % narrow + 5 % admin) hits the "1 s cold" north-star component IF prewarm is comprehensive (no timeout, no enumeration miss).

---

## Tag `0.30.13` (OPTIONAL) — Step 7: paginated Warmer LIST

### Branch base
`0.30.12`. Branch name: `ship-0.30.13-warmer-paginate`.

### What's implemented

**Conditional ship.** Only if `0.30.6` + `0.30.12` measurements show a residual startup-LIST OOM cliff. Per `clean-slate-proposal §4 Step 7`: "ONLY if Steps 1+3 don't already eliminate the need."

**Pre-flight gate:** if peak heap during `0.30.6`'s startup at SCALE=50000 is < 8 GB, SKIP entirely.

If shipped:
- **`internal/cache/warmup.go` (NEW, ~120 LoC).** Optional separate Warmer; post-eager-register, walks compositions resource type with paginated LIST (`Limit=500`); pre-fills informer cache. Capped inner concurrency at 2.
- **`main.go` (MODIFY, ~20 LoC).** Optionally call Warmer after `EagerRegisterAll`, gated by `WARMER_ENABLED=false` default.
- **`internal/cache/warmup_test.go` (NEW, ~30 LoC).**

### Chart values (binding constraint 4)
**Vs `chart-0.30.12`:**
- **ADDED:**
  - `env.WARMER_ENABLED: "false"` (default off; opt-in only after pre-flight gate is met).
  - `env.WARMER_INNER_CONCURRENCY: "2"`.
- **REMOVED:** none.

**Stripped-chart deliverable:** `chart-0.30.13` (= `chart-0.30.12` + 2 env keys).

### Portal compatibility (binding constraint 5)
**Required portal version: `portal-0.30.9-uaf-restored`.** Unchanged. Per Revision 5, no coordination latency.

### Expected results

#### Customer-facing Chrome MCP
- **At SCALE=5000:** ±0 ms (`0.30.6`'s eager covers 5 K). Step 7 likely not needed.
- **At SCALE=50000 (cache=on):**
  - Admin cold: ±0 to -200 ms vs `0.30.11`.
  - Cyberjoker cold: ±0 to -200 ms.
  - Mix-weighted cold: ±0 to -200 ms.
  - Mix-weighted warm-p50: ±0 ms.

#### Convergence (Revision 4)
- ±0 to -100 ms vs `0.30.12`. Warmer affects startup cold, not steady-state convergence.

#### Mechanism-level
- Memory at 50 K: -1 to -3 GB peak during initial LIST.
- Pod restart 30-min: 0 (target).
- Time-to-Ready: +10 to +30 s vs `0.30.12`.

#### Code-path falsifier
INFO at startup: `warmer.complete resource_types=N pages=K total_objects=M wall_time_ms=Y peak_heap_mb=H`. If `peak_heap_mb` is 50 %+ vs `0.30.12` startup peak, Warmer didn't pay. ROLLBACK.

#### Pre-flight falsifier
Measure peak heap during `0.30.12`'s startup at SCALE=50000. If <8 GB, no pagination needed → SKIP.

#### Three-pass expectations (per §3)
- **Pass 1 (S6/S7/S8):** informational; ±0–10% vs `0.30.12` (Warmer affects startup heap, not steady-state mass-stage).
- **Pass 2 (Chrome MCP):** ±0 to -200 ms cold/warm vs `0.30.12`.
- **Pass 3 (per-mutation):** **R4 + R12 binding.** ±0 to -100 ms vs `0.30.12`.

### What this tag does NOT do
- Does NOT replace eager AddResourceType (Step 3).
- Does NOT add cohort prewarm (audit blacklist; targeted prewarm at `0.30.12` is the sharper form).
- Does NOT add per-user warmup (Lever A page-loop walked 0 pages on narrow Role-Based Access Control).
- Does NOT add Q-OOM-WARMER's 2-h-soak regression mode.

### Risks
- **Risk: Warmer regresses RSS at +2 h soak** (audit `0.25.320` Row 11). Mitigation: paginated Warmer is startup-only; no continuous fetch.
- **Risk: Warmer doesn't move cold-path number** (`0.30.6` already covered). Mitigation: PM gate predicts ±200 ms; if delta in noise, ROLLBACK or `WARMER_ENABLED=false`.
- **Risk: customer turns Warmer on without paginating, regresses to audit `0.25.319` OOM.** Mitigation: paginated LIST hardcoded; no env var to disable.

### Estimated LoC and effort
- Code (if shipped): ~150 production + ~30 test = ~180 LoC.
- Chart: ~5 LoC.
- Effort: **0.5 sprint** if shipped; 0 if skipped.

---

## Cross-cutting predictions and trajectory

**Measurement source for every tag:** `e2e/bench/snowplow_test.py` (post-Step-−1) Phases 4 + 5 + 6 + new Phase 8 + new Phase 9 (pprof forensic snapshot, per Revision 15) — orchestrated as the **four-pass measurement structure (§3)**: Pass 1 (Phase 6 destructive cluster setup, ~hours) → Pass 2 (Phases 4+5 steady-state UX, ~30–60 min) → Pass 3 (Phase 8 per-mutation pulse, ~30 min) → Pass 4 (Phase 9 pprof goroutine + heap snapshot, ~5 min; mandatory from `0.30.2` onward).

**Per-tag verification cycle: 2.5–4 hours, NOT 30 minutes.** Total campaign verification: on the order of days minimum across 14 tags. Acceptable per Diego's "minutes are OK" precedent for prewarm — verification budget is a one-time-per-tag deploy cost, not a customer-facing latency cost. **Operational implication:** if a tag fails its R4 (`0.30.8+`) or R12 (`0.30.11+`) gate and requires code change + re-verification, the iteration cycle is ~half-day per attempt; campaign-level STOP gate from `0.30.8` is more expensive to clear than at earlier tags. Build conservatism into the decision to ship `0.30.8` (the first R4-binding tag).

Cumulative honest projection from Tag `0.30.0` → Tag `0.30.13`, mix-weighted Chrome MCP cold, at SCALE=50000:

| Tag      | Predicted mix-weighted cold range          | Cumulative reduction vs `0.30.0` | Convergence p99 at SCALE=50000 cache=on |
|----------|--------------------------------------------|----------------------------------|-----------------------------------------|
| 0.30.0   | TBD; hypothesis ≥20 000 ms or timeout      | baseline (the floor)             | timeout-likely (cache=off)              |
| 0.30.1   | unchanged (cache=off default)              | 0 %                              | timeout-likely (cache=off default)      |
| 0.30.2   | identical to `0.30.0` (±50 ms)             | 0 % (forensic baseline + pprof)  | identical to `0.30.0`                   |
| 0.30.3   | identical to `0.30.1` (±50 ms)             | 0 % (forensic baseline + pprof)  | identical to `0.30.1`                   |
| 0.30.4   | 4 050–10 100 ms                            | 50–80 %                          | 8 000–20 000 ms (intermediate; trajectory only) |
| 0.30.5   | 4 050–10 100 ms (memory delta only)        | 50–80 %                          | 8 000–20 000 ms                         |
| 0.30.6   | 3 050–7 600 ms                             | 60–85 %                          | 8 000–20 000 ms (eager doesn't move convergence) |
| 0.30.7   | 3 050–7 600 ms (warm-p50 main impact)      | 60–85 %                          | bound by time-to-live (intermediate state; refresher lands at `0.30.8`) |
| 0.30.8   | 3 050–7 600 ms (correctness + convergence) | 60–85 %                          | **800–2 500 ms** (first contested <1 s tag at 50 K; STOP gate may fire if mechanically impossible within DELETE-only invalidation) |
| 0.30.9   | ~3 600–5 100 ms (≈ Row 7 4 100 ms)         | 75–95 %                          | 800–2 500 ms (refilter overhead absorbed by refresher tune)              |
| 0.30.10   | ~2 500–3 100 ms                            | 85–95 %                          | **600–1 500 ms** (best case <1 s honoured at 50 K; worst case = STOP at `0.30.8`) |
| 0.30.11   | ±0 ms vs `0.30.10` (infrastructure tag)     | unchanged                        | **400–1 200 ms** (Revision 12 class-aware refresh cadence: HOT entries ≤500 ms; closes the gap if `0.30.8`'s class-blind cadence missed) |
| 0.30.12  | ~1 000–1 800 ms                            | 90–98 %                          | ±50 ms vs `0.30.11` (frontend-mirror prewarm warms cold path, not convergence; Revision 7 Ready-gate adds time-to-Ready cost ~6 s–2 min) |
| 0.30.13  | ~1 000–1 800 ms (±0 to -200 ms)            | unchanged                        | ±0 to -100 ms                           |

Honest reading:
- **Cold:** by `0.30.9`, the campaign should land at-or-below Row 7's 4 100 ms cold (per Diego addition #2). At `0.30.12`, frontend-mirror prewarm closes most of the remaining gap to north-star (1 000 ms cold) for every bound subject's first-nav case (the dominant 95 % of mix). The residual gap at `0.30.12`/`0.30.13` is renderer/SPA-bound. **`0.30.12` is the tag where "1 s fresh" + "1 s cold" north-star likely land together** — fresh via `0.30.8`'s refresher (with `0.30.11`'s class-aware cadence if `0.30.8` alone misses) + cold via prewarm.
- **Convergence:** the campaign first achieves <1 s p99 cache=on at SCALE=50000 in the BEST case at `0.30.10` (~600 ms p99 in the optimistic range). The worst case (~1 500 ms p99) requires the Revision 4 STOP gate from `0.30.8` to fire — at which point Diego receives mechanism data and decides whether to invoke the STOP condition (campaign pauses) or accept north-star miss on the "1 s fresh" component while honouring cold + warm. **`0.30.11`'s class-aware refresh cadence (Revision 12) is the next mechanism to evaluate after `0.30.8` if class-blind cadence misses the gate.** Per Diego's just-in ruling, `feedback_l1_invalidation_delete_only.md` is binding regardless of mechanical outcome; STOP is not a rule-revisit signal.

**Riskiest-tag forecast (post-Revision-15 renumber):**
- **#1 risk: `0.30.8`** (resolved-output cache dependency tracking + refresher; was `0.30.6` pre-Revision-15). First R4 binding gate at SCALE=50K; first contested <1 s p99 tag.
- **#2 risk: `0.30.12`** (Ready-gate + frontend-mirror prewarm; was `0.30.10` pre-Revision-15). Re-couples Ready to prewarm; death-spiral guardrail is the critical mitigation.
- **#3 risk: `0.30.9`** (atomic userAccessFilter + ServiceAccount-dispatch + refilter; was `0.30.7` pre-Revision-15). Correctness-critical atomic ship; cluster-wide-read data without refilter would be a data leak.
- **Bottom-tier risk: `0.30.2` and `0.30.3`** (pprof side-effect import, zero behaviour change at idle). ~2 LoC each; ships paired with ~30 min total dev work + CI build.

Architect-plan citations that previously referenced "0.30.6 refresher = #1 risk" or "0.30.10 Ready-gate = #2 risk" must be re-anchored to `0.30.8` and `0.30.12` respectively, per the Revision-15 renumber. The audit doc and historical regression-journal entries that reference pre-Revision-15 tag numbers remain unchanged (audit is historical; only the forward-plan renumbers).

---

## Stop conditions (from `clean-slate-proposal §4` + 2026-05-09 final revision)

- Reach Row 7 (~4 100 ms cold / ~1 348 ms warm): primary objective. Likely at `0.30.9`.
- Reach within 30 % of north-star: secondary; likely requires SPA work — but `0.30.12` (frontend-mirror prewarm) is the lever that closes most of the cold-path gap for the dominant cyberjoker mix.
- STOP if a step's regression gate fires twice in a row (PM amendment 5).
- STOP if cache=on Chrome MCP exceeds cache=off Chrome MCP at ≤5 K (Diego addition #2).
- STOP if `chart-0.30.X` smoke fails (environment-rejection log on boot).
- STOP if `portal-0.30.X` lags by more than 1 sprint (per Revision 5, snowplow team owns the portal repo; latency is internal).
- **STOP at `0.30.4`: any single `SubjectAccessReview` call observed in cache=on path during bench (Revision 1 hard rule).**
- **STOP at `0.30.8`: convergence_p99 > 1 s at SCALE=50000 cache=on AFTER refresher cadence has been tuned to 200 ms with parallelism = 8 AND `refresher.latency_ms_p99` is honoured (refresher itself is not the bottleneck). Per Diego's just-in ruling, `feedback_l1_invalidation_delete_only.md` is BINDING and PERMANENT — UPDATE never evicts. If <1 s p99 is mechanically impossible within DELETE-only invalidation at scale, the ship loop pauses; the architect surfaces failure data to Diego. THIS IS NOT A RULE-REVISIT SIGNAL.**
- **STOP at `0.30.9`: refilter overhead causes convergence_p99 to regress past `0.30.8`'s post-tune number; re-invoke `0.30.8` STOP gate decision.**
- **STOP at `0.30.12`: frontend-mirror prewarm doesn't move cyberjoker cold by >1 000 ms at SCALE=50000 (audit's mixed prewarm verdict re-applies); ROLLBACK or `PREWARM_ENABLED=false`.**
- **STOP at `0.30.12` (Revision 7): time-to-Ready > 1 200 s (20 min) at SCALE=50000 AND graceful timeout did not fire (kubelet restart-loop = Q-PREWARM-R2 spiral re-emerged); ROLLBACK + investigate why `prewarmComplete=true` was not set on deadline expiry.**
- **STOP at `0.30.12` (Revision 7): pod restart count >0 over a 30-min post-deploy window (death-spiral guardrail not working); ROLLBACK.**
- **STOP at `0.30.11` (Revision 14): tester correctness gate fails — negative-case action ref triggers spurious refresh OR positive-case render ref doesn't trigger refresh; ROLLBACK.**

---

## Operating rules applied throughout

- Per `feedback_data_driven_workflow.md`: every prediction is testable; falsification rules are explicit.
- Per `feedback_failure_mode_data.md`: each step's falsifiers expose failure-mode anomalies.
- Per `feedback_north_star_is_frontend_ux.md`: scoring is Chrome MCP wall-clock cold + warm + **convergence (Revision 4)**; curl is mechanism only.
- Per `feedback_test_scale_50k.md`: SCALE=50000 mandatory; SCALE=5000 added per Diego #3 as escape-hatch tier.
- Per `feedback_compositions_panels_in_scope.md`: compositions-panels included in mix-weighted measurements.
- Per `feedback_l1_invalidation_delete_only.md`: BINDING and PERMANENT (Diego just-in ruling 2026-05-09 mid-edit). Applies to Steps 4a/4b/5/6/6.5/7. The refresher RE-RESOLVES on UPDATE without evicting. **The rule is closed — not a tag-decision.** If <1 s p99 is mechanically impossible within DELETE-only invalidation at scale, that is a STOP condition at `0.30.8`, NOT a rule-revisit signal.
- Per `feedback_no_special_cases.md`: userAccessFilter is a CustomResourceDefinition field, not Go branches.
- Per `feedback_restaction_no_widget_logic.md`: refilter operates on raw informer objects.
- Per `pm-team-operating-model-2026-05-07.md` §3: pre-flight `uptime_at_capture ≥ 600 s` applied to every ledger row.
- Per `feedback_helm_only_for_portal.md` + `feedback_chart_only_for_snowplow.md`: chart values flow exclusively through `helm upgrade --reuse-values --set ...`; no `kubectl set env` survives.
- Per Diego constraint 4: every chart-value claim sourced from current `braghettos/snowplow-chart/chart/values.yaml` (audited 2026-05-09 via `gh api`).
- Per Diego constraint 5: every portal-compatibility claim cites which RestAction YAML field is unsupported at which tag (audited 2026-05-09 via `gh api`); per Revision 5, snowplow team produces portal artefacts directly.
- Per Diego constraint 6: all tags are `x.y.z`; sub-ships consume own `z` increments.
- **Per Diego constraint 7 (Revision 1 binding): cache=on path NEVER calls `SubjectAccessReview`. All Role-Based Access Control checks are in-process via `EvaluateRBAC` against informer-cached `Role`/`RoleBinding`/`ClusterRole`/`ClusterRoleBinding`. Cache=off path retains `SubjectAccessReview` for correctness baseline.**
- **Per Diego constraint 8 (Revision 2 binding): `EvaluateRBAC` fires on every RestAction dispatch in cache=on mode, regardless of userAccessFilter presence. userAccessFilter changes WHO dispatches, not WHETHER Role-Based Access Control is enforced.**
- **Per Diego constraint 9 (Revision 4 binding): convergence p99 < 1 s at SCALE=50000 cache=on is a scoring metric. Phase 6 of the bench captures convergence_p50 + convergence_p99 across cache_on + cache_off (4 new ledger columns). The mechanism is the background refresher within DELETE-only invalidation; the rule is binding and permanent.**
- **Per Diego constraint 10 (Revisions 6 + 9 + 10 + 11 binding): targeted prewarm replays the hardcoded `FrontendInitialRenderCalls` constant (mirrored from `https://github.com/braghettos/frontend` per Revision 11b; CI mirror test enforces drift) for ALL `RoleBinding` + `ClusterRoleBinding` subjects whose bound rules grant `get`/`list` on widget/RestAction-relevant resources (Revision 10: NOT HOT-only — Revision 11a confirms HOT-scoping at startup is architecturally impossible). Lands at Tag `0.30.12`. Bounded concurrency, frontend-mirror call set, and Revision 7 graceful-timeout death-spiral guardrail prevent the audit's blacklisted prewarm-pool failure mode; eager resource-type registration (`0.30.6`) ensures no AddResourceType storm.**
- **Per Diego constraint 11 (Revisions 7 + 7a binding): pod readiness probe gates on `prewarmComplete=true` at Tag `0.30.12`. Default `PREWARM_READY_DEADLINE_SECONDS=1200` (20 min) graceful timeout escape; chart `startupProbe.failureThreshold × periodSeconds` matches deadline. Death-spiral guardrail prevents Q-PREWARM-R2 restart-loop regression.**
- **Per Diego constraint 12 (Revisions 8 + 11c + 12 binding): subject HOT/WARM/COLD activity classification at Tag `0.30.11`. NOT load-bearing for prewarm scoping (Revision 10 reverses HOT-scoping; Revision 11a confirms classifier empty at startup). LOAD-BEARING for Revision 12 class-aware refresh cadence (HOT ≤500 ms p99 / WARM ≤2 000 ms / COLD ≤10 000 ms) and class-aware L1 LRU retention. Activates HOOKS in `0.30.7`/`0.30.8`.**
- **Per Diego constraint 13 (Revision 13 binding): three-edge dependency recording at Tag `0.30.8` — Widget→resourcesRefs RENDER-ONLY (Revision 14 filter via existing `extractActionRefIDs`/`extractChildRefs` pattern from `prewarm.go`); Widget→apiRef→RestAction; RestAction→inner-K8s-call (dynamic, recorded at resolve time via wrapped K8s API call). Four-bucket `depMap` lookup pattern covers exact + ns-list + cluster-name + cluster-list invalidation buckets.**
- **Terminology (Revision 3): forward-plan uses no acronyms beyond universal HTTP/JSON/YAML/API; first-use expansions for Role-Based Access Control and CustomResourceDefinition; "permission-check cache" replaces "B-prime EvaluateRBAC LRU"; "bounded cache" / "least-recently-used eviction" replaces bare "LRU"; "RestAction" replaces "RA"; "resource type" replaces "GVR"; "ServiceAccount" replaces "SA"; "time-to-live" replaces "TTL"; "OpenTelemetry" replaces "OTel"; "per-object cache mirror" is the audit anti-pattern (never re-introduced). Project-codename references (Q-COLD-1, Q-MIRROR-REMOVAL, etc.) remain only as historical pointers to audit phases.**
- **Per Revision 5 (binding): snowplow team owns portal YAML modifications. `portal-0.30.0` (userAccessFilter-stripped) and `portal-0.30.9-uaf-restored` are produced by snowplow dev (portal repo authority); no cross-team coordination latency.**
- **Per Revision 15 (binding): every snowplow binary from `0.30.2` onward MUST expose `/debug/pprof/*` endpoints. Forensic snapshots (Pass 4 — `/debug/pprof/goroutine?debug=2` and `/debug/pprof/heap`) are part of the canonical ledger-row artifact set, stored under `/tmp/snowplow-runs/<tag>/pprof_goroutine.txt` and `/tmp/snowplow-runs/<tag>/pprof_heap.pb.gz`. `0.30.0` and `0.30.1` are preserved as historical superseded baselines (no pprof). `0.30.2` (= `0.30.0` + pprof) and `0.30.3` (= `0.30.1` + pprof) are the forensic-canonical baselines for all cross-tag goroutine-class and heap-class diffs from `0.30.4` onward.**

---

## Source citations (absolute paths)

- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/clean-slate-proposal-from-0.20.5.md`
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/informer-as-cache-primer.md`
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/docs/feature-audit-since-0.20.5.md`
- `/Users/diegobraga/krateo/snowplow-cache/snowplow/.cursor/plans/snowplow_cache_matrix_tests_d0110e82.plan.md`
- `/private/tmp/claude-501/-Users-diegobraga-krateo-snowplow-cache-snowplow/d95b6c4b-bf71-47d1-a8be-fff7e42c9e22/tasks/acb35fb3dcd3709ca.output` (PM amendments)
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/project_redis_removal.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/project_north_star_ledger.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/project_regression_journal.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/project_feature_journal.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_north_star_is_frontend_ux.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_data_driven_workflow.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_failure_mode_data.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_test_scale_50k.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_l1_invalidation_delete_only.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_no_special_cases.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_restaction_no_widget_logic.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_compositions_panels_in_scope.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_helm_only_for_portal.md`
- `/Users/diegobraga/.claude/projects/-Users-diegobraga-krateo-snowplow-cache-snowplow/memory/feedback_chart_only_for_snowplow.md`
- `/Users/diegobraga/.claude/analysis/pm-team-operating-model-2026-05-07.md`
- Source-of-truth chart values audited 2026-05-09: `gh api repos/braghettos/snowplow-chart/contents/chart/values.yaml --jq .content | base64 -d`
- Source-of-truth portal RestActions audited 2026-05-09: `gh api repos/braghettos/portal/contents/blueprint/templates --jq '.[].name'`

— *End of detailed implementation plan.*
