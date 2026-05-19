# Ship C — Monomorphic JSON-tree copier (D1)

**Status:** TEAM-LEAD GATE PASSED (post-§2 revision, 2026-05-20). Awaiting PM
ratification — see "PM gate outcome" below.
**Campaign:** A+B+C resolver-path rebuild (verdict 0.30.136). Ship C is lever
3 of 3 — the bounded-headroom final ship.
**Authoritative reference** for the developer (implements to AC-C.1..AC-C.N
once PM ratifies) and the tester (validates against §7). The architect-review
gate checks the diff against this document.

---

## §1. TRACED current `maps.DeepCopyJSONValue` cost on Ship B (0.30.138)

**Measurement performed live during architect's design session.** Pod
`snowplow-6d87899496-9lkhv` (image `0.30.138@sha256:a36072a7…`, helm rev 224),
read-only port-forward, NOT redeployed.

Protocol: identical to Ship A's §5 (baseline → 60 cyberjoker navmenu /call →
second snapshot → `-base` diff). Artifacts:
`/tmp/snowplow-runs/0.30.138/heap_alloc_pre_b.pprof`,
`/tmp/snowplow-runs/0.30.138/heap_alloc_post_b.pprof`.

**Result:** 60-navmenu delta = **5.04 GB total** (vs 7.6 GB pre-campaign — Ship
A+B cumulative reduction visible). Top-flat:

| Symbol                                                              | Flat / 60 calls | % of delta |
|---------------------------------------------------------------------|-----------------|------------|
| **`plumbing/maps.deepCopyJSONValue`**                                | **3681.86 MB**  | **68.0 %** |
| `gojq.normalizeNumbers`                                              | 565.75 MB       | 10.4 %     |
| `gojq.(*env).Next`                                                   | 349.20 MB       | 6.5 %      |
| `gojq.allocator.makeObject`                                          | 144.56 MB       | 2.7 %      |
| `gojq.updateObject`                                                  | 57.02 MB        | 1.1 %      |
| (everything else combined)                                           | ~620 MB         | ~11 %      |

**100 % of `DeepCopyJSON`'s 3.68 GB flows through ONE caller —
`listEnvelopeValue`** (`apistage.go:213-228`), itself called only from
`gateListItems` → `apistageContentServe`. Verified live:
`pprof peek 'DeepCopyJSON$'` shows `listEnvelopeValue (inline)` as 100 % of
inbound flow.

**Verdict-estimate correction:** the verdict §3 said D1 was ~1.5 GB / 60 calls
INFERRED from a different workload shape. The Ship B navmenu workload drives
the content-cache hit path much harder, so the per-hit isolation copy
dominates the residual: **D1 is ~2.5× the verdict's estimate.** Ship C's
headroom is correspondingly larger than the verdict implied — but see §2.2:
upstream is already a type-switch, so the realistic Ship C **reduction** is
still bounded.

**Per-call cost:** 3.68 GB / 60 = **~61 MB / `/call`** in deep-copy alone.

---

## §2. The snowplow-local copier API

**New file: `internal/resolvers/restactions/api/jsoncopy.go`** — same package
as `apistage.go` and `jqvalue.go` (Ship A precedent), so no new package import
graph and the single migrated caller stays in-package.

**Conceptual signature** (shape parallels `jqvalue.go:61`):

```go
// CopyJSONValue returns a deep copy of v that shares NO mutable state
// with v. Monomorphic — a direct type-switch over the documented JSON
// value-tree types, NO reflect, NO recursive plumbing.DeepCopyJSON.
//
// Accepted types (must equal the union plumbing/maps.deepCopyJSONValue
// accepts at deepcopy.go:11-52):
//
//   nil, bool, int, int32, int64, float32, float64, string, json.Number,
//   []any, map[string]any, []map[string]any
//
// Integer / floating-point normalization is preserved verbatim from
// upstream (`int → int64`, `int32 → int64`, `float32 → float64`) so a
// future input-source change (a custom decoder, Decoder.UseNumber(), an
// `[]int` cast routed through here) cannot silently diverge from
// upstream byte-shape. See §5.1 row-by-row equivalence proof.
//
// PANIC-ON-UNKNOWN behaviour is preserved (§5.6 — falsifier-tied).
// Same fmt.Errorf("cannot deep copy %T", x) panic the upstream raises
// at deepcopy.go:51, so any input-shape divergence remains LOUD (the
// apistage.go:209-210 contract is preserved verbatim).
func CopyJSONValue(v any) any

// CopyJSONMap is the typed convenience analogue of
// plumbing/maps.DeepCopyJSON (deepcopy.go:58-60): asserts the result to
// map[string]any so the caller (listEnvelopeValue) does not need a
// runtime assertion at the call site.
func CopyJSONMap(m map[string]any) map[string]any
```

### §2.1 Type-switch shape (REVISED — upstream byte-equivalence preserved)

```go
switch x := v.(type) {
case nil:
    return nil
case bool, int64, float64, string, json.Number:
    // True passthrough types — upstream returns them as-is at
    // deepcopy.go:42-43. Scalars are immutable; alias is safe.
    return x
case int:
    // Upstream deepcopy.go:44-45 — `case int: return int64(x)`.
    // Preserved verbatim so a future input source carrying `int`
    // (custom decoder, `[]int` cast, etc.) byte-equivalences to upstream.
    return int64(x)
case int32:
    // Upstream deepcopy.go:46-47 — `case int32: return int64(x)`.
    return int64(x)
case float32:
    // Upstream deepcopy.go:48-49 — `case float32: return float64(x)`.
    return float64(x)
case map[string]any:
    if x == nil {
        return x // typed-nil parity, upstream deepcopy.go:14-17
    }
    clone := make(map[string]any, len(x))   // pre-sized — one alloc
    for k, sub := range x {
        clone[k] = CopyJSONValue(sub)
    }
    return clone
case []any:
    if x == nil {
        return x // typed-nil parity, upstream deepcopy.go:24-27
    }
    clone := make([]any, len(x))             // pre-sized — one alloc
    for i, sub := range x {
        clone[i] = CopyJSONValue(sub)
    }
    return clone
case []map[string]any:
    if x == nil {
        return x // typed-nil parity, upstream deepcopy.go:34-36
    }
    // Upstream promotes []map[string]any to []any at deepcopy.go:37-41
    // — the result's element type is `any`, not `map[string]any`. We do
    // the same; downstream consumers expect that element-type promotion.
    clone := make([]any, len(x))
    for i, sub := range x {
        clone[i] = CopyJSONValue(sub)
    }
    return clone
default:
    panic(fmt.Errorf("cannot deep copy %T", v))
}
```

**Case ordering rationale** (not load-bearing for correctness): `nil` and the
immutable-scalar passthrough block hit first because they're the
leaf-dominant cases (~80 % of nodes in a typical JSON tree by visit count).
The three integer/float conversion branches sit between scalars and
containers — unreachable on today's `encoding/json.Unmarshal`-sourced inputs
but kept loud and explicit per the verification-gate finding. Container
cases last. Ordering does not change correctness (a `case` matches on type,
not position) but does help the leaf-passthrough win in §2.2.

### §2.2 Why this allocates less than the upstream (REVISED — bullet 2 deleted)

Upstream `plumbing/maps.deepCopyJSONValue` (`deepcopy.go:11-53`, TRACED — full
source captured in `/tmp/snowplow-runs/0.30.138/deepcopy.go`) **is already a
type-switch**, not a reflect-based copier. The verdict §3's
"reflective vs monomorphic" framing was wrong on this axis: upstream is
monomorphic too. The Ship C wins are correspondingly more modest:

1. **Leaf-scalar fast-path case ordering.** For the dominant case of leaf
   scalars (`bool, int64, float64, string, json.Number`), upstream packs them
   into a single multi-type `case` (`deepcopy.go:42`) which Go compiles as a
   runtime type-switch table jump. Ship C achieves the same compiled shape
   but with **scalar leaves ordered first** in the switch source, so the
   human reader sees the hot path at the top and the optimizing compiler
   (under PGO if ever enabled) has a clearer locality hint. INFERRED win:
   small per-leaf (~5–10 ns saved per leaf on the leaf-fastpath block),
   ~190K leaves per content-cache hit → measurable in the aggregate, but
   **bounded — this is a structural ordering win, not an algorithmic win.**

2. ~~(deleted — was "int normalization removed"; reverted by §2.1.)~~

3. **The 68 %-of-delta win is structural.** `make(map, len)` and
   `make([]any, len)` pre-sized into a fresh tree is a fundamentally O(N)
   operation on ~190K nodes. Ship C does not change the asymptotic shape;
   **the realistic reduction comes from removing one layer of function-call
   indirection** (`maps.DeepCopyJSON` calls `deepCopyJSONValue` which
   type-switches; Ship C's call goes directly to the type-switch — saves
   one stack frame per recursive call, and Go's mid-stack inliner may
   collapse the recursion more aggressively for an in-package function than
   for a cross-module one). INFERRED: this is the dominant Ship C win, and
   it is the reason the headline estimate is 20–35 %, not 80 %+.

**A note on `fmt.Errorf` in the panic path.** Upstream's
`panic(fmt.Errorf(...))` at `deepcopy.go:51` is on the unreachable error
path; Go's panic does not lazily defer the `fmt.Errorf` call (the error
value is constructed before `panic` is called). Ship C keeps the same panic
message (§5.6) — **not a win, equivalence preference.**

**Honest INFERRED estimate (unchanged from §1):** the realistic Ship C
reduction is **~20–35 % of the 3.68 GB**, i.e. **~0.7–1.3 GB / 60 calls.**
The gate threshold in §7 stays at the conservative low end (≥ 0.6 GB). The
PM should accept that Ship C's headroom is *real but bounded* — that
framing is unaffected by the int-conversion fix.

---

## §3. Migration site — ONE call site

`apistage.go:213-228` `listEnvelopeValue`, line `:227`:

```go
// Per-call isolation: hand the caller a private deep copy so the
// downstream in-place gojq mutation never touches the cached maps.
return maps.DeepCopyJSON(envelope)   // ← Ship C migrates this one line
```

**Becomes:**

```go
return CopyJSONMap(envelope)
```

Plus the `import "github.com/krateoplatformops/plumbing/maps"` at
`apistage.go:40` is removed **only if no other in-file use remains** — verify
with a `grep -n 'maps\.'` over `apistage.go`; if any other call site uses
`maps.X`, the import stays. (TRACED at design time: `apistage.go` imports
`maps` only for `DeepCopyJSON`; the import goes.)

**All other `maps.DeepCopyJSON` / `maps.deepCopyJSONValue` callers in the
snowplow tree stay on upstream `plumbing/maps`** — one-lever-per-ship
discipline (same as Ship A.2 deferring sites #5/#6/#8/#9). Grep gate:
`grep -rn 'maps\.DeepCopyJSON' --include='*.go' .` enumerates them; the dev
confirms no migration beyond `apistage.go:227`.

---

## §4. The three cross-ship must-preserve invariants (task #200)

Every invariant Ship A and Ship B depend on the input-isolation copy for
**continues to hold under Ship C**:

### Invariant (a) — gojq mutates input safely

TRACED `gojq@v0.12.17/normalize.go:71-80`: `RunWithContext` calls
`normalizeNumbers(input)` which mutates input maps/slices in place. The copy
at `apistage.go:227` exists precisely so the **cached envelope's maps are not
the trees gojq mutates** — each `/call` gets a private tree.

**Ship C must produce a tree where every `map[string]any` and `[]any` is
freshly `make()`-allocated and contains no aliased pointer to the cached
envelope's interior.** §2.1's type-switch satisfies this:
`make(map[string]any, len(x))` then per-key recursion;
`make([]any, len(x))` then per-index recursion. **Identical structural
guarantee to upstream `deepCopyJSONValue`.** ✓

### Invariant (b) — gojq result may alias output safely

From Ship A AC-A.6 / `jqvalue.go:55-60`: gojq's result CAN alias sub-trees of
its **input**. Ship A's analysis (and `TestEvalValue_AliasingConcurrency_Race`)
is that aliasing is safe IFF the input is per-call-private. The input *is*
the tree Ship C produces. So the same Ship A safety argument carries
through: **whatever Ship A's result aliases, it aliases a Ship-C-private
sub-tree.** No new aliasing hazard. ✓

### Invariant (c) — Ship B `convertUnstructured*` write-time fallback

Ship B's `convertUnstructuredCRB/RB/CR/R` runs at snapshot-rebuild time on
the RBAC informer indexer. **None of those values ever flow through
`listEnvelopeValue`** — the RBAC snapshot is read by
`evaluateAgainstInformer`, not cached as a content envelope. So invariant
(c) is **orthogonal** to Ship C's migration site.

The PM-cited "the copier handles every Go type that `FromUnstructured`
produces" is a SOUNDNESS check on §2.1's type set against what
`runtime.DefaultUnstructuredConverter.FromUnstructured` can introduce into a
downstream value tree. `FromUnstructured` produces typed objects
(`*rbacv1.ClusterRoleBinding` etc.) — those values never sit inside a
`map[string]any` reaching `listEnvelopeValue`. The values inside
`listEnvelopeValue`'s envelope are pure `encoding/json.Unmarshal` outputs
from `parseListEnvelope` (`apistage.go:140-162`), which produces **only**
`nil, bool, float64, string, []any, map[string]any` (the documented
`encoding/json` decode types into `any`). Ship C's type-switch covers all of
them. `int, int32, int64, json.Number, []map[string]any, float32` are also
in §2.1 for parity with the upstream contract (and to remain a drop-in if
the input source ever changes — defensive). ✓

**Cross-ship audit:** invariants (a)+(b) are the load-bearing pair Ship C
must preserve. Invariant (c) is documented in #200 but does not flow through
Ship C's migration site — explicitly noted to prevent a future reviewer (or
PM) from concluding Ship C *should* touch it.

---

## §5. Output-equivalence analysis (correct as written, post-§2 revision)

`plumbing/maps.deepCopyJSONValue` (`deepcopy.go:11-53`) is short enough to
trace exhaustively. The new copier must produce an **observably identical**
tree for every value `listEnvelopeValue` ever feeds.

### §5.1 — Type-by-type equivalence

| Upstream behavior (`deepcopy.go`) | Ship C `CopyJSONValue` | Identical? |
|---|---|---|
| `map[string]any` nil → returns the typed nil (`:14-17`)                       | Same — `if x == nil { return x }`                | ✓ |
| `map[string]any` non-nil → `make(map, len)` + recurse (`:18-22`)              | Same shape                                       | ✓ |
| `[]any` nil → returns the typed nil (`:24-27`)                                | Same                                             | ✓ |
| `[]any` non-nil → `make([]any, len)` + recurse (`:28-32`)                     | Same                                             | ✓ |
| `[]map[string]any` nil → returns the typed nil (`:34-36`)                     | Same                                             | ✓ |
| `[]map[string]any` non-nil → `make([]any, len)` + recurse, **promoting element type from `map[string]any` to `any`** (`:37-41`) | Same — produces `[]any` whose elements are deep-copied maps | ✓ (promotion preserved) |
| `string, int64, bool, float64, nil, json.Number` → return as-is (`:42-43`)    | Same — all true-scalars in one `case` returning `x` (§2.1)     | ✓ |
| `int → returns int64(x)` (`:44-45`)                                           | Same — explicit `case int: return int64(x)` (§2.1, REVISED)   | ✓ |
| `int32 → returns int64(x)` (`:46-47`)                                         | Same — explicit `case int32: return int64(x)` (§2.1, REVISED) | ✓ |
| `float32 → returns float64(x)` (`:48-49`)                                     | Same — explicit `case float32: return float64(x)` (§2.1, REVISED) | ✓ |
| Anything else → `panic(fmt.Errorf("cannot deep copy %T", x))` (`:50-52`)      | Same — verbatim error message                    | ✓ (§5.6) |

### §5.2 — Number representation

Ship A §3.1 established that `encoding/json.Unmarshal` of the apiserver
envelope at `apistage.go:140-162` produces `float64` for every JSON number,
never `int`/`int64`/`json.Number`. So **for the live input** the
`case int/int32/float32` branches are **unreachable** today. They are kept
in the type-switch for upstream parity and defensive correctness against a
future input-source change (e.g. a custom JSON decoder using
`Decoder.UseNumber()`).

If they ever fire, the Ship C output matches upstream — `int → int64`,
`int32 → int64`, `float32 → float64` — so a downstream `json.Marshal`
produces the same bytes. The verification-gate fix to §2.1 makes this
equivalence true *in the in-tree representation*, not merely at the JSON
boundary, so a future caller that probes types via `reflect.TypeOf` (test
seed, debug logger, etc.) sees the same types either side.

### §5.3 — Object key ordering

Upstream `deepcopy.go:19` iterates `for k, v := range x` — Go map iteration
is unordered. Ship C does the same. Both produce maps whose iteration order
is non-deterministic. The downstream serialization (`encoding/json.Marshal`)
**sorts map keys**, so the final widget-prop output is byte-identical
regardless of internal map iteration order. ✓ (Same argument as Ship A
§3.3.)

### §5.4 — Pointer identity / aliasing

Upstream: scalar `case` returns `x` (the same Go value — for `string` this
is an immutable header, for `int64` a value type; aliasing is safe).
Map/slice cases produce **fresh** containers. Ship C: identical contract —
scalar branches return `x` unchanged (zero-copy for immutables), map/slice
branches `make` fresh containers. **No new aliasing**, no loss of isolation.
✓

### §5.5 — `nil` handling

Upstream distinguishes three nils:

- **untyped nil** (`x == nil` at the outer scope) → falls into the
  `case ... nil` at `:42` → returns the untyped nil.
- **typed nil `map[string]any(nil)`** → matches `case map[string]any:` then
  `if x == nil { return x }` → returns the typed nil.
- **typed nil `[]any(nil)`** → matches `case []any:` then
  `if x == nil { return x }` → returns the typed nil.

Ship C reproduces all three behaviors with the same case ordering and the
same typed-nil guard. ✓

### §5.6 — Panic-on-unknown — falsifier-tied

`apistage.go:209-212` explicitly cites the panic behavior as a contract:
*"DeepCopyJSON recursively copies every json.Unmarshal value type
(map/slice/scalars/json.Number); the items come straight from
parseListEnvelope's json.Unmarshal so no non-JSON type can reach its
panic-on-unknown default."*

Ship C preserves the **same panic message format**
(`panic(fmt.Errorf("cannot deep copy %T", v))`). This is intentional:

- If a future input-source change introduces a non-handled type, the panic
  message is identical to what ops + on-call already know to look for. No
  new error-shape vocabulary.
- A silent quiet-skip or a loud-WARN-and-pass-through would **violate the
  apistage.go:209-210 contract** and is rejected for falsifier robustness
  (§8).

The panic IS the design's safety net — a misshaped input loudly aborts the
request rather than serving a corrupted tree.

### §5.7 — Equivalence verdict

For **every input the `listEnvelopeValue` migration site can reach today**
(TRACED to `parseListEnvelope` `encoding/json.Unmarshal` output), and
**every input the upstream-parity branches cover** (`int, int32, float32,
[]map[string]any, json.Number`), Ship C produces a tree that is
**observably identical** to upstream `plumbing/maps.DeepCopyJSON` — same
shape, same scalar types, same typed-nil behavior, same panic message on
unknown. Equivalence proven branch-by-branch. The post-§2.1-revision
`int → int64`, `int32 → int64`, `float32 → float64` conversions are now
preserved verbatim, so equivalence holds **at the Go-type level** and not
merely after a downstream `json.Marshal` round-trip — a strictly tighter
property than the bounced design.

---

## §6. `-race` concurrent test + UpstreamEquivalence test

This is the third ship in a row invoking
`feedback_shared_vs_copy_is_a_concurrency_change`. Ship C is the
input-isolation side — every test must prove that 32 goroutines running
gojq over **independently `CopyJSONValue`-produced trees** never race.

### `TestCopyJSONValue_AliasingConcurrency_Race`

Placed in `jsoncopy_test.go` (same package, mirrors `jqvalue_test.go:578`
precedent):

- **Shared template** — the `pig`-shaped envelope from Ship A's race test
  (TRACED `jqvalue_test.go:582-594`): nested `metadata`/`items`/`slice`
  shape with `float64` numbers, strings, bools, nested maps, and a small
  array.
- **32 goroutines × 50 iterations.** Each iteration:
  1. `priv := CopyJSONValue(template).(map[string]any)` — exercises the new
     copier on a value-shape representative of `gateListItems`'s input.
  2. Asserts the copier produced a **disjoint** tree: `&priv` differs from
     `&template`; `priv["metadata"]` is a different `map[string]any` from
     `template["metadata"]` (probe via `reflect.ValueOf(...).Pointer()`
     comparison is fine here — the race test runs cold; the production
     hot-path never reflects).
  3. `_, _, _ = EvalValue(ctx, ".items[].name", priv, nil)` — runs gojq
     over the private tree (the precise composition of Ship A's wrapper +
     Ship C's copy).
  4. Asserts no panic, no error.
- **Writer goroutine** — concurrently mutates the **template**
  (`template["new-key"] = "mutation"`). If `CopyJSONValue` ever aliases the
  template back into a goroutine's `priv`, the data-race detector catches
  it.
- **Final assertion** — `go test -race ./internal/resolvers/restactions/api/...`
  passes clean.

### `TestCopyJSONValue_NoSharedSubtrees`

Synchronous mutation-probe sanity:

- Construct a template with a known sub-map
  (`m := template["metadata"].(map[string]any)`).
- `priv := CopyJSONValue(template).(map[string]any)`.
- Mutate `m["name"] = "MUTATED"`.
- Assert `priv["metadata"].(map[string]any)["name"] != "MUTATED"`.
- Repeat for sub-slices: mutate `template["items"].([]any)[0]` and assert
  the priv's slice element is unaffected.

### `TestCopyJSONValue_PanicOnUnknown`

Preserves the §5.6 contract:

- Pass a struct value (e.g. `struct{}{}`) and assert the function **panics**
  with a message containing `"cannot deep copy"`. (Use `defer recover` +
  assertion on the panic value.)

### `TestCopyJSONValue_UpstreamEquivalence`

Golden-tree comparison — **the gate that catches the §2.1 bounce-shaped
regression byte-for-byte**:

- For a representative input set covering the §5.1 type catalogue + a deep
  nested envelope, assert
  `reflect.DeepEqual(CopyJSONValue(input),
                     plumbing/maps.DeepCopyJSON(input.(map[string]any)))`.
- The input set MUST include explicit `int`, `int32`, `float32` cases at
  leaf positions — these are the rows the verification-gate bounce
  surfaced. Under the bounced §2.1, this test would have failed on
  `reflect.DeepEqual` because Ship C would have left `int` as `int` while
  upstream promoted it to `int64`. Under the gate-passed §2.1, both produce
  `int64` and the test passes.
- Also include `[]map[string]any` at a sub-position to confirm the
  element-type promotion to `[]any` survives equivalence.
- The test is unit-level proof of §5.7's equivalence verdict; runs in a
  few ms.

---

## §7. Expected alloc reduction & measurement protocol

### §7.1 Architect estimate (honest, post-§1 measurement)

The verdict's "~1.5 GB" estimate is **superseded** by the live Ship B
measurement. New estimate:

- **Current D1 on Ship B baseline:** 3.68 GB / 60 calls (TRACED).
- **Per-call deep-copy work:** ~61 MB / `/call` (mostly `make(map)` +
  `make([]any)` + per-leaf overhead).
- **Realistic Ship C reduction:** INFERRED **20–35 %** of D1 =
  **0.7–1.3 GB / 60 calls.** Upstream is already a type-switch (not
  reflect-based as the verdict assumed); the wins come from (i) `case`
  reordering for the leaf-scalar fast path, (ii) eliminating one layer of
  function-call indirection through in-package recursion. The headline
  1.5–3 GB I implied in the verdict was based on a wrong assumption about
  upstream's mechanism.
- **Gate threshold (architect proposes):** **≥ 0.6 GB net delta on the
  `deepCopyJSONValue` symbol** at the migrated call site. Conservative —
  the low end of the estimate, with noise budget. PM ratifies.
- **Cumulative campaign:** 0.30.136 pre-campaign baseline 7.8 GB → 0.30.137
  (Ship A) 5.17 GB → 0.30.138 (Ship B) **5.04 GB / 60-call** (live this
  session; aligns with the −2.43 GB Ship A topline + the residual variance
  the ledger flagged). Ship C projects 0.30.139 down to
  **~4.0 GB / 60-call** if the gate just holds, **~3.6 GB / 60-call** if
  upper-end. Total campaign reduction at Ship C ratify:
  **3.8–4.2 GB / 60-call from baseline** — ~50 % of pre-campaign cost.

### §7.2 Measurement protocol (same as Ships A & B)

1. Deploy Ship C image to test pod; do NOT redeploy 0.30.138 prod.
2. Read-only `kubectl port-forward` to `:8081`. Capture baseline:
   `curl .../debug/pprof/heap -o heap_pre_c.pprof`.
3. Drive **60 cyberjoker navmenu resolves** — identical workload to Ships A
   and B.
4. Capture `heap_post_c.pprof`.
5. `go tool pprof -alloc_space -base heap_pre_c.pprof -top -nodecount=25 heap_post_c.pprof`.
6. **Pass criteria:**
   - `plumbing/maps.deepCopyJSONValue` drops by **≥ 0.6 GB cum** (or absent
     entirely from the delta if all migrations route to Ship C).
   - The replacement `api.CopyJSONValue` symbol appears with **lower flat
     than the displaced 3.68 GB**.
   - `listEnvelopeValue` cum drops by **≥ 0.6 GB**.
   - **Net per-`/call` delta ≥ 10 MB saved** (60 calls × ≥ 10 MB = 0.6 GB
     gate).
7. **Content check** (`feedback_validate_content_not_just_status`): the
   same 16/16 byte-identical content corpus Ship B passed. Including the
   cyberjoker 403-RBAC-denial body (proves no semantic shift) and the
   4-RBAC-path UAF corpus (Ship B AC-B.11).
8. **AC-A.6 + AC-B.5 parity gate:**
   `go test -race ./internal/resolvers/restactions/api/...` clean.
   `TestCopyJSONValue_AliasingConcurrency_Race` plus the pre-existing
   Ship A and Ship B race tests must all stay green — proves the
   three-ship concurrency invariant chain holds end-to-end.

---

## §8. Falsifier

**"Ship C is wrong if the new copier produces a tree that diverges OBSERVABLY
from `plumbing/maps.DeepCopyJSON`'s output for any reachable input — either
by aliasing where upstream isolates, by mis-typing where upstream preserves
type, or by silently skipping a panic case where upstream loudly aborts."**

### Check — Ship C HOLDS, three sub-cases

#### §8.1 — Aliasing-where-upstream-isolates

The copier's `map[string]any` and `[]any` branches use `make(...)` with the
same pre-sized capacity as upstream `deepcopy.go:18,28,37`. Scalar branches
return the value as-is — upstream does the same (`deepcopy.go:42-49`). For
every type covered by §5.1, the structural copy depth is identical.
**`TestCopyJSONValue_NoSharedSubtrees`** (§6) probes this directly with a
mutation-and-assert pattern.

#### §8.2 — Mis-typing-where-upstream-preserves

Upstream's idiosyncrasies, all preserved by §2.1 (REVISED):

- `int → int64`: §5.1 preserved (explicit `case int: return int64(x)`).
- `int32 → int64`: §5.1 preserved (explicit `case int32: return int64(x)`).
- `float32 → float64`: §5.1 preserved (explicit
  `case float32: return float64(x)`).
- `[]map[string]any → []any` element-type promotion: §5.1 preserved (the
  **most subtle** divergence risk if missed;
  `TestCopyJSONValue_UpstreamEquivalence` catches it via
  `reflect.DeepEqual`).
- `json.Number` left as-is: §5.1 preserved.
- typed-nil maps/slices returned as typed-nil (not converted to fresh empty
  container): §5.5 preserved.

Each of these is a documented case in §2.1. The unit test
`TestCopyJSONValue_UpstreamEquivalence` is the gate — it would have failed
under the bounced §2.1 design on `int`/`int32`/`float32` inputs; it passes
under the gate-passed §2.1.

#### §8.3 — Silent-skip-where-upstream-panics

§5.6: same `panic(fmt.Errorf("cannot deep copy %T", v))` shape. Same
vocabulary for ops. `TestCopyJSONValue_PanicOnUnknown` is the gate.

### Second falsifier

**"Ship C is wrong if the live alloc-gate delta (§7.2) is < 0.6 GB on the
targeted symbol."** If the measured delta falls below the threshold, the
architect's INFERRED 20–35 % reduction estimate was wrong; ship reverts, the
design's framing of "upstream is already a type-switch so wins are
bounded" is confirmed AND further investigation is needed (perhaps the win
route is elsewhere — JIT-compiling per-type copiers via codegen, or a
sync.Pool of pre-sized maps). This is a *real* possibility given the new
estimate is tighter than Ship A's or Ship B's headroom was — the PM gate
should explicitly accept that Ship C is the **bounded-headroom ship of the
campaign** and that the team will accept the small win or revert without
re-spinning.

---

## Load-bearing claims for the architect-review gate

- D1 on Ship B baseline = **3.68 GB / 60 calls** (live alloc diff,
  pod `snowplow-6d87899496-9lkhv`, image `0.30.138@sha256:a36072a7…`).
  Artifacts: `/tmp/snowplow-runs/0.30.138/heap_alloc_pre_b.pprof`,
  `/tmp/snowplow-runs/0.30.138/heap_alloc_post_b.pprof`.
- 100 % of `DeepCopyJSON` flow in the delta traces through
  `listEnvelopeValue` (`apistage.go:213-228`) — single migration site
  covers all of D1.
- Upstream `plumbing/maps.deepCopyJSONValue` is **already a monomorphic
  type-switch** — not reflect-based (verdict §3 was wrong here). Source
  TRACED at `/tmp/snowplow-runs/0.30.138/deepcopy.go`. The honest estimate
  (0.7–1.3 GB reduction) reflects this.
- Three cross-ship invariants from #200: (a) gojq-mutates-input,
  (b) gojq-result-may-alias, (c) Ship B `convertUnstructured*` — Ship C
  migration site touches only (a)+(b); (c) is orthogonal.
- §2.1 REVISED: separate `case int / int32 / float32` branches reproduce
  upstream's `int → int64`, `int32 → int64`, `float32 → float64`
  conversions verbatim. §2.2 bullet 2 (the "int normalization removed" win
  claim) is deleted; no Ship C behaviour change vs upstream remains.
- Equivalence with upstream proven branch-by-branch in §5.1; gated by
  `TestCopyJSONValue_UpstreamEquivalence`.
- `-race` test mirrors Ship A AC-A.6 and Ship B AC-B.5 shape — 32
  goroutines × 50 iters + writer churn.
- Gate threshold: **≥ 0.6 GB net delta on `deepCopyJSONValue` symbol**
  (conservative, low-end of estimate, noise budget).
- Falsifier accepts: Ship C is the **bounded-headroom ship** of the
  campaign — small-win-or-revert posture acknowledged to the PM upfront.

---

## PM gate outcome

*(To be filled in after the PM ratifies. Below is the placeholder skeleton
the architect leaves for the PM, mirroring the Ship A and Ship B
ratification sections — populated values are PM-driven.)*

**Verdict:** ⟨PENDING — PM RATIFIES⟩.

### Acceptance criteria — the dev implements to these

*(PM to enumerate AC-C.1..AC-C.N. Architect proposes the following starting
set; the PM tightens / extends as needed.)*

- **AC-C.1** — `CopyJSONValue` is added in a new snowplow-local file
  `internal/resolvers/restactions/api/jsoncopy.go`, built directly over Go
  std (`encoding/json`, `fmt`) — NO `plumbing/maps` import. Type-switch
  shape verbatim per §2.1 (REVISED), including the explicit `case int /
  int32 / float32` branches that preserve upstream's verbatim
  `int → int64` / `int32 → int64` / `float32 → float64` conversions.
- **AC-C.2** — `CopyJSONMap` typed convenience analogue per §2 signature;
  the migrated `apistage.go:227` call uses `CopyJSONMap`, not
  `CopyJSONValue` + cast.
- **AC-C.3** — Exactly ONE migration site:
  `internal/resolvers/restactions/api/apistage.go:227`. Other
  `maps.DeepCopyJSON` callers in the snowplow tree are UNTOUCHED (Ship C.2
  deferral). Grep gate:
  `grep -rn 'maps\.DeepCopyJSON' --include='*.go' .` confirms post-diff
  the only removed call is at `apistage.go:227`.
- **AC-C.4** — The `import "github.com/krateoplatformops/plumbing/maps"` at
  `apistage.go:40` is removed iff no other use in the file (architect
  confirms: only `DeepCopyJSON` is used at design time; the import goes).
- **AC-C.5 (-race obligation,
  `feedback_shared_vs_copy_is_a_concurrency_change` — THIRD invocation)** —
  `TestCopyJSONValue_AliasingConcurrency_Race` exists with **32 readers ×
  50 iters + 1 writer mutating the shared template**.
  `go test -race ./internal/resolvers/restactions/api/...` passes clean.
  The pre-existing Ship A `TestEvalValue_AliasingConcurrency_Race` and
  Ship B `TestRBACSnapshot_Race_ReaderWriter` MUST also stay green —
  proves the three-ship concurrency invariant chain holds end-to-end.
- **AC-C.6** — `TestCopyJSONValue_NoSharedSubtrees`,
  `TestCopyJSONValue_PanicOnUnknown`, and
  `TestCopyJSONValue_UpstreamEquivalence` per §6, with the
  `UpstreamEquivalence` test covering `int`, `int32`, `float32`,
  `[]map[string]any`, and a deep nested envelope (the rows the
  verification-gate bounce surfaced).
- **AC-C.7 (alloc-gate — HARD)** — the §7.2 measurement protocol on a
  Ship C test pod produces **≥ 0.6 GB net delta on `deepCopyJSONValue`**
  symbol at the migrated call site;
  `listEnvelopeValue` cum drops by ≥ 0.6 GB; per-`/call` delta ≥ 10 MB
  saved. A measured delta below 0.6 GB FAILS the ship (the §8 second
  falsifier fires; revert per the bounded-headroom acknowledgement).
- **AC-C.8 (content-equivalence — HARD)** — the same 16/16 byte-identical
  content corpus Ship B passed must still pass post-Ship-C, including the
  cyberjoker 403-RBAC-denial body and the 4-RBAC-path UAF corpus
  (Ship B AC-B.11 reuse).
- **AC-C.9** — `go build ./...` clean; `go vet` clean; `go test
  ./internal/resolvers/restactions/api/...` clean; `go test -race ...`
  clean.

### Ratified design decisions

*(To be filled in after PM ratifies. Architect notes the following are
expected to be ratified.)*

- **Decision 1 — Bounded-headroom ship.** Ship C's estimated
  reduction is 0.7–1.3 GB / 60 calls, gated at ≥ 0.6 GB, with explicit
  small-win-or-revert posture. The PM acknowledges upstream is already a
  type-switch and the verdict's framing was optimistic.
- **Decision 2 — int/int32/float32 conversions preserved verbatim.** Per
  the team-lead verification-gate bounce, §2.1 explicit-cases the three
  numeric conversions and §5.1 + §5.7 hold byte-equivalence at the
  Go-type level (not merely at the JSON boundary).

### PM tightenings absorbed into the design

*(To be filled in after PM ratifies.)*

### Out of Ship C scope (deferred)

- **All other `maps.DeepCopyJSON` callers** in the snowplow tree —
  one-lever-per-ship discipline. If the post-Ship-C alloc profile shows a
  meaningful residual on `deepCopyJSONValue` from a different caller, a
  follow-up **Ship C.2** ports those callers; otherwise the campaign closes
  at Ship C.
- **Ship A.2 string-site migration** (Ship A's sites #5/#6/#8/#9) — still
  deferred; orthogonal to Ship C and to the resolver-path rebuild's bounded
  scope.
- **Any further wins past the post-Ship-C 4.0 GB / 60-call baseline** —
  requires structural rework (e.g. eliminating the cached-envelope copy via
  a copy-on-write content cache, a multi-ship redesign of the F1 content
  cache itself). Out of campaign scope; PM decides whether a follow-up
  campaign is funded.
