// Package cli dispatches orthogonals subcommands using only the stdlib flag package.
package cli

import (
	"flag"
	"fmt"
	"io"
)

// Version is the binary version, overridden at release time via
// -ldflags "-X github.com/stronautt/orthogonals/internal/cli.Version=v1.2.3".
var Version = "dev"

// Config carries the global flags shared by every subcommand.
type Config struct {
	JSON bool   // machine-readable output
	Yes  bool   // actually apply changes; dry-run is the default
	Root string // path prefix for /sys, /etc, /var — the test seam
}

type command func(cfg *Config, args []string, stdout, stderr io.Writer) int

// commands is the dispatch table; slice order is the usage output order.
// Filled in init: the subcommands reference usage(), which walks this slice,
// so a literal initializer would be an initialization cycle.
type namedCommand struct {
	name string
	fn   command
}

var commands []namedCommand

func init() {
	commands = []namedCommand{
		{"detect", cmdDetect},
		{"preflight", cmdPreflight},
		{"apply", cmdApply},
		{"undo", cmdUndo},
		{"vm", cmdVM},
		{"media", cmdMedia},
		{"verify", cmdVerify},
		{"up", cmdUp},
		{"status", cmdStatus},
		{"recover", cmdRecover},
		{"bundle", cmdBundle},
		{"version", cmdVersion},
	}
}

// Run parses arguments and dispatches to a subcommand. Global flags are
// accepted both before and after the subcommand name; each subcommand parses
// its own arguments (adding command-specific flags where it has them).
func Run(args []string, stdout, stderr io.Writer) int {
	cfg := &Config{}
	rest, ok := parseFlags(cfg, args, stderr)
	if !ok {
		return 2
	}
	if len(rest) == 0 {
		usage(stderr)
		return 2
	}
	name, cmdArgs := rest[0], rest[1:]
	for _, c := range commands {
		if c.name == name {
			return c.fn(cfg, cmdArgs, stdout, stderr)
		}
	}
	fmt.Fprintf(stderr, "orthogonals: unknown command %q\n", name)
	usage(stderr)
	return 2
}

// newFlagSet carries the global flags; subcommands add their own before Parse.
func newFlagSet(cfg *Config, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("orthogonals", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stderr) }
	fs.BoolVar(&cfg.JSON, "json", cfg.JSON, "machine-readable JSON output")
	fs.BoolVar(&cfg.Yes, "yes", cfg.Yes, "apply changes (default is dry-run)")
	fs.StringVar(&cfg.Root, "root", cfg.Root, "path prefix for /sys, /etc, /var (testing)")
	return fs
}

func parseFlags(cfg *Config, args []string, stderr io.Writer) ([]string, bool) {
	fs := newFlagSet(cfg, stderr)
	if err := fs.Parse(args); err != nil {
		return nil, false
	}
	return fs.Args(), true
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "Usage: orthogonals [flags] <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(w, "  %s\n", c.name)
	}
	fmt.Fprintf(w, "\nGlobal flags:\n  --json\tmachine-readable JSON output\n  --yes\tapply changes (default is dry-run)\n  --root\tpath prefix for /sys, /etc, /var (testing)\n")
}

func cmdVersion(_ *Config, _ []string, stdout, _ io.Writer) int {
	fmt.Fprintf(stdout, "orthogonals %s\n", Version)
	return 0
}
