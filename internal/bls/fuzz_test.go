package bls

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func entryRoot(t *testing.T, content string) (root, entry string) {
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
	return root, entry
}

func FuzzWanted(f *testing.F) {
	f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n")
	f.Add("options\n")
	f.Add("options   \t  \n")
	f.Add("title only, no options line\n")
	f.Add("options $kernelopts\n")
	f.Add("Options CapitalizedKeyIsNotAnOptionsLine\n")
	f.Add("options a\noptions b\n")

	f.Fuzz(func(t *testing.T, content string) {
		root, _ := entryRoot(t, content)
		sets, err := tokenSets(root)
		if err != nil {
			return
		}
		for _, toks := range sets {
			for _, tok := range toks {
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
			t.Fatalf("Wanted after tokenSets succeeded: %v", err)
		}
		for _, tok := range strings.Fields(args) {
			if !slices.Contains(w.Present, tok) && !slices.Contains(w.Missing, tok) {
				t.Fatalf("token %q is neither present nor missing", tok)
			}
		}
	})
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
// configuration byte-identically, up to canonicalOptions.
func FuzzAddRemoveArgsRoundTrip(f *testing.F) {
	f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n", "intel_iommu=on iommu=pt")
	f.Add("options ro\n", "a")
	f.Add("options\n", "x=1")
	f.Add("title t\noptions ro quiet\ninitrd /x.img\n", "vfio-pci.ids=10de:2206")
	f.Add("options ro\n", "")
	f.Add("options ro\n", "   ")
	f.Add("options \n", "0")
	f.Add("options  ro   quiet \n", "z=1")
	f.Add("options\tro\tquiet\n", "z=1")
	// an entry that legitimately repeats a token
	f.Add("options 0 0\n", "00")
	// the spec's repeated options key: transforming each line on its own drops a
	// token only one of them carried
	f.Add("options \noptions \xcf", "\xcf")
	f.Add("options ro\noptions iommu=pt\n", "quiet")

	f.Fuzz(func(t *testing.T, content, args string) {
		root, entry := entryRoot(t, content)

		before, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		original, first := parseOptions(strings.Split(string(before), "\n"))
		if first < 0 {
			return // no options line: AddArgs is expected to fail, not round-trip
		}
		// The round trip is only identity for tokens that were not already
		// there; RemoveArgs deletes by exact token and cannot know who added
		// it. Production covers the overlap case via hostcfg.addedKargs.
		add := strings.Fields(args)
		if len(add) == 0 {
			return
		}
		for _, tok := range add {
			if slices.Contains(original, tok) {
				return
			}
		}

		if err := AddArgs(root, args); err != nil {
			return
		}
		mid, err := Wanted(root, args)
		if err != nil {
			t.Fatalf("Wanted after AddArgs: %v", err)
		}
		if len(mid.Missing) > 0 {
			t.Fatalf("AddArgs(%q) left %v off an entry", args, mid.Missing)
		}

		if err := RemoveArgs(root, args); err != nil {
			t.Fatalf("RemoveArgs(%q): %v", args, err)
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
	})
}
