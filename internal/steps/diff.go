package steps

import (
	"fmt"
	"io/fs"
	"strings"
)

// renderDiff shows the pending file change for dry-run output.
func renderDiff(path string, exists bool, old []byte, oldMode fs.FileMode, new []byte, newMode fs.FileMode) string {
	var b strings.Builder
	if !exists {
		fmt.Fprintf(&b, "--- /dev/null\n+++ %s (new, mode %04o)\n", path, newMode)
		for _, l := range splitLines(new) {
			fmt.Fprintf(&b, "+%s\n", l)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", path, path)
	if oldMode != newMode {
		fmt.Fprintf(&b, "mode %04o -> %04o\n", oldMode, newMode)
	}
	oldL, newL := splitLines(old), splitLines(new)
	p := 0
	for p < len(oldL) && p < len(newL) && oldL[p] == newL[p] {
		p++
	}
	s := 0
	for s < len(oldL)-p && s < len(newL)-p && oldL[len(oldL)-1-s] == newL[len(newL)-1-s] {
		s++
	}
	for _, l := range oldL[p : len(oldL)-s] {
		fmt.Fprintf(&b, "-%s\n", l)
	}
	for _, l := range newL[p : len(newL)-s] {
		fmt.Fprintf(&b, "+%s\n", l)
	}
	return b.String()
}

func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
