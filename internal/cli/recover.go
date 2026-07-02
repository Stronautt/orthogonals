package cli

import (
	"fmt"
	"io"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

// cmdRecover is the escape hatch for a botched GPU handover: nvidia-smi
// failing after VM shutdown. Runtime repair only, so nothing is journaled.
func cmdRecover(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := orchestrate.Recover(cfg.Root, cfg.Yes, stdout); err != nil {
		fmt.Fprintf(stderr, "orthogonals recover: %v\n", err)
		return 1
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to recover")
	}
	return 0
}
