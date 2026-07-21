package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

// newStatusCmd reports whether the applied setup is still intact.
func newStatusCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "report whether the applied setup is still intact",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			checks := orchestrate.Status(cfg.Root)
			if cfg.JSON {
				if err := writeJSON(stdout, checks); err != nil {
					fmt.Fprintf(stderr, "orthogonals status: %v\n", err)
					return exitCode(1)
				}
			} else {
				for _, c := range checks {
					mark := "ok     "
					if !c.OK {
						mark = "PROBLEM"
					}
					line := mark + " " + c.Name
					if c.Detail != "" {
						line += " — " + c.Detail
					}
					fmt.Fprintln(stdout, line)
				}
			}
			if !orchestrate.Healthy(checks) {
				return exitCode(1)
			}
			return nil
		},
	}
}
