package bls

// /etc/kernel/cmdline — the target the next kernel's entry is generated from.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// cmdlineTokens is /etc/kernel/cmdline's options tokens. ok is false when the
// host has no such file, which leaves kernel-install falling back to
// /proc/cmdline — already carrying the args once the host has booted with them.
func cmdlineTokens(root string) ([]string, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, KernelCmdlinePath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	toks, _ := parseCmdline(strings.Split(string(b), "\n"))
	return toks, true, nil
}

// editCmdline rewrites /etc/kernel/cmdline in place through fn, leaving comment
// lines alone and collapsing the args onto the first line they occupied.
// Absent file is not an error: AddArgs must not create one, since its presence
// is what makes kernel-install stop consulting /proc/cmdline.
func editCmdline(root string, fn transform) error {
	path := filepath.Join(root, KernelCmdlinePath)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	toks, first := parseCmdline(lines)
	if first < 0 {
		return nil
	}
	for i := first + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			lines[i] = ""
		}
	}
	lines[first] = strings.Join(fn(toks), " ")
	return writeIfChanged(path, b, []byte(strings.Join(lines, "\n")), info.Mode())
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
