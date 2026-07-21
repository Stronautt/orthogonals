package bls

import (
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

func TestTokensUnionFirstSeenOrder(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{
		"a.conf": "options root=UUID=abc ro quiet\n",
		"b.conf": "options root=UUID=abc ro splash\n",
	})
	got, err := Tokens(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root=UUID=abc", "ro", "quiet", "splash"}
	if !slices.Equal(got, want) {
		t.Errorf("Tokens = %v, want %v", got, want)
	}
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
	toks, _ := Tokens(root)
	for _, want := range []string{"intel_iommu=on", "iommu=pt", "root=UUID=abc"} {
		if !slices.Contains(toks, want) {
			t.Errorf("token %q missing after add: %v", want, toks)
		}
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
		if _, err := Tokens(t.TempDir()); err == nil {
			t.Error("want error for missing entries dir")
		}
	})
	t.Run("kernelopts indirection", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "options $kernelopts rhgb\n"})
		if _, err := Tokens(root); err == nil {
			t.Error("want error for $kernelopts entry")
		}
		if err := AddArgs(root, "iommu=pt"); err == nil {
			t.Error("want AddArgs refusal for $kernelopts entry")
		}
	})
	t.Run("no options line", func(t *testing.T) {
		root := t.TempDir()
		seedEntries(t, root, map[string]string{"a.conf": "title x\nlinux /vmlinuz\n"})
		if _, err := Tokens(root); err == nil {
			t.Error("want error for entry without options line")
		}
	})
}

// optionsOf pulls the options-line tokens out of a rendered entry for asserts.
func optionsOf(content string) []string {
	toks, _ := optionsTokens(content)
	return toks
}
