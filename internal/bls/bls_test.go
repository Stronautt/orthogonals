package bls

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func seedEntries(t *testing.T, root string, entries map[string]string) {
	t.Helper()
	dir := filepath.Join(root, EntriesPath)
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

// removeEverywhere strips the same tokens off every target. Production never
// does this — apply journals what it added per target — so it is only for the
// tests whose subject is the edit rather than the undo bookkeeping.
func removeEverywhere(t *testing.T, root, args string) error {
	t.Helper()
	ts, err := targets(root)
	if err != nil {
		return err
	}
	byPath := make(map[string]string, len(ts))
	for _, target := range ts {
		byPath[target.rel] = args
	}
	return RemoveArgs(root, byPath)
}

// Undo removes per target, so what each one lacks has to be tracked separately:
// a union would either strip a token from the entry that had it or leave
// everything apply wrote to the entry that did not.
func TestWantedReportsMissingPerTarget(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{
		"a.conf": "options root=UUID=abc ro quiet\n",
		"b.conf": "options root=UUID=abc ro splash\n",
	})
	got, err := Wanted(root, "ro quiet iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"quiet", "iommu=pt"}; !slices.Equal(got.Missing, want) {
		t.Errorf("Missing = %v, want %v", got.Missing, want)
	}
	want := map[string]string{
		EntriesPath + "/a.conf": "iommu=pt",
		EntriesPath + "/b.conf": "quiet iommu=pt",
	}
	if !maps.Equal(got.MissingIn, want) {
		t.Errorf("MissingIn = %v, want %v", got.MissingIn, want)
	}
}

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
	if err := os.Chmod(filepath.Join(root, EntriesPath), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, EntriesPath), 0o755) })
	if err := CheckAccess(root); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("Readable = %v, want a permission error", err)
	}
}

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
	if err := removeEverywhere(t, root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if want := "# managed by anaconda\nroot=UUID=abc ro\n"; readCmdline(t, root) != want {
		t.Errorf("cmdline after remove = %q, want %q", readCmdline(t, root), want)
	}
}

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

func TestNoOpEditLeavesEntryUntouched(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	path := filepath.Join(root, EntriesPath, "a.conf")
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
	after1, _ := os.ReadFile(filepath.Join(root, EntriesPath, "a.conf"))
	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	after2, _ := os.ReadFile(filepath.Join(root, EntriesPath, "a.conf"))
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
	before, _ := os.ReadFile(filepath.Join(root, EntriesPath, "a.conf"))

	const args = "intel_iommu=on iommu=pt"
	if err := AddArgs(root, args); err != nil {
		t.Fatal(err)
	}
	if err := removeEverywhere(t, root, args); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(root, EntriesPath, "a.conf"))
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
		b, _ := os.ReadFile(filepath.Join(root, EntriesPath, name))
		if !slices.Contains(optionsOf(string(b)), "iommu=pt") {
			t.Errorf("%s not edited: %s", name, b)
		}
	}
}

func TestOptionsLinesCombine(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options root=UUID=abc ro\noptions iommu=pt\n"})
	got, err := Wanted(root, "iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Missing) > 0 {
		t.Errorf("Missing = %v, want none", got.Missing)
	}
}

func TestAddArgsDoesNotDuplicateAcrossOptionsLines(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options ro\noptions iommu=pt\n"})
	path := filepath.Join(root, EntriesPath, "a.conf")

	if err := AddArgs(root, "intel_iommu=on"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	// The second options line is blanked, not deleted, so its newline remains.
	if want := "options ro iommu=pt intel_iommu=on\n\n"; string(b) != want {
		t.Errorf("entry = %q, want %q", b, want)
	}
	if want := []string{"ro", "iommu=pt", "intel_iommu=on"}; !slices.Equal(optionsOf(string(b)), want) {
		t.Errorf("options = %v, want %v", optionsOf(string(b)), want)
	}

	if err := removeEverywhere(t, root, "intel_iommu=on"); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if want := []string{"ro", "iommu=pt"}; !slices.Equal(optionsOf(string(b)), want) {
		t.Errorf("after remove options = %v, want %v", optionsOf(string(b)), want)
	}
}

func TestRemoveToleratesMissingToken(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options root=UUID=abc ro\n"})
	if err := removeEverywhere(t, root, "iommu=pt"); err != nil {
		t.Errorf("RemoveArgs on absent token: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		if err := CheckAccess(t.TempDir()); err == nil {
			t.Error("want error for missing entries dir")
		}
	})
	t.Run("kernelopts indirection", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "options $kernelopts rhgb\n"})
		if err := CheckAccess(root); err == nil {
			t.Error("want error for $kernelopts entry")
		}
		if err := AddArgs(root, "iommu=pt"); err == nil {
			t.Error("want AddArgs refusal for $kernelopts entry")
		}
	})
	t.Run("no options line", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "title x\nlinux /vmlinuz\n"})
		if err := CheckAccess(root); err == nil {
			t.Error("want error for entry without options line")
		}
	})
}

func optionsOf(content string) []string {
	toks, _ := parseOptions(strings.Split(content, "\n"))
	return toks
}
