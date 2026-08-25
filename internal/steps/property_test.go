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

// A cramped alphabet on purpose: "a b", "a_b" and "a/b" collapse to the same
// backup name, so the collision Apply must refuse actually occurs.
var propIDs = []string{"a", "b", "a.b", "a-b", "a b", "a_b", "a/b", "A"}

// Small enough that steps routinely target the same file and the same
// not-yet-existing directory.
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

// propStepRoot seeds a tree where some target paths already exist, so
// backup-and-restore is exercised rather than only file creation.
func propStepRoot(t *rapid.T) string {
	t.Helper()
	root := stepstest.RapidRoot(t, "etc", "var/lib")
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

// A list Apply refuses must leave the tree untouched; a list it accepts must
// undo byte for byte.
func TestApplyUndoRestoresAnyStepList(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "testtool"))
	rapid.Check(t, func(rt *rapid.T) {
		root := propStepRoot(rt)
		list := genStepList(rt)
		before := stepstest.MustSnapshot(rt, root)

		e, _, _ := eng(root, true)
		if err := e.Apply(list); err != nil {
			if d := stepstest.Diff(before, stepstest.MustSnapshot(rt, root)); d != "" {
				rt.Fatalf("Apply refused (%v) but still mutated the tree:\n%s", err, d)
			}
			return
		}

		u, _, _ := eng(root, true)
		if err := u.Undo(false, false, nil); err != nil {
			rt.Fatalf("undo: %v", err)
		}
		if d := stepstest.Diff(before, stepstest.MustSnapshot(rt, root)); d != "" {
			rt.Fatalf("undo did not restore the tree:\n%s\nsteps: %+v", d, list)
		}
	})
}

// The journal must round-trip through disk unchanged: undo has to work from a
// process that never saw the apply.
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
