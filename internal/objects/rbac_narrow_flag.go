//go:build !rbac_narrow_baseline

// rbac_narrow_flag.go — Tag 0.30.101 falsifier build-mode flag.
//
// Default build: rbacNarrowBaseline=false. The GET-path RBAC-narrowing
// falsifier test (informer_serve_rbac_narrow_test.go) asserts Tag-0.30.101
// behaviour (a denied-namespace informer-served GET is NOT served — the
// caller falls through to the apiserver).
//
// The `-tags rbac_narrow_baseline` build flips this to true (see
// rbac_narrow_flag_baseline.go), making the same test assert the
// pre-fix over-exposure — the negative control proving the test catches
// the bug.
//
// This file carries NO production code; the constant is referenced only
// by _test.go files. It compiles into the non-test binary as a single
// unused const (zero cost, no init). Mirrors the `restactions/api`
// package's identically-named flag pair (Tag 0.30.100).

package objects

// rbacNarrowBaseline is false in the normal build. See the file header.
const rbacNarrowBaseline = false
