package bls

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func entryRoot(t *testing.T, content, grub string) (root, entry string) {
	t.Helper()
	root = t.TempDir()
	dir := filepath.Join(root, EntriesPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry = filepath.Join(dir, "fedora-test.conf")
	if err := os.WriteFile(entry, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	seedGrub(t, root, grub)
	return root, entry
}

// grubSeeds are the /etc/default/grub shapes worth pairing with every entry: the
// ordinary one, the append the parser is allowed to make, and the forms it must
// refuse rather than append past.
var grubSeeds = []string{
	fedoraGrub,
	"GRUB_TIMEOUT=5\n",
	"GRUB_CMDLINE_LINUX_DEFAULT=\"quiet\"\n",
	"export GRUB_CMDLINE_LINUX=\"rd.luks.uuid=luks-abc rhgb quiet\"\n",
	"GRUB_CMDLINE_LINUX+=\" rhgb quiet\"\n",
	"GRUB_CMDLINE_LINUX=\"rhgb quiet\" # annotated\n",
	"GRUB_CMDLINE_LINUX=\"acpi_osi=\\\"Windows 2009\\\"\"\n",
}

func FuzzWanted(f *testing.F) {
	for _, grub := range grubSeeds {
		f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n", grub)
	}
	f.Add("options\n", fedoraGrub)
	f.Add("options   \t  \n", fedoraGrub)
	f.Add("title only, no options line\n", fedoraGrub)
	f.Add("options $kernelopts\n", fedoraGrub)
	f.Add("Options CapitalizedKeyIsNotAnOptionsLine\n", fedoraGrub)
	f.Add("options a\noptions b\n", fedoraGrub)

	f.Fuzz(func(t *testing.T, content, grub string) {
		root, _ := entryRoot(t, content, grub)
		ts, err := targets(root)
		if err != nil {
			return
		}
		for _, target := range ts {
			for _, tok := range target.tokens {
				if tok == "" {
					t.Fatalf("empty token from %q", content)
				}
				if strings.ContainsAny(tok, " \t\n") {
					t.Fatalf("token %q contains whitespace", tok)
				}
			}
		}
		const args = "intel_iommu=on iommu=pt"
		w, err := Wanted(root, args)
		if err != nil {
			t.Fatalf("Wanted after targets succeeded: %v", err)
		}
		// MissingIn drives undo: a key naming no target removes nothing, and a
		// token that was never an arg removes something apply never added.
		for rel, missing := range w.MissingIn {
			if !slices.ContainsFunc(ts, func(target target) bool { return target.rel == rel }) {
				t.Fatalf("MissingIn names %q, which is not a target", rel)
			}
			for _, tok := range strings.Fields(missing) {
				if !slices.Contains(strings.Fields(args), tok) {
					t.Fatalf("MissingIn[%q] holds %q, which is not one of the args", rel, tok)
				}
			}
		}
	})
}

// canonicalGrub is renderGrub with an identity token list — the form an edit
// leaves behind, which is what a round trip lands on rather than the original
// bytes. The value is stored as its fields joined by single spaces, so whitespace
// inside it does not survive; and a file that carried no assignment gains one the
// undo empties rather than deletes, since deleting would take out the line on a
// host that shipped an empty one.
func canonicalGrub(content, args string) string {
	lines := strings.Split(content, "\n")
	idx, value, quote, err := grubAssign(lines)
	if err != nil {
		return content
	}
	toks := strings.Fields(value)
	added := absent(strings.Fields(args), toks)
	if idx < 0 {
		if len(added) == 0 {
			return content // nothing to add, so no assignment was ever authored
		}
		return strings.Join(appendLine(lines, grubCmdlineKey+`=""`), "\n")
	}
	// A bare value gains quotes the moment it holds two tokens, and the removal
	// has no way to know it was bare before.
	if quote == 0 && len(toks)+len(added) > 1 {
		quote = '"'
	}
	out, err := renderGrub(lines, idx, quote, toks)
	if err != nil {
		return content
	}
	return string(out)
}

// canonicalOptions is editEntry's normalization with an identity transform. An
// edit normalizes whitespace and collapses the options lines, so it is the
// canonical form — not the original bytes — a round trip must land back on.
func canonicalOptions(content string) string {
	lines := strings.Split(content, "\n")
	toks, first := parseOptions(lines)
	if first < 0 {
		return content
	}
	for i := first + 1; i < len(lines); i++ {
		if _, ok := cutKey(lines[i], "options"); ok {
			lines[i] = ""
		}
	}
	lines[first] = strings.TrimSpace("options " + strings.Join(toks, " "))
	return strings.Join(lines, "\n")
}

// FuzzAddRemoveArgsRoundTrip is the invariant behind undo restoring the boot
// configuration byte-identically, up to canonicalOptions. It pairs AddArgs with
// the MissingIn map exactly as apply and undo do, so the property holds for any
// starting state rather than only where no target already carried an arg.
func FuzzAddRemoveArgsRoundTrip(f *testing.F) {
	for _, grub := range grubSeeds {
		f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n", "intel_iommu=on iommu=pt", grub)
	}
	f.Add("options ro\n", "a", fedoraGrub)
	f.Add("options\n", "x=1", fedoraGrub)
	f.Add("title t\noptions ro quiet\ninitrd /x.img\n", "vfio-pci.ids=10de:2206", fedoraGrub)
	f.Add("options ro\n", "", fedoraGrub)
	f.Add("options ro\n", "   ", fedoraGrub)
	f.Add("options \n", "0", fedoraGrub)
	f.Add("options  ro   quiet \n", "z=1", fedoraGrub)
	f.Add("options\tro\tquiet\n", "z=1", fedoraGrub)
	// an entry that legitimately repeats a token
	f.Add("options 0 0\n", "00", fedoraGrub)
	// the spec's repeated options key: transforming each line on its own drops a
	// token only one of them carried
	f.Add("options \noptions \xcf", "\xcf", fedoraGrub)
	f.Add("options ro\noptions iommu=pt\n", "quiet", fedoraGrub)
	// a token one target carries and another does not: the case a single union
	// of removals gets wrong in one direction or the other
	f.Add("options ro quiet\n", "quiet", fedoraGrub)
	f.Add("options ro\n", "quiet", fedoraGrub)

	f.Fuzz(func(t *testing.T, content, args, grub string) {
		root, entry := entryRoot(t, content, grub)

		before, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		original, first := parseOptions(strings.Split(string(before), "\n"))
		if first < 0 {
			return // no options line: AddArgs is expected to fail, not round-trip
		}
		pre, err := Wanted(root, args)
		if err != nil {
			return // a target this build refuses to edit: AddArgs refuses too
		}

		if err := AddArgs(root, args); err != nil {
			return
		}
		mid, err := Wanted(root, args)
		if err != nil {
			t.Fatalf("Wanted after AddArgs: %v", err)
		}
		if len(mid.Missing) > 0 {
			t.Fatalf("AddArgs(%q) left %v off a target", args, mid.Missing)
		}

		if err := RemoveArgs(root, pre.MissingIn); err != nil {
			t.Fatalf("RemoveArgs(%v): %v", pre.MissingIn, err)
		}
		after, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		if want := canonicalOptions(string(before)); want != string(after) {
			t.Fatalf("add+remove of %q did not restore the entry:\nwant: %q\ngot:  %q",
				args, want, after)
		}
		// Compare against parseOptions, not Wanted: Wanted answers about a token
		// set across every target, so an entry that legitimately repeats a token
		// would not match it.
		post, postFirst := parseOptions(strings.Split(string(after), "\n"))
		if postFirst < 0 {
			t.Fatalf("add+remove of %q left no options line: %q", args, after)
		}
		if !slices.Equal(original, post) {
			t.Fatalf("add+remove of %q changed the token list: %v → %v", args, original, post)
		}
		if want, got := canonicalGrub(grub, args), readGrub(t, root); got != want {
			t.Fatalf("add+remove of %q did not restore %s:\nwant: %q\ngot:  %q",
				args, GrubDefaultsPath, want, got)
		}
	})
}
