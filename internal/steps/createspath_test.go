package steps

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// hostOnlyPath is a path that exists on any Linux host but never inside a
// synthetic --root tree. A step whose product is looked up without the prefix
// would find this one and conclude, wrongly, that it had already been applied.
const hostOnlyPath = "/etc/hostname"

// countingTool installs a stub that appends a line per invocation, and returns
// a PATH plus a func reporting how many times it ran.
func countingTool(t *testing.T, name string) (path string, runs func() int) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\necho ran >> " + log + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH"), func() int {
		b, err := os.ReadFile(log)
		if err != nil {
			return 0
		}
		n := 0
		for _, c := range b {
			if c == '\n' {
				n++
			}
		}
		return n
	}
}

// TestCreatesPathIsRootRelativeForRunCmd pins that a run_cmd step's product is
// looked for inside --root, not on the real filesystem.
func TestCreatesPathIsRootRelativeForRunCmd(t *testing.T) {
	if _, err := os.Stat(hostOnlyPath); err != nil {
		t.Skipf("%s absent here, so the confusion cannot be reproduced", hostOnlyPath)
	}
	path, runs := countingTool(t, "producer")
	t.Setenv("PATH", path)
	root := t.TempDir()
	step := Step{ID: "make-it", Kind: KindRunCmd, Cmd: []string{"producer"}, CreatesPath: hostOnlyPath}

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if runs() != 1 {
		t.Fatalf("command ran %d times on the first apply, want 1", runs())
	}

	// The product does not exist under root, so the step must run again.
	e2, out, _ := eng(root, true)
	if err := e2.Apply([]Step{step}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if runs() != 2 {
		t.Errorf("re-apply skipped the step: it found %s on the real filesystem instead of under --root\n%s",
			hostOnlyPath, out.String())
	}

	// With the product present under root, the step is genuinely done.
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hostOnlyPath), []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e3, _, _ := eng(root, true)
	if err := e3.Apply([]Step{step}); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if runs() != 2 {
		t.Errorf("step re-ran though its product exists under --root (%d runs)", runs())
	}
}

// TestCreatesPathIsRootRelativeForOp is the same contract for op steps, which
// is where the desktop shortcut relies on it.
func TestCreatesPathIsRootRelativeForOp(t *testing.T) {
	if _, err := os.Stat(hostOnlyPath); err != nil {
		t.Skipf("%s absent here, so the confusion cannot be reproduced", hostOnlyPath)
	}
	calls := 0
	ops["creates-probe"] = opEntry{fn: func(*OpClients, string, io.Writer, map[string]string) error {
		calls++
		return nil
	}}
	t.Cleanup(func() { delete(ops, "creates-probe") })

	root := t.TempDir()
	step := Step{ID: "probe", Kind: KindOp, Op: "creates-probe", CreatesPath: hostOnlyPath}

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	e2, out, _ := eng(root, true)
	if err := e2.Apply([]Step{step}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if calls != 2 {
		t.Errorf("re-apply skipped the op: it found %s on the real filesystem instead of under --root\n%s",
			hostOnlyPath, out.String())
	}

	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, hostOnlyPath), []byte("host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e3, _, _ := eng(root, true)
	if err := e3.Apply([]Step{step}); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if calls != 2 {
		t.Errorf("op re-ran though its product exists under --root (%d calls)", calls)
	}
}
