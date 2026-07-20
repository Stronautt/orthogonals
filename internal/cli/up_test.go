package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/steps"
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
	// a non-empty directory at the ISO path: Stat succeeds, os.Remove fails
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

func runUp(t *testing.T, root string, extra ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"up", "--root", root}, extra...)
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUpDryRunPrintsPipeline(t *testing.T) {
	code, out, _ := runUp(t, t.TempDir())
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
	code, _, errOut := runUp(t, t.TempDir(), "--yes")
	if code != 2 || !strings.Contains(errOut, "--win11-iso") {
		t.Fatalf("exit %d, stderr %q — want usage error demanding the ISO", code, errOut)
	}
}

// up applies the host configuration, then stops cleanly at the reboot
// boundary because the new boot config is not live yet.
func TestUpStopsAtRebootBoundary(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out, errOut := runUp(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "reboot now") {
		t.Errorf("missing reboot instruction:\n%s", out)
	}
	if st, _ := orchestrate.LoadState(root); st != orchestrate.StateHostApplied {
		t.Errorf("state = %s, want host-applied", st)
	}
	// host config landed
	if _, err := os.Stat(filepath.Join(root, "/etc/dracut.conf.d/vfio.conf")); err != nil {
		t.Error("apply did not run")
	}
}

// a resume whose boot verification still fails is an error pointing at the
// diagnostics bundle, not another silent reboot prompt.
func TestUpResumeFailedBootVerification(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, out, errOut := runUp(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso"); code != 0 {
		t.Fatalf("boundary run failed: %d\n%s%s", code, out, errOut)
	}
	// "reboot" without the kargs taking effect (still no /proc/cmdline)
	code, _, errOut := runUp(t, root, "--yes", "--user", "testuser", "--win11-iso", "/nonexistent.iso")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, errOut)
	}
	for _, want := range []string{"post-reboot verification", "orthogonals bundle"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q:\n%s", want, errOut)
		}
	}
}

// resume from the installing state: poll provisioning to done, then verify —
// the tail of the pipeline the reboot-boundary test cannot reach.
func TestUpResumesInstallAndVerifies(t *testing.T) {
	root := t.TempDir()
	if err := orchestrate.SaveState(root, orchestrate.StateInstalling); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "domstate")
	if err := os.WriteFile(stateFile, []byte("running"), 0o644); err != nil {
		t.Fatal(err)
	}
	// done provision status, base64 of {"stage":"done","ok":true,"error":""}
	doneB64 := "eyJzdGFnZSI6ImRvbmUiLCJvayI6dHJ1ZSwiZXJyb3IiOiIifQ=="
	virsh := `#!/bin/sh
case "$*" in
domstate*) cat "` + stateFile + `" ;;
domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     9663676416\nPhysical:       9663676416\n' ;;
start*) printf 'running' > "` + stateFile + `" ;;
shutdown*) printf 'shut off' > "` + stateFile + `" ;;
*guest-ping*) echo '{"return":{}}' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":0,"out-data":"` + doneB64 + `"}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "virsh"), []byte(virsh), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\necho 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	virtxmlLog := filepath.Join(dir, "virt-xml.log")
	if err := os.WriteFile(filepath.Join(dir, "virt-xml"),
		[]byte("#!/bin/sh\necho \"$*\" >> \""+virtxmlLog+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	code, out, errOut := runUp(t, root, "--yes")
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
	// the domain is defined with an install-time emulated display (Windows
	// OOBE crash-loops with zero adapters); once provisioning reports done it
	// must be flipped to video=none — Looking Glass needs the VDD monitor to
	// be the guest's only display — and journaled like every other mutation
	x, _ := os.ReadFile(virtxmlLog)
	if !strings.Contains(string(x), "win11 --edit --video clearxml=yes,model=none") {
		t.Errorf("install display not removed after provisioning:\n%s", x)
	}
	// verified means the guest never boots from the installer media again:
	// the three cdroms come off the domain
	for _, dev := range []string{"sda", "sdb", "sdc"} {
		if !strings.Contains(string(x), "win11 --remove-device --disk target="+dev) {
			t.Errorf("installer cdrom %s not detached after verify:\n%s", dev, x)
		}
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has(domain.InstallVideoStepID("win11")) {
		t.Error("install-video edit not journaled")
	}
	if !m.Has(domain.DetachMediaStepID("win11", "sda")) {
		t.Error("media detach not journaled")
	}
}

// a reboot-resume that omits --vm-name must recover the name the first run
// applied (persisted in state.json), or vm define builds the default-named
// domain while the hooks target the custom one — silently broken passthrough.
func TestUpResumeRecoversAppliedVMName(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	// first run applied "gamer" and reached the reboot boundary
	if err := orchestrate.SaveState(root, orchestrate.StateRebooted); err != nil {
		t.Fatal(err)
	}
	if err := orchestrate.SaveVMName(root, "gamer"); err != nil {
		t.Fatal(err)
	}
	// resume WITHOUT --vm-name (media build fails after, which is fine — the vm
	// stage has run by then)
	code, out, _ := runUp(t, root, "--yes", "--user", "testuser", "--win11-iso", "/isos/Win11.iso")
	if code != 1 {
		t.Fatalf("exit %d (media should fail, vm should succeed)\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/gamer.xml")); err != nil {
		t.Errorf("vm define used the default name, not the persisted 'gamer': %v", err)
	}
}

// a verified pipeline plus a --vm-name the journal has no define step for is
// a NEW VM: up must restart the pipeline instead of claiming setup is done.
// On this fixture the restart runs apply and stops at the reboot boundary —
// reaching that boundary from a "verified" state IS the restart.
func TestUpNewVMRestartsPipeline(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := orchestrate.SaveState(root, orchestrate.StateVerified); err != nil {
		t.Fatal(err)
	}
	if err := orchestrate.SaveVMName(root, "win11"); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runUp(t, root, "--yes", "--user", "testuser",
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

// a VM whose pipeline completed (journaled media detach) converges the host
// artifacts and the domain to the current binary (a release rendering
// different XML must reach libvirt), and still names the VM's launcher.
// Deliberately no state.json here: completion is judged from the manifest —
// the state file is ephemeral (undo removes it) and a converged host may not
// have one.
func TestUpCompletedInstallConverges(t *testing.T) {
	t.Setenv("SUDO_USER", "testuser")
	dir := fakeBinDir(t, append(append([]string{}, vmFakeBins...), applyFakeBins...))
	// systemctl is-enabled must answer, or the engine treats units as absent
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "systemctl.log") +
		"\"\nif [ \"$1\" = \"is-enabled\" ]; then echo enabled; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gamer",
		"--display-name", "Gaming Rig", "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	// the pipeline's post-install transitions, then its final state
	e := &steps.Engine{Root: root, Yes: true, Out: os.Stderr, Err: os.Stderr}
	if err := e.Apply([]steps.Step{domain.InstallVideoStep("gamer")}); err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(domain.DetachMediaSteps("gamer")); err != nil {
		t.Fatal(err)
	}

	// dry run announces the pending redefine without mutating
	code, out, errOut := runUp(t, root, "--vm-name", "gamer")
	if code != 0 {
		t.Fatalf("dry exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "(journaled input changed)") {
		t.Errorf("dry run must announce the pending redefine:\n%s", out)
	}

	code, out, errOut = runUp(t, root, "--yes", "--vm-name", "gamer")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(out, "converging host and VM gamer") {
		t.Errorf("converge banner missing:\n%s", out)
	}
	if !strings.Contains(out, `launch with _ort-run-gamer-lg or the "Gaming Rig" desktop entry`) {
		t.Errorf("converge must still name the per-VM launcher:\n%s", out)
	}
	// the redefine reached libvirt: define ran again with the converged XML
	if got := strings.Count(binLog(t, dir, "virsh"), "define /etc/orthogonals/vms/gamer.xml"); got != 2 {
		t.Errorf("virsh define ran %d times, want 2:\n%s", got, binLog(t, dir, "virsh"))
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/gamer.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<model type='none'/>") || strings.Contains(string(xml), "cdrom") {
		t.Errorf("converged XML must carry the post-install state:\n%s", xml)
	}
}

// the sizing flags must reach the vm stage verbatim: a dropped flag still
// "succeeds" with wrong values, which nothing else would catch.
func TestUpForwardsVMSizingFlags(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := orchestrate.SaveState(root, orchestrate.StateRebooted); err != nil {
		t.Fatal(err)
	}
	// media build fails afterwards (no wiminfo/network in this test) — the vm
	// stage has run by then, which is all this asserts
	code, out, _ := runUp(t, root, "--yes", "--user", "testuser",
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
		"<size unit='M'>128</size>", // 4K IVSHMEM sizing
		"<source file='/isos/Win11.iso' startupPolicy='optional'/>",
	} {
		if !strings.Contains(string(xml), want) {
			t.Errorf("domain XML missing %q:\n%s", want, xml)
		}
	}
}
