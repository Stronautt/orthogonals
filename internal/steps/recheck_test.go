package steps

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A step that changed kind between releases must name both sides: reading the
// record through the new kind's fields leaves the refusal with an empty "was:".
func TestKindChangeRefusalNamesTheJournaledStep(t *testing.T) {
	ops["kind-probe"] = opEntry{fn: func(*OpClients, string, io.Writer, map[string]string) error { return nil }}
	t.Cleanup(func() { delete(ops, "kind-probe") })

	root := t.TempDir()
	was := Step{ID: "shortcut", Kind: KindRunCmd, Cmd: []string{"true", "--old-way"},
		CreatesPath: "/etc/shortcut"}
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{was}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}

	now := Step{ID: "shortcut", Kind: KindOp, Op: "kind-probe", Args: map[string]string{"link": "/etc/shortcut"}}
	e2, _, _ := eng(root, true)
	err := e2.Apply([]Step{now})
	if err == nil {
		t.Fatal("a step that changed kind must be refused")
	}
	for _, want := range []string{"journaled as run_cmd, now op", "true --old-way", "kind-probe link=/etc/shortcut", "undo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%v", want, err)
		}
	}
}

// A journaled step whose effect the host can lose behind orthogonals' back runs
// again when the caller says so — the kernel-args case, where a kernel update
// regenerates the BLS entries and "already applied" would leave IOMMU off.
func TestRecheckReappliesJournaledStep(t *testing.T) {
	calls := 0
	ops["recheck-probe"] = opEntry{fn: func(*OpClients, string, io.Writer, map[string]string) error {
		calls++
		return nil
	}}
	t.Cleanup(func() { delete(ops, "recheck-probe") })

	root := t.TempDir()
	step := Step{ID: "probe", Kind: KindOp, Op: "recheck-probe"}

	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	e2, out, _ := eng(root, true)
	if err := e2.Apply([]Step{step}); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if calls != 1 || !strings.Contains(out.String(), "already applied") {
		t.Errorf("without Recheck the journaled step must be skipped (%d runs)\n%s", calls, out)
	}

	step.Recheck = true
	e3, out3, _ := eng(root, true)
	if err := e3.Apply([]Step{step}); err != nil {
		t.Fatalf("recheck apply: %v", err)
	}
	if calls != 2 {
		t.Errorf("Recheck did not re-run the step (%d runs)\n%s", calls, out3)
	}
	if !strings.Contains(out3.String(), "not live on the host") {
		t.Errorf("a reapply must say why:\n%s", out3)
	}
}
