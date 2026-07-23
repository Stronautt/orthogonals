package steps

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// toolThatReadsManifest installs a fake binary that copies the manifest to
// witness, so a test can see what the journal held at the moment the command
// ran, and returns the dir to put on PATH.
func toolThatReadsManifest(t *testing.T, name, root, witness string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat " + ManifestPath(root) + " > " + witness + " 2>/dev/null\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The stub shells out to cat, so the real PATH has to stay reachable.
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// TestRunCmdIsJournaledBeforeItRuns pins the write-ahead ordering: the record
// must already be on disk when the command executes, or a process killed
// between the two leaves an unjournaled host mutation that undo cannot reverse.
func TestRunCmdIsJournaledBeforeItRuns(t *testing.T) {
	root := t.TempDir()
	witness := filepath.Join(t.TempDir(), "seen.json")
	t.Setenv("PATH", toolThatReadsManifest(t, "witnesstool", root, witness))

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{
		ID: "witness", Kind: KindRunCmd, Cmd: []string{"witnesstool"},
	}}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	seen, err := os.ReadFile(witness)
	if err != nil {
		t.Fatalf("the command never saw a manifest: %v", err)
	}
	if !strings.Contains(string(seen), `"witness"`) {
		t.Errorf("run_cmd ran before its record was journaled; manifest at exec time:\n%s", seen)
	}
}

// TestOpIsJournaledBeforeItRuns is the same contract for op steps, the kind
// that carries the kernel-args edit.
func TestOpIsJournaledBeforeItRuns(t *testing.T) {
	root := t.TempDir()
	seenDuringOp := ""
	ops["journal-probe"] = opEntry{fn: func(*OpClients, string, io.Writer, map[string]string) error {
		b, _ := os.ReadFile(ManifestPath(root))
		seenDuringOp = string(b)
		return nil
	}}
	t.Cleanup(func() { delete(ops, "journal-probe") })

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "probe", Kind: KindOp, Op: "journal-probe"}}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(seenDuringOp, `"probe"`) {
		t.Errorf("op ran before its record was journaled; manifest at op time:\n%s", seenDuringOp)
	}
}

// TestFailedStepLeavesNoRecord is the other half of write-ahead journaling: a
// step whose mutation failed must not stay in the manifest, or the next apply
// would report it "already applied" and never retry it.
func TestFailedStepLeavesNoRecord(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "failtool"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "doomed", Kind: KindRunCmd, Cmd: []string{"failtool"}}}); err == nil {
		t.Fatal("apply reported success for a command that exited 3")
	}

	m, err := Load(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Has("doomed") {
		t.Error("a failed run_cmd stayed in the manifest; the next apply would skip it as already applied")
	}
}
