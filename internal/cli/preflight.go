package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/preflight"
)

func cmdPreflight(cfg *Config, args []string, stdout, stderr io.Writer) int {
	if _, ok := parseFlags(cfg, args, stderr); !ok {
		return 2
	}
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		fmt.Fprintf(stderr, "orthogonals preflight: %v\n", err)
		return 1
	}
	checks := preflight.Analyze(res, preflight.GatherFacts(cfg.Root))
	overall := preflight.Overall(checks)

	if cfg.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		report := struct {
			Status preflight.Status  `json:"status"`
			Checks []preflight.Check `json:"checks"`
		}{overall, checks}
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "orthogonals preflight: encode: %v\n", err)
			return 1
		}
		return overall.ExitCode()
	}

	for _, c := range checks {
		fmt.Fprintf(stdout, "%-4s %s: %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Message)
		if c.Remedy != "" && c.Status != preflight.Pass {
			fmt.Fprintf(stdout, "     fix: %s\n", c.Remedy)
		}
	}
	fmt.Fprintf(stdout, "\npreflight: %s\n", strings.ToUpper(string(overall)))
	return overall.ExitCode()
}

func cmdBundle(cfg *Config, args []string, stdout, stderr io.Writer) int {
	rest, ok := parseFlags(cfg, args, stderr)
	if !ok {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orthogonals bundle: %v\n", err)
		return 1
	}
	out := "orthogonals-bundle.tar.gz"
	if len(rest) > 0 {
		out = rest[0]
	}
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return fail(err)
	}
	f, err := os.Create(out)
	if err != nil {
		return fail(err)
	}
	if err := preflight.WriteBundle(f, cfg.Root, res); err != nil {
		_ = f.Close()
		_ = os.Remove(out) // no truncated bundle left behind to be attached to a bug report
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(out)
		return fail(err)
	}
	fmt.Fprintf(stdout, "wrote %s (hostname, serials, MACs, UUIDs, and guest credentials redacted)\n", out)
	return 0
}
