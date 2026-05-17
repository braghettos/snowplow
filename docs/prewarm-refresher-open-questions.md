# Prewarm + Runtime-Refresher — Open-Question Register

> Durable reconstruction of the architect's task-#102 consolidation.
> Recovered from the session transcript and persisted so Ships D/E/F are
> scoped from this doc, never a transcript grep again.
>
> Scope: every open question and flagged item across the prewarm +
> runtime-refresher design work (deliverables #90, #93, #97, #99,
> #100/#101, and the Q-round). Each item carries its one-line statement,
> the architect's proposed resolution, and its current ruling state.

---

## A. Resolved items (R1–R6) — ruled by Diego, listed for completeness

| ID | Statement | Origin | State |
|----|-----------|--------|-------|
| **R1** | **ADD == UPDATE** — `AddFunc→OnAdd` dirty-marks dependent L1 keys identically to `OnUpdate`. | #100 | Resolved — shipped (Ship A). |
| **R2** | **`OnDelete` per-dep-kind split** — evict the deleted object's own GET-dep entry; dirty-mark LIST-dep entries (stale-while-revalidate). | Q2 | Resolved — shipped (Ship A), refined by O16. |
| **R3** | **Offload GET-dep eviction to a worker** — the burst eviction must not run inline on the informer processor goroutine. | Q4 | Resolved — shipped (Ship A). |
| **R4** | **`AddAutoDiscoverGroup` → `ReconcileAutoDiscoverCRDs`** — a runtime new-group addition triggers a CRD re-scan so already-replayed CRDs in that group register. | Q5 | Resolved — shipped (Ship D / 0.30.114, D1). |
| **R5** | **`objects.Get` dep-tracking** — `objects.Get` records an Edge-type-3 `Record` against the ctx L1 key, so widget→`apiRef`-RESTAction (and widget-CR) edges land. | Q6 | Resolved — shipped (0.30.110). |
| **R6** | **CRD-DELETE → informer teardown** — a `DeleteFunc` on the CRD informer + a `RemoveResourceType` stops the leaked per-GVR informer. | Q7 | Resolved — shipped (Ship D / 0.30.115, incl. Option 1 standalone-informer re-add fix). |

> Note: the register defines R1–R6. There is no R7 — references to
> "R4/R6" for Ship D scoping point at R4 and R6 above, NOT the
> convergence binding gate also labelled "R4" in
> `docs/implementation-plan-detailed.md` (a distinct, unrelated numbering).

---

## B. Open items (O1–O16) — with proposed resolution

### #90 — RBAC-derived prewarm

- **O1. Authn group→members source.** Group-subject prewarm must enumerate a group's concrete members; snowplow may only see groups via per-login JWT claims. *Proposal:* do **not** build a group-membership service. Prewarm `User`/`ServiceAccount` subjects directly; for `Group` subjects prewarm only members **previously seen at login** (a process-lifetime set of observed `(user, groups)`); unseen group-only users take one lazy cold first-login. Honest structural limit, not a defect. **State: ACCEPTED (Diego "accept all 8").**
- **O2. `system:authenticated` / cluster-wide-group bindings.** A binding to `system:authenticated` grants everyone — cannot enumerate. *Proposal:* treat as lazy-only; never prewarm an unbounded population. **State: ACCEPTED (Diego "accept all 8").**
- **O3. `extras`-bearing navigations structurally cold.** Filter/search `/call`s carry request-time `extras` in `ComputeKey` — unknowable pre-login. *Proposal:* explicitly scope **out** of prewarm acceptance; the no-`extras` base navigation is prewarmed, `extras` variants lazy-warm + refresher-maintain. **State: ACCEPTED (Diego "accept all 8").**

### #93 — api-stage L1

> **Scope (ratified at Ship E implementation):** the Ship E win is
> **within-RESTAction stage reuse** — re-resolving the SAME RESTAction
> (a second `/call`, a refresher re-resolve, a different page, another
> request by the same identity) reuses that RESTAction's own per-stage
> L1 entries instead of re-dispatching every stage's K8s call. Combined
> with O4's dep-scoped refresh, a stage entry is also kept fresh by the
> refresher when an informer event touches its inputs. The stage key
> folds in the owning RESTAction identity AND the author-chosen
> `stage.Name`, so it is RESTAction-scoped and does NOT dedup a stage
> across DIFFERENT RESTActions.
>
> *Future enhancement (out of Ship E scope):* cross-RESTAction stage
> sharing is possible in a later ship IF the stage key hashes the full
> rendered call signature (path + verb + headers + endpoint) rather than
> the author-chosen `stage.Name` — keying on the name alone would risk a
> wrong-data serve when two RESTActions reuse a stage name for different
> rendered calls.

- **O4. api-stage dep edges.** api-stage L1 entries need their own `RecordList`/`Record` edges or the refresher never re-resolves them. *Proposal:* **adopt** — mandatory; the per-stage key-swap records that stage's K8s call against the stage entry's key. **State: required (absorbed). Ship E scope.**
- **O5. Filter-hash stability.** The stage `filter` jq string is part of the api-stage key; whitespace/template-render variation must not fragment the key space. *Proposal:* hash a **canonicalized** filter (trim, collapse whitespace) — or hash the post-Helm-render string captured once. **State: open — small, decide at impl. Ship E scope.**
- **O6. L1 cardinality / memory budget.** N identities × M api-stages multiplies entry count; content-hash dedup is now load-bearing. *Proposal:* re-size `maxEntries`/`maxBytes` (`resolved.go`), add an eviction-pressure metric, gate the api-stage L1 behind a flag so it can be disabled if pressure is bad. **State: open — needs a measured budget. Ship E scope.**
- **O7. Templated-path informer pre-provisioning.** `lazyRegisterInnerCallPaths` skips `${...}` paths; the informer registers only at first post-substitution dispatch. *Proposal:* **accept** first-dispatch registration — the prewarm walk *is* that first touch. **State: ACCEPTED (Diego "accept all 8").**
- **O8. Stage `dependsOn` ordering — can stage 2 be served from L1 without stage 1?** Stage 2's iterator input is stage 1's output. *Proposal:* stage 1 must still run (cheap — usually one LIST, itself L1-served) to produce the iterator; the win is stage 2's per-namespace calls each being L1 hits. "Top-to-bottom from L1" means *every stage is an L1 lookup*, not *stage 1 is skipped*. **State: ACCEPTED (Diego "accept all 8"); document the interpretation. Ship E scope.**

### #97 — resolve-per-identity

- **O9. subject→bindings index.** `filterListByRBAC` is O(items × bindings) per LIST — costly at 50K objects. *Proposal:* precompute, per identity, the `matchedBindings` set once and reuse it across all per-item `EvaluateRBAC` calls. **State: open — strong recommend; dev follow-up, does not block flat prewarm.**
- **O10. api-stage dep-edge cardinality vs `DEPS_MAX_RECORDS`.** N×M×per-object records may hit the 1M cap (silently drops). *Proposal:* meter `totalRecords`; re-size the cap; prefer LIST-dep granularity (one record each) over GET-deps where possible. **State: open — meter first. Related to Ship E (O6 sibling).**

### #99 — runtime refresher

- **O11. Refresher identity sufficiency / stale group membership.** The refresher re-resolves under `entry.Inputs.Username/Groups`; changed groups mean a stale-group refresh. *Proposal:* **accept** for bridge code — the user's next live `/call` recomputes the key with fresh groups; TTL backstops. Flag, don't fix. **State: ACCEPTED (Diego "accept all 8").**
- **O12. Refresh storm on a hot object.** A frequently-updated GVR re-dirties many keys. *Proposal:* the 500 ms dedup window + bounded queue cap it; meter `skippedFullTotal`; the dormant priority queue is the escalation path if it climbs. **State: ACCEPTED (Diego "accept all 8") — monitor-only.**
- **O13. Legacy `Inputs==nil` entries.** Pre-#93 entries cannot drive a re-resolve. *Proposal:* keep the silent skip-to-TTL; they age out within one TTL of upgrade. **State: ACCEPTED (Diego "accept all 8") — no action.**

### #100/#101 — residual

- **O14. `syncCh[gvr]` nil at `AddFunc` registration.** If nil, `<-nil` blocks → `default` drops every ADD. *Proposal:* the channel is created before the handler is added, so it binds non-nil; add a one-shot WARN as a defensive falsifier. **State: open — recommend accept + WARN.**
- **O15. Empty-L1-key loud-fail mode.** *Proposal:* WARN + counter `recordDroppedNoKey` in prod; **panic in test mode**. **State: open — recommend adopt as specified in #101.**

### Q-round — the key interaction

- **O16. Q6/Q2 interaction — GET-dep on a deleted *definition* object.** When a widget L1 entry GET-depends on a RESTAction CR (R5) and that CR is deleted, the Q2 ruling read literally would evict the widget that *referenced* the CR — but the widget is not the CR's representation, it merely depends on it. *Proposal:* the Q2 evict rule applies **only** to an entry that **IS** the deleted object's resolved representation (key's GVR/ns/name == the deleted object). An entry that merely **GET-depends on** the deleted object is a **dependent** — **dirty-mark it** (stale-while-revalidate), like a LIST-dep. **Refinement of R2/Q2: evict only the self-representation entry; dirty-mark both LIST-deps and dependent-GET-deps.** **State: RULED — refinement adopted (this is the evict-self ruling); shipped in Ship A and reaffirmed by Ship D's D2 (`OnResourceTypeRemoved` dirty-marks, never evicts).**

---

## C. Ruling summary

- Diego ruled **"accept all 8"** → **O1, O2, O3, O7, O8, O11, O12, O13** accepted as proposed.
- **O16** — the evict-self refinement is ruled and adopted: eviction is authorized **only** for an entry that is the deleted object's own self-representation; LIST-deps and dependent-GET-deps are dirty-marked (stale-while-revalidate).
- **R1–R6** — all resolved and shipped (Ships A / D, plus 0.30.110).
- **O4, O5, O6, O8** — Ship E scope (api-stage L1).
- **O9, O14, O15** — open follow-ups, not blocking; **O10** metered alongside Ship E.

---

## D. Ship mapping

| Ship | Scope | Open items | Status |
|------|-------|------------|--------|
| **Ship D** | CRD-lifecycle cache handling | **R4, R6** | DONE — 0.30.114 (D1+D2) + 0.30.115 (R6). |
| **Ship E** | api-stage L1 — **within-RESTAction** stage reuse: cache resolved stage output at RESTAction api-stage granularity behind `RESOLVED_CACHE_APISTAGE_ENABLED` (default off); the same RESTAction re-resolved reuses its own stage entries; dep-scoped refresh keeps them fresh. Depends on Ship C (refresher). | **O4, O5, O6, O8** | Implemented — 0.30.116 (default-off; budget set from 50K bench). |
| **Ship F** | resolve-per-identity prewarm — flat prewarm walk replaying frontend navigation per observed identity. | **O1, O2, O3, O7** (prewarm O-items); **O9** as a perf follow-up. | Not started. |

> Ship C (runtime refresher) shipped at 0.30.112–0.30.113; it is the
> prerequisite for Ship E (the refresher maintains api-stage entries).
