// Package stepstest snapshots filesystem trees for the apply/undo
// byte-identity contract.
package stepstest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot lists every path under root with its permission bits, kind and — for
// regular files — a content hash, so a comparison catches content, mode, and
// type changes alike. WalkDir yields lexical order, so no sort is needed.
func Snapshot(root string) (string, error) {
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			fmt.Fprintf(&b, "%s dir %04o\n", rel, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "%s symlink -> %s\n", rel, target)
		case info.Mode().IsRegular():
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			fmt.Fprintf(&b, "%s file %04o %s\n", rel, info.Mode().Perm(), hex.EncodeToString(sum[:8]))
		default:
			fmt.Fprintf(&b, "%s other %v\n", rel, info.Mode().Type())
		}
		return nil
	})
	return b.String(), err
}

// Diff reports the lines present in only one of two snapshots, "" when they
// match: "+" appeared, "-" vanished.
func Diff(before, after string) string {
	beforeLines := strings.Split(strings.TrimRight(before, "\n"), "\n")
	afterLines := strings.Split(strings.TrimRight(after, "\n"), "\n")
	set := func(lines []string) map[string]bool {
		m := make(map[string]bool, len(lines))
		for _, l := range lines {
			if l != "" {
				m[l] = true
			}
		}
		return m
	}
	inBefore, inAfter := set(beforeLines), set(afterLines)
	var out strings.Builder
	for _, l := range afterLines {
		if l != "" && !inBefore[l] {
			fmt.Fprintf(&out, "+%s\n", l)
		}
	}
	for _, l := range beforeLines {
		if l != "" && !inAfter[l] {
			fmt.Fprintf(&out, "-%s\n", l)
		}
	}
	return out.String()
}

// rapidTB is the part of testing.TB that pgregory.net/rapid's T also provides.
// testing.TB cannot be named here: it carries an unexported method, so nothing
// outside package testing satisfies it and *rapid.T never will.
type rapidTB interface {
	Helper()
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// MustSnapshot is Snapshot for a test that has nothing to do but fail.
func MustSnapshot(t rapidTB, root string) string {
	t.Helper()
	s, err := Snapshot(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// RapidRoot is t.TempDir for a *rapid.T, which has no TempDir of its own,
// pre-seeded with dirs.
//
// Those base directories are load-bearing rather than tidiness: undo removes
// only the directories apply created, so a root missing the ones every real
// host already has reports apply's own MkdirAll as a leak.
func RapidRoot(t rapidTB, dirs ...string) string {
	t.Helper()
	root, err := os.MkdirTemp("", "orthogonals-prop")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return root
}
