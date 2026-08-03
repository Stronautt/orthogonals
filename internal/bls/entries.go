package bls

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func entryTokens(root string) ([][]string, error) {
	files, err := entryFiles(root)
	if err != nil {
		return nil, err
	}
	sets := make([][]string, 0, len(files))
	for _, f := range files {
		toks, err := entryOptions(f)
		if err != nil {
			return nil, err
		}
		sets = append(sets, toks)
	}
	return sets, nil
}

func editEntries(root string, fn transform) error {
	files, err := entryFiles(root)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := editEntry(f, fn); err != nil {
			return err
		}
	}
	return nil
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

func entryOptions(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	toks, first := parseOptions(strings.Split(string(b), "\n"))
	if first < 0 {
		return nil, fmt.Errorf("%s has no options line", path)
	}
	return toks, kerneloptsGuard(path, toks)
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

// editEntry rewrites one entry's options through fn, collapsing the combined set
// onto the first options line and blanking any others.
//
// Collapsing is what makes the edit reversible. Transforming each line on its
// own instead writes an added token to every line that lacked it, and the
// removal then strips it from a line that had carried it all along — so
// add-then-remove silently drops an arg the host booted with.
func editEntry(path string, fn transform) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	toks, first := parseOptions(lines)
	if first < 0 {
		return fmt.Errorf("%s has no options line", path)
	}
	if err := kerneloptsGuard(path, toks); err != nil {
		return err
	}
	for i := first + 1; i < len(lines); i++ {
		if _, ok := cutKey(lines[i], "options"); ok {
			lines[i] = ""
		}
	}
	lines[first] = strings.TrimSpace("options " + strings.Join(fn(toks), " "))
	return writeIfChanged(path, b, []byte(strings.Join(lines, "\n")), info.Mode())
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
