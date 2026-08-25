package cli

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// vmFakeBins are every binary the vm step list still shells out to.
var vmFakeBins = []string{"semanage", "restorecon", "systemd-tmpfiles"}

func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// mustRead fails the test rather than folding an unreadable registry into
// "this VM has no settings".
func mustRead(t *testing.T, root, vm string) domain.Settings {
	t.Helper()
	s, err := domain.ReadSettings(root, vm)
	if err != nil {
		t.Fatalf("read registered settings for %s: %v", vm, err)
	}
	return s
}

func fakeVMPath(t *testing.T) string {
	t.Helper()
	t.Setenv("SUDO_USER", "testuser")
	return fakeBinDir(t, vmFakeBins)
}

func TestVMDefineDryRun(t *testing.T) {
	fakeVMPath(t)
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

	if code, _, _ := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("re-run exit %d", code)
	}
	if volCalls := countCalls(f.Calls, "vol-create"); volCalls != 1 {
		t.Errorf("re-run created the volume again (%d creations)", volCalls)
	}
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
	// Rendering the XML is not defining the domain: without this the test passes
	// on a run that never reached libvirt at all.
	if n := countCalls(f.Calls, "define"); n != 2 {
		t.Errorf("libvirt saw %d defines, want one per run: %v", n, f.Calls)
	}
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
	if got := countCalls(f.Calls, "define"); got != 2 {
		t.Errorf("define reached libvirt %d times, want 2 (install + final redefine)", got)
	}
	if code, _, _ := run(t, "vm", "--root", root, "--yes", "define"); code != 0 {
		t.Fatal("converged re-run failed")
	}
	if got := countCalls(f.Calls, "define"); got != 2 {
		t.Errorf("converged re-run re-defined again (%d invocations)", got)
	}
}

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
	fakeVMPath(t)
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

func TestVMUndefineRemovesProvisionISOWithoutPurge(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes", "define"); code != 0 {
		t.Fatalf("define failed: %s", stderr)
	}
	iso := media.ISOPath(root, "win11")
	if err := os.MkdirAll(filepath.Dir(iso), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(iso, []byte("provision"), 0o600); err != nil {
		t.Fatal(err)
	}
	fakeVirt(t, &virttest.Fake{})

	if code, stdout, _ := run(t, "vm", "--root", root, "undefine"); code != 0 ||
		!strings.Contains(stdout, "would remove the provision ISO") {
		t.Fatalf("dry run: code=%d stdout=%q", code, stdout)
	}
	if _, err := os.Stat(iso); err != nil {
		t.Fatalf("dry run deleted the ISO: %v", err)
	}

	code, stdout, stderr := run(t, "vm", "--root", root, "--yes", "undefine")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(iso); !os.IsNotExist(err) {
		t.Errorf("undefine left the provision ISO on disk: %v", err)
	}
	if !strings.Contains(stdout, "removed the provision ISO") {
		t.Errorf("removal not reported:\n%s", stdout)
	}
}

func TestVMUndefinePurgeRemovesEverything(t *testing.T) {
	dir := fakeVMPath(t)
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

// The message matters as much as the exit code: a name rejected downstream
// fails for another reason.
func TestVMNameValidatedOnEverySubcommand(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	const hostile = "../../etc/passwd"
	cases := map[string][]string{
		"launch":   {"vm", "--root", root, "--vm-name", hostile, "launch"},
		"undefine": {"vm", "--root", root, "--vm-name", hostile, "undefine"},
		"media":    {"media", "--root", root, "--vm-name", hostile, "--win11-iso", "/isos/w.iso"},
		"verify":   {"verify", "--root", root, "--vm-name", hostile},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := run(t, args...)
			if code == 0 {
				t.Fatalf("%s accepted %q", name, hostile)
			}
			if !strings.Contains(stderr, "bad VM name") {
				t.Errorf("%s refused %q for the wrong reason: %q", name, hostile, stderr)
			}
		})
	}
}

// The registry is the whole record: no config.json alongside it.
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
	if got := mustRead(t, root, "work").DisplayName; got != "Work PC" {
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
	if st, err := os.Stat(filepath.Join(root, "/usr/share/applications/work.orthogonals.desktop")); err != nil {
		t.Errorf("desktop entry not written: %v", err)
	} else if st.Mode().Perm() != 0o755 {
		t.Errorf("desktop entry mode = %04o, must be executable to launch from ~/Desktop", st.Mode().Perm())
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

func TestVMDefineSharesSticky(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	for _, dir := range []string{"/srv/docs", "/srv/media"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	define := func(t *testing.T, extra ...string) {
		t.Helper()
		args := append([]string{"vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes"}, extra...)
		if code, _, stderr := run(t, append(args, "define")...); code != 0 {
			t.Fatalf("exit %d\nstderr: %s", code, stderr)
		}
	}
	define(t, "--share", "/srv/docs", "--share", "/srv/media")
	want := []string{"/srv/docs", "/srv/media"}
	if got := mustRead(t, root, "win11").Shares; !reflect.DeepEqual(got, want) {
		t.Fatalf("shares after define = %q, want %q", got, want)
	}
	xmlPath := filepath.Join(root, "/etc/orthogonals/vms/win11.xml")
	b, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{"<access mode='shared'/>", "<source dir='/srv/docs'/>", "<target dir='media'/>"} {
		if !strings.Contains(string(b), frag) {
			t.Errorf("domain XML is missing %q", frag)
		}
	}

	define(t)
	if got := mustRead(t, root, "win11").Shares; !reflect.DeepEqual(got, want) {
		t.Errorf("a converge with no --share dropped the shares: %q", got)
	}

	define(t, "--share", "")
	if got := mustRead(t, root, "win11").Shares; len(got) != 0 {
		t.Errorf(`--share "" left shares %q`, got)
	}
	if b, err := os.ReadFile(xmlPath); err != nil || strings.Contains(string(b), "virtiofs") {
		t.Errorf("clearing the shares left the virtiofs devices behind (err %v)", err)
	}
}

// The guest mount services are made during provisioning only, so a share added
// after the install reaches the domain and mounts nothing. Silence there looks
// like success until the drive letter is missing in Windows.
func TestVMDefineWarnsAboutSharesAddedAfterInstall(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	for _, dir := range []string{"/srv/docs", "/srv/media"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	define := func(t *testing.T, extra ...string) string {
		t.Helper()
		args := append([]string{"vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes"}, extra...)
		code, stdout, stderr := run(t, append(args, "define")...)
		if code != 0 {
			t.Fatalf("exit %d\nstderr: %s", code, stderr)
		}
		return stdout
	}

	if out := define(t, "--share", "/srv/docs"); strings.Contains(out, "already installed") {
		t.Errorf("a share registered at install time provisions normally; no warning is due:\n%s", out)
	}
	out := define(t, "--stage", "final", "--share", "/srv/docs", "--share", "/srv/media")
	if !strings.Contains(out, "already installed") {
		t.Fatalf("a share added after the install warned nothing:\n%s", out)
	}
	// The service and drive come from the share's position, so the instructions
	// are wrong for share two if it is described with share one's identity.
	for _, frag := range []string{"/srv/media", "Y:"} {
		if !strings.Contains(out, frag) {
			t.Errorf("the instructions do not name %q:\n%s", frag, out)
		}
	}
	if out := define(t, "--stage", "final", "--share", ""); !strings.Contains(out, "sc.exe delete") {
		t.Errorf("clearing the shares leaves orphan services in the guest unmentioned:\n%s", out)
	}
}

func TestVMDefineRefusesAMissingShare(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	code, _, stderr := run(t, "vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes",
		"--share", "/srv/not-there", "define")
	if code == 0 {
		t.Fatal("define accepted a share directory that does not exist — the VM would fail to start")
	}
	if !strings.Contains(stderr, "/srv/not-there") {
		t.Errorf("the refusal does not name the bad path: %s", stderr)
	}
}

// settingsFlags gives every domain.Settings field the flag that sets it and the
// value the flag registers.
var settingsFlags = map[string]struct{ flag, arg, want string }{
	"DisplayName":   {"--display-name", "Work PC", "Work PC"},
	"User":          {"--user", "pavlo", "pavlo"},
	"RAMGiB":        {"--ram", "12", "12"},
	"Disk":          {"--disk", "/tank/win11.qcow2", "/tank/win11.qcow2"},
	"DiskSizeGiB":   {"--disk-size", "222", "222"},
	"Resolution":    {"--resolution", "2560x1440", "2560x1440"},
	"GuestUser":     {"--guest-user", "pavlo", "pavlo"},
	"GuestPassword": {"--guest-password", "s3cret", "s3cret"},
	"Locale":        {"--locale", "uk-UA", "uk-UA"},
	"Win11ISO":      {"--win11-iso", "/isos/Win11.iso", "/isos/Win11.iso"},
	// registered as the installed copy, never the source the flag named
	"GPUROM": {"--gpu-rom", "", "/var/lib/orthogonals/vbios/win11.rom"},
	"Shares": {"--share", "/srv/docs", "[/srv/docs]"},
}

// Every knob given once must survive the flag-less converge `up` runs on a
// defined VM. Walked by reflection against domain.Settings, so a knob that does
// not exist yet is covered too.
func TestVMSettingsAllSticky(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "/srv/docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	rom := filepath.Join(t.TempDir(), "vbios.rom")
	if err := os.WriteFile(rom, []byte{0x55, 0xaa, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}

	fields := reflect.VisibleFields(reflect.TypeOf(domain.Settings{}))
	args := []string{"vm", "--root", root, "--yes"}
	for _, f := range fields {
		tc, ok := settingsFlags[f.Name]
		if !ok {
			t.Fatalf("domain.Settings.%s has no entry in settingsFlags — add one, "+
				"or the knob ships without anything proving it survives a converge", f.Name)
		}
		args = append(args, tc.flag, cmp.Or(tc.arg, rom))
	}
	if code, _, stderr := run(t, append(args, "define")...); code != 0 {
		t.Fatalf("define exit %d\nstderr: %s", code, stderr)
	}

	registered := func(t *testing.T, when string) {
		t.Helper()
		v := reflect.ValueOf(mustRead(t, root, "win11"))
		for _, f := range fields {
			got := fmt.Sprintf("%v", v.FieldByIndex(f.Index).Interface())
			if want := settingsFlags[f.Name].want; got != want {
				t.Errorf("%s: %s = %q, want %q", when, f.Name, got, want)
			}
		}
	}
	registered(t, "after define")

	// The converge: no flags at all, which is what `up --yes` reaches here with.
	if code, _, stderr := run(t, "vm", "--root", root, "--yes", "define"); code != 0 {
		t.Fatalf("converge exit %d\nstderr: %s", code, stderr)
	}
	registered(t, "after a flag-less converge")

	// And through a stage transition, the other flag-less re-render `up` runs.
	if code, _, stderr := run(t, "vm", "--root", root, "--stage", "final", "--yes", "define"); code != 0 {
		t.Fatalf("stage re-render exit %d\nstderr: %s", code, stderr)
	}
	registered(t, "after a stage re-render")

	// Re-resolving what was just registered must change nothing, or the next
	// define renders different XML and the define op re-runs on every converge.
	// This record is the only one carrying the vBIOS and the shares, the two
	// knobs resolveSettings rewrites rather than copies.
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	prev := mustRead(t, root, "win11")
	resolved, _, err := resolveSettings(root, m, vmOpts{vmName: "win11"}, prev)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resolved, prev) {
		t.Errorf("re-resolving the registered record changed it:\n got %+v\nwant %+v", resolved, prev)
	}

	st, err := os.Stat(filepath.Join(root, "/etc/orthogonals/vms/win11.xml"))
	if err != nil {
		t.Fatalf("domain XML not written: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("domain XML mode = %04o, want 0600: it carries the guest password", st.Mode().Perm())
	}
}

// A later flag beats the registered value, and the record is not the only thing
// that follows it — the rendered domain has to move too.
func TestVMSettingsFlagOverridesRegistered(t *testing.T) {
	fakeVMPath(t)
	root := hwtest.ReferenceRoot(t)
	define := func(extra ...string) {
		t.Helper()
		args := append([]string{"vm", "--root", root, "--win11-iso", "/isos/Win11.iso", "--yes"}, extra...)
		if code, _, stderr := run(t, append(args, "define")...); code != 0 {
			t.Fatalf("exit %d\nstderr: %s", code, stderr)
		}
	}
	define("--ram", "12", "--locale", "uk-UA")
	define("--ram", "10")
	s := mustRead(t, root, "win11")
	if s.RAMGiB != 10 || s.Locale != "uk-UA" {
		t.Errorf("second define = ram %d locale %q, want ram 10 locale uk-UA", s.RAMGiB, s.Locale)
	}
	// The registered settings and the rendered domain must agree. A flag that
	// stays in the record but not in the XML boots the guest at the old size.
	xml, err := os.ReadFile(filepath.Join(steps.VMsDir(root), "win11.xml"))
	if err != nil {
		t.Fatalf("read the rendered domain: %v", err)
	}
	if want := "<memory unit='MiB'>10240</memory>"; !strings.Contains(string(xml), want) {
		t.Errorf("the domain does not render %q", want)
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
