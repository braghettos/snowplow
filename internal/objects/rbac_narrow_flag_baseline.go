//go:build rbac_narrow_baseline

// rbac_narrow_flag_baseline.go — Tag 0.30.101 falsifier negative-control
// build mode.
//
// Built only with `-tags rbac_narrow_baseline`. Flips rbacNarrowBaseline
// to true so the GET-path RBAC-narrowing falsifier test asserts the
// PRE-fix over-exposure (a denied-namespace informer-served GET IS
// served — the object reaches a narrow-RBAC user with no `get` grant).
// Used to capture the negative control at the preflight gate: against
// pre-fix code the test must PASS in baseline mode (the bug reproduced)
// and FAIL in default mode (the bug present). After Tag 0.30.101 lands
// the inverse holds. Mirrors the `restactions/api` package's identically-
// named negative-control flag (Tag 0.30.100).

package objects

// rbacNarrowBaseline is true under -tags rbac_narrow_baseline.
const rbacNarrowBaseline = true
