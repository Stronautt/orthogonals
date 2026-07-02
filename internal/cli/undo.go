package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/stronautt/orthogonals/internal/steps"
)

func cmdUndo(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	force := fs.Bool("force", false, "restore files even if they changed after apply")
	purge := fs.Bool("purge", false, "also remove the VM disk image, ISO cache, state, and config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	e := &steps.Engine{Root: cfg.Root, Yes: cfg.Yes, Out: stdout, Err: stderr}
	if err := e.Undo(*force, *purge, os.Stdin); err != nil {
		fmt.Fprintf(stderr, "orthogonals undo: %v\n", err)
		return 1
	}
	return 0
}
