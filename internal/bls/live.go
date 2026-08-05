package bls

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// LiveCmdlinePath is what the running kernel booted with — a read-only fourth
// view of the boot config, never a target: it reports what the last boot
// produced, so writing to it is meaningless and reading it is the only way to
// tell "configured" from "in effect".
const LiveCmdlinePath = "/proc/cmdline"

// MissingLive is the tokens of args the running kernel did not boot with.
// Per-token, because /proc/cmdline carries them in whatever order and company
// the bootloader assembled — a substring test on the joined args answers about
// adjacency, not about the kernel.
func MissingLive(root, args string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(root, LiveCmdlinePath))
	if err != nil {
		return nil, fmt.Errorf("read kernel cmdline: %w", err)
	}
	live := strings.Fields(string(b))
	var missing []string
	for tok := range strings.FieldsSeq(args) {
		if !slices.Contains(live, tok) {
			missing = append(missing, tok)
		}
	}
	return missing, nil
}
