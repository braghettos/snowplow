# Ship B — Typed RBAC snapshot (D2)

**Status:** GATE-PASSED — GO for development (PM gate, 2026-05-20).
**Campaign:** A+B+C resolver-path rebuild (verdict 0.30.136). Ship B is lever 2 of 3.
**Authoritative reference** for the developer (implements to AC-B.1..B.13) and the
tester (validates against §6). The architect-review gate checks the diff against
this document.

---

## §0. Re-attribution correction (load-bearing)

The verdict §3 framing said D2 includes "decode-on-access of every `*bytesObject`."
That is wrong, and the design corrects it before designing.

**TRACED:** the 4 RBAC GVRs route to the *stock* informer with `stripAndType*`
transforms (`strip.go:139-153` + `strip.go:101-106`), so the indexer holds typed
`*rbacv1.{ClusterRoleBinding, RoleBinding, ClusterRole, Role}` directly.
`asRuntimeObject` (`bytesobject.go:423-440`) takes the typed-passthrough branch at
`bytesobject.go:434` — **allocation-free**. Confirmed by the alloc diff:
`asRuntimeObject` does not appear in the delta. The 0.30.6 ship (`d0b3baf`)
already eliminated per-call decode.

The actual 1.23 GB / 60-call D2 allocation is entirely:

| Symbol                                                | Flat / 60 calls | What allocates                                                                                  |
|-------------------------------------------------------|-----------------|-------------------------------------------------------------------------------------------------|
| `cache.(*ResourceWatcher).ListTypedObjects`           | **614 MB**      | `out := make([]runtime.Object, 0, len(items))` + `append` (`watcher.go:1826-1834`) — N pointer-slots per call, N=31797 cluster-wide |
| `client-go/tools/cache.(*threadSafeMap).List`         | **574 MB**      | `cache.List()` builds a fresh `[]interface{}` of every indexer key (`watcher.go:1816`)          |
| `cache.(*threadSafeMap).ByIndex`                      | ~43 MB          | Namespace-indexed RoleBindings slice (`watcher.go:1818`)                                        |

Per call: cluster-wide CRB slice = 31797 × 16 B (pointer) ≈ 509 KB per call from
each of the two slices = ~1 MB. Across 60 calls × ~10 `evaluateAgainstInformer`
invocations/call (one CRB cluster-wide LIST per RBAC eval; RoleBindings
additionally if `opts.Namespace!=""` — cardinality measured from `rbacMemo` use at
`informer_dispatch_rbac.go:148`) ≈ 600 invocations × ~2 MB ≈ 1.2 GB — matches the
diff.

**Ship B target reduces to: eliminate the two slice rebuilds (`threadSafeMap.List`
copy + `ListTypedObjects` result-slice make/append).** The lever is shaped the same
as the verdict described, but the rationale is "no per-call slice rebuild," not
"no per-call decode."

---

## §1. Mechanism — the typed RBAC snapshot

### §1.1 Data structure

A new package-level singleton in a new file `internal/cache/rbac_snapshot.go`
(sibling to `cache.go`, `bytesobject.go`, `strip.go`):

```go
// rbacSnapshot is an immutable, atomically-published view of the four RBAC
// GVRs — built by an informer event handler, read lock-free by
// evaluateAgainstInformer. Replaces the per-call ListTypedObjects +
// GetTypedObject indexer reads (Ship B / D2).
//
// Immutable post-publish: see §3 invariants. NO field is ever mutated by any
// goroutine after rbacSnap.Store(s) completes.
type rbacSnapshot struct {
    // Cluster-wide ClusterRoleBindings — read at evaluate.go:198.
    clusterRoleBindings []*rbacv1.ClusterRoleBinding

    // Namespace-keyed RoleBindings — read at evaluate.go:223.
    // map[namespace] -> []*rbacv1.RoleBinding. A namespace absent from the
    // map yields a nil slice; readers range over it with zero iterations.
    roleBindingsByNS map[string][]*rbacv1.RoleBinding

    // ClusterRoles + Roles indexed by lookup key — read at evaluate.go:254
    // (ClusterRole by name) and :270 (Role by ns/name). One map lookup per
    // matched binding's roleRef.
    clusterRolesByName map[string]*rbacv1.ClusterRole // key: name
    rolesByNSName      map[string]*rbacv1.Role        // key: ns + "/" + name
}

var rbacSnap atomic.Pointer[rbacSnapshot] // single writer, many readers
```

`atomic.Pointer` is Go std (`sync/atomic`, Go 1.19+). Snowplow already uses
`atomic.Uint64` (`evaluate.go:37`) and `atomic.Bool`. No new dependency.

### §1.2 Writer — informer event handlers

`NewResourceWatcher`'s `factory.Start` sets up the 4 RBAC informers with
`stripAndType*` transforms (TRACED `strip.go:144-153`). Ship B attaches an
additional `ResourceEventHandlerFuncs` to each of the 4 RBAC informers at the same
site `addResourceTypeLocked`/`startInformer` already attaches `depEventHandlers`
(TRACED `watcher.go:744`, `:1070`).

A single internal `rebuildRBACSnapshot(rw *ResourceWatcher)` function walks the 4
RBAC indexers **once**:

- `rw.informers[clusterRoleBindingsGVR].Informer().GetIndexer().List()` →
  type-assert each item to `*rbacv1.ClusterRoleBinding` (typed transform guarantees
  this; the defensive `asClusterRoleBinding` fallback at `evaluate.go:486-501` is
  preserved for the Unstructured-fallback case — kept BELOW the typed fast path).
- For RoleBindings: walk and partition into `map[ns][]*rbacv1.RoleBinding`.
- For ClusterRoles + Roles: walk and index by `name` / `ns + "/" + name`.
- Build a new `*rbacSnapshot`; `rbacSnap.Store(newSnap)`.

**Trigger:** every ADD/UPDATE/DELETE on any of the 4 RBAC informers schedules a
snapshot rebuild via:

```go
// rbacRebuildLock + rbacRebuildDirty: single-writer atomic.Bool tryLock with a
// dirty-flag re-rebuild. Multiple events during an in-flight rebuild collapse
// into ONE follow-up rebuild — k8s informer event handlers must NOT block.
//
// Same pattern as the watcher's L1 refresh (watcher.go:1028 lineage). Bounds
// goroutine count under event churn (no unbounded spawning).
var (
    rbacRebuildLock  atomic.Bool
    rbacRebuildDirty atomic.Bool
)

// scheduleRBACRebuild flips the dirty bit; if no rebuild is in flight, spawns
// ONE goroutine that drains the bit by looping: rebuild -> check dirty -> if
// dirty rebuild again. Exits cleanly when dirty is false on entry to the
// post-rebuild check.
func scheduleRBACRebuild(rw *ResourceWatcher) { ... }
```

This is exactly the **`atomic.Bool` tryLock** pattern Bug 7 fixed at
`watcher.go:1028` lineage (TRACED `project_cache_v0.25.104_handoff.md`'s "Bounded
async L1 refresh"). One in-flight rebuild + one queued; events beyond that
collapse. **Named-constant debounce window:** none today (event-driven, not
time-debounced); the dirty-flag re-rebuild absorbs bursts naturally. The ~10 ms
rebuild-duration estimate (§4.2) is recorded as a comment on
`rebuildRBACSnapshot` for future verification — NOT as a hard sleep. AC-B.12
empirically gates the resulting end-to-end mutation-propagation window.

**Initial population:** after `factory.Start` returns and the 4 RBAC informers
have `WaitForCacheSync`'d (TRACED `waitInformerSync` at `watcher.go:388`),
`rebuildRBACSnapshot` is called **synchronously once** to publish the initial
snapshot before the first request can hit `evaluateAgainstInformer`. A new
`AssertRBACSnapshotWired()` panics at boot if the 4 RBAC informers are registered
but no snapshot ever publishes — analogous to `AssertRBACTypedOverridesRegistered`
(`strip.go:173`).

### §1.3 Reader — `evaluateAgainstInformer`

The 3 reader call sites (`evaluate.go:198`, `:223`, `:254`, `:270`) **migrate to a
SINGLE `rbacSnap.Load()` at the top of `EvaluateRBAC`**, threaded down through
`evaluateAgainstInformer` and `roleRefPermits` as an explicit parameter.

**This is correctness-load-bearing — see AC-B.3.** A `Load()` per site would let
two reads observe two different snapshots within one evaluation: a ClusterRole
the snapshot just dropped could be read by site `:254` after site `:198`
already iterated a snapshot that referenced it, producing an incoherent permit
verdict against a half-deleted RBAC graph. The migration MUST pass one snapshot
pointer through the eval call chain so every read of a single eval sees the same
snapshot version.

Post-Ship-B reader shape:

```go
func EvaluateRBAC(ctx context.Context, opts EvaluateOptions) (bool, error) {
    // ... existing cache.Disabled() / cache.Global() preamble ...

    snap := rbacSnap.Load()
    if snap == nil {
        // AC-B.8 — degrade-to-deny pre-readiness gate. cache=on activated
        // but the 4 RBAC Servable channels have not yet all closed AND the
        // initial rebuildRBACSnapshot has not yet published. Fail closed —
        // never fall through to UserCan (would violate Revision 1).
        log.Warn("rbac.evaluate: snapshot not yet built — denying", ...)
        return false, fmt.Errorf("rbac: snapshot not yet built")
    }

    allowed, err := evaluateAgainstInformer(ctx, rw, snap, opts)
    // ...
}

func evaluateAgainstInformer(ctx, rw, snap *rbacSnapshot, opts) (bool, error) {
    for _, crb := range snap.clusterRoleBindings { // §198 — no List(), no slice alloc
        if !anySubjectMatches(crb.Subjects, opts) { continue }
        permits, err := roleRefPermits(rw, snap, "", crb.RoleRef, opts, log)
        // ...
    }
    if opts.Namespace != "" {
        for _, rb := range snap.roleBindingsByNS[opts.Namespace] { // §223
            // ...
        }
    }
}

func roleRefPermits(rw, snap *rbacSnapshot, ns string, ref RoleRef, opts, log) {
    switch ref.Kind {
    case "ClusterRole":
        cr, ok := snap.clusterRolesByName[ref.Name] // §254 — map lookup, no GetTypedObject
        if !ok {
            recordSnapshotMissCanary(ref.Kind, "", ref.Name) // AC-B.10
            return false, nil
        }
        return rulesPermit(cr.Rules, opts), nil
    case "Role":
        if ns == "" { return false, nil }
        r, ok := snap.rolesByNSName[ns+"/"+ref.Name] // §270
        if !ok {
            recordSnapshotMissCanary(ref.Kind, ns, ref.Name) // AC-B.10
            return false, nil
        }
        return rulesPermit(r.Rules, opts), nil
    }
}
```

Per-item `as{Kind}` type-assert helpers (`evaluate.go:486-547`) **stay**: they
remain reachable through `cache.Global()` direct paths only on the
Unstructured-fallback read path (test seeding, transform conversion failure). Per
the 0.30.6 plan §"Code-path falsifier" — fallback rate < 1% — Ship B does not
change that path; it bypasses the slice-allocation hot path that
`as{Kind}` sits behind.

`ListTypedObjects` and `GetTypedObject` are **NOT removed** — they have potential
non-RBAC callers (TRACED `watcher.go:1782-1786` doc). They stay; Ship B simply
stops calling them from the RBAC hot path.

---

## §2. Scope

The 4 RBAC GVRs, byte-identical to `rbacTypedGVRs` (TRACED `strip.go:101-106`):

| GVR                                                  | Snapshot field                                              | Reader site         |
|------------------------------------------------------|-------------------------------------------------------------|---------------------|
| `clusterrolebindings.rbac.authorization.k8s.io/v1`   | `clusterRoleBindings []*rbacv1.ClusterRoleBinding`          | `evaluate.go:198`   |
| `rolebindings.rbac.authorization.k8s.io/v1`          | `roleBindingsByNS map[string][]*rbacv1.RoleBinding`         | `evaluate.go:223`   |
| `clusterroles.rbac.authorization.k8s.io/v1`          | `clusterRolesByName map[string]*rbacv1.ClusterRole`         | `evaluate.go:254`   |
| `roles.rbac.authorization.k8s.io/v1`                 | `rolesByNSName map[string]*rbacv1.Role`                     | `evaluate.go:270`   |

**Ship B EXTENDS the 0.30.6 typed-RBAC indexer** (commit `d0b3baf`) — it does NOT
introduce a parallel mechanism. Typed transforms (`stripAndType*`,
`strip.go:144-153`) are unchanged; informer-routing exception delivering typed
items (`strip.go:139-142`) is unchanged; `as{Kind}` helpers stay as the defensive
fallback. Ship B adds **one** thing: a snapshot rebuilt on event, that bypasses
the per-call `ListTypedObjects`/`GetTypedObject` slice-allocation cost.

**`feedback_no_special_cases` compliance:** the snapshot's *fields* are
kind-typed (`*rbacv1.ClusterRoleBinding` etc.) because the `rbacv1` Go types are
kind-specific by construction — that is not a path/resource/user special-case in
resolver Go; it is the inherent shape of `k8s.io/api/rbac/v1`. The discrimination
of "which GVRs participate" is driven by the existing `rbacTypedGVRs`
registration set (`strip.go:101-106`); Ship B reads from the same set. No new
literal GVR list, no per-resource carve-out outside `rbacv1`'s own typing.

---

## §3. Concurrency — the load-bearing section

This is a **shared-vs-copy change** per
`feedback_shared_vs_copy_is_a_concurrency_change`. Pre-Ship-B every
`evaluateAgainstInformer` call materialized a fresh `[]runtime.Object` slice
(per-call private); post-Ship-B every call reads a **shared `*rbacSnapshot`
pointer**. The same hazard that bit Ship 0.30.128 (concurrent gojq map iteration)
applies if the shared snapshot is ever mutated by a reader. The design prevents
that by construction:

### §3.1 Invariants

1. **Snapshot immutability.** Once `rbacSnap.Store(s)` publishes a
   `*rbacSnapshot`, none of its fields are mutated by any goroutine. The writer
   builds a brand-new `*rbacSnapshot` (fresh maps, fresh slices) and `Store`s it.
   Readers `Load()` a pointer; the snapshot they hold remains valid for the
   duration of their use; Go's GC reclaims old snapshots after the last reader
   releases its pointer. No `sync.Mutex` needed on the read path.

2. **Reader contract: never write.** Readers iterate slices and maps
   (`for _, crb := range snap.clusterRoleBindings`) and read pointer fields
   (`crb.Subjects`, `crb.RoleRef`). They never assign to map keys, never append
   to slices, never modify the `*rbacv1.{...}` pointed-to objects.

3. **Writer contract: single writer.** `scheduleRBACRebuild` enforces *at most
   one in-flight rebuild* via `atomic.Bool` tryLock (`watcher.go:1028` lineage).
   Multiple events queue ONE follow-up. The writer never mutates a
   previously-published snapshot.

4. **Atomic publish.** `atomic.Pointer.Store` is a single memory-ordered write;
   `atomic.Pointer.Load` is a single read. No torn read, no half-built snapshot
   visible to a reader.

5. **No reader-writer lock contention.** Read path takes zero locks (`Load()` is
   lock-free). Writer takes zero locks on the read path (`Store()` is lock-free).
   The writer's *internal* rebuild may take whatever locks
   `Informer().GetIndexer().List()` takes inside client-go (`RWMutex.RLock` on
   `threadSafeMap`) — that contention is UNCHANGED from today because today's
   `evaluateAgainstInformer` already calls `List()` on the same indexer; Ship B
   moves the call from per-request to per-event, dramatically reducing frequency.

6. **Single-snapshot-per-evaluation.** AC-B.3 — a single `rbacSnap.Load()` at the
   top of `EvaluateRBAC` is threaded into `evaluateAgainstInformer` and
   `roleRefPermits` as an explicit parameter. A `Load()` per site is forbidden;
   it would let two reads in the same eval see two different snapshots.

### §3.2 The k8s indexer reads — concurrency-safe by client-go contract

The writer calls `gi.Informer().GetIndexer().List()` to read typed pointers.
Client-go's documented contract (`k8s.io/client-go/tools/cache.Store`): `List()`
returns a copy of the indexer's items slice; readers may treat the *slice* as
their own, but pointed-to objects are still owned by the indexer and **must be
treated as read-only**. Ship B's writer follows the same rule: it never mutates
the typed `*rbacv1.{...}` pointers it puts into the snapshot. Since
`evaluateAgainstInformer` also only reads those pointers (pre-Ship-B today),
safety posture is unchanged — the snapshot is just a per-event-cached copy of the
slice-of-pointers the reader already had today.

### §3.3 The `-race` test (prescribed — AC-B.5)

A new test `rbac_snapshot_race_test.go`:

```
TestRBACSnapshot_Race_ReaderWriter:
  - Seed the informer indexer with N=200 ClusterRoleBindings (typed
    pointers; deterministic subjects so reader assertions are meaningful).
  - Start 32 reader goroutines × 50 iters: each calls
    evaluateAgainstInformer(ctx, rw, opts) over a randomized opts and
    reads the result.
  - Concurrently start 1 writer goroutine: pumps 500 synthetic
    ResourceEventHandlerFuncs.OnAdd / OnUpdate / OnDelete callbacks at the
    rw.rbac handler, churning the indexer + triggering scheduleRBACRebuild.
  - Wait. `go test -race` must pass clean.
  - ADDITIONAL ASSERTION (AC-B.5): instrument scheduleRBACRebuild to count
    in-flight rebuild goroutines via atomic. At test end assert max
    in-flight <= 1 (the atomic.Bool tryLock invariant holds under churn).
```

`feedback_shared_vs_copy_is_a_concurrency_change` directly discharged.

### §3.4 Memory bound

A snapshot holds N₁ + N₂ pointers + N₃ + N₄ map entries. At the live cluster's
**N₁=31797, N₂=63316, N₃=31806, N₄=63314**:

- CRB slice: 31797 × 8 B = **254 KB**
- RB partition: ~506 KB + namespace map overhead ≈ **1 MB total** (INFERRED)
- ClusterRoles map: ~**2 MB**
- Roles map: ~**5 MB**
- **Total resident: ~8 MB.** Two snapshots transiently during rebuild = **16 MB
  peak.** Negligible against the 11 GB RSS. No memory regression.

---

## §4. RBAC correctness

The snapshot must produce **the same set of matched bindings as the current
List-based eval** for any `EvaluateOptions`. The 0.30.109+ fidelity contract
(TRACED `evaluate.go:1-12`) is not relaxed.

### §4.1 Equivalence argument

Current path: `gi.Informer().GetIndexer().List()` → iterate → for each item,
type-assert and inspect `Subjects` + `RoleRef`. Ship B path:
`rbacSnap.Load().clusterRoleBindings` → iterate → for each pointer, inspect
`Subjects` + `RoleRef`.

Both iterate the same set of typed pointers as long as the snapshot's set equals
the indexer's set at the moment of the read. The only difference is *timing*:
today materializes at request time; Ship B materializes at the most recent
ADD/UPDATE/DELETE-driven rebuild. §4.2 covers the timing gap.

For `roleRefPermits` lookups, the snapshot's maps contain a binding-targetable
name for every ClusterRole/Role the indexer holds. Writer iterates the same
indexer `GetTypedObject` reads from, so the set of indexed names is identical.
Map collision impossible — cluster-scoped names are unique by Kubernetes
invariant; namespaced Roles are unique per `ns/name`, mirrored by the
`ns + "/" + name` key.

### §4.2 Eventual consistency — the timing gap

Between event arrival at the informer and snapshot publish there is a **bounded
delay**: `scheduleRBACRebuild`'s `atomic.Bool` tryLock + dirty-flag re-rebuild
makes the snapshot fresh as of the last rebuild, lagging the indexer by at most
one rebuild duration.

**Window bound:** rebuild walks 4 indexers in O(N₁+N₂+N₃+N₄) ≈ O(190K pointer
copies + 95K map inserts). At Go map insert speeds (~50 ns) that is **~5–10 ms
per rebuild** on commodity hardware (INFERRED — recorded as a comment on
`rebuildRBACSnapshot`, NOT as a hard timeout). Worst case: an RBAC change made
now takes effect in `evaluateAgainstInformer` after the next rebuild — a few
tens of ms. Same order of magnitude as the informer's own event-delivery lag.

**Is the lag a correctness regression?** Apiserver-equivalent contract
(`evaluate.go:108-116`) is "semantics match Kubernetes apiserver" — the *semantic
rules*. It is NOT "instantaneous reflection of an apiserver state change."
Snowplow's cache=on premise is eventual consistency; Ship B's added lag is
bounded and small. No fidelity contract is relaxed. §7 checks the missed-binding
edge. **AC-B.12 empirically gates end-to-end mutation propagation under 1 s**
(100× headroom over the ~10 ms estimate).

---

## §5. Cache-toggle compliance

**TRACED current behavior:** `EvaluateRBAC` at `evaluate.go:124` early-returns to
`UserCan` (SubjectAccessReview) when `cache.Disabled() == true`.
`evaluateAgainstInformer` is **only called** when `cache.Disabled() == false`.

**Ship B compliance:**

- Snapshot constructed **only in cache=on mode** — `NewResourceWatcher`'s
  informer-startup path (cache=on; cache=off uses `modePassthrough` and never
  registers the 4 RBAC informers with `stripAndType*`).
- `evaluate.go:124` `cache.Disabled()` early-return is UNCHANGED — cache=off
  never touches the snapshot.
- A `rbacSnap.Load() == nil` check in `EvaluateRBAC` (analogous to the
  `cache.Global() == nil` check at `evaluate.go:138`) returns `(false, err)` with
  the same "degrade-to-deny" rationale — preserves the "zero
  SubjectAccessReview in cache=on" Revision 1 contract. **AC-B.8 hard gates this
  pre-readiness behavior.**
- `CACHE_ENABLED=false` build + serve: `go build ./...` remains clean; snapshot
  wiring lives entirely in `internal/cache/` and is reached only through the
  cache=on watcher startup; flipping `CACHE_ENABLED=false` bypasses watcher
  construction entirely (TRACED `watcher.go:40`). No change to the cache-toggle
  contract.

`project_caching_is_provisional` and `project_redis_removal` both demand the
cache is cleanly removable; Ship B does not impede that — removing the watcher
removes the snapshot.

---

## §6. Expected alloc reduction & measurement

**Expected (verdict §3):** ~1.1 GB / 60-call on the targeted chain.

| Symbol                                                | Pre-Ship-B / 60 calls | Post-Ship-B expectation              | Lever                                  |
|-------------------------------------------------------|-----------------------|--------------------------------------|----------------------------------------|
| `cache.(*ResourceWatcher).ListTypedObjects`           | 614 MB flat           | **≤ 50 MB or absent from top-25**    | Snapshot read replaces it              |
| `client-go/tools/cache.(*threadSafeMap).List`         | 574 MB flat           | **≤ 50 MB or absent from top-25**    | `List()` not called per request        |
| `client-go/tools/cache.(*threadSafeMap).ByIndex`      | 43 MB flat            | **≤ 5 MB**                           | RoleBindings NS lookup via snapshot map|

**Mechanism gate (mirrors Ship A's §5 protocol):**

1. Deploy Ship B image to test pod (NOT redeploy prod). Read-only
   `kubectl port-forward` to `:8081`.
2. Baseline: `curl .../debug/pprof/heap -o heap_pre.pprof`.
3. Drive **60 cyberjoker navmenu resolves** — identical workload to Ship A
   measurement (`/call?resource=navmenus&apiVersion=widgets.templates.krateo.io%2Fv1beta1&name=sidebar-nav-menu&namespace=krateo-system`
   ×60). The navmenu drives `compositions`/`compositions-list` LISTs through
   `filterListByRBAC` which fans many `evaluateAgainstInformer` calls — same
   fanout as today.
4. Capture `heap_post.pprof`.
5. `go tool pprof -alloc_space -base heap_pre.pprof -top -nodecount=25 heap_post.pprof`.
6. **Pass criteria:**
   - `ListTypedObjects` AND `threadSafeMap.List` BOTH drop out of the top-25 (or
     below ~50 MB cum) on the `evaluateAgainstInformer` path.
   - **Net delta on the targeted chain ≥ 1.0 GB** (conservative vs the 1.1 GB
     estimate; noise budget per Ship A's −2.43 GB precedent).
   - `evaluateAgainstInformer` cum drops by ≥ 1.0 GB.
7. **Content check** (`feedback_validate_content_not_just_status`): **content
   corpus must cover all 4 RBAC paths** (PM tightening):
   - **(i)** cyberjoker navmenu where permit traces through a
     ClusterRoleBinding → kind=ClusterRole;
   - **(ii)** a UAF RESTAction permit traces through a RoleBinding →
     kind=Role (per-namespace path);
   - **(iii)** a permit traces through a ClusterRoleBinding → kind=ClusterRole
     (cluster-wide path);
   - **(iv)** a permit traces through a RoleBinding → kind=ClusterRole
     (mixed-ref path).
   Each served body must byte-equal the Ship A 0.30.137 capture for the
   same identity. Then perform a live RBAC mutation (`kubectl create
   rolebinding ...`) and re-issue a relevant `/call` — verify the new binding
   takes effect **within 1 s** (snapshot rebuild + reissue). **AC-B.12.**

**Unit/integration tests:**

- `TestRBACSnapshot_Equivalence` — seed N=100 typed CRBs/RBs/CRs/Rs, build a
  snapshot, run `evaluateAgainstInformer` against the snapshot vs against
  `ListTypedObjects` for 50 random `EvaluateOptions`; assert byte-identical
  permit/deny outcomes.
- `TestRBACSnapshot_EventDriven_Updates` — wire a fake informer, fire
  ADD/UPDATE/DELETE callbacks, assert the snapshot's content tracks within
  bounded delay.
- `TestRBACSnapshot_Race_ReaderWriter` (§3.3 / AC-B.5) — the `-race` gate.
- `TestRBACSnapshot_DegradeToDeny` (AC-B.8) — pre-snapshot-build,
  `EvaluateRBAC` returns `(false, err)`, never falls through to `UserCan`.
- `TestRBACSnapshot_MissCanary` (AC-B.10) — seed a snapshot deliberately missing
  a CR/R that the indexer holds; assert `recordSnapshotMissCanary` fires and
  the metric counter increments.

---

## §7. Falsifier

**"Ship B is wrong if the snapshot can ever miss a binding that is visible to the
live informer indexer at the moment `evaluateAgainstInformer` runs, such that a
user IS permitted by the indexer state but the snapshot DENIES — a security
incorrectness, not a performance regression."**

### Check — Ship B HOLDS, with explicit bounds

Five failure modes considered:

1. **Race: indexer ADD vs snapshot read.** Informer adds at T₁, fires OnAdd, rebuild
   at T₂, `Store` at T₃. An eval call between T₁ and T₃ sees the OLD snapshot,
   missing the new binding. **Verdict: denial in favor of staleness — strictly
   safer than a false permit.** Same eventual-consistency posture snowplow
   cache=on operates under today. Not a regression.

2. **Race: indexer DELETE vs snapshot read.** Snapshot briefly retains a deleted
   binding. Could permit a user whose binding was just deleted. **Verdict: same
   window the pre-Ship-B path already has** — the informer indexer itself has a
   between-watch-event-and-handler window. Rebuild lag (~10 ms) is the same
   order of magnitude as watch-delivery lag. For sub-second immediate-revoke the
   apiserver is the only source of truth, and snowplow cache=on already accepts
   that.

3. **Concurrent reads vs publish.** §3 invariants prove race-free: `atomic.Pointer`
   memory model + immutable post-publish.

4. **Snapshot never rebuilt.** `EvaluateRBAC`'s `rbacSnap.Load() == nil`
   degrade-to-deny (AC-B.8) + `AssertRBACSnapshotWired` startup panic make a
   missing snapshot loud at boot, not silent at request time. Writer is
   panic-recovering (recover, log, mark dirty, retry on next event).

5. **Initial sync race.** Request arrives before `WaitForCacheSync` completes for
   one of the 4 RBAC GVRs → snapshot built from a partial indexer. **Verdict:**
   initial snapshot rebuild runs AFTER all 4 RBAC `Servable` channels close
   (TRACED `watcher.go:498`). Until then, `EvaluateRBAC` returns degrade-to-deny
   — same fail-closed posture as today's `cache.Global() == nil` check. No
   widened window.

**Second falsifier:** "Ship B is wrong if the snapshot mutation hazard from §3 is
real — i.e. some reader path mutates a snapshot field, racing concurrent
readers." Checked by `TestRBACSnapshot_Race_ReaderWriter` (§3.3 / AC-B.5) under
`-race`. If that test ever fails, Ship B reverts.

---

## PM gate outcome

**Verdict: GO for development** (PM gate, 2026-05-20).

### Acceptance criteria — the dev implements to these

- **AC-B.1** — `rbacSnapshot` type lives in a new file
  `internal/cache/rbac_snapshot.go`, fields exactly as §1.1, package-private.
  `rbacSnap atomic.Pointer[rbacSnapshot]` is the sole publish container.

- **AC-B.2** — `rebuildRBACSnapshot(rw)` walks the 4 RBAC indexers from
  `rw.informers[g].Informer().GetIndexer().List()` once each per rebuild;
  type-asserts to typed `*rbacv1.{...}` (typed-fast path); never mutates a
  previously-published snapshot; uses `rbacSnap.Store(new)` to publish.

- **AC-B.3 (correctness-load-bearing)** — `EvaluateRBAC` does a SINGLE
  `rbacSnap.Load()` at the top and threads the resulting `*rbacSnapshot` as an
  explicit parameter into `evaluateAgainstInformer` AND `roleRefPermits`. **NO
  per-site `Load()`.** All 4 reader sites (CRB list at `:198`, RB list at `:223`,
  CR lookup at `:254`, R lookup at `:270`) read from the same passed-in snapshot
  pointer. A `Load()` per site is a correctness regression (cross-snapshot read
  race within one eval) and the architect-review gate rejects on this alone.

- **AC-B.4** — `scheduleRBACRebuild` is attached to all 4 RBAC informers via
  `AddEventHandler(ResourceEventHandlerFuncs{AddFunc, UpdateFunc, DeleteFunc})`
  at the same site `depEventHandlers` registers (`watcher.go:744`, `:1070`).
  Single-writer `atomic.Bool` tryLock + dirty-flag re-rebuild. NO unbounded
  goroutine spawning. Writer is panic-recovering (defer recover → log → mark
  dirty → return).

- **AC-B.5 (-race obligation,
  `feedback_shared_vs_copy_is_a_concurrency_change`)** —
  `TestRBACSnapshot_Race_ReaderWriter` exists with **32 reader goroutines × 50
  iters + 1 writer churning ADD/UPDATE/DELETE events at 500 ops**. `go test
  -race ./internal/cache/...` AND `./internal/rbac/...` pass clean. Additional
  assertion: max in-flight rebuild goroutines ≤ 1 throughout the test (the
  tryLock invariant). Goroutine count after `wg.Wait()` matches goroutine count
  at test start (no accumulation).

- **AC-B.6** — `evaluateAgainstInformer` post-Ship-B issues ZERO calls to
  `rw.ListTypedObjects` and ZERO calls to `rw.GetTypedObject`. (Both functions
  remain in the public API for non-RBAC callers; the architect-review gate
  greps the rbac package for these symbols and rejects on any remaining
  call.)

- **AC-B.7 (cache toggle)** — `CACHE_ENABLED=false`: `go build ./...` clean;
  startup wiring does not register the 4 RBAC informers; `EvaluateRBAC` continues
  to use the `cache.Disabled() == true` early-return to `UserCan`. A Ship B
  test asserts this path is byte-identical pre/post.

- **AC-B.8 (degrade-to-deny pre-readiness — HARD gate)** — between cache=on
  activation and the moment all 4 RBAC `Servable` channels close AND the initial
  `rebuildRBACSnapshot` publishes, `rbacSnap.Load() == nil`. `EvaluateRBAC` in
  that window returns `(false, fmt.Errorf("rbac: snapshot not yet built"))` —
  fail-closed, NEVER falls through to `UserCan` (would violate Revision 1).
  `TestRBACSnapshot_DegradeToDeny` exercises this state. `AssertRBACSnapshotWired`
  panics at boot if the 4 RBAC informers are registered but the snapshot never
  publishes — analogous to `AssertRBACTypedOverridesRegistered` (`strip.go:173`).

- **AC-B.9 (initial-snapshot ordering)** — initial `rebuildRBACSnapshot()` is
  called **synchronously after** all 4 RBAC `Servable` channels close (TRACED
  `watcher.go:498`). No goroutine race window where the first request could
  arrive at `EvaluateRBAC` with a built-but-stale snapshot — AC-B.8 fail-closed
  covers any earlier arrival.

- **AC-B.10 (snapshot-miss canary — PM tightening, mirrors 0.30.6's
  `fallback=true` invariant at `evaluate.go:185-186`)** — every
  `roleRefPermits` map-lookup miss (`!ok` on `clusterRolesByName` or
  `rolesByNSName`) calls `recordSnapshotMissCanary(kind, ns, name)` which:
  - logs at WARN level with `subsystem="rbac"`,
    `event="snapshot.miss"`, kind, ns, name, and the current `rbacSnap.Load()`
    pointer's identity hash;
  - increments a package-level `atomic.Uint64` counter exposed via a getter
    `RBACSnapshotMissCount() uint64`.
  Invariant — same shape as the 0.30.6 typed-RBAC fallback rate:
  **miss-count / total-eval-count MUST stay below 1%** over any 1-minute window
  in production. The tester polls this ratio for a 5-minute window during the
  Ship B mechanism gate; a >1% ratio FAILS the ship. The acceptable >0% steady
  state is the eventual-consistency window from §4.2 (a binding deleted in the
  apiserver but still referenced by an in-flight CRB whose snapshot copy has
  not yet rebuilt — rare and self-healing on the next rebuild).

- **AC-B.11 (content corpus — all 4 RBAC paths)** — the tester content check
  (§6 pass criterion 7) MUST cover **all four** of:
  - (i) navmenu permit via ClusterRoleBinding → kind=ClusterRole;
  - (ii) UAF permit via RoleBinding → kind=Role;
  - (iii) UAF permit via ClusterRoleBinding → kind=ClusterRole (cluster-wide);
  - (iv) UAF permit via RoleBinding → kind=ClusterRole (mixed-ref).
  Each served body byte-equals the Ship A 0.30.137 capture for the same
  identity. A divergence on any of the four FAILS the ship.

- **AC-B.12 (live RBAC mutation propagation — HARD gate, empirical falsifier on
  the ~10 ms estimate, 100× headroom)** — during the mechanism-gate run the
  tester performs:
  ```
  kubectl create rolebinding <new-rb-granting-cyberjoker-on-some-NS>
  # immediately, with no sleep, hit a /call whose RBAC verdict depends on it
  curl ... /call?... (the affected resource)
  ```
  The new binding MUST take effect within **1 s** of `kubectl create` returning
  (snapshot rebuild + reissue end-to-end). A propagation time > 1 s FAILS the
  ship. Symmetric test for DELETE: `kubectl delete rolebinding <...>`, hit the
  affected `/call`, the revoke MUST take effect within 1 s. The 1 s target is
  100× headroom over the ~10 ms rebuild estimate (§4.2); a measured value > 1 s
  invalidates the estimate and the design's eventual-consistency framing — that
  is a falsifier hit, not a tuning knob.

- **AC-B.13 (memory regression bound)** — RSS after Ship B deploy is within
  **+50 MB** of the Ship A 0.30.137 baseline (the snapshot itself is ~8 MB
  resident; +50 MB allows for transient rebuild peaks + slightly different GC
  behavior). A larger regression fails the ship.

### Ratified design decisions

- **Decision 1 (CR + R maps in the same snapshot — RATIFIED).** The
  `roleRefPermits` map lookups (`clusterRolesByName`, `rolesByNSName`) ride in
  Ship B alongside the CRB/RB slices. They are part of the same
  `evaluateAgainstInformer` call chain; splitting them would force two
  `Load()`s per eval, directly violating AC-B.3. The snapshot is a single
  coherent abstraction.

- **Decision 2 (~10 ms eventual-consistency lag — RATIFIED).** The
  event-debounced rebuild's ~10 ms typical lag is accepted as the eventual-
  consistency baseline. **Condition:** the estimate is recorded as a comment
  on `rebuildRBACSnapshot` citing this design; AC-B.12's 1 s mutation-
  propagation gate is the hard empirical falsifier. If a measured propagation
  exceeds 1 s, the design's framing is wrong and the ship reverts — not a
  tuning knob.

### PM tightenings absorbed into the design

- **Snapshot-miss canary (AC-B.10)** — mirrors the 0.30.6 typed-RBAC
  `fallback=true` invariant at `evaluate.go:185-186`. <1% over any 1-minute
  window; the steady-state >0% is the eventual-consistency rebuild window from
  §4.2.

- **Content corpus covers all 4 RBAC paths (AC-B.11)** — CRB→ClusterRole,
  RB→Role, CRB→ClusterRole, RB→ClusterRole. Single-path coverage would miss a
  per-kind regression.

- **Pre-readiness fail-closed (AC-B.8)** — `rbacSnap.Load() == nil` returns
  `(false, err)`, NEVER falls through to `UserCan`. Preserves the
  "zero SubjectAccessReview in cache=on" Revision 1 contract.

### Out of Ship B scope (deferred)

- **Removing `ListTypedObjects` / `GetTypedObject`** — both stay; only the RBAC
  hot-path callers move off. Future non-RBAC typed-GVR consumers may use them
  unchanged.

- **Other RBAC informer indexer reads** (e.g. potential future typed GVRs added
  to `rbacTypedGVRs`) — only the existing 4 GVRs participate in Ship B. Adding
  a new typed GVR to `rbacTypedGVRs` is a follow-up that must also extend
  `rbacSnapshot`'s fields + the migrated reader sites; a `// TODO` comment on
  `rbacSnapshot` documents this co-evolution.

- **D1 input-isolation copy** — Ship C scope; Ship B does not touch the input
  side at all.
