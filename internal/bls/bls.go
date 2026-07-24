// Package bls reads and edits the host's boot configuration: the Boot Loader
// Specification entries (/boot/loader/entries/*.conf) that boot today, and
// /etc/kernel/cmdline, which kernel-install copies into the entry it generates
// for the next kernel.
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

// EntriesPath is the BLS entries directory.
const EntriesPath = "/boot/loader/entries"

// KernelCmdlinePath is what kernel-install reads when it writes a new entry's
// options line. An arg missing from it survives only until the next kernel
// update regenerates the entries (see /usr/lib/kernel/install.d/90-loaderentry.install).
const KernelCmdlinePath = "/etc/kernel/cmdline"

// Dir is the entries directory under root.
func Dir(root string) string { return filepath.Join(root, EntriesPath) }

// Args is what the boot config already says about a wanted arg set. The two
// halves answer different questions and a token can be in both: Present decides
// what apply may claim it added (and so may undo), Missing decides whether apply
// still has writing to do.
type Args struct {
	// Present is the wanted tokens at least one target already carries.
	Present []string
	// Missing is the wanted tokens at least one target lacks.
	Missing []string
}

// Wanted reports the state of args across every boot-config target.
func Wanted(root, args string) (Args, error) {
	sets, err := tokenSets(root)
	if err != nil {
		return Args{}, err
	}
	var a Args
	for _, t := range strings.Fields(args) {
		if slices.ContainsFunc(sets, func(s []string) bool { return slices.Contains(s, t) }) {
			a.Present = append(a.Present, t)
		}
		if slices.ContainsFunc(sets, func(s []string) bool { return !slices.Contains(s, t) }) {
			a.Missing = append(a.Missing, t)
		}
	}
	return a, nil
}

// Readable reports whether the host's boot config can be read and edited at
// all — a non-BLS or root-only /boot/loader/entries takes no kernel args.
func Readable(root string) error {
	_, err := tokenSets(root)
	return err
}

// tokenSets is the options tokens of every target edit writes to.
func tokenSets(root string) ([][]string, error) {
	files, err := entryFiles(root)
	if err != nil {
		return nil, err
	}
	var sets [][]string
	for _, f := range files {
		toks, err := entryOptions(f)
		if err != nil {
			return nil, err
		}
		sets = append(sets, toks)
	}
	toks, ok, err := kernelCmdline(root)
	if err != nil {
		return nil, err
	}
	if ok {
		sets = append(sets, toks)
	}
	return sets, nil
}

// AddArgs appends the tokens in args not already present to every entry's
// options line and to /etc/kernel/cmdline.
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

// RemoveArgs deletes the exact tokens in args from every entry's options line
// and from /etc/kernel/cmdline.
func RemoveArgs(root, args string) error {
	drop := strings.Fields(args)
	return edit(root, func(cur []string) []string {
		return slices.DeleteFunc(cur, func(t string) bool { return slices.Contains(drop, t) })
	})
}

// entryFiles lists the *.conf entries under root, sorted.
func entryFiles(root string) ([]string, error) {
	dir := Dir(root)
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

// edit maps every target's options through transform and writes it back: the
// entries that boot today, and /etc/kernel/cmdline so the next kernel's
// generated entry keeps the args.
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
	return editCmdline(root, transform)
}

// kernelCmdline reads /etc/kernel/cmdline's tokens; ok is false when the host
// has no such file, which leaves kernel-install falling back to /proc/cmdline —
// already carrying the args once the host has booted with them.
func kernelCmdline(root string) ([]string, bool, error) {
	b, err := os.ReadFile(filepath.Join(root, KernelCmdlinePath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	toks, _ := cmdlineTokens(strings.Split(string(b), "\n"))
	return toks, true, nil
}

// cmdlineTokens collects the tokens of every non-comment line and the index of
// the first of them; kernel-install joins those lines into one options line.
func cmdlineTokens(lines []string) ([]string, int) {
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

// editCmdline rewrites /etc/kernel/cmdline in place, leaving comment lines
// alone and collapsing the args onto the first line they occupied.
func editCmdline(root string, transform func([]string) []string) error {
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
	toks, first := cmdlineTokens(lines)
	if first < 0 {
		return nil
	}
	for i := first + 1; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "#") {
			lines[i] = ""
		}
	}
	lines[first] = strings.Join(transform(toks), " ")
	return writeIfChanged(path, b, []byte(strings.Join(lines, "\n")), info.Mode())
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
	return writeIfChanged(path, b, []byte(strings.Join(lines, "\n")), info.Mode())
}

// writeIfChanged skips the rewrite when transform was a no-op: apply re-checks
// the boot config on every run, and /boot does not need the churn.
func writeIfChanged(path string, was, now []byte, mode os.FileMode) error {
	if string(now) == string(was) {
		return nil
	}
	return writeAtomic(path, now, mode)
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir fsyncs a directory so a rename into it survives power loss — a boot
// entry whose data blocks never hit disk leaves the machine unbootable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
