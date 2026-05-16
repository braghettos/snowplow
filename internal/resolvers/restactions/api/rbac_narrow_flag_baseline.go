//go:build rbac_narrow_baseline

// rbac_narrow_flag_baseline.go — Tag 0.30.100 falsifier negative-control
// build mode.
//
// Built only with `-tags rbac_narrow_baseline`. Flips rbacNarrowBaseline
// to true so the RBAC-narrowing falsifier tests assert the PRE-Tag-A
// over-exposure (served LIST == full unfiltered partition). Used to
// capture the negative control at the preflight gate: against current
// code these tests must PASS in baseline mode (the bug reproduced) and
// FAIL in default mode (the bug present). After Tag A lands the inverse
// holds.

package api

// rbacNarrowBaseline is true under -tags rbac_narrow_baseline.
const rbacNarrowBaseline = true
