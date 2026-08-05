// Package bls reads and edits the host's boot configuration: the Boot Loader
// Specification entries (/boot/loader/entries/*.conf) that boot today,
// /etc/kernel/cmdline, which kernel-install copies into the entry it generates
// for the next kernel, and /etc/default/grub, which grub2-mkconfig regenerates
// both of the others from. All three are targets of every operation: an arg
// written only to the derived two survives until the next regeneration and no
// longer.
package bls

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/utils"
)

const EntriesPath = "/boot/loader/entries"

// KernelCmdlinePath is what kernel-install reads when it writes a new entry's
// options line. An arg missing from it survives only until the next kernel
// update regenerates the entries (see /usr/lib/kernel/install.d/90-loaderentry.install).
const KernelCmdlinePath = "/etc/kernel/cmdline"

// target is one file whose kernel args this package manages. render returns its
// bytes carrying a new token list; it never mutates what it closed over, so a
// caller may render every target before committing any of them.
type target struct {
	// rel is the journal key: undo removes per target, so a record has to name
	// them in a form that survives a --root prefix changing.
	rel    string
	path   string
	was    []byte
	mode   fs.FileMode
	tokens []string
	render func([]string) ([]byte, error)
}

// Args is what the boot config already says about a wanted arg set.
type Args struct {
	// Missing is the union: a token any target lacks means apply has work left.
	Missing []string
	// MissingIn is per target, keyed by the path undo will find it at — exactly
	// what AddArgs will write there, and so exactly what undo may take back out.
	// Keeping it per target is what stops undo stripping a token from a file
	// that carried it before apply ran.
	//
	// Every target gets a key, empty value included: the keys are also the record
	// of which targets existed, and RemoveArgs tells a BLS entry that needed
	// nothing from one generated after apply by whether it is named here.
	MissingIn map[string]string
}

// Wanted reports the state of args across every boot-config target.
func Wanted(root, args string) (Args, error) {
	ts, err := targets(root)
	if err != nil {
		return Args{}, err
	}
	want := strings.Fields(args)
	a := Args{MissingIn: map[string]string{}}
	for _, t := range ts {
		a.MissingIn[t.rel] = strings.Join(absent(want, t.tokens), " ")
	}
	// Separately, so Missing keeps the caller's arg order rather than the order
	// the targets happen to be parsed in.
	for _, tok := range want {
		lacking := func(t target) bool { return !slices.Contains(t.tokens, tok) }
		if slices.ContainsFunc(ts, lacking) && !slices.Contains(a.Missing, tok) {
			a.Missing = append(a.Missing, tok)
		}
	}
	return a, nil
}

// CheckAccess reports why this process cannot read the host's boot config, nil
// when it can. It runs the same parse the edits do, so a nil result means an
// edit would get as far as writing.
//
// preflight calls it to tell a root-only /boot/loader/entries (0700 on Fedora,
// so an unprivileged preflight cannot judge the entries and says so) apart from
// a host that takes no kernel args at all. Both leave args unwritable; only the
// first is fixed by re-running as root.
func CheckAccess(root string) error {
	_, err := targets(root)
	return err
}

// AddArgs appends the tokens in args not already present to every target.
func AddArgs(root, args string) error {
	add := strings.Fields(args)
	return write(root, func(t target) []string {
		return append(slices.Clone(t.tokens), absent(add, t.tokens)...)
	})
}

// RemoveArgs deletes from each target only the tokens byPath names for it, so
// undo strips exactly what apply added to that file and leaves alone whatever it
// carried beforehand.
//
// A BLS entry absent from the map was generated after apply: kernel-install
// copied it from /etc/kernel/cmdline, so it carries that target's set. An entry
// in the map but gone from disk is skipped.
func RemoveArgs(root string, byPath map[string]string) error {
	return write(root, func(t target) []string {
		drop := strings.Fields(dropFrom(t.rel, byPath))
		return slices.DeleteFunc(slices.Clone(t.tokens),
			func(tok string) bool { return slices.Contains(drop, tok) })
	})
}

func dropFrom(rel string, byPath map[string]string) string {
	if args, ok := byPath[rel]; ok {
		return args
	}
	if filepath.Dir(rel) == EntriesPath {
		return byPath[KernelCmdlinePath]
	}
	return ""
}

// absent is the tokens of want that toks lacks, in want's order and without
// repeats: an arg named twice must not be added twice, nor journaled for a
// removal that would then take out a token the target already carried.
func absent(want, toks []string) []string {
	var out []string
	for _, tok := range want {
		if !slices.Contains(toks, tok) && !slices.Contains(out, tok) {
			out = append(out, tok)
		}
	}
	return out
}

// targets parses every managed file. Every refusal happens here, which is what
// lets write commit only after all of them have been understood.
func targets(root string) ([]target, error) {
	ts, err := entryTargets(root)
	if err != nil {
		return nil, err
	}
	// grub last: the other two are what the host boots from today, so they are
	// repaired first, and grub only decides what a regeneration produces.
	for _, load := range []func(string) (*target, error){cmdlineTarget, grubTarget} {
		t, err := load(root)
		if err != nil {
			return nil, err
		}
		if t != nil {
			ts = append(ts, *t)
		}
	}
	return ts, nil
}

// write renders every target before committing any, so a file that refuses to
// parse or render leaves the others exactly as it found them.
//
// The window it does not close is a commit that fails on I/O after an earlier
// one succeeded: the step's write-ahead record is dropped, so undo will not take
// those args back out. Recheck re-applies the step on the next run.
func write(root string, fn func(target) []string) error {
	ts, err := targets(root)
	if err != nil {
		return err
	}
	now := make([][]byte, len(ts))
	for i, t := range ts {
		if now[i], err = t.render(fn(t)); err != nil {
			return err
		}
	}
	for i, t := range ts {
		if err := writeIfChanged(t.path, t.was, now[i], t.mode); err != nil {
			return err
		}
	}
	return nil
}

// fileLines is a target's lines and mode, nil lines when the file is absent —
// each caller documents what absence means for its own target, and the three
// answers differ. Splitting on "\n" round-trips through a Join, so the lines are
// also the "was" bytes writeIfChanged compares against, taken before any render
// rewrites them.
func fileLines(path string) ([]string, fs.FileMode, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return strings.Split(string(b), "\n"), info.Mode(), nil
}

// appendLine puts a new line after the last non-blank one, so a file ending in a
// newline does not gain a blank line before it. It writes through lines' backing
// array, so callers pass a clone: a render closure's own lines are read again by
// every later render of that target.
func appendLine(lines []string, line string) []string {
	last := len(lines)
	for last > 0 && strings.TrimSpace(lines[last-1]) == "" {
		last--
	}
	return append(lines[:last], append([]string{line}, lines[last:]...)...)
}

// writeIfChanged skips the rewrite when the transform was a no-op: apply
// re-checks the boot config on every run, and /boot does not need the churn.
func writeIfChanged(path string, was, now []byte, mode fs.FileMode) error {
	if string(now) == string(was) {
		// Undoing a killed kernel-args edit lands here: the add never reached
		// the entry, so WriteAtomic's sweep never runs.
		utils.SweepTemps(filepath.Dir(path))
		return nil
	}
	return utils.WriteAtomic(path, now, mode)
}
