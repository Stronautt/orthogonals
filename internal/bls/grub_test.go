package bls

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fedoraGrub = `GRUB_TIMEOUT=5
GRUB_DISTRIBUTOR="$(sed 's, release .*$,,g' /etc/system-release)"
GRUB_DEFAULT=saved
GRUB_CMDLINE_LINUX="rhgb quiet"
GRUB_ENABLE_BLSCFG=true
`

func seedGrub(t *testing.T, root, content string) {
	t.Helper()
	path := filepath.Join(root, GrubDefaultsPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readGrub(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, GrubDefaultsPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func grubCmdline(t *testing.T, root string) string {
	t.Helper()
	for l := range strings.SplitSeq(readGrub(t, root), "\n") {
		if rest, ok := strings.CutPrefix(l, grubCmdlineKey+"="); ok {
			v, _, _ := unquoted(rest)
			return v
		}
	}
	t.Fatalf("no %s in %s", grubCmdlineKey, readGrub(t, root))
	return ""
}

// The whole point of the third target: an arg only in the derived files is
// dropped by the next regeneration, so it has to reach grub too.
func TestAddArgsReachesGrub(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, fedoraGrub)

	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if got := grubCmdline(t, root); got != "rhgb quiet intel_iommu=on iommu=pt" {
		t.Errorf("GRUB_CMDLINE_LINUX = %q", got)
	}
	if !strings.Contains(readGrub(t, root), `GRUB_DISTRIBUTOR="$(sed`) {
		t.Error("editor rewrote a line other than GRUB_CMDLINE_LINUX")
	}
}

// A regeneration that stripped the args leaves grub without them while the
// derived files may still agree — Missing is what makes apply recheck.
func TestWantedCountsGrub(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": "options root=UUID=abc ro iommu=pt\n"})
	seedGrub(t, root, "GRUB_CMDLINE_LINUX=\"rhgb quiet\"\n")

	got, err := Wanted(root, "iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Missing, []string{"iommu=pt"}) {
		t.Errorf("Missing = %v, want the token grub lacks", got.Missing)
	}
}

func TestGrubRoundTrip(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, fedoraGrub)

	if err := AddArgs(root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if err := removeEverywhere(t, root, "intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if got := readGrub(t, root); got != fedoraGrub {
		t.Errorf("round trip changed the file:\ngot  %q\nwant %q", got, fedoraGrub)
	}
}

func TestGrubAbsentFileIsNotATarget(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})

	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	// Creating it would hand grub2-mkconfig a cmdline containing only our args.
	if _, err := os.Stat(filepath.Join(root, GrubDefaultsPath)); err == nil {
		t.Error("AddArgs created /etc/default/grub")
	}
	got, err := Wanted(root, "iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Missing) != 0 {
		t.Errorf("Missing = %v, want none — an absent file is not a target", got.Missing)
	}
}

// A file without the key has a live regenerator supplying nothing, so it is a
// target that lacks every arg.
func TestGrubKeylessFileGainsTheAssignment(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, "GRUB_TIMEOUT=5\nGRUB_ENABLE_BLSCFG=true\n")

	got, err := Wanted(root, "iommu=pt")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Missing, []string{"iommu=pt"}) {
		t.Errorf("Missing = %v, want the arg", got.Missing)
	}
	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if got := grubCmdline(t, root); got != "iommu=pt" {
		t.Errorf("GRUB_CMDLINE_LINUX = %q", got)
	}
	if !strings.HasSuffix(readGrub(t, root), "GRUB_ENABLE_BLSCFG=true\nGRUB_CMDLINE_LINUX=\"iommu=pt\"\n") {
		t.Errorf("assignment not appended cleanly: %q", readGrub(t, root))
	}
	// Undo empties the assignment it authored rather than deleting the line:
	// deleting would take out the line on a host that shipped an empty one, and
	// grub2-mkconfig reads an empty assignment and an absent one the same way.
	if err := removeEverywhere(t, root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if want := "GRUB_TIMEOUT=5\nGRUB_ENABLE_BLSCFG=true\nGRUB_CMDLINE_LINUX=\"\"\n"; readGrub(t, root) != want {
		t.Errorf("after undo = %q, want %q", readGrub(t, root), want)
	}
}

// sh takes the last assignment, so that is the one that decides the boot.
func TestGrubLastAssignmentWins(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, "GRUB_CMDLINE_LINUX=\"first\"\n#GRUB_CMDLINE_LINUX=\"commented\"\nGRUB_CMDLINE_LINUX=\"second\"\n")

	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	want := "GRUB_CMDLINE_LINUX=\"first\"\n#GRUB_CMDLINE_LINUX=\"commented\"\nGRUB_CMDLINE_LINUX=\"second iommu=pt\"\n"
	if got := readGrub(t, root); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Every line this package will not model, and why. A refusal here is the whole
// safety argument: the alternative to naming the line is appending a fresh
// assignment past it, which wins under sh and takes the host's rd.luks.uuid=
// with it at the next regeneration.
func TestGrubRefusals(t *testing.T) {
	const luks = "rd.luks.uuid=luks-abc"
	tests := []struct {
		name string
		grub string
		line int
		why  string
	}{
		{"export", "GRUB_TIMEOUT=5\nexport " + grubCmdlineKey + `="` + luks + `"` + "\n", 2, whyForm},
		{"append operator", grubCmdlineKey + `+=" rhgb"` + "\n", 1, whyForm},
		{"unset", grubCmdlineKey + `="quiet"` + "\nunset " + grubCmdlineKey + "\n", 2, whyForm},
		{"reference", grubCmdlineKey + `="quiet"` + "\necho \"$" + grubCmdlineKey + "\"\n", 2, whyForm},
		{"spaced assignment", grubCmdlineKey + ` = "quiet"` + "\n", 1, whyForm},
		{"trailing comment", grubCmdlineKey + `="quiet" # my args` + "\n", 1, whyQuoting},
		{"escaped quote", grubCmdlineKey + `="acpi_osi=\"Windows 2009\""` + "\n", 1, whyQuoting},
		{"unbalanced quote", grubCmdlineKey + `="quiet` + "\n", 1, whyQuoting},
		{"expansion", grubCmdlineKey + `="quiet $EXTRA_ARGS"` + "\n", 1, whyExpansion},
		{"command substitution", grubCmdlineKey + "=\"quiet `id`\"\n", 1, whyExpansion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			seedEntries(t, root, map[string]string{"a.conf": entryA})
			seedGrub(t, root, tc.grub)
			entry := filepath.Join(root, EntriesPath, "a.conf")
			before, err := os.ReadFile(entry)
			if err != nil {
				t.Fatal(err)
			}

			var ge *GrubError
			if _, err := Wanted(root, "iommu=pt"); !errors.As(err, &ge) {
				t.Fatalf("Wanted = %v, want a *GrubError", err)
			}
			if ge.Line != tc.line || ge.Why != tc.why {
				t.Errorf("refused line %d for %q, want line %d for %q", ge.Line, ge.Why, tc.line, tc.why)
			}
			if err := AddArgs(root, "iommu=pt"); !errors.As(err, &ge) {
				t.Fatalf("AddArgs = %v, want a *GrubError", err)
			}
			// Every target is parsed before any is written, so the refusal
			// cannot land after the entries have already been edited.
			if now, _ := os.ReadFile(entry); string(now) != string(before) {
				t.Errorf("a refused grub file still let the entry be rewritten:\nwas %q\nnow %q", before, now)
			}
			if got := readGrub(t, root); got != tc.grub {
				t.Errorf("refused grub file was rewritten: %q", got)
			}
		})
	}
}

// GRUB_CMDLINE_LINUX_DEFAULT is additive and deliberately not a target. Reading
// it as an unmanageable mention of GRUB_CMDLINE_LINUX would refuse every host
// that carries one.
func TestGrubDefaultSuffixIsNotTheKey(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, grubCmdlineKey+"_DEFAULT=\"quiet\"\n"+grubCmdlineKey+"=\"rhgb\"\n")

	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	want := grubCmdlineKey + "_DEFAULT=\"quiet\"\n" + grubCmdlineKey + "=\"rhgb iommu=pt\"\n"
	if got := readGrub(t, root); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A file with no mention at all is the one state where appending is safe.
func TestGrubDefaultSuffixAloneStillGainsTheKey(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, grubCmdlineKey+"_DEFAULT=\"quiet\"\n")

	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	want := grubCmdlineKey + "_DEFAULT=\"quiet\"\n" + grubCmdlineKey + "=\"iommu=pt\"\n"
	if got := readGrub(t, root); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// An unquoted single-token value gains quotes once it holds two. Semantically
// identical under sh, so it is pinned rather than pretended away.
func TestGrubUnquotedValueGainsQuotes(t *testing.T) {
	root := t.TempDir()
	seedEntries(t, root, map[string]string{"a.conf": entryA})
	seedGrub(t, root, "GRUB_CMDLINE_LINUX=quiet\n")

	if err := AddArgs(root, "iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if got := readGrub(t, root); got != "GRUB_CMDLINE_LINUX=\"quiet iommu=pt\"\n" {
		t.Errorf("got %q", got)
	}
}
