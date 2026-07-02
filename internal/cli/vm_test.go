package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

// vmFakeBins are every binary the vm step list shells out to, plus rm —
// the disk step's undo command targets the un-rooted host path, and a test
// must observe it, never run it.
var vmFakeBins = []string{"qemu-img", "virsh", "semanage", "restorecon", "rm", "virt-xml"}

func fakeVMPath(t *testing.T) string {
	t.Helper()
	return fakeBinDir(t, vmFakeBins)
}

func TestVMDefineDryRun(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, stdout, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"would run: qemu-img create -f qcow2 /var/lib/libvirt/images/win11.qcow2 100G",
		"would run: virsh define /etc/orthogonals/vms/win11.xml",
		"dry run — re-run with --yes to apply",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml")); err == nil {
		t.Error("dry run wrote the domain XML")
	}
	if got := binLog(t, dir, "qemu-img"); got != "" {
		t.Errorf("dry run executed qemu-img: %s", got)
	}
}

func TestVMDefineApplies(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<name>win11</name>") {
		t.Errorf("domain XML content wrong:\n%s", xml)
	}
	if got := binLog(t, dir, "virsh"); !strings.Contains(got, "define /etc/orthogonals/vms/win11.xml") {
		t.Errorf("virsh log = %q", got)
	}
	if got := binLog(t, dir, "qemu-img"); !strings.Contains(got, "create -f qcow2") {
		t.Errorf("qemu-img log = %q", got)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-domain-xml-win11", "vm-disk-image-win11", "vm-define-win11"} {
		if !m.Has(id) {
			t.Errorf("manifest missing %s", id)
		}
	}

	// re-run is idempotent: no duplicate journal entries, no re-execution
	before := len(binLog(t, dir, "qemu-img"))
	if code, _, _ := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("re-run exit %d", code)
	}
	if after := len(binLog(t, dir, "qemu-img")); after != before {
		t.Error("re-run executed qemu-img again")
	}
}

func TestVMDefineRefusesForeignDisk(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	hwtest.WriteFile(t, root, "var/lib/libvirt/images/win11.qcow2", "precious")
	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "define")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not orthogonals-managed") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestVMUndefine(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	// a completed install journaled the video edit; undefine must drop it too
	e := &steps.Engine{Root: root, Yes: true, Out: os.Stderr, Err: os.Stderr}
	if err := e.Apply([]steps.Step{domain.InstallVideoStep("win11")}); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	m0, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m0.Has(domain.InstallVideoStepID("win11")) {
		t.Error("undefine left the install-video record — a re-install would skip the display edit")
	}
	if got := binLog(t, dir, "virsh"); !strings.Contains(got, "undefine win11 --nvram --tpm") {
		t.Errorf("virsh log = %q", got)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("vm-define-win11") {
		t.Error("vm-define-win11 must leave the manifest")
	}
	if !m.Has("vm-disk-image-win11") {
		t.Error("undefine must keep the disk record for full undo/purge")
	}
	// the domain XML is the registry entry — undefine must deregister the VM
	if m.Has("vm-domain-xml-win11") {
		t.Error("undefine left the domain XML record")
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml")); err == nil {
		t.Error("undefine left the registry entry — the hook still answers for the VM")
	}
}

func TestVMUndefinePurgeRemovesEverything(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	hwtest.WriteFile(t, root, "var/lib/orthogonals/state.json", `{"state":"verified"}`)

	// flags after the verb, the order a user naturally types
	code, stdout, stderr := run(t, "vm", "--root", root, "undefine", "--purge", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if got := binLog(t, dir, "virsh"); !strings.Contains(got, "undefine win11 --nvram --tpm") {
		t.Errorf("virsh log = %q", got)
	}
	if got := binLog(t, dir, "rm"); !strings.Contains(got, "-f /var/lib/libvirt/images/win11.qcow2") {
		t.Errorf("rm log = %q", got)
	}
	if got := binLog(t, dir, "semanage"); !strings.Contains(got, "fcontext -d /var/lib/libvirt/images/win11.qcow2") {
		t.Errorf("semanage log = %q", got)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-define-win11", "vm-disk-restorecon-win11", "vm-disk-fcontext-win11", "vm-disk-image-win11", "vm-domain-xml-win11"} {
		if m.Has(id) {
			t.Errorf("purge left %s in the manifest", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml")); err == nil {
		t.Error("purge kept the domain XML")
	}
	if _, err := os.Stat(filepath.Join(root, "/var/lib/orthogonals/state.json")); err == nil {
		t.Error("purge kept state.json — up would claim setup is complete")
	}
	if !strings.Contains(stdout, "reinstall with") {
		t.Errorf("stdout missing the reinstall hint: %q", stdout)
	}
}

func TestVMUndefinePurgeDryRunChangesNothing(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	hwtest.WriteFile(t, root, "var/lib/orthogonals/state.json", `{"state":"verified"}`)

	code, stdout, stderr := run(t, "vm", "--root", root, "--purge", "undefine")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "dry run") {
		t.Errorf("stdout = %q", stdout)
	}
	if got := binLog(t, dir, "rm"); got != "" {
		t.Errorf("dry run executed rm: %s", got)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Has("vm-define-win11") || !m.Has("vm-disk-image-win11") {
		t.Error("dry run modified the manifest")
	}
	if _, err := os.Stat(filepath.Join(root, "/var/lib/orthogonals/state.json")); err != nil {
		t.Error("dry run removed state.json")
	}
}

func TestVMUndefineRefusesWhileRunning(t *testing.T) {
	dir := fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	script := "#!/bin/sh\nif [ \"$1\" = \"domstate\" ]; then echo running; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "virsh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "running") || !strings.Contains(stderr, "virsh shutdown") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestVMUndefineNothingDefined(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	code, stdout, _ := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestVMFlagOverrides(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, stdout, stderr := run(t, "vm", "--root", root,
		"--vm-name", "gamer", "--ram", "12", "--disk", "/tank/vm.qcow2",
		"--disk-size", "200", "--resolution", "3840x2160",
		"--win11-iso", "/isos/Win11.iso", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"qemu-img create -f qcow2 /tank/vm.qcow2 200G",
		"virsh define /etc/orthogonals/vms/gamer.xml",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q\n%s", want, stdout)
		}
	}
}

func TestVMBadArgs(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no action", []string{"vm", "--root", root}, 2},
		{"unknown action", []string{"vm", "--root", root, "destroy"}, 2},
		{"missing win11 iso", []string{"vm", "--root", root, "define"}, 2},
		{"bad resolution", []string{"vm", "--root", root, "--win11-iso", "/isos/w.iso", "--resolution", "huge", "define"}, 1},
		{"ram too small", []string{"vm", "--root", root, "--win11-iso", "/isos/w.iso", "--ram", "4", "define"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := run(t, tc.args...)
			if code != tc.want {
				t.Fatalf("exit %d, want %d", code, tc.want)
			}
		})
	}
}

// vm define registers the domain in the registry (the domain XML doubles as
// the entry), so undo's still-defined/running guard finds a custom name —
// with no side config file.
func TestVMDefineRegistersName(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gamer",
		"--win11-iso", "/isos/Win11.iso", "--yes", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if got := steps.VMNames(root); len(got) != 1 || got[0] != "gamer" {
		t.Errorf("VMNames = %v, want [gamer]", got)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/orthogonals/config.json")); err == nil {
		t.Error("vm define wrote config.json")
	}
}

func TestVMDefineWritesVMArtifacts(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "work",
		"--display-name", "Work PC", "--win11-iso", "/isos/Win11.iso", "--yes", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if got := hostcfg.DisplayName(root, "work"); got != "Work PC" {
		t.Errorf("registered display name = %q, want Work PC", got)
	}
	st, err := os.Stat(filepath.Join(root, "/usr/local/bin/_ort-run-work-lg"))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("launcher missing or not executable: %v", err)
	}
	desktop, err := os.ReadFile(filepath.Join(root, "/usr/share/applications/work.orthogonals.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Name=Work PC", "Exec=/usr/local/bin/_ort-run-work-lg"} {
		if !strings.Contains(string(desktop), want) {
			t.Errorf("desktop entry missing %q:\n%s", want, desktop)
		}
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"_ort-run-work-lg", "desktop-entry-work"} {
		if !m.Has(id) {
			t.Errorf("manifest missing %s", id)
		}
	}
}

func TestVMDefineSecondVMCoexists(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("first define failed: %s", stderr)
	}
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gaming",
		"--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("second define failed: %s", stderr)
	}
	vms := steps.VMNames(root)
	if len(vms) != 2 {
		t.Fatalf("VMNames = %v, want both VMs", vms)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-define-win11", "vm-define-gaming", "vm-domain-xml-win11", "vm-domain-xml-gaming"} {
		if !m.Has(id) {
			t.Errorf("manifest missing %s", id)
		}
	}
	// with two VMs managed, undefine and verify must demand --vm-name
	code, _, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 1 || !strings.Contains(stderr, "--vm-name") {
		t.Errorf("undefine without --vm-name: exit %d, stderr %q — want a refusal", code, stderr)
	}
	if code, _, stderr := run(t, "verify", "--root", root); code != 2 || !strings.Contains(stderr, "--vm-name") {
		t.Errorf("verify without --vm-name: exit %d, stderr %q — want a refusal", code, stderr)
	}
	// undefining one VM leaves the other fully intact
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gaming", "--yes", "undefine"); code != 0 {
		t.Fatalf("undefine gaming failed: %s", stderr)
	}
	m, err = steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-domain-xml-gaming", "_ort-run-gaming-lg", "desktop-entry-gaming", "vm-define-gaming"} {
		if m.Has(id) {
			t.Errorf("undefine left %s in the manifest", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/gaming.xml")); err == nil {
		t.Error("undefine left the gaming registry entry — the hook would still answer for it")
	}
	if _, err := os.Stat(filepath.Join(root, "/usr/local/bin/_ort-run-win11-lg")); err != nil {
		t.Error("undefining gaming removed the win11 launcher")
	}
	if !m.Has("vm-define-win11") {
		t.Error("undefining gaming removed the win11 domain record")
	}
}

// Guest settings given at define land in the domain XML's <metadata> block
// and survive a re-define without flags — a rebuild must not silently reset
// credentials, locale, or resolution on an installed guest.
func TestVMDefineGuestSettingsSticky(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes",
		"--guest-user", "pavlo", "--guest-password", "s3cret", "--locale", "uk-UA",
		"--resolution", "2560x1440", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	want := domain.GuestConfig{User: "pavlo", Password: "s3cret", Locale: "uk-UA", Resolution: "2560x1440"}
	if got := domain.ReadGuestConfig(root, "win11"); got != want {
		t.Fatalf("metadata after define = %+v, want %+v", got, want)
	}
	st, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("domain XML carries the password and must be 0600, got %v %v", st.Mode().Perm(), err)
	}

	// re-define without flags keeps everything
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("re-define exit %d\nstderr: %s", code, stderr)
	}
	if got := domain.ReadGuestConfig(root, "win11"); got != want {
		t.Errorf("metadata after re-define = %+v, want %+v", got, want)
	}
}

func TestVMDefineRejectsHostileName(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win$(reboot)",
		"--win11-iso", "/isos/Win11.iso", "--yes", "define")
	if code != 1 || !strings.Contains(stderr, "bad VM name") {
		t.Fatalf("exit %d, stderr %q — want a VM-name validation error", code, stderr)
	}
}
