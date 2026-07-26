// Package utils holds filesystem and hashing helpers shared across packages.
// It imports nothing from this module, so any package may depend on it.
package utils

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Exists reports whether path exists. A stat error other than fs.ErrNotExist —
// most often a parent directory that is not searchable — is returned rather
// than folded into false: "cannot tell" is not "absent".
func Exists(path string) (bool, error) {
	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// ReadTrim reads a one-line file, returning "" for any error. Sized for sysfs
// and procfs, where an absent attribute and an empty one mean the same thing;
// callers that must tell them apart should use os.ReadFile.
func ReadTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ReadUint reads a file holding a single unsigned integer.
func ReadUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// LinkBase is the last element of a symlink's target, "" for any error. Sized
// for sysfs, where a link is how the kernel names a binding (a device's driver,
// its iommu_group) and an absent link means unbound.
func LinkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// IsTerminal reports whether w is a terminal, the usual test for whether a
// human is reading the output directly.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// SyncDir fsyncs a directory so a rename into it survives power loss. Without
// it a renamed file's data can reach disk while the directory entry naming it
// does not.
func SyncDir(dir string) error {
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

// WriteSync writes content to path and fsyncs it before returning. Not atomic:
// a crash mid-write leaves a torn file. Use WriteAtomic unless the target is a
// fresh path no reader knows about yet.
func WriteSync(path string, content []byte, mode fs.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	// OpenFile subtracts the umask from mode; Chmod does not.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// TempPrefix marks WriteAtomic's temporary files, so SweepTemps can tell them
// from anything else sharing the directory.
const TempPrefix = ".orthogonals-tmp-"

// SweepTemps deletes WriteAtomic temporaries stranded in dir by a process that
// died between creating one and renaming it into place: a deferred cleanup does
// not run on SIGKILL. Best-effort and silent — these are litter, never state.
//
// Callers that write into dir get this for free from WriteAtomic. It is exported
// for the paths that remove rather than write, where nothing else would look.
//
// ponytail: unconditional, so it assumes no second orthogonals process is
// mid-write in the same directory. Apply is sequential and single-process today;
// if that changes, encode the writer's pid in the name and skip live ones.
func SweepTemps(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), TempPrefix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// WriteAtomic writes content to path via a temp file in the same directory,
// renaming it into place and creating parent directories. Both the file and its
// directory are fsynced, so a reader sees either the old contents or the new
// ones across a power loss, never a partial write.
func WriteAtomic(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Before adding one of our own, so a temp a killed run left here goes even
	// when this write later fails.
	SweepTemps(dir)
	f, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	// CreateTemp opens 0600 regardless of mode, and OpenFile would subtract the
	// umask; Chmod does neither.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// CopyFile copies src to dst, truncating dst. Not fsynced.
func CopyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Chmod(mode); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// XMLEscape makes s safe as XML text or as a quoted attribute value. Backed by
// encoding/xml rather than a character replacer: control bytes and invalid
// UTF-8 are illegal in XML 1.0, and a replacer passes them through for the
// parser on the far side to reject.
func XMLEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// PowerShellEscape makes s safe inside a PowerShell single-quoted string. Only
// the quote needs doubling: single-quoted PowerShell does not interpolate, so
// $, backtick and newline are already literal.
func PowerShellEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }

// WriteJSON encodes v to w as indented JSON, the shared --json output form.
// The two-space indent is part of that contract: the golden files compare bytes.
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// SHA256Hex is the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileSHA256 is the lowercase hex SHA-256 of the file at path, streamed.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
