package cli

import (
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
			return finish(stderr, "undo", e.Undo(force, purge, os.Stdin))
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore files even if they changed after apply")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the VM disk image, ISO cache, state, and config")
	return cmd
}
