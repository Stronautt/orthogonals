package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/steps"
)

func TestUndoCommand(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "etc/foo.conf")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := &steps.Engine{Root: root, Yes: true, Out: io.Discard, Err: io.Discard}
	err := e.Apply([]steps.Step{
		{ID: "foo", Kind: steps.KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
	})
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := run(t, "undo", "--root", root)
	if code != 0 || !strings.Contains(stdout, "would restore /etc/foo.conf") {
		t.Fatalf("dry-run undo: code=%d stdout=%q", code, stdout)
	}
	if b, _ := os.ReadFile(p); string(b) != "new\n" {
		t.Fatalf("dry-run undo modified the file: %q", b)
	}

	code, _, stderr := run(t, "undo", "--root", root, "--yes")
	if code != 0 {
		t.Fatalf("undo --yes: code=%d stderr=%q", code, stderr)
	}
	if b, _ := os.ReadFile(p); string(b) != "old\n" {
		t.Fatalf("undo did not restore the file: %q", b)
	}

	code, stdout, _ = run(t, "undo", "--root", root)
	if code != 0 || !strings.Contains(stdout, "nothing to undo") {
		t.Fatalf("empty undo: code=%d stdout=%q", code, stdout)
	}
}
