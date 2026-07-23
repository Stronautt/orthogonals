package steps

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
)

// propIDs is a deliberately cramped id alphabet: "a b", "a_b", and "a/b" all
// collapse to the same backup name, so collisions — the case Apply must refuse
// rather than corrupt — actually occur.
var propIDs = []string{"a", "b", "a.b", "a-b", "a b", "a_b", "a/b", "A"}

// propPaths is small enough that steps routinely target the same file and the
// same not-yet-existing directory.
var propPaths = []string{
	"/etc/one.conf",
	"/etc/two.conf",
	"/etc/sub/three.conf",
	"/var/lib/deep/nest/four.conf",
}

func genStepList(t *rapid.T) []Step {
	n := rapid.IntRange(1, 6).Draw(t, "steps")
	list := make([]Step, 0, n)
	for i := range n {
		id := rapid.SampledFrom(propIDs).Draw(t, "id")
		switch rapid.SampledFrom([]Kind{KindWriteFile, KindRunCmd, KindEnableUnit}).Draw(t, "kind") {
		case KindWriteFile:
			list = append(list, Step{
				ID:      id,
				Kind:    KindWriteFile,
				Path:    rapid.SampledFrom(propPaths).Draw(t, "path"),
				Content: []byte(rapid.StringN(0, 32, -1).Draw(t, "content")),
				Mode:    rapid.SampledFrom([]fs.FileMode{0o644, 0o600, 0o755}).Draw(t, "mode"),
			})
		case KindRunCmd:
			list = append(list, Step{
				ID:      id,
				Kind:    KindRunCmd,
				Cmd:     []string{"testtool", "do", id},
				UndoCmd: []string{"testtool", "undo", id},
			})
		case KindEnableUnit:
			list = append(list, Step{
				ID:     id,
				Kind:   KindEnableUnit,
				Unit:   rapid.SampledFrom([]string{"alpha.service", "beta.service"}).Draw(t, "unit"),
				Enable: rapid.Bool().Draw(t, "enable"),
			})
		}
		_ = i
	}
	return list
}

// propStepRoot seeds a tree where some target paths already exist, so the
// backup-and-restore path is exercised rather than only file creation.
func propStepRoot(t *rapid.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "orthogonals-steps-prop")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// Base dirs every real host already has. Without them the manifest's own
	// MkdirAll would look like a leak undo failed to clean up.
	for _, d := range []string{"etc", "var/lib"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("%v", err)
		}
	}
	for _, p := range propPaths {
		if !rapid.Bool().Draw(t, "preexisting") {
			continue
		}
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("%v", err)
		}
		if err := os.WriteFile(full, []byte("original "+p), 0o640); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return root
}

func snap(t *rapid.T, root string) string {
	t.Helper()
	s, err := stepstest.Snapshot(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// TestApplyUndoRestoresAnyStepList reaches step combinations the host-config
// plan never produces. A list Apply refuses must leave the tree untouched; a
// list it accepts must undo byte for byte.
func TestApplyUndoRestoresAnyStepList(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "testtool"))
	rapid.Check(t, func(rt *rapid.T) {
		root := propStepRoot(rt)
		list := genStepList(rt)
		before := snap(rt, root)

		e, _, _ := eng(root, true)
		if err := e.Apply(list); err != nil {
			if d := stepstest.Diff(before, snap(rt, root)); d != "" {
				rt.Fatalf("Apply refused (%v) but still mutated the tree:\n%s", err, d)
			}
			return
		}

		u, _, _ := eng(root, true)
		if err := u.Undo(false, false, nil); err != nil {
			rt.Fatalf("undo: %v", err)
		}
		if d := stepstest.Diff(before, snap(rt, root)); d != "" {
			rt.Fatalf("undo did not restore the tree:\n%s\nsteps: %+v", d, list)
		}
	})
}

// TestManifestSurvivesAFreshProcess asserts the journal round-trips through
// disk unchanged: undo has to work from a process that never saw the apply.
func TestManifestSurvivesAFreshProcess(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "testtool"))
	rapid.Check(t, func(rt *rapid.T) {
		root := propStepRoot(rt)
		list := genStepList(rt)

		e, _, _ := eng(root, true)
		if err := e.Apply(list); err != nil {
			return // refusal is covered by the restore property
		}

		saved, err := Load(root)
		if err != nil {
			rt.Fatalf("load: %v", err)
		}
		reloaded, err := Load(root)
		if err != nil {
			rt.Fatalf("reload: %v", err)
		}
		if len(saved.Records) != len(reloaded.Records) {
			rt.Fatalf("record count changed across loads: %d vs %d",
				len(saved.Records), len(reloaded.Records))
		}
		for i := range saved.Records {
			if saved.Records[i].ID != reloaded.Records[i].ID {
				rt.Fatalf("record %d id changed across loads: %q vs %q",
					i, saved.Records[i].ID, reloaded.Records[i].ID)
			}
			if saved.Records[i].Kind != reloaded.Records[i].Kind {
				rt.Fatalf("record %d kind changed across loads", i)
			}
		}
	})
}
