package cli

import (
	"fmt"
	"io"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

func cmdVerify(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	vmName := fs.String("vm-name", "", "libvirt domain name (default: the sole managed VM)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	name := *vmName
	if name == "" {
		var err error
		if name, err = soleVMName(cfg.Root); err != nil {
			fmt.Fprintf(stderr, "orthogonals verify: %v\n", err)
			return 2
		}
	}
	if err := orchestrate.Verify(cfg.Root, name, stdout); err != nil {
		fmt.Fprintf(stderr, "orthogonals verify: %v\ncollect diagnostics with: orthogonals bundle orthogonals-diagnostics.tar.gz\n", err)
		return 1
	}
	return 0
}
