package steps

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func eng(root string, yes bool) (*Engine, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	return &Engine{Root: root, Yes: yes, Out: &out, Err: &errBuf}, &out, &errBuf
}

func write(t *testing.T, root, rel, content string, mode fs.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func assertFile(t *testing.T, root, rel, want string, mode fs.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	if string(b) != want {
		t.Fatalf("%s content = %q, want %q", rel, b, want)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != mode {
		t.Fatalf("%s mode = %04o, want %04o", rel, st.Mode().Perm(), mode)
	}
}

// fakePath creates a dir for fake binaries and prepends it to PATH — the
// exec test seam: no mocking library, tiny scripts recording argv to files.
func fakePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeBin installs an executable stub that appends its argv to a log file,
// then runs extra shell (canned output, exit codes). Returns the log path.
func fakeBin(t *testing.T, dir, name, extra string) string {
	t.Helper()
	log := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\n" + extra + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func mustLoad(t *testing.T, root string) *Manifest {
	t.Helper()
	m, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWriteFileApplyRecordsAndUndoRestores(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, _, _ := eng(root, true)

	step := Step{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "new\n", 0o600)

	m := mustLoad(t, root)
	if len(m.Records) != 1 {
		t.Fatalf("manifest records = %d, want 1", len(m.Records))
	}
	r := m.Records[0]
	if !r.Existed || r.OrigMode != 0o644 || r.Backup == "" {
		t.Fatalf("record should carry original state, got %+v", r)
	}
	if r.NewSHA256 != sha256hex([]byte("new\n")) {
		t.Fatalf("record checksum = %q", r.NewSHA256)
	}
	backup, err := os.ReadFile(filepath.Join(backupDir(root), r.Backup))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old\n" {
		t.Fatalf("backup = %q, want original content", backup)
	}

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
	if _, err := os.Stat(ManifestPath(root)); !os.IsNotExist(err) {
		t.Fatalf("manifest should be removed after full undo, stat err = %v", err)
	}
	if _, err := os.Stat(backupDir(root)); !os.IsNotExist(err) {
		t.Fatalf("backup dir should be removed after full undo, stat err = %v", err)
	}
}

func TestWriteFileNewFileUndoRemovesCreatedDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, _, _ := eng(root, true)

	step := Step{ID: "udev-rule", Kind: KindWriteFile, Path: "/etc/udev/rules.d/61-mutter.rules", Content: []byte("rule\n"), Mode: 0o644}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/udev/rules.d/61-mutter.rules", "rule\n", 0o644)

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/udev")); !os.IsNotExist(err) {
		t.Fatalf("undo should remove directories apply created, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err != nil {
		t.Fatalf("undo must not remove pre-existing dirs: %v", err)
	}
}

func TestReapplyIdempotent(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, _, _ := eng(root, true)
	step := Step{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}

	for i := 0; i < 2; i++ {
		if err := e.Apply([]Step{step}); err != nil {
			t.Fatal(err)
		}
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 {
		t.Fatalf("re-apply duplicated manifest entries: %d records", len(m.Records))
	}

	// external drift: re-apply resyncs but keeps the ORIGINAL backup
	write(t, root, "etc/foo.conf", "drifted\n", 0o644)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "new\n", 0o600)
	m = mustLoad(t, root)
	if len(m.Records) != 1 {
		t.Fatalf("drift resync duplicated manifest entries: %d records", len(m.Records))
	}
	backup, err := os.ReadFile(filepath.Join(backupDir(root), m.Records[0].Backup))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "old\n" {
		t.Fatalf("backup overwritten on re-apply: %q", backup)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	dir := fakePath(t)
	grubbyLog := fakeBin(t, dir, "grubby", "")
	e, out, _ := eng(root, false) // dry-run: the default

	list := []Step{
		{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
		{ID: "args", Kind: KindRunCmd, Cmd: []string{"grubby", "--args=x"}, UndoCmd: []string{"grubby", "--remove-args=x"}},
		{ID: "unit", Kind: KindEnableUnit, Unit: "libvirt-guests.service", Enable: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
	if _, err := os.Stat(ManifestPath(root)); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create a manifest")
	}
	if lines := logLines(t, grubbyLog); lines != nil {
		t.Fatalf("dry-run executed a command: %v", lines)
	}
	for _, want := range []string{"-old", "+new", "would run: grubby --args=x", "would run: systemctl enable libvirt-guests.service"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
}

func TestUndoDriftSkipsUnlessForce(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, _, errBuf := eng(root, true)
	step := Step{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	write(t, root, "etc/foo.conf", "hand-edited\n", 0o600)

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "hand-edited\n", 0o600)
	if !strings.Contains(errBuf.String(), "changed since apply") {
		t.Fatalf("expected drift warning, got: %q", errBuf.String())
	}
	if m := mustLoad(t, root); len(m.Records) != 1 {
		t.Fatalf("skipped record must stay in manifest, got %d records", len(m.Records))
	}

	if err := e.Undo(true, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
	if m := mustLoad(t, root); len(m.Records) != 0 {
		t.Fatalf("forced undo should clear manifest, got %d records", len(m.Records))
	}
}

func TestWriteFileRestorecon(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	rcLog := fakeBin(t, dir, "restorecon", "")
	e, _, _ := eng(root, true)

	step := Step{ID: "shm", Kind: KindWriteFile, Path: "/etc/tmpfiles.d/lg.conf", Content: []byte("f /dev/shm/looking-glass\n"), Mode: 0o644, Restorecon: true}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, rcLog)
	if len(lines) != 1 || !strings.Contains(lines[0], filepath.Join(root, "etc/tmpfiles.d/lg.conf")) {
		t.Fatalf("restorecon not invoked on the written file: %v", lines)
	}
}

func TestRunCmdAppliesAndUndoes(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	grubbyLog := fakeBin(t, dir, "grubby", "")
	e, _, _ := eng(root, true)

	step := Step{
		ID: "kernel-args", Kind: KindRunCmd,
		Cmd:     []string{"grubby", "--update-kernel=ALL", "--args=intel_iommu=on"},
		UndoCmd: []string{"grubby", "--update-kernel=ALL", "--remove-args=intel_iommu=on"},
	}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, grubbyLog)
	if len(lines) != 1 || lines[0] != "--update-kernel=ALL --args=intel_iommu=on" {
		t.Fatalf("apply invocation = %v", lines)
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 || m.Records[0].UndoCmd[1] != "--update-kernel=ALL" {
		t.Fatalf("both commands must be journaled, got %+v", m.Records)
	}

	// re-apply: already journaled, must not run again
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	if lines = logLines(t, grubbyLog); len(lines) != 1 {
		t.Fatalf("re-apply re-ran the command: %v", lines)
	}

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	lines = logLines(t, grubbyLog)
	if len(lines) != 2 || lines[1] != "--update-kernel=ALL --remove-args=intel_iommu=on" {
		t.Fatalf("undo invocation = %v", lines)
	}
}

func TestPartialFailureLeavesManifestConsistent(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	dir := fakePath(t)
	fakeBin(t, dir, "boom", "exit 1")
	e, _, _ := eng(root, true)

	list := []Step{
		{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
		{ID: "explode", Kind: KindRunCmd, Cmd: []string{"boom"}},
	}
	err := e.Apply(list)
	if err == nil || !strings.Contains(err.Error(), "explode") {
		t.Fatalf("expected failure naming the step, got %v", err)
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 || m.Records[0].ID != "foo" {
		t.Fatalf("manifest must hold exactly the applied steps, got %+v", m.Records)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
}

func TestEnableUnitRestoresPriorState(t *testing.T) {
	tests := []struct {
		name     string
		enable   bool
		prior    string
		undoVerb string // "" = undo must leave the unit alone
	}{
		{"disable step, prior enabled", false, "enabled", "enable"},
		{"enable step, prior disabled", true, "disabled", "disable"},
		{"prior static is not restorable", true, "static", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := fakePath(t)
			sysLog := fakeBin(t, dir, "systemctl",
				"if [ \"$1\" = \"is-enabled\" ]; then echo "+tt.prior+"; fi")
			e, out, _ := eng(root, true)

			step := Step{ID: "unit", Kind: KindEnableUnit, Unit: "svc.service", Enable: tt.enable}
			if err := e.Apply([]Step{step}); err != nil {
				t.Fatal(err)
			}
			verb := "enable"
			if !tt.enable {
				verb = "disable"
			}
			lines := logLines(t, sysLog)
			if len(lines) != 2 || lines[0] != "is-enabled svc.service" || lines[1] != verb+" svc.service" {
				t.Fatalf("apply invocations = %v", lines)
			}
			if m := mustLoad(t, root); m.Records[0].PriorState != tt.prior {
				t.Fatalf("prior state = %q, want %q", m.Records[0].PriorState, tt.prior)
			}

			if err := e.Undo(false, false, strings.NewReader("")); err != nil {
				t.Fatal(err)
			}
			lines = logLines(t, sysLog)
			if tt.undoVerb == "" {
				if len(lines) != 2 {
					t.Fatalf("undo must not toggle a non-restorable unit: %v", lines)
				}
				if !strings.Contains(out.String(), "static") {
					t.Fatalf("undo should explain why it left the unit alone:\n%s", out.String())
				}
			} else if len(lines) != 3 || lines[2] != tt.undoVerb+" svc.service" {
				t.Fatalf("undo invocations = %v, want final %q", lines, tt.undoVerb+" svc.service")
			}
		})
	}
}

func TestEnableUnitReassertsDrift(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	// fake is-enabled reads its answer from a state file so the test can
	// simulate a driver update re-enabling the unit between apply runs
	state := filepath.Join(dir, "state")
	write(t, dir, "state", "enabled\n", 0o644)
	sysLog := fakeBin(t, dir, "systemctl",
		"if [ \"$1\" = \"is-enabled\" ]; then cat \""+state+"\"; fi")
	e, out, _ := eng(root, true)

	step := Step{ID: "unit", Kind: KindEnableUnit, Unit: "nvidia-persistenced.service", Enable: false}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}

	// unit drifted back to enabled (e.g. driver update preset): re-apply
	// must disable it again, without touching the journaled prior state
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	lines := logLines(t, sysLog)
	want := []string{
		"is-enabled nvidia-persistenced.service",
		"disable nvidia-persistenced.service",
		"is-enabled nvidia-persistenced.service",
		"disable nvidia-persistenced.service",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("invocations = %v, want %v", lines, want)
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 || m.Records[0].PriorState != "enabled" {
		t.Fatalf("re-assert must keep the original journal record, got %+v", m.Records)
	}

	// unit still disabled: re-apply is a no-op past the state query
	write(t, dir, "state", "disabled\n", 0o644)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	if lines := logLines(t, sysLog); len(lines) != 5 {
		t.Fatalf("no-op re-apply must only query state, got %v", lines)
	}
	if !strings.Contains(out.String(), "already applied") {
		t.Fatalf("no-op re-apply should say already applied:\n%s", out.String())
	}
}

func TestApplyValidation(t *testing.T) {
	e, _, _ := eng(t.TempDir(), true)
	tests := []struct {
		name string
		list []Step
	}{
		{"missing id", []Step{{Kind: KindRunCmd, Cmd: []string{"true"}}}},
		{"relative path", []Step{{ID: "x", Kind: KindWriteFile, Path: "etc/foo", Content: []byte("x"), Mode: 0o644}}},
		{"missing mode", []Step{{ID: "x", Kind: KindWriteFile, Path: "/etc/foo", Content: []byte("x")}}},
		{"empty cmd", []Step{{ID: "x", Kind: KindRunCmd}}},
		{"missing unit", []Step{{ID: "x", Kind: KindEnableUnit}}},
		{"unknown kind", []Step{{ID: "x", Kind: "frobnicate"}}},
		{"duplicate ids", []Step{
			{ID: "x", Kind: KindRunCmd, Cmd: []string{"true"}},
			{ID: "x", Kind: KindRunCmd, Cmd: []string{"true"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := e.Apply(tt.list); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestReapplyChangedContentKeepsOriginalBackup(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "original\n", 0o644)
	step := Step{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("v1\n"), Mode: 0o644}
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	// new template version: re-apply with different content
	step.Content = []byte("v2\n")
	e2, _, _ := eng(root, true)
	if err := e2.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 {
		t.Fatalf("re-apply must update the record in place, got %d records", len(m.Records))
	}
	assertFile(t, root, "etc/foo.conf", "v2\n", 0o644)
	// undo must restore the pre-FIRST-apply bytes, not v1
	u, _, _ := eng(root, true)
	if err := u.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "original\n", 0o644)
}

func TestApplyRefusesDivergedRunCmd(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	fakeBin(t, dir, "grubby", "")
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "args", Kind: KindRunCmd, Cmd: []string{"grubby", "--args=a"}}}); err != nil {
		t.Fatal(err)
	}
	// e.g. apply --binding=static after a dynamic apply
	e2, _, _ := eng(root, true)
	err := e2.Apply([]Step{{ID: "args", Kind: KindRunCmd, Cmd: []string{"grubby", "--args=a vfio-pci.ids=x"}}})
	if err == nil || !strings.Contains(err.Error(), "undo first") {
		t.Fatalf("diverged run_cmd must refuse and point at undo, got %v", err)
	}
	if !strings.Contains(err.Error(), "--args=a vfio-pci.ids=x") || !strings.Contains(err.Error(), "--args=a") {
		t.Errorf("error should show both commands: %v", err)
	}
}

func TestApplyRefusesDivergedWriteFilePath(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "xml", Kind: KindWriteFile, Path: "/etc/a.xml", Content: []byte("x\n"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	e2, _, _ := eng(root, true)
	err := e2.Apply([]Step{{ID: "xml", Kind: KindWriteFile, Path: "/etc/b.xml", Content: []byte("x\n"), Mode: 0o644}})
	if err == nil || !strings.Contains(err.Error(), "undo first") {
		t.Fatalf("diverged write_file path must refuse, got %v", err)
	}
}

func TestDisableMissingUnitIsNoOp(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	// is-enabled prints nothing for a unit that does not exist
	sysLog := fakeBin(t, dir, "systemctl", "")
	e, out, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "no-persistenced", Kind: KindEnableUnit, Unit: "nvidia-persistenced.service", Enable: false}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Errorf("missing unit should be reported as a no-op:\n%s", out.String())
	}
	for _, line := range logLines(t, sysLog) {
		if strings.HasPrefix(line, "disable") {
			t.Errorf("disable ran against a missing unit: %s", line)
		}
	}
	if mustLoad(t, root).Has("no-persistenced") {
		t.Error("a skipped unit step must not be journaled")
	}
}

func TestApplyRefusesBackupNameCollision(t *testing.T) {
	e, _, _ := eng(t.TempDir(), true)
	err := e.Apply([]Step{
		{ID: "a/b", Kind: KindWriteFile, Path: "/etc/one", Content: []byte("x"), Mode: 0o644},
		{ID: "a:b", Kind: KindWriteFile, Path: "/etc/two", Content: []byte("y"), Mode: 0o644},
	})
	if err == nil || !strings.Contains(err.Error(), "backup") {
		t.Fatalf("colliding backup names must refuse, got %v", err)
	}
}

func TestApplyRefusesCrossRunBackupCollision(t *testing.T) {
	root := t.TempDir()
	write(t, root, "/etc/one", "original-one", 0o644)
	write(t, root, "/etc/two", "original-two", 0o644)
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "a/b", Kind: KindWriteFile, Path: "/etc/one", Content: []byte("x"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	err := e.Apply([]Step{{ID: "a:b", Kind: KindWriteFile, Path: "/etc/two", Content: []byte("y"), Mode: 0o644}})
	if err == nil || !strings.Contains(err.Error(), "a/b") {
		t.Fatalf("second run colliding on backup/a_b must refuse naming the journaled step, got %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "/var/lib/orthogonals/backup/a_b"))
	if err != nil || string(b) != "original-one" {
		t.Fatalf("first run's backup must survive intact, got %q, %v", b, err)
	}
}

func TestApplyOverwritesOrphanedBackup(t *testing.T) {
	// a crash between writeBackup and manifest save leaves a backup file with
	// no record; a re-apply of the same step must overwrite it, not refuse
	root := t.TempDir()
	write(t, root, "/etc/one", "original", 0o644)
	write(t, root, "/var/lib/orthogonals/backup/a_b", "stale-crashed-attempt", 0o600)
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "a/b", Kind: KindWriteFile, Path: "/etc/one", Content: []byte("x"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "/var/lib/orthogonals/backup/a_b"))
	if err != nil || string(b) != "original" {
		t.Fatalf("orphaned backup must be replaced with the current original, got %q, %v", b, err)
	}
}

func TestCheckVMName(t *testing.T) {
	for _, ok := range []string{"win11", "Win-11", "vm_2.0"} {
		if err := CheckVMName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "win 11", "win$(reboot)", "a<b", "x'y"} {
		if err := CheckVMName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestCheckUser(t *testing.T) {
	for _, ok := range []string{"alice", "_svc", "user-1", "Bob_2"} {
		if err := CheckUser(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	// interpolated into the tmpfiles owner column and shell-quoted hook vars,
	// and passed to usermod/virsh — must reject whitespace, shell metachars,
	// a leading digit/'-', and the AD machine-account '$'.
	for _, bad := range []string{"", "a b", "-flag", "1user", `q"y`, "m$", "u;id"} {
		if err := CheckUser(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
