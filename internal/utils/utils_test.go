package utils

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stronautt/orthogonals/internal/testsupport"
	"golang.org/x/sys/unix"
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

// A rewrite must not relabel the file it replaces. The temp file takes its
// parent directory's SELinux type transition, which for /etc/default is etc_t
// while /etc/default/grub is bootloader_etc_t — so without this a bootloader
// file is silently downgraded on every apply and undo never puts it back.
//
// security.selinux is LSM-mediated and an unprivileged test cannot set it, so
// the copy is exercised through a user.* attribute; test/tmt/reboot.sh checks
// the real label with matchpathcon on a host that has one.
func TestWriteAtomicCarriesTheSecurityLabel(t *testing.T) {
	const label = "system_u:object_r:bootloader_etc_t:s0"
	testsupport.Swap(t, &SecurityXattr, "user.orthogonals-test-label")

	path := filepath.Join(t.TempDir(), "grub")
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, SecurityXattr, []byte(label), 0); err != nil {
		t.Skipf("this filesystem refuses user xattrs: %v", err)
	}
	if err := WriteAtomic(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(label))
	n, err := unix.Lgetxattr(path, SecurityXattr, got)
	if err != nil {
		t.Fatalf("the rewritten file lost its label: %v", err)
	}
	if string(got[:n]) != label {
		t.Errorf("label = %q, want %q", got[:n], label)
	}
}

// A file being created has no label to carry, and a filesystem without xattrs
// has none to read: neither may fail the write.
func TestWriteAtomicWithNoLabelToCarry(t *testing.T) {
	testsupport.Swap(t, &SecurityXattr, "user.orthogonals-test-label")

	dir := t.TempDir()
	if err := WriteAtomic(filepath.Join(dir, "new"), []byte("x\n"), 0o644); err != nil {
		t.Errorf("creating a file: %v", err)
	}
	unlabelled := filepath.Join(dir, "unlabelled")
	if err := os.WriteFile(unlabelled, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(unlabelled, []byte("new\n"), 0o644); err != nil {
		t.Errorf("replacing an unlabelled file: %v", err)
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

func TestReadTrim(t *testing.T) {
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

// A SIGKILL between CreateTemp and Rename strands a temp file no deferred
// cleanup can reach, so the next write into that directory has to clear it.
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

// A staging directory can hold a secret, so the sweep must take its contents
// with it. media.BuildISO stages the provision files, which carry the guest
// password, under a name with this prefix.
func TestSweepTempsRemovesADirectoryWithItsContents(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, TempPrefix+"staging", "inner")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "secret"), []byte("password"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "other-tool-staging")
	if err := os.Mkdir(keep, 0o700); err != nil {
		t.Fatal(err)
	}

	SweepTemps(dir)

	if _, err := os.Stat(filepath.Join(dir, TempPrefix+"staging")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stranded staging directory survived: stat = %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("swept a directory it does not own: %v", err)
	}
}

func TestSyncDirRejectsAMissingDirectory(t *testing.T) {
	if err := SyncDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("want an error syncing a missing directory, got nil")
	}
}

// Consumers: virt.CreateVolumeQCow2 (the --disk path in pool and volume XML)
// and the domain and media templates (VM name, locale, guest password).
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
