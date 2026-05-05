// Package jqlint provides a static lint over the outer `filter:` jq
// expression of a RESTAction CR.
//
// Q-RBAC-DECOUPLE C(d) v5 — Option A jq AST validator (audit 2026-05-05, D2).
//
// Motivation: when an api[] entry with `continueOnError: true` fails, the
// resolver writes `dict[<errorKey>]` and leaves `dict[<api.name>]` absent.
// If the outer filter then iterates `.<api.name>[]` without a defensive
// guard (// [] default, ? optional suffix, or `if (...) | type == "array"`
// dispatch), gojq evaluates `null[]` and aborts with
// `cannot iterate over: null`, surfacing a 500 to the user.
//
// The lint walks the gojq AST of the outer filter and rejects any
// unguarded `.<NAME>[]` iteration where NAME matches a `continueOnError:
// true` api[] entry's name.
//
// Scope: this lint covers RESTAction YAMLs known at build time (the 6
// portal-cache YAMLs shipped with snowplow). It does NOT enforce the
// invariant on out-of-tree customer-authored YAMLs — that is intentionally
// deferred (see SPEC.md §2.3 Option B).
package jqlint

import (
	"fmt"
	"strings"

	"github.com/itchyny/gojq"
)

// ProtectedField describes one api[] entry whose `name` is the dict key
// that the resolver may leave unset on error (when continueOnError is true).
type ProtectedField struct {
	Name string
}

// LintFilter parses the outer jq `filter` expression and reports a list
// of human-readable violations: each violation is one unguarded
// `.<NAME>[]` iteration whose source field matches a ProtectedField.
//
// A nil/empty list means the filter is null-safe with respect to the
// supplied protected fields.
//
// Returns a parse error if `filter` is not valid jq.
func LintFilter(filter string, protected []ProtectedField) ([]string, error) {
	q, err := gojq.Parse(filter)
	if err != nil {
		return nil, fmt.Errorf("parse jq filter: %w", err)
	}

	protectedSet := make(map[string]struct{}, len(protected))
	for _, p := range protected {
		if p.Name != "" {
			protectedSet[p.Name] = struct{}{}
		}
	}
	if len(protectedSet) == 0 {
		// Nothing to protect → trivially safe.
		return nil, nil
	}

	var v violations
	walkQuery(q, &v, protectedSet, false)
	return v.list, nil
}

type violations struct {
	list []string
}

func (v *violations) add(name string) {
	v.list = append(v.list,
		fmt.Sprintf("unguarded iteration `.%s[]` over a continueOnError api[] field — wrap in `(.%s // [])[]` or `.%s[]?` or `if (.%s|type)==\"array\" then ... else [] end`", name, name, name, name))
}

// walkQuery recursively descends a *gojq.Query AST.
//
// `safeContext` is true when the surrounding production has already
// guaranteed the current value cannot be null (e.g. inside the LHS of a
// `// []` alternation, or inside the `then` branch of an
// `if (.x | type) == "array"`). In those positions, an unguarded `.foo[]`
// is locally OK because we proved upstream that the value is an array.
//
// We propagate `safeContext` only to the immediate child positions where
// the upstream guard genuinely applies; we do NOT propagate it across
// `Term.SuffixList` boundaries (a guard on `.crds` does not protect a
// later `.namespaces[]` in a comma expression).
func walkQuery(q *gojq.Query, v *violations, protected map[string]struct{}, safeContext bool) {
	if q == nil {
		return
	}

	// `A // B` — visiting B is always safe (B is the default, only used
	// when A produced null/empty). Visiting A is safe in the sense that
	// even if A fails, B runs — but A's own iterations can still iterate
	// over null. So we do NOT propagate safeContext to A.
	//
	// However, the outer expression `(.foo // [])` — when its result is
	// then iterated by a Suffix `[]` on the enclosing Term — IS safe
	// because the // [] default guarantees an array. That case is handled
	// at the Term level via isAlternationProvidingArrayDefault.
	if q.Right != nil && q.Op == gojq.OpAlt {
		walkQuery(q.Left, v, protected, false)
		walkQuery(q.Right, v, protected, true)
		return
	}

	if q.Right != nil {
		// Other binary ops (comma, pipe, arithmetic, etc.) — descend
		// into both sides without propagating safeContext.
		walkQuery(q.Left, v, protected, false)
		walkQuery(q.Right, v, protected, false)
		// Pattern bodies (e.g. `as $x`) don't introduce iteration
		// themselves; the iteration source is in q.Left or q.Right
		// already visited.
		return
	}

	// Pure term query.
	walkTerm(q.Term, v, protected, safeContext)

	// Function definitions (rare in outer filters but possible).
	for _, fd := range q.FuncDefs {
		if fd != nil {
			walkQuery(fd.Body, v, protected, false)
		}
	}
}

// walkTerm descends into a *gojq.Term and inspects its SuffixList for
// unguarded protected-field iterations.
func walkTerm(t *gojq.Term, v *violations, protected map[string]struct{}, safeContext bool) {
	if t == nil {
		return
	}

	// Inspect the head of the term: is it a bare `.<NAME>` field whose
	// SuffixList starts with a non-Optional `[]` (Iter without `?`)?
	// That's the offending pattern.
	//
	// gojq parses `[]?` as TWO consecutive suffixes:
	//   Suffix{Iter:true}, Suffix{Optional:true}
	// so `.crds[]?` is safe iff the suffix immediately after the Iter is
	// Optional=true. Likewise `.crds.items[]?` chains: Index{items},
	// Iter, Optional — the iter is Optional-followed → safe.
	if t.Type == gojq.TermTypeIndex && t.Index != nil && t.Index.Name != "" {
		name := t.Index.Name
		if _, isProtected := protected[name]; isProtected && !safeContext {
			for i, sfx := range t.SuffixList {
				if sfx == nil {
					continue
				}
				if sfx.Iter {
					nextOpt := i+1 < len(t.SuffixList) &&
						t.SuffixList[i+1] != nil &&
						t.SuffixList[i+1].Optional
					if !nextOpt {
						v.add(name)
					}
					// Whether safe or not, stop scanning at the first
					// Iter — downstream suffixes apply per-item.
					break
				}
				if sfx.Optional {
					// Bare `.crds?` (no Iter) — short-circuits to null
					// silently if absent; never iterates → safe.
					break
				}
				// Index suffix (e.g. `.crds.items`) — continue scanning;
				// safety determined by the next iter+? pair.
			}
		}
	}

	// Parenthesized sub-query: `(...)`.
	if t.Query != nil {
		// The entire sub-query lives behind a Term whose SuffixList may
		// include an `[]` iteration. If a top-level OpAlt RHS supplies
		// `[]`, the iteration over the sub-query result is safe.
		subSafe := safeContext || isAlternationProvidingArrayDefault(t.Query)
		walkQuery(t.Query, v, protected, subSafe)
	}

	// `if Cond then Then [elif] else Else end` — type-dispatch idiom:
	// if Cond is `(.X | type) == "array"`, the Then branch can iterate
	// `.X[]` safely because we already proved `.X` is an array.
	if t.If != nil {
		safeFields := arrayTypeGuardedFields(t.If.Cond)
		walkQuery(t.If.Cond, v, protected, safeContext)
		walkQuery(t.If.Then, v, withSafeFields(protected, safeFields), safeContext)
		for _, elif := range t.If.Elif {
			if elif == nil {
				continue
			}
			elifSafe := arrayTypeGuardedFields(elif.Cond)
			walkQuery(elif.Cond, v, protected, safeContext)
			walkQuery(elif.Then, v, withSafeFields(protected, elifSafe), safeContext)
		}
		walkQuery(t.If.Else, v, protected, safeContext)
	}

	// `try BODY catch CATCH` — the body is run with errors suppressed.
	// Treat as safe.
	if t.Try != nil {
		walkQuery(t.Try.Body, v, protected, true)
		walkQuery(t.Try.Catch, v, protected, false)
	}

	// Object / Array literal bodies: descend into each value.
	if t.Object != nil {
		for _, kv := range t.Object.KeyVals {
			if kv == nil {
				continue
			}
			walkQuery(kv.Val, v, protected, safeContext)
			walkQuery(kv.KeyQuery, v, protected, safeContext)
		}
	}
	if t.Array != nil {
		walkQuery(t.Array.Query, v, protected, safeContext)
	}

	// Reduce / Foreach / Label — descend into bodies (rare in outer
	// filters but covered for correctness).
	if t.Reduce != nil {
		walkQuery(t.Reduce.Query, v, protected, safeContext)
		walkQuery(t.Reduce.Start, v, protected, safeContext)
		walkQuery(t.Reduce.Update, v, protected, safeContext)
	}
	if t.Foreach != nil {
		walkQuery(t.Foreach.Query, v, protected, safeContext)
		walkQuery(t.Foreach.Start, v, protected, safeContext)
		walkQuery(t.Foreach.Update, v, protected, safeContext)
		walkQuery(t.Foreach.Extract, v, protected, safeContext)
	}
	if t.Label != nil {
		walkQuery(t.Label.Body, v, protected, safeContext)
	}

	// Function call args.
	if t.Func != nil {
		for _, arg := range t.Func.Args {
			walkQuery(arg, v, protected, false)
		}
	}

	// Unary expression.
	if t.Unary != nil {
		walkTerm(t.Unary.Term, v, protected, safeContext)
	}

	// String interpolation segments may contain queries.
	if t.Str != nil {
		for _, seg := range t.Str.Queries {
			walkQuery(seg, v, protected, safeContext)
		}
	}
}

// isAlternationProvidingArrayDefault returns true for queries shaped like
// `EXPR // []` or `EXPR // [...]`. In that case, when the query result is
// then iterated by an outer `[]` suffix, the iteration is safe because
// the `// []` default guarantees a (possibly empty) array.
func isAlternationProvidingArrayDefault(q *gojq.Query) bool {
	if q == nil || q.Op != gojq.OpAlt || q.Right == nil {
		return false
	}
	// Right side must be an array literal.
	if q.Right.Term == nil {
		return false
	}
	return q.Right.Term.Type == gojq.TermTypeArray
}

// arrayTypeGuardedFields scans a condition expression for the idiom
// `(.NAME | type) == "array"` and returns the set of NAMEs that the
// condition proves are arrays. Used to mark the `then` branch as safe
// for those fields.
func arrayTypeGuardedFields(cond *gojq.Query) map[string]struct{} {
	if cond == nil {
		return nil
	}
	// We use the canonical String() form of the AST for a robust string
	// match — gojq's printer normalizes whitespace which makes this
	// reliable across formatting variations of the same idiom.
	s := cond.String()
	out := map[string]struct{}{}
	// Patterns to recognize:
	//   (.NAME | type) == "array"
	//   (.NAME|type) == "array"
	for _, eqLit := range []string{`== "array"`, `=="array"`} {
		idx := 0
		for {
			at := strings.Index(s[idx:], eqLit)
			if at < 0 {
				break
			}
			pre := s[:idx+at]
			pre = strings.TrimRight(pre, " ")
			// pre should end with `| type)` or `|type)`.
			if strings.HasSuffix(pre, "| type)") || strings.HasSuffix(pre, "|type)") {
				cut := strings.LastIndex(pre, "(")
				if cut >= 0 {
					inner := pre[cut+1:]
					inner = strings.TrimSuffix(inner, "| type)")
					inner = strings.TrimSuffix(inner, "|type)")
					inner = strings.TrimSpace(inner)
					inner = strings.TrimSuffix(inner, "|")
					inner = strings.TrimSpace(inner)
					if strings.HasPrefix(inner, ".") {
						field := strings.TrimPrefix(inner, ".")
						if field != "" && !strings.ContainsAny(field, "[].|()") {
							out[field] = struct{}{}
						}
					}
				}
			}
			idx += at + len(eqLit)
		}
	}
	return out
}

// withSafeFields returns a derived protected-set with the given safeFields
// REMOVED — i.e. they are no longer considered protected for the duration
// of the visit. Used to express "inside this branch, .NAME is known to be
// an array, so .NAME[] is locally safe."
func withSafeFields(protected, safeFields map[string]struct{}) map[string]struct{} {
	if len(safeFields) == 0 {
		return protected
	}
	out := make(map[string]struct{}, len(protected))
	for k := range protected {
		if _, isSafe := safeFields[k]; !isSafe {
			out[k] = struct{}{}
		}
	}
	return out
}
