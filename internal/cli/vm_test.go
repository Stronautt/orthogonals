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
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// vmFakeBins are every binary the vm step list still shells out to.
var vmFakeBins = []string{"semanage", "restorecon"}

// countCalls counts fake-client calls whose verb prefix matches.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func fakeVMPath(t *testing.T) string {
	t.Helper()
	t.Setenv("SUDO_USER", "testuser")
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
		"would: create-volume path=/var/lib/libvirt/images/win11.qcow2 size-gib=100",
		"would: define-domain name=win11 xml=/etc/orthogonals/vms/win11.xml",
		"dry run — re-run with --yes to apply",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml")); err == nil {
		t.Error("dry run wrote the domain XML")
	}
	_ = dir
}

func TestVMDefineApplies(t *testing.T) {
	fakeVMPath(t)
	f := fakeVirt(t, &virttest.Fake{})
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
	if !f.Logged("define") {
		t.Errorf("define never reached libvirt: %v", f.Calls)
	}
	if !f.Logged("vol-create /var/lib/libvirt/images/win11.qcow2 100G") {
		t.Errorf("volume never created: %v", f.Calls)
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

	before := len(f.Calls)
	if code, _, _ := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("re-run exit %d", code)
	}
	if volCalls := countCalls(f.Calls, "vol-create"); volCalls != 1 {
		t.Errorf("re-run created the volume again (%d creations)", volCalls)
	}
	_ = before
}

func TestVMDefineGPURom(t *testing.T) {
	fakeVMPath(t)
	f := fakeVirt(t, &virttest.Fake{UUID: "1c07f749-5d72-4e9e-9be1-178cb6d28cd3"})
	root := hwtest.ReferenceRoot(t)

	src := filepath.Join(t.TempDir(), "vbios.rom")
	if err := os.WriteFile(src, []byte{0x55, 0xaa, 0x11, 0x22}, 0o644); err != nil {
		t.Fatal(err)
	}
	const canonical = "/var/lib/orthogonals/vbios/win11.rom"

	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso",
		"--gpu-rom", src, "--yes", "define"); code != 0 {
		t.Fatalf("define exit %d\nstderr: %s", code, stderr)
	}
	installed, err := os.ReadFile(filepath.Join(root, canonical))
	if err != nil {
		t.Fatalf("vBIOS not installed: %v", err)
	}
	if string(installed) != "\x55\xaa\x11\x22" {
		t.Errorf("installed vBIOS bytes = %q", installed)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<rom file='"+canonical+"'/>") {
		t.Errorf("domain XML missing the rom file:\n%s", xml)
	}

	// A later stage re-render without --gpu-rom keeps the registered vBIOS.
	if code, _, stderr := run(t, "vm", "--root", root, "--stage", "final", "--yes", "define"); code != 0 {
		t.Fatalf("final-stage redefine exit %d\nstderr: %s", code, stderr)
	}
	xml, err = os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<rom file='"+canonical+"'/>") {
		t.Errorf("stage re-render dropped the vBIOS:\n%s", xml)
	}
	_ = f
}

func TestVMDefineGPURomRefusals(t *testing.T) {
	fakeVMPath(t)
	fakeVirt(t, &virttest.Fake{})
	root := hwtest.ReferenceRoot(t)

	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/i.iso",
		"--gpu-rom", "/no/such/rom", "--yes", "define")
	if code == 0 || !strings.Contains(stderr, "read --gpu-rom") {
		t.Errorf("missing vBIOS: code=%d stderr=%q", code, stderr)
	}

	bad := filepath.Join(t.TempDir(), "bad.rom")
	if err := os.WriteFile(bad, []byte("not a rom"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = run(t, "vm", "--root", root, "--win11-iso", "/i.iso",
		"--gpu-rom", bad, "--yes", "define")
	if code == 0 || !strings.Contains(stderr, "0x55 0xAA") {
		t.Errorf("bad-signature vBIOS: code=%d stderr=%q", code, stderr)
	}
}

// a release rendering different domain XML converges an installed VM.
func TestVMDefineRedefineConverges(t *testing.T) {
	fakeVMPath(t)
	f := fakeVirt(t, &virttest.Fake{UUID: "1c07f749-5d72-4e9e-9be1-178cb6d28cd3"})
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso",
		"--disk", "/tank/win11.qcow2", "--yes", "define"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if code, _, stderr := run(t, "vm", "--root", root, "--stage", "final", "--yes", "define"); code != 0 {
		t.Fatalf("final-stage redefine exit %d\nstderr: %s", code, stderr)
	}
	xml, err := os.ReadFile(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(xml), "<model type='none'/>") || strings.Contains(string(xml), "cdrom") {
		t.Errorf("redefine must render the post-install state:\n%s", xml)
	}
	if !strings.Contains(string(xml), "<uuid>1c07f749-5d72-4e9e-9be1-178cb6d28cd3</uuid>") {
		t.Errorf("redefine must carry the existing domain's UUID:\n%s", xml)
	}
	if !strings.Contains(string(xml), "<source file='/tank/win11.qcow2'/>") {
		t.Errorf("redefine must render the journaled disk path:\n%s", xml)
	}
	if got := countCalls(f.Calls, "vol-create"); got != 1 {
		t.Errorf("volume created %d times, want 1", got)
	}
	defines := func() int {
		n := 0
		for _, c := range f.Calls {
			if c == "define" {
				n++
			}
		}
		return n
	}
	if got := defines(); got != 2 {
		t.Errorf("define reached libvirt %d times, want 2 (install + final redefine)", got)
	}
	if code, _, _ := run(t, "vm", "--root", root, "--yes", "define"); code != 0 {
		t.Fatal("converged re-run failed")
	}
	if got := defines(); got != 2 {
		t.Errorf("converged re-run re-defined again (%d invocations)", got)
	}
}

// a fresh define with no journaled detach steps requires the ISO.
func TestVMDefineFreshRequiresISO(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "define"); code != 2 || !strings.Contains(stderr, "--win11-iso") {
		t.Fatalf("fresh define without ISO must exit 2 with usage, got %d: %s", code, stderr)
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
	f := fakeVirt(t, &virttest.Fake{})
	code, stdout, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !f.Logged("undefine win11") {
		t.Errorf("undefine never reached libvirt: %v", f.Calls)
	}
	if !strings.Contains(stdout, "removed /home/testuser/Desktop/win11.orthogonals.desktop") {
		t.Errorf("undefine must remove the ~/Desktop link:\n%s", stdout)
	}
	_ = dir
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
	if m.Has("vm-domain-xml-win11") {
		t.Error("undefine left the domain XML record")
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml")); err == nil {
		t.Error("undefine left the registry entry — the hook still answers for the VM")
	}
}

func TestVMUndefinePurgeRemovesEverything(t *testing.T) {
	dir := fakeVMPath(t)
	_ = dir
	f := fakeVirt(t, &virttest.Fake{})
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	hwtest.WriteFile(t, root, "var/lib/orthogonals/state.json", `{"state":"verified"}`)

	code, stdout, stderr := run(t, "vm", "--root", root, "undefine", "--purge", "--yes")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !f.Logged("undefine win11") {
		t.Errorf("undefine never reached libvirt: %v", f.Calls)
	}
	if !strings.Contains(stdout, "removed /var/lib/libvirt/images/win11.qcow2") {
		t.Errorf("purge must remove the disk image:\n%s", stdout)
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

	code, stdout, stderr := run(t, "vm", "--root", root, "undefine", "--purge")
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
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	fakeVirt(t, &virttest.Fake{State: "running"})
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
		"create-volume path=/tank/vm.qcow2 size-gib=200",
		"define-domain name=gamer xml=/etc/orthogonals/vms/gamer.xml",
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

// vm define registers the domain name in the registry with no side config file.
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
	desktop, err := os.ReadFile(filepath.Join(root, "/usr/share/applications/work.orthogonals.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Name=Work PC", "Exec=/usr/bin/orthogonals vm launch --vm-name work"} {
		if !strings.Contains(string(desktop), want) {
			t.Errorf("desktop entry missing %q:\n%s", want, desktop)
		}
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"desktop-entry-work", "desktop-link-work"} {
		if !m.Has(id) {
			t.Errorf("manifest missing %s", id)
		}
	}
	if st, err := os.Stat(filepath.Join(root, "/usr/share/applications/work.orthogonals.desktop")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("desktop entry must be executable to launch from ~/Desktop, got %v %v", st.Mode().Perm(), err)
	}
	// ~/Desktop gets a symlink, not a copy, so a re-rendered entry is picked up.
	link := filepath.Join(root, "/home/testuser/Desktop/work.orthogonals.desktop")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("~/Desktop did not get a link: %v", err)
	}
	if want := "/usr/share/applications/work.orthogonals.desktop"; target != want {
		t.Errorf("link points at %q, want %q", target, want)
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
	code, _, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 1 || !strings.Contains(stderr, "--vm-name") {
		t.Errorf("undefine without --vm-name: exit %d, stderr %q — want a refusal", code, stderr)
	}
	if code, _, stderr := run(t, "verify", "--root", root); code != 2 || !strings.Contains(stderr, "--vm-name") {
		t.Errorf("verify without --vm-name: exit %d, stderr %q — want a refusal", code, stderr)
	}
	if code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "gaming", "--yes", "undefine"); code != 0 {
		t.Fatalf("undefine gaming failed: %s", stderr)
	}
	m, err = steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-domain-xml-gaming", "desktop-entry-gaming", "desktop-link-gaming", "vm-define-gaming"} {
		if m.Has(id) {
			t.Errorf("undefine left %s in the manifest", id)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/gaming.xml")); err == nil {
		t.Error("undefine left the gaming registry entry — the hook would still answer for it")
	}
	if _, err := os.Stat(filepath.Join(root, "/usr/share/applications/win11.orthogonals.desktop")); err != nil {
		t.Error("undefining gaming removed the win11 desktop entry")
	}
	if !m.Has("vm-define-win11") {
		t.Error("undefining gaming removed the win11 domain record")
	}
}

// guest settings given at define land in the domain XML and survive a re-define.
func TestVMDefineGuestSettingsSticky(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes",
		"--guest-user", "pavlo", "--guest-password", "s3cret", "--locale", "uk-UA",
		"--resolution", "2560x1440", "define")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	want := domain.GuestConfig{User: "pavlo", Password: "s3cret", Locale: "uk-UA", Resolution: "2560x1440", Win11ISO: "/isos/Win11.iso"}
	if got := domain.ReadGuestConfig(root, "win11"); got != want {
		t.Fatalf("metadata after define = %+v, want %+v", got, want)
	}
	st, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("domain XML carries the password and must be 0600, got %v %v", st.Mode().Perm(), err)
	}

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
