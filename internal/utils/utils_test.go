package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withUmask makes a mode assertion meaningful: the developer's umask would
// otherwise decide whether a 0644 request lands as 0644.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func TestExistsSeparatesAbsentFromUnreadable(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if ok, err := Exists(present); !ok || err != nil {
		t.Errorf("present file: got (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := Exists(filepath.Join(dir, "absent")); ok || err != nil {
		t.Errorf("absent file: got (%v, %v), want (false, nil)", ok, err)
	}

	// The distinction this function exists for: a file under a directory that
	// cannot be searched is not the same answer as a file that is not there.
	if os.Geteuid() == 0 {
		t.Skip("root searches any directory, so no path is unreadable")
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	ok, err := Exists(filepath.Join(locked, "any"))
	if ok {
		t.Error("unreadable path reported as existing")
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("unreadable path: got err %v, want fs.ErrPermission", err)
	}
}

func TestWriteAtomicAppliesModeDespiteUmask(t *testing.T) {
	withUmask(t, 0o077)

	path := filepath.Join(t.TempDir(), "sub", "file.conf")
	if err := WriteAtomic(path, []byte("body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", st.Mode().Perm())
	}
	if b, _ := os.ReadFile(path); string(b) != "body\n" {
		t.Errorf("content = %q, want %q", b, "body\n")
	}
}

func TestWriteAtomicLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAtomic(filepath.Join(dir, "f"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(filepath.Join(dir, "f"), []byte("bb"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), TempPrefix) {
			t.Errorf("temp file %s survived the rename", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("got %d entries, want 1", len(entries))
	}
}

func TestWriteAtomicReportsAnUnwritableParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into any directory")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := WriteAtomic(filepath.Join(dir, "f"), []byte("x"), 0o600); err == nil {
		t.Error("want an error writing into a read-only directory, got nil")
	}
}

func TestReadTrimAndReadUint(t *testing.T) {
	dir := t.TempDir()
	num := filepath.Join(dir, "nr")
	if err := os.WriteFile(num, []byte(" 128\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := ReadTrim(num); got != "128" {
		t.Errorf("ReadTrim = %q, want %q", got, "128")
	}
	if got := ReadTrim(filepath.Join(dir, "absent")); got != "" {
		t.Errorf("ReadTrim of an absent file = %q, want %q", got, "")
	}
	if got, err := ReadUint(num); err != nil || got != 128 {
		t.Errorf("ReadUint = (%d, %v), want (128, nil)", got, err)
	}
	if _, err := ReadUint(filepath.Join(dir, "absent")); err == nil {
		t.Error("ReadUint of an absent file: want an error, got nil")
	}

	text := filepath.Join(dir, "text")
	if err := os.WriteFile(text, []byte("not a number\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadUint(text); err == nil {
		t.Error("ReadUint of a non-numeric file: want an error, got nil")
	}
}

func TestSHA256HexMatchesFileSHA256(t *testing.T) {
	content := []byte("looking glass\n")
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	want := SHA256Hex(content)
	got, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("FileSHA256 = %s, SHA256Hex = %s", got, want)
	}
	if _, err := FileSHA256(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("FileSHA256 of an absent file: want an error, got nil")
	}
}

func TestCopyFile(t *testing.T) {
	withUmask(t, 0o077)

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CopyFile(src, dst, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" {
		t.Errorf("content = %q, want %q", b, "payload")
	}
	st, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", st.Mode().Perm())
	}
	if err := CopyFile(filepath.Join(dir, "absent"), dst, 0o644); err == nil {
		t.Error("CopyFile from an absent source: want an error, got nil")
	}
}

// A SIGKILL between CreateTemp and Rename strands a temp file that no deferred
// cleanup can reach. The next write into that directory has to clear it, or a
// killed apply leaves litter in /etc that undo cannot account for.
func TestWriteAtomicSweepsAStrandedTemp(t *testing.T) {
	dir := t.TempDir()
	stranded := filepath.Join(dir, TempPrefix+"1234567")
	if err := os.WriteFile(stranded, []byte("half-written"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not ours to touch: same directory, different namespace.
	keep := filepath.Join(dir, ".other-tool-tmp")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := WriteAtomic(filepath.Join(dir, "target"), []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stranded); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stranded temp survived: stat = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("swept a file it does not own: %v", err)
	}
}

func TestSweepTempsToleratesAMissingDirectory(t *testing.T) {
	SweepTemps(filepath.Join(t.TempDir(), "absent")) // must not panic
}

func TestSyncDirRejectsAMissingDirectory(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("want an error syncing a missing directory, got nil")
	}
}

// The two hand-built XML consumers are virt.CreateVolumeQCow2, which
// interpolates the --disk path into pool and volume XML, and the domain and
// media templates, which interpolate the VM name, locale and guest password.
func TestXMLEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary path", "/var/lib/libvirt/images/win11.qcow2", "/var/lib/libvirt/images/win11.qcow2"},
		{"ampersand", "a&b", "a&amp;b"},
		{"angle brackets", "<pool>", "&lt;pool&gt;"},
		{"apostrophe", "it's", "it&#39;s"},
		{"double quote", `say "hi"`, "say &#34;hi&#34;"},
		{"empty", "", ""},
		// A replacer would pass these straight through, and XML 1.0 forbids
		// them: the document only fails once libvirt parses it.
		{"control byte", "a\x00b", "a�b"},
		{"newline", "a\nb", "a&#xA;b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := XMLEscape(tt.in); got != tt.want {
				t.Errorf("XMLEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
