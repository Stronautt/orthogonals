package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

// applyFakeBins are every binary the apply step list shells out to, plus all
// hw.RequiredTools so preflight's LookPath gate never depends on the real host.
// bash: the lg-client-build step runs the rendered lg-build.sh.
var applyFakeBins = append([]string{
	"dnf", "systemctl", "bash", "usermod",
}, hw.RequiredTools...)

// fakeBinDir installs argv-logging stubs for the named binaries and prepends
// the dir to PATH. Returns the log dir (one <name>.log per binary).
func fakeBinDir(t *testing.T, names []string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, name+".log") + "\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func fakeApplyPath(t *testing.T) string {
	t.Helper()
	dir := fakeBinDir(t, applyFakeBins)
	// systemctl is-enabled must answer, or the engine treats units as absent
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "systemctl.log") +
		"\"\nif [ \"$1\" = \"is-enabled\" ]; then echo enabled; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func binLog(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func runApply(t *testing.T, root string, extra ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"--root", root, "--user", "testuser"}, extra...)
	code := Run(append([]string{"apply"}, args...), &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	dir := fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out := runApply(t, root)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"would run: grubby --update-kernel=ALL --args=intel_iommu=on iommu=pt",
		"a reboot will be required",
		"recovery: ",
		"dry run — re-run with --yes to apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/dracut.conf.d/vfio.conf")); err == nil {
		t.Error("dry run wrote vfio.conf")
	}
	if _, err := os.Stat(filepath.Join(root, "/var/lib/orthogonals/manifest.json")); err == nil {
		t.Error("dry run wrote a manifest")
	}
	if got := binLog(t, dir, "grubby"); got != "" {
		t.Errorf("dry run executed grubby: %s", got)
	}
}

func TestApplyYesWritesEverything(t *testing.T) {
	dir := fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out := runApply(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	for _, path := range []string{
		"/etc/dracut.conf.d/vfio.conf",
		"/etc/udev/rules.d/61-mutter-ignore-nvidia.rules",
		"/etc/environment.d/50-orthogonals-igpu.conf",
		"/etc/tmpfiles.d/looking-glass.conf",
		"/etc/sysconfig/libvirt-guests",
		"/var/lib/orthogonals/lg-build.sh",
		"/var/lib/orthogonals/manifest.json",
		"/etc/libvirt/hooks/qemu",
		"/etc/libvirt/hooks/orthogonals-gpu-detach.sh",
		"/etc/libvirt/hooks/orthogonals-gpu-reattach.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("missing %s after apply --yes", path)
		}
	}
	if st, err := os.Stat(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("qemu hook must be executable, mode = %v", st.Mode().Perm())
	}
	// per-VM launcher and desktop entry are owned by `vm define` now
	if _, err := os.Stat(filepath.Join(root, "/usr/local/bin/_ort-run-win11-lg")); err == nil {
		t.Error("apply must not write the per-VM launcher")
	}
	// the recovery escape hatch is `orthogonals recover`, not an installed script
	if _, err := os.Stat(filepath.Join(root, "/usr/local/bin/gpu-recover.sh")); err == nil {
		t.Error("apply must not install gpu-recover.sh")
	}
	if b, err := os.ReadFile(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err != nil || !strings.Contains(string(b), `VMS_DIR="/etc/orthogonals/vms"`) {
		t.Errorf("dispatcher must gate on the VM registry: %s", b)
	}
	b, err := os.ReadFile(filepath.Join(root, "/etc/tmpfiles.d/looking-glass.conf"))
	if err != nil || !strings.Contains(string(b), "0660 testuser qemu") {
		t.Errorf("tmpfiles entry not rendered for --user: %s", b)
	}

	if got := binLog(t, dir, "grubby"); !strings.Contains(got, "--update-kernel=ALL --args=intel_iommu=on iommu=pt") {
		t.Errorf("grubby invocation = %q", got)
	}
	if got := binLog(t, dir, "bash"); !strings.Contains(got, "/var/lib/orthogonals/lg-build.sh") {
		t.Errorf("lg-build.sh not executed: bash log = %q", got)
	}
	if got := binLog(t, dir, "dracut"); !strings.Contains(got, "-f --regenerate-all") {
		t.Errorf("dracut invocation = %q", got)
	}
	if got := binLog(t, dir, "semanage"); !strings.Contains(got, "fcontext -a -t svirt_tmpfs_t /dev/shm/looking-glass") {
		t.Errorf("semanage invocation = %q", got)
	}
	if got := binLog(t, dir, "virsh"); !strings.Contains(got, "net-autostart default") || !strings.Contains(got, "net-start default") {
		t.Errorf("virsh invocations = %q", got)
	}
	sysctl := binLog(t, dir, "systemctl")
	for _, want := range []string{"disable nvidia-persistenced.service", "enable libvirt-guests.service", "enable switcheroo-control.service"} {
		if !strings.Contains(sysctl, want) {
			t.Errorf("systemctl log missing %q:\n%s", want, sysctl)
		}
	}
	if !strings.Contains(out, "REBOOT REQUIRED — kernel arguments and initramfs changed") {
		t.Errorf("missing reboot banner:\n%s", out)
	}
	if !strings.Contains(out, "intel_iommu=on iommu=pt") {
		t.Errorf("missing escape hatch:\n%s", out)
	}

	// before the reboot lands, a re-apply must keep demanding it
	code, outPending := runApply(t, root, "--yes")
	if code != 0 {
		t.Fatalf("pre-reboot re-apply exit %d\n%s", code, outPending)
	}
	if !strings.Contains(outPending, "REBOOT REQUIRED") {
		t.Errorf("re-apply before the reboot must still demand it:\n%s", outPending)
	}

	// simulate the reboot: journaled args now live on the kernel cmdline
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cmdline"),
		[]byte("BOOT_IMAGE=vmlinuz rhgb quiet intel_iommu=on iommu=pt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// re-apply is a no-op: no reboot banner, no duplicate records
	m1, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	code, out2 := runApply(t, root, "--yes")
	if code != 0 {
		t.Fatalf("re-apply exit %d\n%s", code, out2)
	}
	if strings.Contains(out2, "REBOOT REQUIRED") {
		t.Errorf("no-op re-apply must not demand a reboot:\n%s", out2)
	}
	m2, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Records) != len(m1.Records) {
		t.Errorf("re-apply grew the manifest: %d → %d records", len(m1.Records), len(m2.Records))
	}
}

func TestApplyStaticBinding(t *testing.T) {
	dir := fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out := runApply(t, root, "--yes", "--binding", "static")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if got := binLog(t, dir, "grubby"); !strings.Contains(got, "vfio-pci.ids=10de:2206,10de:1aef") {
		t.Errorf("static binding grubby invocation = %q", got)
	}
	if !strings.Contains(out, "vfio-pci.ids=10de:2206,10de:1aef") {
		t.Errorf("escape hatch must list the static kargs:\n%s", out)
	}
	// static binding leaves the GPU on vfio-pci permanently — a reattach hook
	// would fight it, so no hooks are installed
	if _, err := os.Stat(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err == nil {
		t.Error("static binding must not install libvirt hooks")
	}
}

func TestApplyUndoKeepsPreexistingKargs(t *testing.T) {
	dir := fakeApplyPath(t)
	// grubby reports intel_iommu=on already configured before apply
	script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, "grubby.log") + "\"\n" +
		"if [ \"$1\" = \"--info=ALL\" ]; then echo 'args=\"ro rhgb quiet intel_iommu=on\"'; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "grubby"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := hwtest.ReferenceRoot(t)
	code, out := runApply(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Records {
		if r.ID != "kernel-args" {
			continue
		}
		want := "grubby --update-kernel=ALL --remove-args=iommu=pt"
		if got := strings.Join(r.UndoCmd, " "); got != want {
			t.Errorf("undo = %q, want %q — undo must not remove the user's own intel_iommu=on", got, want)
		}
		return
	}
	t.Fatal("no kernel-args record journaled")
}

func TestApplyBadFlags(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _ := runApply(t, root, "--binding", "sideways"); code != 1 {
		t.Errorf("bad binding: exit %d, want 1", code)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"apply", "--root", root, "--user", ""}, &out, &errOut); code != 1 {
		t.Errorf("empty user: exit %d, want 1\n%s", code, errOut.String())
	}
}

// apply must refuse a host preflight fails — the Overview contract is that
// unsupported hardware is never mutated.
func TestApplyRefusesPreflightFail(t *testing.T) {
	dir := fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	// laptop chassis is a hard preflight refusal
	hwtest.WriteFile(t, root, "sys/class/dmi/id/chassis_type", "10\n")
	code, out := runApply(t, root, "--yes")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"preflight chassis", "laptop", "refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if got := binLog(t, dir, "grubby"); got != "" {
		t.Errorf("refused apply still executed grubby: %s", got)
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals/manifest.json")); err == nil {
		t.Error("refused apply journaled steps")
	}
}

// hosts applied before the `recover` subcommand carry an installed
// /usr/local/bin/gpu-recover.sh and its journal record; re-apply must remove
// both.
func TestApplyRemovesStaleRecoverScript(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	e := &steps.Engine{Root: root, Yes: true, Out: os.Stderr, Err: os.Stderr}
	stale := steps.Step{
		ID: "gpu-recover", Kind: steps.KindWriteFile,
		Path: "/usr/local/bin/gpu-recover.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755,
	}
	if err := e.Apply([]steps.Step{stale}); err != nil {
		t.Fatal(err)
	}

	code, out := runApply(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "/usr/local/bin/gpu-recover.sh")); err == nil {
		t.Error("stale gpu-recover.sh survived re-apply")
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("gpu-recover") {
		t.Error("stale gpu-recover journal record survived re-apply")
	}
}
