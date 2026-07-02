package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/stronautt/orthogonals/internal/orchestrate"
)

// cmdStatus is the lightweight health check (research §C5): exit 0 when the
// applied setup is intact, 1 when something (kernel update, driver update,
// manual change) has undone part of it.
func cmdStatus(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	checks := orchestrate.Status(cfg.Root)
	if cfg.JSON {
		if err := json.NewEncoder(stdout).Encode(checks); err != nil {
			fmt.Fprintf(stderr, "orthogonals status: %v\n", err)
			return 1
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
		return 1
	}
	return 0
}
