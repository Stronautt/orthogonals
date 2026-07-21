package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/preflight"
)

func newPreflightCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "preflight",
		Short: "check whether the host meets the requirements",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return runPreflight(cfg, stdout, stderr)
		},
	}
}

func runPreflight(cfg *Config, stdout, stderr io.Writer) error {
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		fmt.Fprintf(stderr, "orthogonals preflight: %v\n", err)
		return exitCode(1)
	}
	checks := preflight.Analyze(res, preflight.GatherFacts(cfg.Root))
	overall := preflight.Overall(checks)

	if cfg.JSON {
		report := struct {
			Status preflight.Status  `json:"status"`
			Checks []preflight.Check `json:"checks"`
		}{overall, checks}
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "orthogonals preflight: encode: %v\n", err)
			return exitCode(1)
		}
		return codeErr(overall.ExitCode())
	}

	for _, c := range checks {
		fmt.Fprintf(stdout, "%-4s %s: %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Message)
		if c.Remedy != "" && c.Status != preflight.Pass {
			fmt.Fprintf(stdout, "     fix: %s\n", c.Remedy)
		}
	}
	fmt.Fprintf(stdout, "\npreflight: %s\n", strings.ToUpper(string(overall)))
	return codeErr(overall.ExitCode())
}

func newBundleCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "bundle [output.tar.gz]",
		Short: "write a redacted diagnostics bundle",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			out := "orthogonals-bundle.tar.gz"
			if len(args) > 0 {
				out = args[0]
			}
			return finish(stderr, "bundle", runBundle(cfg, out, stdout))
		},
	}
}

func runBundle(cfg *Config, out string, stdout io.Writer) error {
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if err := preflight.WriteBundle(f, cfg.Root, res); err != nil {
		_ = f.Close()
		_ = os.Remove(out)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(out)
		return err
	}
	fmt.Fprintf(stdout, "wrote %s (hostname, serials, MACs, UUIDs, and guest credentials redacted)\n", out)
	return nil
}
