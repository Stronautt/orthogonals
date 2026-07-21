package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

func newVerifyCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var vmName string
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "check the guest is up and the GPU is passed through",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			name, err := vmNameOrSole(cfg.Root, vmName)
			if err != nil {
				fmt.Fprintf(stderr, "orthogonals verify: %v\n", err)
				return exitCode(2)
			}
			c := virtClient()
			defer func() { _ = c.Close() }()
			if err := orchestrate.Verify(c, cfg.Root, name, stdout); err != nil {
				fmt.Fprintf(stderr, "orthogonals verify: %v\ncollect diagnostics with: orthogonals bundle orthogonals-diagnostics.tar.gz\n", err)
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vmName, "vm-name", "", "libvirt domain name (default: the sole managed VM)")
	return cmd
}
