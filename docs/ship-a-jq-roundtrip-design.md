# Ship A — Eliminate the jq string round-trip (D3 + D4)

**Status:** GATE-PASSED — GO for development (PM gate, 2026-05-19).
**Campaign:** A+B+C resolver-path rebuild (verdict 0.30.136). Ship A is lever 1 of 3.
**Authoritative reference** for the developer (implements to AC-A.1..A.7) and the
tester (validates against §5). The architect-review gate checks the diff against
this document.

---

## §0. What the round-trip actually is (TRACED)

The current hot path in `jsonHandlerCore`
(`internal/resolvers/restactions/api/handler.go:95-105`):

1. `jqutil.Eval(...)` — TRACED `jqutil.go:20-63`: parses, compiles, runs
   `code.RunWithContext`, and for each yielded value calls `enc.encode(v)` — the
   **custom** jqutil encoder (`encoder.go:41-74`), **not `encoding/json`** —
   accumulating into a `bytes.Buffer`. Returns `enc.w.String()` — a JSON **string**.
2. `json.Unmarshal([]byte(s), &tmp)` — TRACED `handler.go:103`: standard
   `encoding/json` decodes that string **straight back** into an `any` tree.

The isolated `-base` alloc diff (verdict §2, 60-navmenu workload on live 0.30.136)
attributes:

- **D4** `jqutil.(*encoder).encode` chain: **1.47 GB / 60 calls**
  (`bytes.growSlice` 1.03 GB + `bytes.Buffer.String` 0.39 GB).
- **D3** `jsonHandlerCore -> json.Unmarshal`: **1.48 GB / 60 calls**
  (`peek 'Unmarshal$'` shows 1478 MB flows from `jsonHandlerCore`).

Combined **~2.95 GB / 60 calls = ~49 MB per `/call`**. gojq's `code.RunWithContext`
**already produced the result as a Go `any` value** inside `Eval`
(`jqutil.go:42` `v, ok := iter.Next()`); the encode + re-decode converts that value
-> string -> value with **no semantic purpose** on the `handler.go` path, which
immediately needs a value again.

**Constraint note:** `jqutil` is `github.com/krateoplatformops/plumbing/jqutil`
(go.mod:9) — **UPSTREAM**. Ship A does **not** patch `jqutil`. It **adds** a
snowplow-local thin wrapper over `github.com/itchyny/gojq` (third-party, freely
usable) and migrates 3 call sites. Ship A adds a wrapper + migrates call sites; it
does not "delete a hop in jqutil."

---

## §1. The wrapper API

**New file: `internal/resolvers/restactions/api/jqvalue.go`** (snowplow-local; same
package as `handler.go` — no export-surface change, no new package import graph).

Conceptual signature:

```
// EvalValue runs query against data and returns gojq's result VALUE
// directly — no encode-to-string, no json.Unmarshal-back.
func EvalValue(ctx context.Context, query string, data any, ml gojq.ModuleLoader) (value any, ok bool, err error)
```

`EvalValue` outcome contract — locks the §3.4 truth tables:

| gojq outcome                    | `EvalValue` returns                                  |
|---------------------------------|------------------------------------------------------|
| parse error                     | `(nil, false, err)`                                  |
| compile error                   | `(nil, false, err)`                                  |
| runtime error-value yielded     | `(nil, false, err)` — matches `jqutil.go:46-48`      |
| zero values (jq `empty`)        | `(nil, false, nil)` — `ok=false`, **no error**       |
| exactly one value               | `(v, true, nil)`                                     |
| more than one value             | `(nil, false, ErrMultiYield)` — sentinel error       |

`ErrMultiYield` is a package-level sentinel (`errors.New("jq query yielded N
values, expected 1")`-style; comparable via `errors.Is`). The wrapper **collapses
zero-yield and multi-yield onto the `ok`/`err` channels** so each call site routes
them; the wrapper does **not** decide stage-fail vs skip — that is the call site's
job (§3.4).

**Internals — all gojq-native, no jqutil:**

- `gojq.Parse(query)` — TRACED `gojq.go`/parser; the same call jqutil makes at
  `jqutil.go:23`.
- `gojq.Compile(query, gojq.WithModuleLoader(ml))` — TRACED `jqutil.go:32`.
- `code.RunWithContext(ctx, data)` -> `gojq.Iter` — TRACED `compiler.go:43`.
- Loop `iter.Next()`; the first non-error value is the result; an `error`-typed
  yielded value -> return as `err` (TRACED jqutil does this at `jqutil.go:46-48`).
- The result value is whatever gojq yields: documented types
  `nil, bool, int, float64, *big.Int, string, []any, map[string]any` —
  TRACED `gojq/type.go:11,18`.

**Compile caching is OUT of Ship A scope** (PM directive — one lever per ship for
clean alloc attribution; `gojq.Parse`/`Compile` did not surface in the verdict's
alloc top-25). `EvalValue` calls `gojq.Parse` + `gojq.Compile` **inline every
call**, exactly as `jqutil.Eval` does today. Compile-caching is deferred to
**Ship A.2** (alongside the #5/#6/#8/#9 string-site migration).

**Toggle compliance:** the wrapper is pure compute, reachable only when the
resolver runs. `CACHE_ENABLED=false` does not change whether jq filters run
(filters are RESTAction spec, not cache). Ship A is orthogonal to the cache toggle
and does not touch it.

---

## §2. Call-site migration list

TRACED enumeration of every non-test `jqutil.Eval/ForEach/Extract` in the live
tree (`.claude/worktrees/*` excluded — stale copies):

| #  | Site                                              | Result consumption                                 | Hot per-`/call`? | Migrate?                       |
|----|---------------------------------------------------|-----------------------------------------------------|------------------|--------------------------------|
| 1  | `api/handler.go:95`                               | `json.Unmarshal([]byte(s),&tmp)` — re-decodes value | YES              | **MIGRATE** — the D3/D4 lever  |
| 2  | `api/refilter.go:292` (`resolveUAFResources`)     | `json.Unmarshal([]byte(v),&arr)` — `[]any`          | YES (UAF active) | **MIGRATE**                    |
| 3  | `api/refilter.go:342` (`evalJQString`)            | `trimJSONString(s)` — wants Go string               | YES (UAF active) | **MIGRATE**                    |
| 4  | `restactions/restactions.go:60`                   | `raw = []byte(s)` — wants JSON bytes (`Status.Raw`) | per RESTAction   | KEEP — consumer wants bytes    |
| 5  | `api/setup.go:49` (`ForEach`)                     | iterates a JSON array                               | YES              | KEEP — Ship A.2                |
| 6  | `api/setup.go:85` (`evalJQ`)                      | wants Go string (URL/header/payload)                | YES              | KEEP — Ship A.2                |
| 7  | `widgets/widgetdatatemplate/resolve.go:48`        | `jqutil.InferType(s)` — string->type heuristic      | per element      | KEEP — Ship A.2 (not hot path) |
| 8  | `widgets/resourcesrefstemplate/resolve.go:59`     | `ForEach` — iterates                                | per widget       | KEEP — Ship A.2                |
| 9  | `widgets/resourcesrefstemplate/resolve.go:88`     | `evalJQ` — wants Go string                          | per field        | KEEP — Ship A.2                |
| 10 | `handlers/jq.go:69`                               | `InferType` — `/api/jq` debug endpoint              | NO               | KEEP — not hot                 |

**Ship A migrates #1, #2, #3.** Boundary rationale:

- **#4 KEEP:** `raw = []byte(s)` is assigned to `runtime.RawExtension.Raw`, which
  **is** a `[]byte` JSON field — the string IS the product, no round-trip waste.
  Not a D3/D4 site.
- **#5/#6/#8/#9 KEEP for Ship A:** `ForEach`/`evalJQ` consume **strings** or
  iterate. They pay an encode but are lower-volume (not in the alloc top-25) and
  migrating them re-introduces a value->string path. One lever per ship — deferred
  to **Ship A.2**.
- **#7/#10 KEEP:** `InferType` is a string->type heuristic (`infer.go:16-71`);
  feeding a value would require re-implementing it. `handlers/jq.go` is the
  `/api/jq` debug endpoint, not the resolver `/call` path.

---

## §3. Output-equivalence analysis (load-bearing)

Question: does `value -> gojq -> jqutil-encode -> json.Unmarshal` produce a
*different* `any` tree than `value -> gojq -> (direct)`? If the round-trip
normalizes anything, the wrapper must replicate it.

### §3.1 — Number representation (highest-risk axis)

TRACED chain:

1. **Input** to `handler.go:95` is `pig` = `map[string]any{opts.key: tmp, ...}`.
   `tmp` originates from `apistage.go` decode via **`encoding/json`**
   (`apistage.go:32` imports `"encoding/json"`, no `UseNumber`). Input numbers are
   **`float64`**.
2. gojq's `RunWithContext` calls `normalizeNumbers(v)` on the input
   (`compiler.go:52`). For `float64`, `normalizeNumbers` (`normalize.go:28-83`)
   has **no `case float64`** — `float32` is handled, `float64` falls to
   `default: return v`. Input `float64` stays `float64`.
3. gojq's **output** value types are exactly
   `nil, bool, int, float64, *big.Int, string, []any, map[string]any`
   (`type.go:11,18`). gojq emits **`int`** for integral results (arithmetic,
   `length`, indices), `float64` for fractional, `*big.Int` for huge integers.
4. **Round-trip path:** the jqutil encoder serializes (`encoder.go:41-74`):
   `int`->`strconv.AppendInt`, `float64`->`encodeFloat64`, `*big.Int`->`v.Append`.
   Then `json.Unmarshal` with **no `UseNumber`** (`handler.go:103`) decodes
   **every JSON number back to `float64`**.

**THE NORMALIZATION:** the round-trip collapses gojq's `int` and `*big.Int`
outputs to `float64`. The direct path preserves them as `int` / `*big.Int`.

**Observable at the widget-prop boundary?** The result flows into
`opts.out[opts.key]` -> `dict` -> RESTAction `Status` / widget props. The final
serialization to the browser is a downstream `json.Marshal`. `json.Marshal` of
`int(5)` -> `5`; of `float64(5.0)` -> `5`. **For integral values the rendered JSON
is byte-identical.** For `*big.Int` beyond float64's 2^53 exact-integer range, the
round-trip path **already loses precision** — the direct path is strictly *more*
correct. For genuine `float64` fractionals, both paths carry `float64`.

**Conclusion:** the round-trip's only number normalization is the `int->float64`
collapse, invisible after the final `json.Marshal` for the exact-integer range and
a precision *loss* the direct path fixes beyond it. The wrapper **must NOT
replicate** the collapse — replicating it would re-introduce the bug. The direct
path is equivalent-or-better.

### §3.2 — gojq output value types vs `json.Unmarshal` types

`json.Unmarshal` into `any` produces `nil, bool, float64, string, []any,
map[string]any`. gojq produces those **plus `int` and `*big.Int`**. The only
type-set difference is the number types — fully covered by §3.1. Strings, bools,
nulls, arrays, objects: gojq emits the identical Go types `json.Unmarshal` would.
Equivalent.

### §3.3 — Object key ordering

jqutil's encoder **sorts object keys** (`encoder.go:193-195`). `json.Unmarshal`
into `map[string]any` is unordered anyway. The direct path hands back gojq's
`map[string]any` — also unordered. The sort is cosmetic to the string form only;
once decoded to a map it is irrelevant. The final downstream `json.Marshal` of a
`map[string]any` also sorts keys. Equivalent at the widget-prop boundary.

### §3.4 — Yield-cardinality and error routing: the migrated-site contract

(REVISED — gate-passed version. Replaces the bounced §3.4 in full.)

#### §3.4.0 — Exact current behavior (TRACED)

`jqutil.Eval` (`jqutil.go:20-63`) returns `(string, error)`:

- **parse error** -> `("", err)` (`jqutil.go:24-25`).
- **compile error** -> `("", err)` (`jqutil.go:36-37`).
- **runtime gojq-error-value** — a yielded value type-asserts to `error` ->
  `("", err)` (`jqutil.go:46-48`); the loop stops, prior values discarded.
- **zero-yield** (jq `empty`) — zero `enc.encode` calls -> `enc.w` empty ->
  returns **`("", nil)`** — empty string, **no error**.
- **single-yield** -> `enc.encode` once -> `(jsonString, nil)`.
- **multi-yield** -> `enc.encode` N times, **no separator** (`jqutil.go:41-52`) ->
  `(concatenatedInvalidJSON, nil)` — e.g. `{...}{...}`.

Zero-yield returns `("", nil)` and multi-yield returns `(invalidJSON, nil)` —
**both with `err==nil`**. So the *caller's* subsequent `json.Unmarshal` is what
converts them into observable behavior:

- `json.Unmarshal([]byte(""), &x)` -> error `"unexpected end of JSON input"`.
- `json.Unmarshal([]byte("{...}{...}"), &x)` -> error
  `"invalid character '{' after top-level value"`.

#### §3.4.1 — Wrapper contract

See §1 — `EvalValue` separates the five outcomes onto `(value, ok, err)`:
zero-yield is `(nil,false,nil)`; multi-yield is `(nil,false,ErrMultiYield)`;
parse/compile/runtime is `(nil,false,err)` with `err != ErrMultiYield`.

#### §3.4.2 — Site #1: `handler.go:95` (`jsonHandlerCore` filter)

| Case               | Current behavior (TRACED)                                                              | Ship A behavior                                                              | Identical?      |
|--------------------|----------------------------------------------------------------------------------------|------------------------------------------------------------------------------|-----------------|
| single value       | `json.Unmarshal` ok -> `tmp` updated -> stage continues                                | `EvalValue`->`(v,true,nil)` -> `tmp=v` -> stage continues                    | YES             |
| zero-yield         | `s==""` -> `json.Unmarshal("")` **errors** -> `return err` -> **stage FAILS**           | `EvalValue`->`(nil,false,nil)` -> caller `return err` -> **stage FAILS**      | YES             |
| multi-yield        | `"{...}{...}"` -> `json.Unmarshal` **errors** -> `return err` -> **stage FAILS**        | `EvalValue`->`(nil,false,ErrMultiYield)` -> `return err` -> **stage FAILS**   | YES             |
| parse/compile err  | `err!=nil` -> `log.Error` -> **`tmp` unchanged, stage CONTINUES**                       | `EvalValue`->`(nil,false,err)` -> `log.Error` -> continue                    | YES             |
| runtime error-val  | `Eval` returns `("",err)` -> `log.Error` -> **`tmp` unchanged, stage CONTINUES**        | `EvalValue`->`(nil,false,err)` -> `log.Error` -> continue                    | YES             |

The caller must distinguish two `(nil,false,*)` outcomes that route oppositely:
parse/compile/runtime-error -> log + continue; zero-yield + multi-yield -> stage
fails. `EvalValue` separates them: zero-yield `err==nil`; multi-yield
`err==ErrMultiYield`; parse/compile/runtime `err!=nil && err!=ErrMultiYield`.

**Exact migrated `handler.go:95-106` block:**

```go
if opts.filter != nil {
    q := ptr.Deref(opts.filter, "")
    log.Debug("found local filter on api result", slog.String("filter", q))
    v, ok, err := EvalValue(context.TODO(), q, pig, jqsupport.ModuleLoader())
    switch {
    case errors.Is(err, ErrMultiYield):
        // Current: multi-yield -> invalid concatenated JSON ->
        // json.Unmarshal errors -> return err -> stage fails.
        return err
    case err != nil:
        // Parse/compile/runtime gojq-error. Current: log.Error, tmp
        // unchanged, stage continues. (jqutil.Eval err branch.)
        log.Error("unable to evaluate JQ filter",
            slog.String("filter", q), slog.Any("error", err))
    case !ok:
        // Zero-yield (jq `empty`). Current: jqutil.Eval returns "",
        // json.Unmarshal("") errors -> return err -> stage fails.
        return fmt.Errorf("jq filter %q yielded no value", q)
    default:
        // Single value. Current: json.Unmarshal(s) -> tmp. Ship A:
        // tmp = v directly (the §3.1-3.3 equivalence proof).
        tmp = v
    }
}
```

Adds `errors`, `fmt` imports (std). The `encoding/json` import **stays** —
`jsonHandlerBytesApply` (`handler.go:70`) still uses `json.Unmarshal`. Only the
`handler.go:103` `json.Unmarshal` line is removed.

#### §3.4.3 — Site #2: `refilter.go:292` (`resolveUAFResources`)

Fail-closed security path — any anomaly returns `(nil,false)` and the caller drops
all items.

| Case                       | Current behavior (TRACED)                                                              | Ship A behavior                                                       | Identical?      |
|----------------------------|----------------------------------------------------------------------------------------|-----------------------------------------------------------------------|-----------------|
| single value, is `[]any`   | `json.Unmarshal` ok, `[]any` ok -> element loop runs                                   | `EvalValue`->`(v,true,nil)`; `v.([]any)` ok -> element loop           | YES             |
| single value, not `[]any`  | `json.Unmarshal` into `[]any` of non-array **errors** -> `log.Warn` -> `return nil,false`| `v.([]any)` assert **fails** -> `log.Warn` -> `return nil,false`     | YES             |
| zero-yield                 | `s==""` -> `json.Unmarshal("")` **errors** -> `log.Warn` -> `return nil,false`          | `EvalValue`->`(nil,false,nil)` -> `log.Warn` -> `return nil,false`    | YES (fail-closed)|
| multi-yield                | `json.Unmarshal` **errors** -> `log.Warn` -> `return nil,false`                         | `EvalValue`->`(nil,false,ErrMultiYield)` -> `log.Warn` -> `nil,false` | YES             |
| parse/compile err          | `Eval` `err!=nil` -> `log.Warn "JQ eval failed"` -> `return nil,false`                  | `EvalValue`->`(nil,false,err)` -> `log.Warn "JQ eval failed"` -> `nil,false`| YES        |
| runtime error-val          | `Eval` returns `("",err)` -> `log.Warn "JQ eval failed"` -> `return nil,false`          | `EvalValue`->`(nil,false,err)` -> `log.Warn "JQ eval failed"` -> `nil,false`| YES        |

Every non-single-array outcome routes to the same `(nil,false)`. The current code
keeps two warn messages ("JQ eval failed" / "not a JSON array"); Ship A keeps both,
mapped to the same gojq outcomes.

**Exact migrated block (replacing `refilter.go:292-312`, through the `var arr
[]any` decode):**

```go
v, ok, err := EvalValue(ctx, uaf.ResourcesFrom, dict, jqsupport.ModuleLoader())
if err != nil {
    // Parse/compile/runtime error OR ErrMultiYield. Current: the
    // jqutil.Eval err branch logs "JQ eval failed"; the multi-yield case
    // currently surfaces as a json.Unmarshal error logged "not a JSON
    // array". Both fail-closed identically — one warn; the distinction
    // was never security-load-bearing.
    log.Warn("userAccessFilter: ResourcesFrom JQ eval failed; fail-closed",
        slog.String("expr", uaf.ResourcesFrom), slog.Any("err", err))
    return nil, false
}
arr, isArr := v.([]any) // !ok (zero-yield) => v==nil => isArr false
if !ok || !isArr {
    // Zero-yield, or a non-array result. Current: json.Unmarshal of "" or
    // of a non-array into []any errors -> "not a JSON array" -> fail-closed.
    log.Warn("userAccessFilter: ResourcesFrom result is not a JSON array; fail-closed",
        slog.String("expr", uaf.ResourcesFrom))
    return nil, false
}
```

The downstream element loop (`refilter.go:313-331`, `for _, e := range arr` with
the per-element `e.(string)` assertion) is **unchanged** — `arr` is the identical
`[]any`.

**Element-type subtlety (security gate):** the current path's `json.Unmarshal`
decodes JSON numbers to `float64`; the direct path may carry gojq `int`. But the
element loop only accepts `string` elements (`e.(string)`, `refilter.go:316`) — a
numeric element fails the assertion on **both** paths and triggers the same
fail-closed `return nil,false`. No divergence in the security verdict.

#### §3.4.4 — Site #3: `refilter.go:342` (`evalJQString`)

`evalJQString` returns `(string, error)`. Current path: `jqutil.Eval` ->
`trimJSONString(s)`. **`trimJSONString` (`refilter.go:359-381`) is the critical
detail** — it is *not* a `json.Unmarshal`; it is a string-level trim:
returns `""` for `s=="null"` or `s==""`; strips one layer of surrounding `"` if
present; otherwise returns `s` verbatim.

| Case                          | Current behavior (TRACED)                                                                 | Ship A behavior                                                       | Identical?              |
|--------------------------------|-------------------------------------------------------------------------------------------|-----------------------------------------------------------------------|-------------------------|
| single value, string `"foo"`   | `s==`\``"foo"`\`` ` -> `trimJSONString` strips quotes -> `("foo",nil)`                    | `EvalValue`->`(v,true,nil)`, `v` is Go `string` -> return `(v,nil)`   | YES                     |
| single value, null (jq `null`) | `s=="null"` -> `trimJSONString` -> `("",nil)`                                             | `EvalValue`->`(nil,true,nil)`, `v==nil` -> `("",nil)`                 | YES                     |
| single value, non-string       | `s` e.g. `5`/`{...}`/`true` -> `trimJSONString` (no quotes) -> returns `s` **verbatim**    | `EvalValue`->`(v,true,nil)`, non-string -> `encodeValueCompact(v)`    | YES (see note)          |
| zero-yield                      | `Eval`->`("",nil)` -> `trimJSONString("")` -> **`("",nil)`**                              | `EvalValue`->`(nil,false,nil)` -> caller maps `!ok && err==nil`->`("",nil)`| YES                |
| multi-yield                     | `Eval`->`("{...}{...}",nil)` -> `trimJSONString` -> returns concatenation **verbatim, no error** | `EvalValue`->`(nil,false,ErrMultiYield)` -> `return "",err`     | **DELIBERATE CHANGE**   |
| parse/compile err               | `Eval` `err!=nil` -> `return "",err`                                                      | `EvalValue`->`(nil,false,err)` -> `return "",err`                     | YES                     |
| runtime error-val               | `Eval` returns `("",err)` -> `return "",err`                                              | `EvalValue`->`(nil,false,err)` -> `return "",err`                     | YES                     |

**Non-string-single-value note:** the current code returns the *JSON
serialization* of a non-string gojq result (`trimJSONString` is a no-op with no
surrounding quotes). To stay byte-identical, Ship A's `evalJQString` on the
non-string branch serializes `v` as the jqutil encoder would. This is a **cold,
rare branch** — `evalJQString` callers expect identity-string expressions
(`.metadata.name`, `.`). The wrapper exposes a tiny `encodeValueCompact(v any)
string` helper used **only here** (it may use `encoding/json.Marshal` of the single
value — non-hot, correctness over speed; the §3.1 number caveat is documented in
the test).

**Exact migrated `evalJQString` (replacing `refilter.go:342-356`):**

```go
func evalJQString(ctx context.Context, expr string, data any) (string, error) {
    v, ok, err := EvalValue(ctx, expr, data, jqsupport.ModuleLoader())
    if errors.Is(err, ErrMultiYield) {
        // DELIBERATE CHANGE — see §3.4.5. Pre-Ship-A a multi-yield expr
        // returned the concatenated invalid-JSON string verbatim (latent
        // garbage). Ship A surfaces it as an error.
        return "", err
    }
    if err != nil {
        return "", err
    }
    if !ok {
        // Zero-yield. Current: trimJSONString("") == "".
        return "", nil
    }
    switch s := v.(type) {
    case string:
        return s, nil // current: trimJSONString strips the quotes
    case nil:
        return "", nil // current: trimJSONString("null") == ""
    default:
        // Non-string single value. Current: trimJSONString of the JSON
        // serialisation == the serialisation verbatim. Cold branch.
        return encodeValueCompact(v), nil
    }
}
```

`trimJSONString` (`refilter.go:359-381`) becomes **dead code** once #3 migrates —
Ship A deletes it (verify with a `grep trimJSONString` gate before deletion; it has
no other caller). `evalJQString` adds the `errors` import.

#### §3.4.5 — Deliberate-change call-out (RATIFIED by PM gate)

One case across the three sites is NOT preserved byte-identical, by deliberate
decision:

> **Site #3, multi-yield.** Pre-Ship-A, an `evalJQString` expression that
> multi-yields returns the **concatenated invalid-JSON string verbatim, with
> `err==nil`** — garbage that then flows into a URL path / `ResourceRef` field /
> RBAC namespace. This is a **latent bug**: it never errors, it silently produces a
> malformed value. Ship A surfaces it as an explicit `ErrMultiYield` error, so
> `evalJQString` returns `("", err)` and the caller's existing error handling
> fires (fail-closed at both consumers — see §3.4.6).

**Rationale:** `evalJQString` is documented (`refilter.go:334-341`) as evaluating a
single-value expression (`.metadata.name`). A multi-yield expression at this site
is malformed usage; pre-Ship-A silent-garbage is strictly worse than an error.
Consistent with `feedback_cache_must_not_constrain_jq` (which protects *valid*,
layering-contract-conformant expressions — a multi-yield `evalJQString` expression
is not valid for this site). **PM gate outcome: RATIFIED.**

Sites #1 and #2 multi-yield are preserved exactly — both already fail
(stage-fail / fail-closed) on multi-yield today. Zero-yield at all three sites is
preserved exactly — #1 stage-fails, #2 fails-closed, #3 returns `""`.

#### §3.4.6 — `evalJQString` has TWO consumers (PM finding — design absorbs it)

`evalJQString` is called at **two** sites — TRACED:

1. `refilter.go:230` — `evalSingle`, for the UAF `NamespaceFrom` namespace. This
   is a **fail-closed RBAC security path**: a non-nil error from `evalJQString`
   makes `evalSingle` `log.Warn "...treating item as denied"` and `return false`
   (`refilter.go:231-238`) — the item is dropped.
2. (the `evalJQ`-style URL/field uses are sites #6/#9 — those call a *different*
   `evalJQ`, not `evalJQString`; `evalJQString` itself has only the one
   `refilter.go:230` consumer besides being defined at `:342`.)

Because Ship A migrates the **`evalJQString` function body**, the `refilter.go:230`
`NamespaceFrom` consumer is covered automatically. The §3.4.5 deliberate change
(multi-yield -> error) at `NamespaceFrom` means a multi-yield `NamespaceFrom`
expression now **fails the item closed** instead of computing a namespace from
garbage — strictly safer for the security path. The tester's content-capture
(§5) **must exercise a UAF RESTAction with a `NamespaceFrom` expression**, not only
`ResourcesFrom`, so this consumer is validated live.

#### §3.4.7 — Test coverage delta (extends §5's `jqvalue_test.go`)

For **each of the 3 sites**, an explicit assertion per truth-table row: single /
zero-yield / multi-yield / parse-error / compile-error / runtime-error-value —
asserting the migrated function's `(return value, error, side-effect on
tmp/dict)` matches a golden capture of the pre-Ship-A function. The multi-yield row
for #3 asserts the **new** error behavior and is tagged in the test as the §3.4.5
deliberate change. The #3 cases run against **both** consumers — the
`evalJQString` unit and an `evalSingle`/`NamespaceFrom` integration assertion.

### §3.5 — The `slice` / `filter` shapes in `handler.go`

`pig` carries `{opts.key: tmp, "slice": ...}` (`handler.go:85-90`). Nothing in the
round-trip is `slice`-aware; `slice` is just input data. The direct path feeds the
identical `pig` to gojq. Equivalent.

### §3.6 — Equivalence verdict

For **valid, layering-contract-conformant** jq expressions on the migrated sites,
the direct path produces a widget-prop output **observably identical** after the
final downstream `json.Marshal`, and strictly more correct for large integers. The
only behavior the wrapper deliberately replicates is the **yield-cardinality
contract** (§3.4) — pinned per-site, byte-identical except the one RATIFIED
deliberate change (§3.4.5).

---

## §4. Output aliasing / isolation

**Does gojq's result value alias the input tree?** jq is a functional
transformation — `{a: .x}` builds a new map; but jq's identity filter `.` yields
the input value *itself*, and `.foo` yields the *same* sub-object reference held in
the input map. So **the result CAN alias sub-trees of the input.**

**Is this a Ship A problem? No — precise boundary:**

- The **input** to `handler.go:95` is `pig`, built fresh at `handler.go:85` as a
  **new** `map[string]any` each call. Its values (`tmp`) come from
  `apistageContentServe` -> `listEnvelopeValue`, which **already deep-copies** via
  `maps.DeepCopyJSON` (`apistage.go:227`) — the **D1 / Ship C** isolation copy. So
  `tmp` is already a private tree.
- A gojq result aliasing `tmp` therefore aliases a tree already private to this
  call. Putting it into `dict` is safe — no other goroutine shares it.
- The **current** round-trip path also adds no isolation: `json.Unmarshal` builds a
  fresh tree, but from a string derived from the already-private `tmp`. So
  round-trip-vs-direct does not change the isolation posture — both rely on D1.

**Conclusion:** Ship A's output handling is **isolation-neutral**. The result may
alias `tmp`, but `tmp` is already per-call-private (D1/Ship C's job). Ship A
introduces no new aliasing hazard.

**Cross-ship invariant (Ship C must preserve):** Ship A correctness depends on D1
keeping `tmp` private — it does today (`apistage.go:227`). Ship C makes that copy
*cheaper*, never removes it (verdict §6 falsifier). **Ship C's design must cite
this dependency.** This is a cross-ship invariant, not a Ship A defect.

See **AC-A.6** — the dev must discharge this with file:line read-only proof for all
3 migrated callers OR a `-race` concurrent test.

---

## §5. Expected allocation reduction & measurement

**Expected:** D3+D4 = ~2.95 GB / 60 navmenu `/call` = ~49 MB per `/call`
eliminated. Residual: gojq's own result tree (`makeObject`, `env.Next`) — TRACED
~0.4 GB in the diff, unavoidable (D5, structural). INFERRED net reduction
~2.5 GB / 60 calls, i.e. per-`/call` resolver allocation drops from ~127 MB toward
~80 MB.

**Tester measurement protocol (the verdict's method):**

1. Deploy the Ship A image to a **test pod**; do NOT redeploy 0.30.136 prod.
2. Read-only `kubectl port-forward` to the Ship A pod `:8081` (pprof on the main
   mux — TRACED `main.go:543`).
3. Capture baseline: `curl .../debug/pprof/heap -o heap_pre.pprof`.
4. Drive **60 cyberjoker navmenu resolves** — identical workload:
   `curl -H "Authorization: Bearer <cj.jwt>" ".../call?resource=navmenus&apiVersion=widgets.templates.krateo.io%2Fv1beta1&name=sidebar-nav-menu&namespace=krateo-system"`
   x60. Confirm the navmenu has a `Filter` (exercises `handler.go:95`) via a
   Debug-log `found local filter`.
5. Capture `heap_post.pprof`.
6. `go tool pprof -alloc_space -base heap_pre.pprof -top -nodecount=25 heap_post.pprof`.
7. **Pass criteria:** `jqutil.(*encoder).encode` AND the
   `jsonHandlerCore -> json.Unmarshal` edge **both drop out of the top-25 / fall
   below ~0.2 GB**; total delta drops by **>= 2.0 GB** (conservative vs the 2.5 GB
   estimate, allowing noise). `Resolve.func4` cum drops correspondingly.
8. **Content check** (`feedback_validate_content_not_just_status`): diff the served
   navmenu `/call` body byte-for-byte against a 0.30.136 capture — must be
   identical. ALSO exercise a UAF RESTAction with both a `ResourcesFrom` and a
   `NamespaceFrom` expression (§3.4.6) and verify the served, RBAC-narrowed content
   is identical to 0.30.136.

**Unit test `jqvalue_test.go`** (proves §3): representative jq expressions —
integers (incl. > 2^53), floats, negative/zero, strings (unicode, escapes — the
`encoder.go:103` paths), bools, nulls, arrays, nested objects, the
`{opts.key:..., "slice":...}` `pig` shape, `empty` (zero-yield), multi-yield. Number
cases explicitly assert the `int->float64` collapse is invisible post-`Marshal`.
Plus the §3.4.7 per-site truth-table rows.

---

## §6. Falsifier

**"Ship A is wrong if the jqutil string round-trip performs a semantic
normalization — beyond the `int->float64` collapse identified in §3.1 — that the
direct gojq value lacks, such that a valid jq filter produces a *different*
widget-prop output on the direct path."**

**Check — Ship A HOLDS.** The round-trip is `jqutil-encoder.encode`
(`encoder.go:41-218`) then `encoding/json.Unmarshal` (`handler.go:103`). Every
encoder transform:

- numbers -> §3.1 (`int->float64` collapse — the only normalization, invisible
  post-`Marshal`, a precision-loss the direct path fixes);
- strings -> `encoder.go:103-156` standard JSON escaping; `json.Unmarshal` reverses
  it exactly -> identical Go string; the direct path already yields that string.
  No divergence.
- object key sort -> §3.3, cosmetic to the string only, irrelevant post-decode.
- arrays/objects -> structural, faithful JSON serialization, faithfully reversed ->
  identical tree shape. No divergence.
- `NaN`/`±Inf` floats -> `encoder.go:78-86` maps `NaN`->`null`, clamps `±Inf`. A
  real normalization the direct path lacks — but jq's number domain produces no
  `NaN`/`Inf` from valid arithmetic over JSON inputs (`1/0` raises a jq *error*,
  caught as `err`), and JSON has no `NaN`/`Inf` literal, so the `encoding/json`
  decode of an apiserver envelope cannot produce one. **Unreachable on valid
  inputs.** Documented; the §5 test includes a `NaN`-input case to mark the
  boundary.

The only normalization on a reachable, valid input is the `int->float64` collapse,
proven invisible at the widget-prop boundary and a loss the direct path corrects.
No reachable valid jq filter produces a different widget-prop output on the direct
path. Falsifier checked; Ship A holds.

---

## PM gate outcome

**Verdict: GO for development** (PM gate, 2026-05-19).

### Acceptance criteria — the dev implements to these

- **AC-A.1** — `EvalValue` is added in a new snowplow-local file
  `internal/resolvers/restactions/api/jqvalue.go`, built over
  `github.com/itchyny/gojq` directly. **No `krateoplatformops/plumbing/jqutil`
  patch.** `jqutil` remains an unmodified upstream dependency.
- **AC-A.2** — `EvalValue` honours the §1 outcome contract exactly: parse/compile/
  runtime-error -> `(nil,false,err)`; zero-yield -> `(nil,false,nil)`; single ->
  `(v,true,nil)`; multi -> `(nil,false,ErrMultiYield)`. `ErrMultiYield` is a
  package-level sentinel matchable via `errors.Is`.
- **AC-A.3** — Sites #1 (`handler.go:95`), #2 (`refilter.go:292`), #3
  (`refilter.go:342` `evalJQString`) are migrated to `EvalValue` using the exact
  caller code in §3.4.2 / §3.4.3 / §3.4.4. Sites #4–#10 are NOT touched.
- **AC-A.4** — Per-site behavior is byte-identical to pre-Ship-A for every
  truth-table row in §3.4, with the **single exception** of §3.4.5 (site #3
  multi-yield -> explicit error), which is RATIFIED.
- **AC-A.5** — `trimJSONString` is deleted after #3 migrates; a `grep
  trimJSONString` over the non-test tree confirms zero remaining callers before
  deletion.
- **AC-A.6 (aliasing/concurrency — HARD dev obligation,
  `feedback_shared_vs_copy_is_a_concurrency_change`):** `EvalValue` returns gojq's
  result value, which can **alias sub-trees of the input** (gojq returns input
  sub-trees by reference). The dev must EITHER prove, with file:line citations,
  that all 3 migrated callers treat the result strictly read-only and it never
  enters a shared cache — OR add a `-race` concurrent test exercising two
  concurrent `EvalValue` calls over a shared input. §4 gives the expected
  argument (the input `tmp` is already per-call-private via `apistage.go:227`); the
  dev must confirm it holds at every migrated call site, not assume it.
- **AC-A.7** — `jqvalue_test.go` proves output equivalence (§3.1–3.3) across the
  representative expression set AND the §3.4.7 per-site truth-table rows. The
  tester runs the §5 measurement protocol (>= 2.0 GB alloc-delta reduction) and the
  content check, the latter exercising a UAF RESTAction with **both**
  `ResourcesFrom` and `NamespaceFrom` (§3.4.6).

### Ratified decisions

- **§3.4.5 deliberate change RATIFIED:** site #3 `evalJQString` multi-yield, which
  pre-Ship-A returned concatenated invalid-JSON garbage with `err==nil`, now
  surfaces an explicit `ErrMultiYield` error. At both `evalJQString` consumers this
  fails closed (stage error / item denied) — strictly safer than silent garbage.
  Consistent with `feedback_cache_must_not_constrain_jq` (a multi-yield
  `evalJQString` expression is not a valid expression for that single-value site).

### PM finding absorbed into the design

- **`evalJQString` has a second consumer — `refilter.go:230`** (`evalSingle`, UAF
  `NamespaceFrom` — a fail-closed RBAC security path, not just URL-building).
  Migrating the `evalJQString` function body covers it automatically. The design
  names this consumer in **§3.4.6**; AC-A.7 requires the tester's content-capture
  to exercise a `NamespaceFrom` expression so this path is validated live.

### Out of Ship A scope (deferred)

- **Compile-caching** of `gojq.Parse`/`gojq.Compile` — deferred to **Ship A.2**
  (one lever per ship; did not surface in the verdict alloc top-25).
- **Sites #5/#6/#8/#9** string-consumer migration — deferred to **Ship A.2**.
- **D1 input-isolation copy** — that is **Ship C**'s scope. Ship A only concerns
  the *output*; §4 records the cross-ship invariant Ship C must preserve.

---

## §7. Cross-ship invariant for Ship C (architect follow-up, dev-review gate)

The per-call-private input tree produced by `maps.DeepCopyJSON` at
`internal/resolvers/restactions/api/apistage.go:227` is **load-bearing in two
directions** — Ship C must preserve it (not remove, not shallow-copy):

1. **gojq mutates its input in place.** `gojq.Code.RunWithContext` calls
   `normalizeNumbers(input)` (`github.com/itchyny/gojq@v0.12.17/compiler.go:52`
   → `normalize.go:71-80`), which walks every `map[string]any` / `[]any`
   sub-tree and **writes back** `v[k] = normalizeNumbers(x)`. This is the
   documented cause of the **0.30.128/0.30.129 deploy crash** — two concurrent
   resolvers Get-hitting the same content entry both ran gojq over the same
   maps and raised `fatal error: concurrent map iteration and map write`.
   `apistage.go:213-227` (`listEnvelopeValue`) cites this directly. This
   hazard is **pre-existing** — Ship A inherits it unchanged from
   `jqutil.Eval` (the same `RunWithContext` call) and is NOT a Ship A
   regression. Ship A's `internal/resolvers/restactions/api/jqvalue.go`
   header notes the same finding.

2. **gojq's RESULT can alias sub-trees of the input** (Ship A's new
   surface, §4). The identity filter `.` and field-access `.foo` yield input
   sub-objects **by reference**, not a fresh copy. EvalValue performs no
   defensive copy. So even if a future Ship C convinces itself the input
   mutation is harmless (it is not — see direction 1), aliasing alone
   forbids a shallower copy: an aliased result returned into `dict` /
   `opts.out` would still share refs with whatever Ship C tries to leave
   shared upstream.

**Concrete Ship C guard-rails:**

- The `DeepCopyJSON` at `apistage.go:227` MUST NOT be removed or replaced
  with a shallow copy.
- Any Ship C "cheaper isolation" must produce a tree where every
  `map[string]any` / `[]any` reachable from the returned root is owned
  solely by the current call. A copy-on-write strategy is only sound if it
  triggers BEFORE gojq's `RunWithContext` touches the tree.
- The Ship A `-race` test in `jqvalue_test.go`
  (`TestEvalValue_AliasingConcurrency_Race`) models the production
  invariant — each goroutine deep-copies its input. Ship C should extend,
  not relax, that test: a "ship-C-cheap-copy + concurrent EvalValue"
  variant that still passes `-race` is the minimum bar.

This subsection is the **architect's non-blocking follow-up** recorded at
the Ship A dev-review gate (2026-05-19); Ship C's design must cite it.
