package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/hooks"
)

// newHookCmd is the libvirt hook entry point, invoked by libvirtd.
func newHookCmd(cfg *Config, stderr io.Writer) *cobra.Command {
	var user string
	hook := &cobra.Command{
		Use:    "hook",
		Short:  "libvirt qemu hook entry point (internal)",
		Hidden: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if os.Getenv("PATH") == "" {
				_ = os.Setenv("PATH", "/usr/sbin:/usr/bin:/sbin:/bin")
			}
			return nil
		},
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(stderr, "usage: orthogonals hook qemu <vm> <op> <subop> | inhibit <vm>")
			return exitCode(2)
		},
	}
	hook.PersistentFlags().StringVar(&user, "user", "", "desktop user for failure notifications")
	hook.AddCommand(newHookQemuCmd(cfg, stderr, &user), newHookInhibitCmd(stderr))
	return hook
}

func newHookQemuCmd(cfg *Config, stderr io.Writer, user *string) *cobra.Command {
	return &cobra.Command{
		Use:   "qemu <vm> <op> <subop>",
		Short: "dispatch a libvirt qemu hook event",
		Args:  cobra.MinimumNArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			exe, err := executablePath()
			if err != nil {
				if exe, err = os.Executable(); err != nil {
					fmt.Fprintf(stderr, "orthogonals hook: %v\n", err)
					return exitCode(1)
				}
			}
			if err := hooks.Dispatch(cfg.Root, sysdClient(), args[0], args[1], args[2], *user, exe); err != nil {
				fmt.Fprintf(stderr, "orthogonals: %v\n", err)
				return exitCode(1)
			}
			return nil
		},
	}
}

func newHookInhibitCmd(stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "inhibit <vm>",
		Short: "hold a sleep inhibitor (the transient unit body)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := hooks.InhibitSleep(args[0]); err != nil {
				fmt.Fprintf(stderr, "orthogonals hook inhibit: %v\n", err)
				return exitCode(1)
			}
			return nil
		},
	}
}
