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

// Snapshot renders a deterministic listing of every path under root: relative
// path, permission bits, kind, and — for regular files — a content hash, so a
// comparison catches content, mode, and type changes alike. WalkDir yields
// lexical order, so the result is stable without an explicit sort.
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
// match. Lines keep snapshot order so a failure reads top-down: "+" appeared,
// "-" vanished.
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
