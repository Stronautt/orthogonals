package steps

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzManifestLoad asserts arbitrary journal bytes never panic and never yield
// a partially-populated manifest: undo replays this file from a fresh process,
// so a half-parsed journal would undo the wrong set of steps.
func FuzzManifestLoad(f *testing.F) {
	f.Add(`{"records":[]}`)
	f.Add(`{"records":[{"id":"a","kind":"write_file","path":"/etc/x","mode":420}]}`)
	f.Add(`{"records":[{"id":"a","kind":"bogus_kind"}]}`)
	f.Add(`{"records":null}`)
	f.Add(`{"records":[{"id":"","kind":"write_file"}]}`)
	f.Add(`{`)
	f.Add(``)
	f.Add(`[]`)
	f.Add(`{"records":[{"id":"a","kind":"op","op_args":{"a":"b"}}]}`)

	// made once for the whole run: a t.TempDir() per execution costs more I/O
	// than the parse under test, and stalls every worker on a loaded runner
	root := f.TempDir()
	path := ManifestPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		m, err := Load(root)
		if err != nil {
			if m != nil {
				t.Fatalf("Load returned a manifest alongside error %v", err)
			}
			return
		}
		if m == nil {
			t.Fatal("Load returned neither a manifest nor an error")
		}
		// Load only parses; Apply is what rejects malformed steps. What has to
		// hold is that a manifest Load accepts survives being written back: the
		// engine rewrites this file after every step, so a record that parses
		// but does not re-marshal loses its undo data for a mutation already on
		// disk. Re-reading the same bytes would prove only that reads are
		// deterministic, which they are by construction.
		if err := m.save(root); err != nil {
			t.Fatalf("save a manifest Load accepted: %v", err)
		}
		canonical, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Load(root)
		if err != nil {
			t.Fatalf("reload of a saved manifest failed: %v", err)
		}
		if err := again.save(root); err != nil {
			t.Fatal(err)
		}
		round, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		// Compared as written bytes, not with DeepEqual: a nil and an empty slice
		// undo identically but are not deeply equal, and the contract is about the
		// file, not the in-memory shape.
		if !bytes.Equal(canonical, round) {
			t.Fatalf("save∘load is not a fixed point:\n first:\n%s\n second:\n%s", canonical, round)
		}
	})
}
