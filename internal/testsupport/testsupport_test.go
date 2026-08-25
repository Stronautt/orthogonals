package testsupport

import (
	"go/build"
	"strings"
	"testing"
)

// TestImportsNothingFromThisModule keeps this package importable from the two
// places that cannot take a dependency. internal/utils is the package
// everything may depend on and its tests are in package utils, so one import
// of utils here closes a cycle; internal/hw/hwtest is linked into
// test/fixture's binary, so anything reached from here would ship in it.
//
// Both failures land far from the edit that causes them, so the rule is
// asserted where it has to hold rather than left to the package comment.
func TestImportsNothingFromThisModule(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range pkg.Imports {
		if strings.HasPrefix(imp, "github.com/stronautt/orthogonals/") {
			t.Errorf("imports %s: testsupport must stay stdlib-only", imp)
		}
	}
}
