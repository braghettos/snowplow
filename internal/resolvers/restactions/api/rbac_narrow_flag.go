//go:build !rbac_narrow_baseline

// rbac_narrow_flag.go — Tag 0.30.100 falsifier build-mode flag.
//
// Default build: rbacNarrowBaseline=false. The RBAC-narrowing falsifier
// tests assert Tag-A behaviour (served LIST == authorized subset).
//
// The `-tags rbac_narrow_baseline` build flips this to true (see
// rbac_narrow_flag_baseline.go), making the same tests assert the
// pre-Tag-A over-exposure — the negative control proving the tests
// catch the bug.
//
// This file carries NO production code; the constant is referenced only
// by _test.go files. It compiles into the non-test binary as a single
// unused const (zero cost, no init).

package api

// rbacNarrowBaseline is false in the normal build. See the file header.
const rbacNarrowBaseline = false
