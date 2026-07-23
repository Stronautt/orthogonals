package bls

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// entryRoot writes one BLS entry with the given file content and returns the root.
func entryRoot(t *testing.T, content string) (root, entry string) {
	t.Helper()
	root = t.TempDir()
	dir := Dir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	entry = filepath.Join(dir, "fedora-test.conf")
	if err := os.WriteFile(entry, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, entry
}

// FuzzTokens asserts reading arbitrary entry content never panics, and that a
// returned token list is free of duplicates and whitespace.
func FuzzTokens(f *testing.F) {
	f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n")
	f.Add("options\n")
	f.Add("options   \t  \n")
	f.Add("title only, no options line\n")
	f.Add("options $kernelopts\n")
	f.Add("Options CapitalizedKeyIsNotAnOptionsLine\n")
	f.Add("options a\noptions b\n")

	f.Fuzz(func(t *testing.T, content string) {
		root, _ := entryRoot(t, content)
		toks, err := Tokens(root)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, tok := range toks {
			if tok == "" {
				t.Fatalf("empty token from %q", content)
			}
			if strings.ContainsAny(tok, " \t\n") {
				t.Fatalf("token %q contains whitespace", tok)
			}
			if seen[tok] {
				t.Fatalf("duplicate token %q", tok)
			}
			seen[tok] = true
		}
	})
}

// canonicalOptions rewrites the options line the way editOne does: a single
// space between tokens and no trailing space. Editing an entry normalizes that
// line's whitespace, so it is the canonical form — not the original bytes —
// that a round trip must land back on.
func canonicalOptions(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		rest, ok := cutKey(line, "options")
		if !ok {
			continue
		}
		lines[i] = strings.TrimSpace("options " + strings.Join(strings.Fields(rest), " "))
	}
	return strings.Join(lines, "\n")
}

// FuzzAddRemoveArgsRoundTrip is the invariant behind undo restoring the boot
// configuration: adding kernel args that were not already present and then
// removing them must leave the entry exactly as it was, up to the options-line
// whitespace normalization every edit applies.
func FuzzAddRemoveArgsRoundTrip(f *testing.F) {
	f.Add("title Fedora\noptions root=UUID=aaaa ro quiet\n", "intel_iommu=on iommu=pt")
	f.Add("options ro\n", "a")
	f.Add("options\n", "x=1")
	f.Add("title t\noptions ro quiet\ninitrd /x.img\n", "vfio-pci.ids=10de:2206")
	f.Add("options ro\n", "")
	f.Add("options ro\n", "   ")
	// whitespace forms the edit normalizes rather than preserves
	f.Add("options \n", "0")
	f.Add("options  ro   quiet \n", "z=1")
	f.Add("options\tro\tquiet\n", "z=1")
	// an entry that legitimately repeats a token
	f.Add("options 0 0\n", "00")

	f.Fuzz(func(t *testing.T, content, args string) {
		root, entry := entryRoot(t, content)

		before, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		original, ok := optionsTokens(string(before))
		if !ok {
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
		mid, err := Tokens(root)
		if err != nil {
			t.Fatalf("Tokens after AddArgs: %v", err)
		}
		for _, tok := range add {
			if !slices.Contains(mid, tok) {
				t.Fatalf("AddArgs(%q) did not add %q (got %v)", args, tok, mid)
			}
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
		// Compare against optionsTokens, not Tokens: Tokens returns the
		// deduplicated union across entries, so an entry that legitimately
		// repeats a token would not match it.
		post, ok := optionsTokens(string(after))
		if !ok {
			t.Fatalf("add+remove of %q left no options line: %q", args, after)
		}
		if !slices.Equal(original, post) {
			t.Fatalf("add+remove of %q changed the token list: %v → %v", args, original, post)
		}
	})
}
