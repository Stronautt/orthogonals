package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newUndoCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var force, purge bool
	cmd := &cobra.Command{
		Use:   "undo",
		Short: "restore the host to its pre-apply state",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			e := newEngine(cfg, stdout, stderr)
			if err := e.Undo(force, purge, os.Stdin); err != nil {
				fmt.Fprintf(stderr, "orthogonals undo: %v\n", err)
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore files even if they changed after apply")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the VM disk image, ISO cache, state, and config")
	return cmd
}
