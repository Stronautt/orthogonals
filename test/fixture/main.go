// Command fixture emits the hwtest reference sysfs tree for the shell test
// harnesses (test/integration, test/vm), so the synthetic topology lives in
// exactly one place: internal/hw/hwtest.BuildReferenceRoot.
package main

import (
	"fmt"
	"os"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./test/fixture <dir>")
		os.Exit(2)
	}
	if err := hwtest.BuildReferenceRoot(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}
