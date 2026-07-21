// Package bls reads and edits Boot Loader Specification entries (/boot/loader/entries/*.conf).
package bls

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// EntriesPath is the BLS entries directory.
const EntriesPath = "/boot/loader/entries"

// Dir is the entries directory under root.
func Dir(root string) string { return filepath.Join(root, EntriesPath) }

// Tokens is the union of options-line tokens across every entry, in first-seen order.
func Tokens(root string) ([]string, error) {
	files, err := entryFiles(root)
	if err != nil {
		return nil, err
	}
	var tokens []string
	seen := map[string]bool{}
	for _, f := range files {
		toks, err := entryOptions(f)
		if err != nil {
			return nil, err
		}
		for _, t := range toks {
			if !seen[t] {
				seen[t] = true
				tokens = append(tokens, t)
			}
		}
	}
	return tokens, nil
}

// AddArgs appends the tokens in args not already present to every entry's options line.
func AddArgs(root, args string) error {
	add := strings.Fields(args)
	return edit(root, func(cur []string) []string {
		for _, t := range add {
			if !slices.Contains(cur, t) {
				cur = append(cur, t)
			}
		}
		return cur
	})
}

// RemoveArgs deletes the exact tokens in args from every entry's options line.
func RemoveArgs(root, args string) error {
	drop := strings.Fields(args)
	return edit(root, func(cur []string) []string {
		return slices.DeleteFunc(cur, func(t string) bool { return slices.Contains(drop, t) })
	})
}

// entryFiles lists the *.conf entries under root, sorted.
func entryFiles(root string) ([]string, error) {
	dir := Dir(root)
	files, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no BLS entries in %s — not a Boot Loader Spec host (run `grub2-switch-to-blscfg` to convert a legacy grub config)", dir)
	}
	sort.Strings(files)
	return files, nil
}

// entryOptions returns the options-line tokens of one entry.
func entryOptions(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	toks, ok := optionsTokens(string(b))
	if !ok {
		return nil, fmt.Errorf("%s has no options line", path)
	}
	return toks, kerneloptsGuard(path, toks)
}

// edit maps every entry's options line through transform and writes it back.
func edit(root string, transform func([]string) []string) error {
	files, err := entryFiles(root)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := editOne(f, transform); err != nil {
			return err
		}
	}
	return nil
}

func editOne(path string, transform func([]string) []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, line := range lines {
		rest, ok := cutKey(line, "options")
		if !ok {
			continue
		}
		toks := strings.Fields(rest)
		if err := kerneloptsGuard(path, toks); err != nil {
			return err
		}
		lines[i] = strings.TrimSpace("options " + strings.Join(transform(toks), " "))
		found = true
	}
	if !found {
		return fmt.Errorf("%s has no options line", path)
	}
	return writeAtomic(path, []byte(strings.Join(lines, "\n")), info.Mode())
}

// optionsTokens returns the tokens of the first options line.
func optionsTokens(content string) ([]string, bool) {
	for line := range strings.SplitSeq(content, "\n") {
		if rest, ok := cutKey(line, "options"); ok {
			return strings.Fields(rest), true
		}
	}
	return nil, false
}

// cutKey splits a BLS "key value" line, returning the trimmed value.
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

func kerneloptsGuard(path string, toks []string) error {
	for _, t := range toks {
		if strings.Contains(t, "$kernelopts") {
			return fmt.Errorf("%s uses $kernelopts (grubenv indirection) — run `grub2-switch-to-blscfg` to expand kernel args into the entries", path)
		}
	}
	return nil
}

// writeAtomic writes content to path via a temp file and rename.
func writeAtomic(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".orthogonals-bls-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
