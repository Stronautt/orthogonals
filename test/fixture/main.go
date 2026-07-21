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
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./test/fixture <dir> [reference|laptop|laptop-amd]")
		os.Exit(2)
	}
	kind := "reference"
	if len(os.Args) == 3 {
		kind = os.Args[2]
	}
	build := map[string]func(string) error{
		"reference":  hwtest.BuildReferenceRoot,
		"laptop":     hwtest.BuildLaptopRoot,
		"laptop-amd": hwtest.BuildLaptopAMDRoot,
	}[kind]
	if build == nil {
		fmt.Fprintf(os.Stderr, "fixture: unknown kind %q (reference|laptop|laptop-amd)\n", kind)
		os.Exit(2)
	}
	if err := build(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		os.Exit(1)
	}
}
