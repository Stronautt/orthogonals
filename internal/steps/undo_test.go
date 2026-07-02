package steps

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUndoDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	dir := fakePath(t)
	grubbyLog := fakeBin(t, dir, "grubby", "")
	sysLog := fakeBin(t, dir, "systemctl", "if [ \"$1\" = \"is-enabled\" ]; then echo disabled; fi")
	e, _, _ := eng(root, true)
	list := []Step{
		{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
		{ID: "args", Kind: KindRunCmd, Cmd: []string{"grubby", "--args=x"}, UndoCmd: []string{"grubby", "--remove-args=x"}},
		{ID: "unit", Kind: KindEnableUnit, Unit: "svc.service", Enable: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	applied := len(logLines(t, grubbyLog)) + len(logLines(t, sysLog))

	dry, out, _ := eng(root, false)
	if err := dry.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "new\n", 0o600)
	if m := mustLoad(t, root); len(m.Records) != len(list) {
		t.Fatalf("dry-run undo must not touch the manifest, got %d records", len(m.Records))
	}
	if now := len(logLines(t, grubbyLog)) + len(logLines(t, sysLog)); now != applied {
		t.Fatal("dry-run undo executed a command")
	}
	for _, want := range []string{
		"would restore /etc/foo.conf",
		"would run: grubby --remove-args=x",
		"would run: systemctl disable svc.service",
		"dry run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
}

func TestLoadCorruptManifest(t *testing.T) {
	root := t.TempDir()
	write(t, root, "var/lib/orthogonals/manifest.json", "{not json", 0o600)
	if _, err := Load(root); err == nil {
		t.Fatal("corrupt manifest must be an error, not silently empty")
	}
}

func TestUndoRunCmdWithoutUndoCommand(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	log := fakeBin(t, dir, "once", "")
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "one-way", Kind: KindRunCmd, Cmd: []string{"once"}}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if lines := logLines(t, log); len(lines) != 1 {
		t.Fatalf("undo must not re-run a command with no undo pair: %v", lines)
	}
	if _, err := os.Stat(ManifestPath(root)); !os.IsNotExist(err) {
		t.Fatal("record without undo command should still clear from the manifest")
	}
}

func TestUndoNothingToDo(t *testing.T) {
	e, out, _ := eng(t.TempDir(), true)
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to undo") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestDataStepsSurviveDefaultUndo(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, out, _ := eng(root, true)

	list := []Step{
		{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
		{ID: "disk", Kind: KindWriteFile, Path: "/var/lib/libvirt/images/win11.qcow2", Content: []byte("qcow2\n"), Mode: 0o600, Data: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
	assertFile(t, root, "var/lib/libvirt/images/win11.qcow2", "qcow2\n", 0o600)
	if !strings.Contains(out.String(), "--purge") {
		t.Fatalf("undo should point at --purge for kept data steps:\n%s", out.String())
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 || m.Records[0].ID != "disk" {
		t.Fatalf("data record must stay in manifest, got %+v", m.Records)
	}
}

func TestPurgeRemovesDataAndState(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	list := []Step{
		{ID: "disk", Kind: KindWriteFile, Path: "/var/lib/libvirt/images/win11.qcow2", Content: []byte("qcow2\n"), Mode: 0o600, Data: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	// simulate state written by later stages
	write(t, root, "var/lib/orthogonals/state.json", "{}\n", 0o600)
	write(t, root, "var/lib/orthogonals/cache/virtio-win.iso", "iso\n", 0o644)
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>\n", 0o600)

	if err := e.Undo(false, true, strings.NewReader("purge\n")); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{
		"var/lib/libvirt/images/win11.qcow2",
		"var/lib/orthogonals",
		"etc/orthogonals",
	} {
		if _, err := os.Stat(filepath.Join(root, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone after purge, stat err = %v", gone, err)
		}
	}
}

func TestPurgeNeedsTypedConfirmation(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	list := []Step{
		{ID: "disk", Kind: KindWriteFile, Path: "/var/lib/libvirt/images/win11.qcow2", Content: []byte("qcow2\n"), Mode: 0o600, Data: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}

	err := e.Undo(false, true, strings.NewReader("nope\n"))
	if err == nil || !strings.Contains(err.Error(), "purge") {
		t.Fatalf("mistyped confirmation must abort, got %v", err)
	}
	assertFile(t, root, "var/lib/libvirt/images/win11.qcow2", "qcow2\n", 0o600)
	if m := mustLoad(t, root); len(m.Records) != 1 {
		t.Fatal("aborted purge must not touch the manifest")
	}
}

func TestUndoRunsCommandsAfterFileRestores(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	conf := filepath.Join(root, "etc/dracut.conf.d/vfio.conf")
	seen := filepath.Join(dir, "seen.log")
	// dracut records what the conf file contains at each invocation
	fakeBin(t, dir, "dracut",
		"cat \""+conf+"\" >> \""+seen+"\" 2>/dev/null\necho --- >> \""+seen+"\"")
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	e, _, _ := eng(root, true)

	list := []Step{
		{ID: "vfio-conf", Kind: KindWriteFile, Path: "/etc/dracut.conf.d/vfio.conf", Content: []byte("vfio\n"), Mode: 0o644},
		{ID: "initramfs", Kind: KindRunCmd, Cmd: []string{"dracut", "-f"}, UndoCmd: []string{"dracut", "-f"}},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	// apply: dracut sees the written conf; undo: conf restored (removed) BEFORE
	// dracut re-runs, so the regeneration works from restored inputs.
	b, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "vfio\n---\n---\n" {
		t.Fatalf("dracut input timeline = %q, want conf visible at apply only", b)
	}
}

func TestUndoAbortsWhenPairedCommandFails(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	fakeBin(t, dir, "okcmd", "")         // forward command succeeds at apply
	fakeBin(t, dir, "failcmd", "exit 1") // paired undo command fails
	e, _, _ := eng(root, true)

	step := Step{ID: "regen", Kind: KindRunCmd, Cmd: []string{"okcmd"}, UndoCmd: []string{"failcmd"}}
	if err := e.Apply([]Step{step}); err != nil {
		t.Fatal(err)
	}
	err := e.Undo(false, false, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "undo regen:") {
		t.Fatalf("err = %v, want a wrapped undo error naming the step", err)
	}
	// a failed undo must not clear the manifest — the host is half-restored and
	// the record has to survive so the operator can retry the rollback
	if !mustLoad(t, root).Has("regen") {
		t.Error("failed undo dropped the record; nothing left to retry")
	}
}

const (
	nvrm570 = "NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n"
	nvrm575 = "NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  575.64.03  Release Build  (dvs-builder)  Mon Jun 16 08:03:23 UTC 2025\n"
)

// appliedRoot applies one plain WriteFile step so undo has work to refuse.
func appliedRoot(t *testing.T) (string, *Engine) {
	t.Helper()
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, _, _ := eng(root, true)
	err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	return root, e
}

func TestUndoRefusesWhileVMPresent(t *testing.T) {
	cases := []struct {
		name, state, want string
	}{
		{"running", "running", "running"},
		{"paused", "paused", "paused"},
		{"defined", "shut off", "defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, e := appliedRoot(t)
			write(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>", 0o600)
			fakeBin(t, fakePath(t), "virsh",
				`[ "$1 $2" = "domstate win11" ] && { echo "`+tc.state+`"; exit 0; }
exit 1`)
			err := e.Undo(false, false, strings.NewReader(""))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("undo error = %v, want mention of %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "virsh") {
				t.Fatalf("refusal must tell the user how to remove the VM first: %v", err)
			}
			assertFile(t, root, "etc/foo.conf", "new\n", 0o600)
			if m := mustLoad(t, root); len(m.Records) != 1 {
				t.Fatal("refused undo must not touch the manifest")
			}
		})
	}
}

func TestVMNames(t *testing.T) {
	root := t.TempDir()
	if got := VMNames(root); len(got) != 0 {
		t.Fatalf("empty root VMNames = %v, want none", got)
	}
	write(t, root, "etc/orthogonals/vms/work.xml", "<domain/>", 0o600)
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>", 0o600)
	got := VMNames(root)
	if !reflect.DeepEqual(got, []string{"win11", "work"}) {
		t.Fatalf("VMNames = %v, want [win11 work]", got)
	}
}

func TestUndoRefusesWhileSecondVMPresent(t *testing.T) {
	root, e := appliedRoot(t)
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>", 0o644)
	write(t, root, "etc/orthogonals/vms/gaming.xml", "<domain/>", 0o644)
	// win11 is gone from libvirt, gaming is live
	fakeBin(t, fakePath(t), "virsh",
		`[ "$1 $2" = "domstate gaming" ] && { echo running; exit 0; }
exit 1`)
	err := e.Undo(false, false, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "gaming") {
		t.Fatalf("undo error = %v, want refusal naming the second VM", err)
	}
}

// a registered VM whose domain is already gone from libvirt (virsh errors)
// must not block undo — same for a host where virsh is absent entirely.
func TestUndoIgnoresGoneDomains(t *testing.T) {
	root, e := appliedRoot(t)
	write(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>", 0o600)
	fakeBin(t, fakePath(t), "virsh", "exit 1")
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatalf("gone domain must not block undo: %v", err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
}

func TestUndoRefusesWhileGPUOnVfio(t *testing.T) {
	root, e := appliedRoot(t)
	dev := "sys/bus/pci/devices/0000:01:00.0"
	write(t, root, dev+"/vendor", "0x10de\n", 0o644)
	if err := os.Symlink("../../../../bus/pci/drivers/vfio-pci",
		filepath.Join(root, dev, "driver")); err != nil {
		t.Fatal(err)
	}
	err := e.Undo(false, false, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "vfio-pci") ||
		!strings.Contains(err.Error(), "0000:01:00.0") {
		t.Fatalf("undo error = %v, want vfio-pci refusal naming the GPU", err)
	}
	if !strings.Contains(err.Error(), "reattach") {
		t.Fatalf("refusal must tell the user how to reattach first: %v", err)
	}
	assertFile(t, root, "etc/foo.conf", "new\n", 0o600)
}

func TestUndoProceedsWithGPUOnHostDriver(t *testing.T) {
	root, e := appliedRoot(t)
	dev := "sys/bus/pci/devices/0000:01:00.0"
	write(t, root, dev+"/vendor", "0x10de\n", 0o644)
	if err := os.Symlink("../../../../bus/pci/drivers/nvidia",
		filepath.Join(root, dev, "driver")); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatalf("GPU on host driver must not block undo: %v", err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
}

func TestUndoPrintsRebootRequired(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	fakeBin(t, dir, "grubby", "")
	e, out, _ := eng(root, true)
	list := []Step{{ID: "kernel-args", Kind: KindRunCmd, Reboot: true,
		Cmd:     []string{"grubby", "--args=x"},
		UndoCmd: []string{"grubby", "--remove-args=x"}}}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}

	dry, dryOut, _ := eng(root, false)
	if err := dry.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dryOut.String(), "reboot") {
		t.Fatalf("dry-run undo of a boot step must mention the reboot:\n%s", dryOut.String())
	}

	out.Reset()
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "reboot required") {
		t.Fatalf("undo of a boot step must print reboot required:\n%s", out.String())
	}
}

func TestUndoNoRebootNoteWithoutBootSteps(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, out, _ := eng(root, true)
	err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "reboot") {
		t.Fatalf("no boot step, no reboot note:\n%s", out.String())
	}
}

func TestApplyStampsHostVersions(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/sys/kernel/osrelease", "6.14.9-200.fc42.x86_64\n", 0o644)
	write(t, root, "proc/driver/nvidia/version", nvrm570, 0o644)
	e, _, _ := eng(root, true)
	err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	m := mustLoad(t, root)
	if m.Kernel != "6.14.9-200.fc42.x86_64" {
		t.Errorf("Kernel = %q, want the osrelease value", m.Kernel)
	}
	if m.NVIDIAVersion != "570.153.02" || m.NVIDIAFlavor != "proprietary" {
		t.Errorf("NVIDIA stamp = %q (%q), want 570.153.02 (proprietary)", m.NVIDIAVersion, m.NVIDIAFlavor)
	}
}

func TestUndoPrintsVersionDrift(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/sys/kernel/osrelease", "6.14.9-200.fc42.x86_64\n", 0o644)
	write(t, root, "proc/driver/nvidia/version", nvrm570, 0o644)
	e, _, _ := eng(root, true)
	err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	// host updates kernel and driver between apply and undo
	write(t, root, "proc/sys/kernel/osrelease", "6.15.3-100.fc43.x86_64\n", 0o644)
	write(t, root, "proc/driver/nvidia/version", nvrm575, 0o644)

	u, out, _ := eng(root, true)
	if err := u.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"6.14.9-200.fc42.x86_64 → 6.15.3-100.fc43.x86_64",
		"570.153.02 (proprietary) → 575.64.03 (open)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("undo output missing drift note %q:\n%s", want, out.String())
		}
	}
}

func TestUndoNoDriftNoteWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/sys/kernel/osrelease", "6.14.9-200.fc42.x86_64\n", 0o644)
	write(t, root, "proc/driver/nvidia/version", nvrm570, 0o644)
	e, out, _ := eng(root, true)
	err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "has since updated") {
		t.Fatalf("no drift, no note:\n%s", out.String())
	}
}

func TestUndoKeepsStampsInPartialManifest(t *testing.T) {
	root := t.TempDir()
	write(t, root, "proc/sys/kernel/osrelease", "6.14.9-200.fc42.x86_64\n", 0o644)
	e, _, _ := eng(root, true)
	list := []Step{
		{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o600},
		{ID: "disk", Kind: KindWriteFile, Path: "/var/lib/libvirt/images/win11.qcow2", Content: []byte("qcow2\n"), Mode: 0o600, Data: true},
	}
	if err := e.Apply(list); err != nil {
		t.Fatal(err)
	}
	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 || m.Kernel != "6.14.9-200.fc42.x86_64" {
		t.Fatalf("partial undo must keep the apply-time stamps, got kernel %q records %d", m.Kernel, len(m.Records))
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			m[rel] = "dir"
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		m[rel] = fmt.Sprintf("%04o:%s", info.Mode().Perm(), b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Byte-identical scope (research §C2): the files WE wrote under root — not
// system state (kernel, driver), which is stamped at apply and drift-reported
// by undo instead.
func TestApplyUndoRoundTripByteIdentical(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/environment", "LANG=C\n", 0o644)
	write(t, root, "etc/dracut.conf.d/50-host.conf", "keep\n", 0o644)
	if err := os.MkdirAll(filepath.Join(root, "var/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := fakePath(t)
	fakeBin(t, dir, "grubby", "")
	fakeBin(t, dir, "dracut", "")
	fakeBin(t, dir, "restorecon", "")
	fakeBin(t, dir, "systemctl", "if [ \"$1\" = \"is-enabled\" ]; then echo disabled; fi")

	before := snapshot(t, root)

	full := []Step{
		{ID: "dracut-conf", Kind: KindWriteFile, Path: "/etc/dracut.conf.d/vfio.conf", Content: []byte("force_drivers+=\" vfio_pci \"\n"), Mode: 0o644, Restorecon: true},
		{ID: "env-pins", Kind: KindWriteFile, Path: "/etc/environment", Content: []byte("LANG=C\nEGL_DEVICE=igpu\n"), Mode: 0o600},
		{ID: "udev-rule", Kind: KindWriteFile, Path: "/etc/udev/rules.d/61-mutter.rules", Content: []byte("ENV{MUTTER_IGNORE}=\"1\"\n"), Mode: 0o644},
		{ID: "kernel-args", Kind: KindRunCmd,
			Cmd:     []string{"grubby", "--update-kernel=ALL", "--args=intel_iommu=on iommu=pt"},
			UndoCmd: []string{"grubby", "--update-kernel=ALL", "--remove-args=intel_iommu=on iommu=pt"}},
		{ID: "initramfs", Kind: KindRunCmd,
			Cmd:     []string{"dracut", "-f", "--regenerate-all"},
			UndoCmd: []string{"dracut", "-f", "--regenerate-all"}},
		{ID: "libvirt-guests", Kind: KindEnableUnit, Unit: "libvirt-guests.service", Enable: true},
	}
	e, _, _ := eng(root, true)
	if err := e.Apply(full); err != nil {
		t.Fatal(err)
	}
	if m := mustLoad(t, root); len(m.Records) != len(full) {
		t.Fatalf("manifest records = %d, want %d", len(m.Records), len(full))
	}

	if err := e.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, root)
	if !reflect.DeepEqual(before, after) {
		for k, v := range before {
			if after[k] != v {
				t.Errorf("changed/missing: %s (before %q, after %q)", k, v, after[k])
			}
		}
		for k, v := range after {
			if _, ok := before[k]; !ok {
				t.Errorf("leftover: %s (%q)", k, v)
			}
		}
		t.Fatal("tree not byte-identical after undo")
	}
}

func TestUndoIDRemovesSingleRecord(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	log := fakeBin(t, dir, "virsh", "")
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{
		{ID: "vm-domain-xml", Kind: KindWriteFile, Path: "/etc/orthogonals/win11.xml", Content: []byte("<domain/>"), Mode: 0o644},
		{ID: "vm-define", Kind: KindRunCmd, Cmd: []string{"virsh", "define", "/etc/orthogonals/win11.xml"},
			UndoCmd: []string{"virsh", "undefine", "win11", "--nvram", "--tpm"}},
	}); err != nil {
		t.Fatal(err)
	}

	found, err := e.UndoID("vm-define", false)
	if err != nil || !found {
		t.Fatalf("UndoID = %v, %v; want found, nil", found, err)
	}
	lines := logLines(t, log)
	if len(lines) != 2 || lines[1] != "undefine win11 --nvram --tpm" {
		t.Fatalf("virsh log = %q, want the paired undo command", lines)
	}
	m := mustLoad(t, root)
	if m.find("vm-define") != nil {
		t.Error("vm-define record must leave the manifest")
	}
	if m.find("vm-domain-xml") == nil {
		t.Error("other records must stay")
	}
	assertFile(t, root, "etc/orthogonals/win11.xml", "<domain/>", 0o644)
}

func TestUndoIDMissingRecord(t *testing.T) {
	e, _, _ := eng(t.TempDir(), true)
	found, err := e.UndoID("vm-define", false)
	if err != nil || found {
		t.Fatalf("UndoID = %v, %v; want not found, nil", found, err)
	}
}

func TestUndoIDDryRunKeepsRecord(t *testing.T) {
	root := t.TempDir()
	dir := fakePath(t)
	log := fakeBin(t, dir, "virsh", "")
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "vm-define", Kind: KindRunCmd,
		Cmd: []string{"virsh", "define", "x"}, UndoCmd: []string{"virsh", "undefine", "win11"}}}); err != nil {
		t.Fatal(err)
	}
	dry, out, _ := eng(root, false)
	found, err := dry.UndoID("vm-define", false)
	if err != nil || !found {
		t.Fatalf("UndoID = %v, %v; want found, nil", found, err)
	}
	if !strings.Contains(out.String(), "would run: virsh undefine win11") {
		t.Errorf("dry run must print the undo command, got %q", out)
	}
	if lines := logLines(t, log); len(lines) != 1 {
		t.Fatalf("dry run executed the undo command: %q", lines)
	}
	if mustLoad(t, root).find("vm-define") == nil {
		t.Error("dry run must keep the record")
	}
}

func TestUndoPurgeRefusedWhileDriftKept(t *testing.T) {
	root := t.TempDir()
	write(t, root, "etc/foo.conf", "old\n", 0o644)
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("new\n"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	write(t, root, "etc/foo.conf", "hand-edited\n", 0o644) // drift

	u, _, _ := eng(root, true)
	err := u.Undo(false, true, strings.NewReader("purge\n"))
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("purge over drift-kept records must refuse and point at --force, got %v", err)
	}
	// backups and manifest must survive so undo --force can still restore
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals/manifest.json")); err != nil {
		t.Error("manifest destroyed by refused purge")
	}
	m := mustLoad(t, root)
	if len(m.Records) != 1 {
		t.Fatalf("kept record dropped: %+v", m.Records)
	}
	f, _, _ := eng(root, true)
	if err := f.Undo(true, true, strings.NewReader("purge\n")); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root, "etc/foo.conf", "old\n", 0o644)
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals")); !os.IsNotExist(err) {
		t.Error("state dir survived forced purge")
	}
}

func TestUndoResetsPipelineState(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "foo", Kind: KindWriteFile, Path: "/etc/foo.conf", Content: []byte("x\n"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	write(t, root, "var/lib/orthogonals/state.json", `{"state":"verified"}`+"\n", 0o600)
	u, _, _ := eng(root, true)
	if err := u.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals/state.json")); !os.IsNotExist(err) {
		t.Error("stale up-pipeline state survived undo — a later `up` would claim setup is complete")
	}
}

func TestUndoDryRunCreatedFileAndPurge(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "made", Kind: KindWriteFile, Path: "/etc/made.conf", Content: []byte("x\n"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	dry, out, _ := eng(root, false)
	if err := dry.Undo(false, true, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"would remove /etc/made.conf", "would remove", "dry run"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out.String())
		}
	}
	assertFile(t, root, "etc/made.conf", "x\n", 0o644)
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals/manifest.json")); err != nil {
		t.Error("dry-run purge touched the manifest")
	}
}

func TestUndoTwiceReportsAlreadyRemoved(t *testing.T) {
	root := t.TempDir()
	e, _, _ := eng(root, true)
	if err := e.Apply([]Step{{ID: "made", Kind: KindWriteFile, Path: "/etc/sub/made.conf", Content: []byte("x\n"), Mode: 0o644}}); err != nil {
		t.Fatal(err)
	}
	u1, _, _ := eng(root, true)
	if err := u1.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	// resurrect the manifest record by hand: simulates a second undo racing a
	// partially removed state dir
	m := &Manifest{Records: []Record{{ID: "made", Kind: KindWriteFile, Path: "/etc/sub/made.conf", MadeDirs: []string{"/etc/sub"}}}}
	if err := m.save(root); err != nil {
		t.Fatal(err)
	}
	u2, out, _ := eng(root, true)
	if err := u2.Undo(false, false, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "already removed") {
		t.Errorf("second undo should report already removed:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "etc/sub")); !os.IsNotExist(err) {
		t.Error("created dir survived the re-undo cleanup")
	}
}

func TestUndoUnknownRecordKind(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Records: []Record{{ID: "weird", Kind: "teleport"}}}
	if err := m.save(root); err != nil {
		t.Fatal(err)
	}
	u, _, _ := eng(root, true)
	err := u.Undo(false, false, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "teleport") {
		t.Fatalf("unknown record kind must fail loudly, got %v", err)
	}
}
