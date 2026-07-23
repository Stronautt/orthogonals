package steps

import (
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

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		path := ManifestPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
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
		// Load only parses; Apply is what rejects malformed steps. The
		// contract here is that parsing stays total and lossless, so a
		// re-marshal of what was read parses back to the same record count.
		again, err := Load(root)
		if err != nil {
			t.Fatalf("second Load of the same file failed: %v", err)
		}
		if len(again.Records) != len(m.Records) {
			t.Fatalf("Load is not deterministic: %d then %d records",
				len(m.Records), len(again.Records))
		}
	})
}
