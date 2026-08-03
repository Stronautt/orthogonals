// Package bls reads and edits the host's boot configuration: the Boot Loader
// Specification entries (/boot/loader/entries/*.conf) that boot today, and
// /etc/kernel/cmdline, which kernel-install copies into the entry it generates
// for the next kernel. Both are targets of every operation.
package bls

import (
	"io/fs"
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

type transform func([]string) []string

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

// CheckAccess reports why this process cannot read the host's boot config, nil
// when it can. It runs the same reads the edits do, so a nil result means an
// edit would get as far as writing.
//
// preflight calls it to tell a root-only /boot/loader/entries (0700 on Fedora,
// so an unprivileged preflight cannot judge the entries and says so) apart from
// a host that takes no kernel args at all. Both leave args unwritable; only the
// first is fixed by re-running as root.
func CheckAccess(root string) error {
	_, err := tokenSets(root)
	return err
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

// tokenSets is the options tokens of every target edit writes to, one set per
// target. A host with no /etc/kernel/cmdline contributes no set rather than an
// empty one, which would read as a target missing every arg.
func tokenSets(root string) ([][]string, error) {
	sets, err := entryTokens(root)
	if err != nil {
		return nil, err
	}
	toks, ok, err := cmdlineTokens(root)
	if err != nil {
		return nil, err
	}
	if ok {
		sets = append(sets, toks)
	}
	return sets, nil
}

func edit(root string, fn transform) error {
	if err := editEntries(root, fn); err != nil {
		return err
	}
	return editCmdline(root, fn)
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
