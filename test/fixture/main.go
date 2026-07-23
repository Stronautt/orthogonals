// Command fixture writes a synthetic host tree for the tmt tests (test/tmt),
// so every topology lives in exactly one place: the hwtest.Roots registry.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func main() {
	kinds := strings.Join(hwtest.RootNames(), "|")
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintf(os.Stderr, "usage: go run ./test/fixture <dir> [%s]\n", kinds)
		os.Exit(2)
	}
	kind := "reference"
	if len(os.Args) == 3 {
		kind = os.Args[2]
	}
	build := hwtest.Roots[kind]
	if build == nil {
		fmt.Fprintf(os.Stderr, "fixture: unknown kind %q (%s)\n", kind, kinds)
		os.Exit(2)
	}
	if err := build(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}
