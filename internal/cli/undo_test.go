package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

// The ISO carries the password in cleartext and is not journaled, so undo must
// clear it or it outlives the whole install.
func TestUndoRemovesProvisionISO(t *testing.T) {
	root := t.TempDir()
	iso := media.ISOPath(root, "win11")
	if err := os.MkdirAll(filepath.Dir(iso), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("provision"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := &steps.Engine{Root: root, Yes: true, Out: io.Discard, Err: io.Discard}
	if err := e.Apply([]steps.Step{
		{ID: "foo", Kind: steps.KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
	}); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := run(t, "undo", "--root", root)
	if code != 0 || !strings.Contains(stdout, "would remove the provision ISO") {
		t.Fatalf("dry-run undo: code=%d stdout=%q", code, stdout)
	}
	if _, err := os.Stat(iso); err != nil {
		t.Fatalf("dry-run undo deleted the ISO: %v", err)
	}

	code, stdout, stderr := run(t, "undo", "--root", root, "--yes")
	if code != 0 {
		t.Fatalf("undo --yes: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Errorf("undo left the provision ISO on disk: %v", err)
	}
	if !strings.Contains(stdout, "removed the provision ISO") {
		t.Errorf("removal not reported: %q", stdout)
	}
}

// --step is what a refused apply asks for: reverse the one step that diverged
// and leave the rest of the manifest applied.
func TestUndoStepReversesOnlyThatStep(t *testing.T) {
	root := t.TempDir()
	e := &steps.Engine{Root: root, Yes: true, Out: io.Discard, Err: io.Discard}
	if err := e.Apply([]steps.Step{
		{ID: "keep", Kind: steps.KindWriteFile, Path: "/etc/keep.conf", Content: []byte("keep\n"), Mode: 0o600},
		{ID: "drop", Kind: steps.KindWriteFile, Path: "/etc/drop.conf", Content: []byte("drop\n"), Mode: 0o600},
	}); err != nil {
		t.Fatal(err)
	}

	if code, stdout, _ := run(t, "undo", "--root", root, "--step", "drop"); code != 0 ||
		!strings.Contains(stdout, "dry run") {
		t.Fatalf("dry-run undo --step: code=%d stdout=%q", code, stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/drop.conf")); err != nil {
		t.Fatalf("dry run removed the file: %v", err)
	}

	if code, _, stderr := run(t, "undo", "--root", root, "--step", "drop", "--yes"); code != 0 {
		t.Fatalf("undo --step: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/drop.conf")); !os.IsNotExist(err) {
		t.Errorf("undo --step left the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/keep.conf")); err != nil {
		t.Errorf("undo --step took the other step with it: %v", err)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("drop") || !m.Has("keep") {
		t.Error("the journal must lose the undone step and keep the rest")
	}

	code, _, stderr := run(t, "undo", "--root", root, "--step", "nosuch", "--yes")
	if code == 0 || !strings.Contains(stderr, "no journaled step") {
		t.Errorf("an unknown step id must fail loudly: code=%d stderr=%q", code, stderr)
	}
}

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
