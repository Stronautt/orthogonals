package bls

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func entryTargets(root string) ([]target, error) {
	files, err := entryFiles(root)
	if err != nil {
		return nil, err
	}
	ts := make([]target, 0, len(files))
	for _, f := range files {
		t, err := entryTarget(f, filepath.Join(EntriesPath, filepath.Base(f)))
		if err != nil {
			return nil, err
		}
		ts = append(ts, *t)
	}
	return ts, nil
}

// entryTarget is the one target for which an absent file is an error: entryFiles
// has just listed it, so it went missing mid-read rather than naming a host that
// keeps none.
func entryTarget(path, rel string) (*target, error) {
	lines, mode, err := fileLines(path)
	if err != nil {
		return nil, err
	}
	if lines == nil {
		return nil, fmt.Errorf("%s disappeared while the boot entries were being read", path)
	}
	toks, first := parseOptions(lines)
	if first < 0 {
		return nil, fmt.Errorf("%s has no options line", path)
	}
	if err := kerneloptsGuard(path, toks); err != nil {
		return nil, err
	}
	return &target{
		rel: rel, path: path, mode: mode, was: []byte(strings.Join(lines, "\n")), tokens: toks,
		render: func(toks []string) ([]byte, error) { return renderEntry(lines, first, toks), nil },
	}, nil
}

func entryFiles(root string) ([]string, error) {
	dir := filepath.Join(root, EntriesPath)
	// os.ReadDir, not filepath.Glob: Glob reports no matches for a directory it
	// cannot read, so an unprivileged caller would be told this is not a BLS
	// host — /boot/loader/entries is 0700 root on Fedora.
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	var files []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no BLS entries in %s — not a Boot Loader Spec host (run `grub2-switch-to-blscfg` to convert a legacy grub config)", dir)
	}
	return files, nil
}

// parseOptions collects the tokens of every options line and the index of the
// first of them. The BLS spec lets the key repeat and combines the values, so an
// entry carries one option set however many lines spell it out — reading only
// the first line would report a token on the second as absent.
func parseOptions(lines []string) ([]string, int) {
	var toks []string
	first := -1
	for i, line := range lines {
		rest, ok := cutKey(line, "options")
		if !ok {
			continue
		}
		toks = append(toks, strings.Fields(rest)...)
		if first < 0 {
			first = i
		}
	}
	return toks, first
}

// renderEntry collapses the combined set onto the first options line and blanks
// any others.
//
// Collapsing is what makes the edit reversible. Transforming each line on its
// own instead writes an added token to every line that lacked it, and the
// removal then strips it from a line that had carried it all along — so
// add-then-remove silently drops an arg the host booted with.
func renderEntry(lines []string, first int, toks []string) []byte {
	out := slices.Clone(lines)
	for i := first + 1; i < len(out); i++ {
		if _, ok := cutKey(out[i], "options"); ok {
			out[i] = ""
		}
	}
	out[first] = strings.TrimSpace("options " + strings.Join(toks, " "))
	return []byte(strings.Join(out, "\n"))
}

// cutKey splits a BLS "key value" line. The separator must be real whitespace,
// so "optionsfoo" is not an options line.
func cutKey(line, key string) (string, bool) {
	if line == key {
		return "", true
	}
	rest, ok := strings.CutPrefix(line, key)
	if !ok || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// kerneloptsGuard refuses an entry whose args live in grubenv instead of the
// entry: editing the indirection here would change nothing the host boots with.
func kerneloptsGuard(path string, toks []string) error {
	for _, t := range toks {
		if strings.Contains(t, "$kernelopts") {
			return fmt.Errorf("%s uses $kernelopts (grubenv indirection) — run `grub2-switch-to-blscfg` to expand kernel args into the entries", path)
		}
	}
	return nil
}
