package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

// newRecoverCmd is the escape hatch for a botched GPU handover.
func newRecoverCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "recover",
		Short: "repair the GPU state after a botched handover",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s := sysdClient()
			defer func() { _ = s.Close() }()
			if err := orchestrate.Recover(cfg.Root, s, cfg.Yes, stdout); err != nil {
				fmt.Fprintf(stderr, "orthogonals recover: %v\n", err)
				return exitCode(1)
			}
			if !cfg.Yes {
				fmt.Fprintln(stdout, "dry run — re-run with --yes to recover")
			}
			return nil
		},
	}
}
