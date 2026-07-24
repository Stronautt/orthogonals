package bls

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// seedEntries writes named entries under root's BLS dir.
func seedEntries(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range entries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const entryA = `title Fedora Linux (6.15.0) 44
version 6.15.0
linux /vmlinuz-6.15.0
initrd /initramfs-6.15.0.img
options root=UUID=abc ro rhgb quiet
`

// a token one entry lacks is both present (do not undo it) and missing (write
// it there) — the two halves are separate questions.
func TestWantedSplitsPresentFromMissing(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{
		"a.conf": "options root=UUID=abc ro quiet\n",
		"b.conf": "options root=UUID=abc ro splash\n",
	})
	got, err := Wanted(root, "ro quiet iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ro", "quiet"}; !slices.Equal(got.Present, want) {
		t.Errorf("Present = %v, want %v", got.Present, want)
	}
	if want := []string{"quiet", "iommu=pt"}; !slices.Equal(got.Missing, want) {
		t.Errorf("Missing = %v, want %v", got.Missing, want)
	}
}

// /etc/kernel/cmdline counts as a target: an arg missing there is dropped by
// the next kernel update, whatever the entries say today.
func TestWantedCountsKernelCmdline(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options root=UUID=abc ro iommu=pt\n"})
	seedCmdline(t, root, "root=UUID=abc ro\n")
	got, err := Wanted(root, "iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Missing, "iommu=pt") {
		t.Errorf("Missing = %v, want iommu=pt — it is not in /etc/kernel/cmdline", got.Missing)
	}
}

// the entries dir being unreadable must not read as "not a BLS host".
func TestReadableSurfacesPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	if err := os.Chmod(Dir(root), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(Dir(root), 0o755) })
	if err := Readable(root); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Readable = %v, want a permission error", err)
	}
}

// AddArgs/RemoveArgs keep /etc/kernel/cmdline in sync, the file kernel-install
// copies into the entry it generates for the next kernel.
func TestArgsFollowKernelCmdline(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedCmdline(t, root, "# managed by anaconda\nroot=UUID=abc ro\n")

	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	got := readCmdline(t, root)
	if want := "# managed by anaconda\nroot=UUID=abc ro intel_iommu=on iommu=pt\n"; got != want {
		t.Errorf("cmdline after add = %q, want %q", got, want)
	}
	if err := RemoveArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if want := "# managed by anaconda\nroot=UUID=abc ro\n"; readCmdline(t, root) != want {
		t.Errorf("cmdline after remove = %q, want %q", readCmdline(t, root), want)
	}
}

// an absent /etc/kernel/cmdline is left absent: kernel-install then falls back
// to /proc/cmdline, which carries the args once the host has booted with them.
func TestAddArgsLeavesAbsentCmdlineAlone(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, KernelCmdlinePath)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat /etc/kernel/cmdline = %v, want not-exist", err)
	}
}

// a no-op edit must not rewrite the entry: apply rechecks on every run.
func TestNoOpEditLeavesEntryUntouched(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	path := filepath.Join(Dir(root), "a.conf")
	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a no-op AddArgs rewrote the entry")
	}
}

func seedCmdline(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, KernelCmdlinePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCmdline(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, KernelCmdlinePath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAddArgsIdempotent(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})

	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	after1, _ := os.ReadFile(filepath.Join(Dir(root), "a.conf"))
	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	after2, _ := os.ReadFile(filepath.Join(Dir(root), "a.conf"))
	if string(after1) != string(after2) {
		t.Errorf("AddArgs not idempotent:\n%s\nvs\n%s", after1, after2)
	}
	w, _ := Wanted(root, "intel_iommu=on iommu=pt root=UUID=abc")
	if len(w.Missing) > 0 {
		t.Errorf("tokens missing after add: %v", w.Missing)
	}
}

func TestAddRemoveRoundTrip(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	before, _ := os.ReadFile(filepath.Join(Dir(root), "a.conf"))

	const args = "intel_iommu=on iommu=pt"
	if err := AddArgs(root, args); err != nil {
		t.Fatal(err)
	}
	if err := RemoveArgs(root, args); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(Dir(root), "a.conf"))
	if string(before) != string(after) {
		t.Errorf("add→remove not byte-identical:\nbefore %q\nafter  %q", before, after)
	}
}

func TestEditAllEntries(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{
		"a.conf": "options root=UUID=abc ro\n",
		"b.conf": "options root=UUID=abc ro\n",
	})
	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.conf", "b.conf"} {
		b, _ := os.ReadFile(filepath.Join(Dir(root), name))
		if !slices.Contains(optionsOf(string(b)), "iommu=pt") {
			t.Errorf("%s not edited: %s", name, b)
		}
	}
}

func TestRemoveToleratesMissingToken(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options root=UUID=abc ro\n"})
	if err := RemoveArgs(root, "iommu=pt"); err != nil {
		t.Errorf("RemoveArgs on absent token: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		if err := Readable(t.TempDir()); err == nil {
			t.Error("want error for missing entries dir")
		}
	})
	t.Run("kernelopts indirection", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "options $kernelopts rhgb\n"})
		if err := Readable(root); err == nil {
			t.Error("want error for $kernelopts entry")
		}
		if err := AddArgs(root, "iommu=pt"); err == nil {
			t.Error("want AddArgs refusal for $kernelopts entry")
		}
	})
	t.Run("no options line", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "title x\nlinux /vmlinuz\n"})
		if err := Readable(root); err == nil {
			t.Error("want error for entry without options line")
		}
	})
}

// optionsOf pulls the options-line tokens out of a rendered entry for asserts.
func optionsOf(content string) []string {
	toks, _ := optionsTokens(content)
	return toks
}
