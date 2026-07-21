package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// exitCode is a RunE return carrying a specific process exit code.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// Run builds a fresh command tree and dispatches args.
func Run(args []string, stdout, stderr io.Writer) int {
	cfg := &Config{}
	root := newRootCmd(cfg, stdout, stderr)
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ec exitCode
	if errors.As(err, &ec) {
		return int(ec)
	}
	fmt.Fprintf(stderr, "orthogonals: %v\n", err)
	return 2
}

// newRootCmd assembles the command tree bound to cfg.
func newRootCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "orthogonals",
		Short:         "Turn a Linux desktop into a VM host with native GPU passthrough",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
			return exitCode(2)
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	pf := root.PersistentFlags()
	pf.BoolVar(&cfg.JSON, "json", false, "machine-readable JSON output")
	pf.BoolVar(&cfg.Yes, "yes", false, "apply changes (default is dry-run)")
	pf.StringVar(&cfg.Root, "root", "", "path prefix for /sys, /etc, /var (testing)")

	root.AddCommand(
		newDetectCmd(cfg, stdout, stderr),
		newPreflightCmd(cfg, stdout, stderr),
		newApplyCmd(cfg, stdout, stderr),
		newUndoCmd(cfg, stdout, stderr),
		newVMCmd(cfg, stdout, stderr),
		newMediaCmd(cfg, stdout, stderr),
		newVerifyCmd(cfg, stdout, stderr),
		newUpCmd(cfg, stdout, stderr),
		newStatusCmd(cfg, stdout, stderr),
		newRecoverCmd(cfg, stdout, stderr),
		newHookCmd(cfg, stderr),
		newBundleCmd(cfg, stdout, stderr),
		newVersionCmd(stdout),
	)
	return root
}

// finish maps a run* result to a RunE return.
func finish(stderr io.Writer, name string, err error) error {
	if err == nil {
		return nil
	}
	var ec exitCode
	if errors.As(err, &ec) {
		return ec
	}
	fmt.Fprintf(stderr, "orthogonals %s: %v\n", name, err)
	return exitCode(1)
}

// codeErr turns a computed exit code into a RunE return.
func codeErr(n int) error {
	if n == 0 {
		return nil
	}
	return exitCode(n)
}

// writeJSON encodes v as indented JSON to w, the shared --json output form.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newVersionCmd(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "print the orthogonals version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(stdout, "orthogonals %s\n", Version)
			return nil
		},
	}
}
