package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

func writeISO(t *testing.T, root string) string {
	t.Helper()
	iso := media.ISOPath(root, "win11")
	if err := os.MkdirAll(filepath.Dir(iso), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("provision-iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	return iso
}

func TestRemoveProvisionISODeletes(t *testing.T) {
	root := t.TempDir()
	iso := writeISO(t, root)
	var out bytes.Buffer
	removeProvisionISO(root, "win11", &out)
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Error("ISO left on disk")
	}
	if !strings.Contains(out.String(), "removed") {
		t.Errorf("output = %q, want a removal notice", out.String())
	}
}

func TestRemoveProvisionISOAbsentIsSilent(t *testing.T) {
	var out bytes.Buffer
	removeProvisionISO(t.TempDir(), "win11", &out)
	if out.Len() != 0 {
		t.Errorf("absent ISO should be silent, got %q", out.String())
	}
}

func TestRemoveProvisionISORemoveError(t *testing.T) {
	root := t.TempDir()
	iso := media.ISOPath(root, "win11")
	if err := os.MkdirAll(filepath.Join(iso, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	removeProvisionISO(root, "win11", &out)
	if !strings.Contains(out.String(), "could not remove") {
		t.Errorf("output = %q, want a remove-error notice", out.String())
	}
}

func runUpCLI(t *testing.T, root string, extra ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"up", "--root", root}, extra...)
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUpDryRunPrintsPipeline(t *testing.T) {
	code, out, _ := runUpCLI(t, t.TempDir())
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"pipeline state: fresh", "apply host configuration", "dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestUpRequiresISOBeforeMediaStage(t *testing.T) {
	code, _, errOut := runUpCLI(t, t.TempDir(), "--yes")
	if code != 2 || !strings.Contains(errOut, "--win11-iso") {
		t.Fatalf("exit %d, stderr %q — want usage error demanding the ISO", code, errOut)
	}
}

// up stops cleanly at the reboot boundary after applying host config.
func TestUpStopsAtRebootBoundary(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out, errOut := runUpCLI(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "reboot now") {
		t.Errorf("missing reboot instruction:\n%s", out)
	}
	if st, _ := orchestrate.LoadState(root); st != orchestrate.StateHostApplied {
		t.Errorf("state = %s, want host-applied", st)
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/dracut.conf.d/vfio.conf")); err != nil {
		t.Error("apply did not run")
	}
}

// a resume whose boot verification still fails errors at the bundle.
func TestUpResumeFailedBootVerification(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, out, errOut := runUpCLI(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso"); code != 0 {
		t.Fatalf("boundary run failed: %d\n%s%s", code, out, errOut)
	}
	code, _, errOut := runUpCLI(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, errOut)
	}
	for _, want := range []string{"post-reboot verification", "orthogonals bundle"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// resume from the installing state polls provisioning done, then verifies.
func TestUpResumesInstallAndVerifies(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	dir := fakeVMPath(t)
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\necho 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := fakeVirt(t, &virttest.Fake{State: "running", Phys: 9663676416,
		Agent: virttest.Responder(`{"stage":"done","ok":true,"error":""}`, "", 0)})
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	if err := orchestrate.SaveState(root, orchestrate.StateInstalling); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runUpCLI(t, root, "--yes", "--user", "testuser")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"provisioning complete", "PASS guest nvidia-smi", "setup complete"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if st, _ := orchestrate.LoadState(root); st != orchestrate.StateVerified {
		t.Errorf("state = %s, want verified", st)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<model type='none'/>") || strings.Contains(string(xml), "cdrom") {
		t.Errorf("final XML must carry video=none and no installer media:\n%s", xml)
	}
	if got := domain.CurrentStage(root, "win11"); got != domain.StageFinal {
		t.Errorf("stage after verify = %s, want final", got)
	}
	if !strings.Contains(f.XML, "<model type='none'/>") {
		t.Errorf("re-defined domain never reached libvirt:\n%v", f.Calls)
	}
}

// a reboot-resume without --vm-name recovers the persisted name.
func TestUpResumeRecoversAppliedVMName(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := orchestrate.SaveState(root, orchestrate.StateRebooted); err != nil {
		t.Fatal(err)
	}
	if err := orchestrate.SaveVMName(root, "gamer"); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runUpCLI(t, root, "--yes", "--user", "testuser", "--win11-iso", "/isos/Win11.iso")
	if code != 1 {
		t.Fatalf("exit %d (media should fail, vm should succeed)\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/gamer.xml")); err != nil {
		t.Errorf("vm define used the default name, not the persisted 'gamer': %v", err)
	}
}

// a verified pipeline plus an undefined --vm-name restarts the pipeline.
func TestUpNewVMRestartsPipeline(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := orchestrate.SaveState(root, orchestrate.StateVerified); err != nil {
		t.Fatal(err)
	}
	if err := orchestrate.SaveVMName(root, "win11"); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runUpCLI(t, root, "--yes", "--user", "testuser",
		"--vm-name", "gaming", "--display-name", "Gaming", "--win11-iso", "/isos/Win11.iso")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "VM gaming is not defined yet") {
		t.Errorf("missing restart notice:\n%s", out)
	}
	if strings.Contains(out, "setup complete") {
		t.Errorf("restarted pipeline must not claim completion:\n%s", out)
	}
	if st, _ := orchestrate.LoadState(root); st != orchestrate.StateHostApplied {
		t.Errorf("state = %s, want host-applied (pipeline restarted)", st)
	}
	if got, _ := orchestrate.SavedVMName(root); got != "gaming" {
		t.Errorf("saved VM name = %q, want gaming", got)
	}
}

// a completed VM converges host artifacts and the domain to the current binary.
func TestUpCompletedInstallConverges(t *testing.T) {
	t.Setenv("SUDO_USER", "testuser")
	dir := fakeBinDir(t, append(append([]string{}, vmFakeBins...), applyFakeBins...))
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "systemctl.log") +
		"\"\nif [ \"$1\" = \"is-enabled\" ]; then echo enabled; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f := fakeVirt(t, &virttest.Fake{})
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gamer",
		"--display-name", "Gaming Rig", "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gamer",
		"--stage", "final", "--yes", "define"); code != 0 {
		t.Fatalf("final-stage redefine failed: %s", stderr)
	}

	code, out, errOut := runUpCLI(t, root, "--vm-name", "gamer")
	if code != 0 {
		t.Fatalf("dry exit %d\n%s%s", code, out, errOut)
	}

	code, out, errOut = runUpCLI(t, root, "--yes", "--vm-name", "gamer")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "converging host and VM gamer") {
		t.Errorf("converge banner missing:\n%s", out)
	}
	if !strings.Contains(out, "launch with `orthogonals vm launch --vm-name gamer` or the \"Gaming Rig\" desktop entry") {
		t.Errorf("converge must name the launch command and desktop entry:\n%s", out)
	}
	if got := len(slices.DeleteFunc(slices.Clone(f.Calls), func(c string) bool { return c != "define" })); got < 2 {
		t.Errorf("define reached libvirt %d times, want at least 2:\n%v", got, f.Calls)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/gamer.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<model type='none'/>") || strings.Contains(string(xml), "cdrom") {
		t.Errorf("converged XML must carry the post-install state:\n%s", xml)
	}
	if got := domain.CurrentStage(root, "gamer"); got != domain.StageFinal {
		t.Errorf("converged stage = %s, want final", got)
	}
}

// up forwards the VM sizing flags verbatim to the vm stage.
func TestUpForwardsVMSizingFlags(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := orchestrate.SaveState(root, orchestrate.StateRebooted); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runUpCLI(t, root, "--yes", "--user", "testuser",
		"--win11-iso", "/isos/Win11.iso", "--vm-name", "gamer",
		"--ram", "12", "--disk", "/tank/vm.qcow2", "--disk-size", "200",
		"--resolution", "3840x2160")
	if code != 1 {
		t.Fatalf("exit %d (media should fail, vm should succeed)\n%s", code, out)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/gamer.xml"))
	if err != nil {
		t.Fatalf("vm stage did not write the domain XML: %v", err)
	}
	for _, want := range []string{
		"<name>gamer</name>",
		"<memory unit='MiB'>12288</memory>",
		"<source file='/tank/vm.qcow2'/>",
		"<size unit='M'>128</size>",
		"<source file='/isos/Win11.iso' startupPolicy='optional'/>",
	} {
		if !strings.Contains(string(xml), want) {
			t.Errorf("domain XML missing %q:\n%s", want, xml)
		}
	}
}
