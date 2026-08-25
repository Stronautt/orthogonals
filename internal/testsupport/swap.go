package testsupport

import "testing"

// Swap sets *p to v for the test and restores it after.
//
// The func-var and timing seams this module tests through were each saved and
// restored by hand at 32 call sites, six of them behind wrapper functions that
// differed only in the variable they closed over. One of those hand-rolls kept
// eleven saved values in an []any and restored them by index.
func Swap[T any](t testing.TB, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}
