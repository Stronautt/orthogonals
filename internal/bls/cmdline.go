package bls

import (
	"path/filepath"
	"slices"
	"strings"
)

// cmdlineTarget is /etc/kernel/cmdline. A host without one contributes no
// target, which leaves kernel-install falling back to /proc/cmdline — already
// carrying the args once the host has booted with them. AddArgs must not create
// one: its presence is what makes kernel-install stop consulting /proc/cmdline.
func cmdlineTarget(root string) (*target, error) {
	path := filepath.Join(root, KernelCmdlinePath)
	lines, mode, err := fileLines(path)
	if err != nil || lines == nil {
		return nil, err
	}
	toks, first := parseCmdline(lines)
	return &target{
		rel: KernelCmdlinePath, path: path, mode: mode,
		was:    []byte(strings.Join(lines, "\n")),
		tokens: toks,
		render: func(toks []string) ([]byte, error) { return renderCmdline(lines, first, toks), nil },
	}, nil
}

// renderCmdline leaves comment lines alone and collapses the args onto the first
// line they occupied. A file of nothing but comments has no such line and gains
// one: without it the target reports every arg missing forever, and apply
// rechecks a step that can never satisfy it.
func renderCmdline(lines []string, first int, toks []string) []byte {
	out := slices.Clone(lines)
	if first < 0 {
		// Nothing to say and no line to say it on: appending an empty one would
		// leave a line behind that undo could never take back off.
		if len(toks) == 0 {
			return []byte(strings.Join(out, "\n"))
		}
		return []byte(strings.Join(appendLine(out, strings.Join(toks, " ")), "\n"))
	}
	for i := first + 1; i < len(out); i++ {
		if t := strings.TrimSpace(out[i]); t != "" && !strings.HasPrefix(t, "#") {
			out[i] = ""
		}
	}
	out[first] = strings.Join(toks, " ")
	return []byte(strings.Join(out, "\n"))
}

// parseCmdline collects the tokens of every non-comment line and the index of
// the first of them; kernel-install joins those lines into one options line.
func parseCmdline(lines []string) ([]string, int) {
	var toks []string
	first := -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		toks = append(toks, strings.Fields(t)...)
		if first < 0 {
			first = i
		}
	}
	return toks, first
}
