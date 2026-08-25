package cli

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// bootConfigCarries reports whether any boot-config file under root mentions
// tok. Wanted answers per target; these tests want the union across all of them,
// which is the question "did anything write to the boot config at all".
func bootConfigCarries(t *testing.T, root, tok string) bool {
	t.Helper()
	carries := false
	for _, p := range []string{
		filepath.Join(root, bls.EntriesPath),
		filepath.Join(root, bls.KernelCmdlinePath),
		filepath.Join(root, bls.GrubDefaultsPath),
	} {
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // an absent target is not a carrier
			}
			b, readErr := os.ReadFile(path)
			if readErr == nil && strings.Contains(string(b), tok) {
				carries = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", p, err)
		}
	}
	return carries
}

// applyFakeBins lists every binary apply shells out to, plus hw.RequiredTools.
var applyFakeBins = append([]string{"systemctl"}, hw.RequiredTools...)

// fakeBinDir installs argv-logging stubs on PATH and returns the log dir.
func fakeBinDir(t *testing.T, names []string) string {
	t.Helper()
	return hwtest.LoggingTools(t, names...)
}

// enabledSystemctl answers is-enabled as well as recording its argv.
const enabledSystemctl = "if [ \"$1\" = \"is-enabled\" ]; then echo enabled; fi"

func fakeApplyPath(t *testing.T) string {
	t.Helper()
	dir := fakeBinDir(t, applyFakeBins)
	hwtest.FakeTool(t, dir, "systemctl", enabledSystemctl)
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

func runApplyCLI(t *testing.T, root string, extra ...string) (int, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"--root", root, "--user", "testuser"}, extra...)
	code := Run(append([]string{"apply"}, args...), &out, &errOut)
	return code, out.String() + errOut.String()
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out := runApplyCLI(t, root)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"would: kernel-args-add args=intel_iommu=on iommu=pt",
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
	if bootConfigCarries(t, root, "intel_iommu=on") {
		t.Error("dry run edited the boot config")
	}
}

func TestApplyYesWritesEverything(t *testing.T) {
	dir := fakeApplyPath(t)
	fv := fakeVirt(t, &virttest.Fake{})
	fs := fakeSysd(t, &sysdtest.Fake{States: map[string]string{
		"nvidia-persistenced.service": "enabled",
		"libvirt-guests.service":      "disabled",
		"switcheroo-control.service":  "disabled",
	}})
	root := hwtest.ReferenceRoot(t)
	code, out := runApplyCLI(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	for _, path := range []string{
		"/etc/dracut.conf.d/vfio.conf",
		"/etc/udev/rules.d/61-mutter-ignore-nvidia.rules",
		"/etc/environment.d/50-orthogonals-igpu.conf",
		"/etc/tmpfiles.d/looking-glass.conf",
		"/etc/sysconfig/libvirt-guests",
		"/var/lib/orthogonals/manifest.json",
		"/etc/libvirt/hooks/qemu",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("missing %s after apply --yes", path)
		}
	}
	if st, err := os.Stat(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("qemu hook must be executable, mode = %v", st.Mode().Perm())
	}
	if b, err := os.ReadFile(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err != nil ||
		!strings.Contains(string(b), "hook --user testuser qemu") {
		t.Errorf("hook shim must exec the binary: %s", b)
	}
	b, err := os.ReadFile(filepath.Join(root, "/etc/tmpfiles.d/looking-glass.conf"))
	if err != nil || !strings.Contains(string(b), "0660 testuser qemu") {
		t.Errorf("tmpfiles entry not rendered for --user: %s", b)
	}

	if w, _ := bls.Wanted(root, "intel_iommu=on iommu=pt"); len(w.Missing) > 0 {
		t.Errorf("boot config missing IOMMU kargs after apply: %v", w.Missing)
	}
	if got := binLog(t, dir, "dracut"); !strings.Contains(got, "-f --regenerate-all") {
		t.Errorf("dracut invocation = %q", got)
	}
	if got := binLog(t, dir, "semanage"); !strings.Contains(got, "fcontext -a -t svirt_tmpfs_t /dev/shm/looking-glass") {
		t.Errorf("semanage invocation = %q", got)
	}
	if !fv.Logged("net-autostart default") || !fv.Logged("net-active default") {
		t.Errorf("network ops never reached libvirt: %v", fv.Calls)
	}
	for _, want := range []string{"disable nvidia-persistenced.service", "enable libvirt-guests.service", "enable switcheroo-control.service"} {
		if !fs.Logged(want) {
			t.Errorf("systemd calls missing %q: %v", want, fs.Calls)
		}
	}
	if !fs.Logged("restart virtqemud.socket") || !fs.Logged("restart virtnetworkd.socket") {
		t.Errorf("socket reload op incomplete: %v", fs.Calls)
	}
	if !strings.Contains(out, "REBOOT REQUIRED — kernel arguments and initramfs changed") {
		t.Errorf("missing reboot banner:\n%s", out)
	}
	if !strings.Contains(out, "intel_iommu=on iommu=pt") {
		t.Errorf("missing escape hatch:\n%s", out)
	}

	code, outPending := runApplyCLI(t, root, "--yes")
	if code != 0 {
		t.Fatalf("pre-reboot re-apply exit %d\n%s", code, outPending)
	}
	if !strings.Contains(outPending, "REBOOT REQUIRED") {
		t.Errorf("re-apply before the reboot must still demand it:\n%s", outPending)
	}

	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/cmdline"),
		[]byte("BOOT_IMAGE=vmlinuz rhgb quiet intel_iommu=on iommu=pt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m1, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	code, out2 := runApplyCLI(t, root, "--yes")
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
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	code, out := runApplyCLI(t, root, "--yes", "--binding", "static")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if w, _ := bls.Wanted(root, "vfio-pci.ids=10de:2206,10de:1aef"); len(w.Missing) > 0 {
		t.Errorf("static binding must add vfio-pci.ids to the boot config: %v", w.Missing)
	}
	if !strings.Contains(out, "vfio-pci.ids=10de:2206,10de:1aef") {
		t.Errorf("escape hatch must list the static kargs:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "/etc/libvirt/hooks/qemu")); err == nil {
		t.Error("static binding must not install libvirt hooks")
	}
}

// A kernel update regenerates the BLS entries from /etc/kernel/cmdline, so the
// journaled kernel-args step is no proof the args are still on the host: apply
// re-adds them, and undo still strips everything the first apply added.
func TestApplyRepairsKargsLostToAKernelUpdate(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, out := runApplyCLI(t, root, "--yes"); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}

	entries, err := filepath.Glob(filepath.Join(root, "boot/loader/entries/*.conf"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no boot entries: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			t.Fatal(err)
		}
		stripped := strings.NewReplacer(" intel_iommu=on", "", " iommu=pt", "").Replace(string(b))
		if err := os.WriteFile(e, []byte(stripped), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	code, out := runApplyCLI(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if w, _ := bls.Wanted(root, "intel_iommu=on iommu=pt"); len(w.Missing) > 0 {
		t.Errorf("apply left %v off the boot config — a journaled step is not a live one:\n%s", w.Missing, out)
	}

	if code, stdout, stderr := run(t, "undo", "--root", root, "--yes"); code != 0 {
		t.Fatalf("undo exit %d\n%s%s", code, stdout, stderr)
	}
	for _, tok := range []string{"intel_iommu=on", "iommu=pt"} {
		if bootConfigCarries(t, root, tok) {
			t.Errorf("undo after a repair left %s on the boot config", tok)
		}
	}
}

func kernelArgsRecord(t *testing.T, m *steps.Manifest) steps.Record {
	t.Helper()
	for _, r := range m.Records {
		if r.ID == "kernel-args" {
			return r
		}
	}
	t.Fatal("no kernel-args record journaled")
	return steps.Record{}
}

// A host whose boot-config targets disagree before apply: one entry already
// carries intel_iommu=on and nothing else does. Undo owes that entry its token
// back and owes every other target the removal of what apply put there — a
// single union of removals gets one of those two wrong whichever way it leans.
func TestApplyUndoKeepsPreexistingKargs(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	hwtest.WriteFile(t, root, "boot/loader/entries/fedora-6.15.0.conf",
		"title Fedora Linux (6.15.0) 44\noptions root=UUID=aaaa ro rhgb quiet intel_iommu=on\n")
	bootFiles := []string{
		"boot/loader/entries/fedora-6.15.0.conf",
		"boot/loader/entries/fedora-6.14.0.conf",
		"etc/kernel/cmdline",
		"etc/default/grub",
	}
	before := map[string]string{}
	for _, f := range bootFiles {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		before[f] = string(b)
	}

	code, out := runApplyCLI(t, root, "--yes")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m, err := steps.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	rec := kernelArgsRecord(t, m)
	want := map[string]string{
		"/boot/loader/entries/fedora-6.15.0.conf": "iommu=pt",
		"/boot/loader/entries/fedora-6.14.0.conf": "intel_iommu=on iommu=pt",
		bls.KernelCmdlinePath:                     "intel_iommu=on iommu=pt",
		bls.GrubDefaultsPath:                      "intel_iommu=on iommu=pt",
	}
	if rec.UndoOp != steps.OpKernelArgsRem || !maps.Equal(rec.UndoArgs, want) {
		t.Errorf("undo = %s %v,\nwant %v", rec.UndoOp, rec.UndoArgs, want)
	}

	if code, stdout, stderr := run(t, "undo", "--root", root, "--yes"); code != 0 {
		t.Fatalf("undo exit %d\n%s%s", code, stdout, stderr)
	}
	for _, f := range bootFiles {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != before[f] {
			t.Errorf("undo did not restore %s:\nwas %q\nnow %q", f, before[f], b)
		}
	}
}

func TestApplyBadFlags(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	if code, _ := runApplyCLI(t, root, "--binding", "sideways"); code != 1 {
		t.Errorf("bad binding: exit %d, want 1", code)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"apply", "--root", root, "--user", ""}, &out, &errOut); code != 1 {
		t.Errorf("empty user: exit %d, want 1\n%s", code, errOut.String())
	}
}

func TestApplyRefusesPreflightFail(t *testing.T) {
	fakeApplyPath(t)
	root := hwtest.ReferenceRoot(t)
	// force a preflight Fail: the dGPU loses its function-level reset file.
	if err := os.Remove(filepath.Join(root, "sys/bus/pci/devices/0000:01:00.0/reset")); err != nil {
		t.Fatal(err)
	}
	code, out := runApplyCLI(t, root, "--yes")
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"preflight gpu-reset", "reset", "refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if bootConfigCarries(t, root, "intel_iommu=on") {
		t.Error("refused apply still edited the boot config")
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals/manifest.json")); err == nil {
		t.Error("refused apply journaled steps")
	}
}

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

	code, out := runApplyCLI(t, root, "--yes")
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
