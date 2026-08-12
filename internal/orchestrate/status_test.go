package orchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

const switcherooWants = "etc/systemd/system/multi-user.target.wants/switcheroo-control.service"

func healthyRoot(t *testing.T) string {
	t.Helper()
	root := rebootedRoot(t)
	hwtest.AddPCI(t, root, hwtest.Dev{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 1})
	for _, p := range hooks.InstalledPaths() {
		write(t, root, p, "#!/bin/bash\n")
	}
	write(t, root, switcherooWants, "")
	return root
}

// A missing check ends the test here rather than at the caller: every caller
// dereferences the result, and staticcheck does not see that a t.Fatal on nil
// stops them (SA5011).
func failing(t *testing.T, cs []Check, prefix, why string) *Check {
	t.Helper()
	for i := range cs {
		if strings.HasPrefix(cs[i].Name, prefix) && !cs[i].OK {
			return &cs[i]
		}
	}
	t.Fatalf("%s: no failing %q check in %+v", why, prefix, cs)
	return nil
}

func TestStatusHealthy(t *testing.T) {
	cs := Status(healthyRoot(t))
	if !Healthy(cs) {
		t.Fatalf("want healthy, got %+v", cs)
	}
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	all := strings.Join(names, ",")
	for _, name := range []string{"kernel arguments", "iommu", "vfio module", "gpu binding", "libvirt hooks", "switcheroo-control"} {
		if !strings.Contains(all, name) {
			t.Errorf("missing check %q in %s", name, all)
		}
	}
}

func TestStatusNothingApplied(t *testing.T) {
	cs := Status(t.TempDir())
	if Healthy(cs) {
		t.Fatal("pristine host must not report healthy")
	}
	if len(cs) != 1 || !strings.Contains(cs[0].Detail, "orthogonals apply") {
		t.Errorf("want a single not-applied check, got %+v", cs)
	}
}

func TestStatusDetectsDrift(t *testing.T) {
	cases := []struct {
		name, check string
		corrupt     func(t *testing.T, root string)
	}{
		{"kernel update dropped kargs", "kernel arguments", func(t *testing.T, root string) {
			write(t, root, "proc/cmdline", "BOOT_IMAGE=vmlinuz rhgb quiet\n")
		}},
		{"gpu lost its driver", "gpu binding", func(t *testing.T, root string) {
			hwtest.AddPCI(t, root, hwtest.Dev{Addr: "0000:02:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Group: 2})
		}},
		{"hooks removed", "libvirt hooks", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "etc/libvirt/hooks/qemu")); err != nil {
				t.Fatal(err)
			}
		}},
		{"switcheroo disabled", "switcheroo-control", func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, switcherooWants)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := healthyRoot(t)
			tc.corrupt(t, root)
			cs := Status(root)
			if Healthy(cs) {
				t.Fatalf("drift not detected: %+v", cs)
			}
			failing(t, cs, tc.check, "check did not fail")
		})
	}
}

// An unreadable hook path is not a missing one: /etc/libvirt is 0700, so a
// status run as the desktop user hits EACCES on a host that is in fact fine,
// and "re-run apply" is advice that cannot resolve it.
func TestStatusHookUnreadableIsNotMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches any directory")
	}
	root := healthyRoot(t)
	dir := filepath.Dir(filepath.Join(root, hooks.InstalledPaths()[0]))
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	c := failing(t, Status(root), "libvirt hooks", "an unreadable hook path must not report healthy")
	if !strings.Contains(c.Detail, "run as root") {
		t.Errorf("detail should say the check could not look, got %q", c.Detail)
	}
	if strings.Contains(c.Detail, "apply") {
		t.Errorf("detail advises an apply that cannot fix EACCES: %q", c.Detail)
	}
}

func TestStatusVFIOBoundIsHealthy(t *testing.T) {
	root := rebootedRoot(t)
	hwtest.AddPCI(t, root, hwtest.Dev{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "vfio-pci", Group: 1})
	for _, p := range hooks.InstalledPaths() {
		write(t, root, p, "#!/bin/bash\n")
	}
	write(t, root, switcherooWants, "")
	if cs := Status(root); !Healthy(cs) {
		t.Errorf("vfio-pci binding (VM running) must be healthy: %+v", cs)
	}
}

func TestStatusUnexpectedDriver(t *testing.T) {
	root := healthyRoot(t)
	hwtest.AddPCI(t, root, hwtest.Dev{Addr: "0000:02:00.0", Vendor: "0x10de", Device: "0x2216", Class: "0x030000", Driver: "pci-stub", Group: 2})
	c := failing(t, Status(root), "gpu binding 0000:02:00.0", "pci-stub-bound GPU must not report healthy")
	if !strings.Contains(c.Detail, "pci-stub") {
		t.Errorf("check should name the unexpected driver, got %q", c.Detail)
	}
}

func TestStatusMissingKernelArgsRecord(t *testing.T) {
	root := healthyRoot(t)
	m := `{"records":[{"id":"hook-qemu-dispatcher","kind":"write_file","path":"/etc/libvirt/hooks/qemu"}]}`
	write(t, root, "var/lib/orthogonals/manifest.json", m)
	c := failing(t, Status(root), "kernel arguments", "missing kernel-args record must surface as a failing check")
	if !strings.Contains(c.Detail, "kernel-args") {
		t.Errorf("detail should point at the missing step, got %q", c.Detail)
	}
}

func writeKVMFRDomain(t *testing.T, root, vm string) {
	t.Helper()
	write(t, root, filepath.Join(steps.VMsDirPath, vm+".xml"),
		`<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>`+vm+`</name>
  <qemu:commandline>
    <qemu:arg value='-object'/>
    <qemu:arg value='{"qom-type":"memory-backend-file","id":"looking-glass","mem-path":"`+
			steps.KVMFRDevice+`","size":134217728,"share":true}'/>
  </qemu:commandline>
</domain>`)
}

func TestStatusKVMFRModuleMissingAfterKernelUpdate(t *testing.T) {
	root := healthyRoot(t)
	writeKVMFRDomain(t, root, "win11")

	c := failing(t, Status(root), "looking glass", "a kvmfr domain without the module must not report healthy")
	for _, want := range []string{"orthogonals up", steps.LookingGlassSHM} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail does not mention %q: %s", want, c.Detail)
		}
	}
}

func TestStatusKVMFRModuleBuilt(t *testing.T) {
	root := healthyRoot(t)
	writeKVMFRDomain(t, root, "win11")
	const release = "7.1.4-204.fc44.x86_64"
	write(t, root, "proc/sys/kernel/osrelease", release+"\n")
	write(t, root, filepath.Join("lib/modules", release, "modules.dep"), "extra/kvmfr.ko.xz:\n")

	cs := Status(root)
	if !Healthy(cs) {
		t.Fatalf("want healthy, got %+v", cs)
	}
	var detail string
	for _, c := range cs {
		if strings.HasPrefix(c.Name, "looking glass") {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "DMABUF") || !strings.Contains(detail, "128 MiB") {
		t.Errorf("backend check does not report the live buffer: %q", detail)
	}
}

func TestStatusSHMBackendReported(t *testing.T) {
	root := healthyRoot(t)
	write(t, root, filepath.Join(steps.VMsDirPath, "win11.xml"),
		"<domain type='kvm'><name>win11</name><shmem name='looking-glass'/></domain>")
	cs := Status(root)
	if !Healthy(cs) {
		t.Fatalf("want healthy, got %+v", cs)
	}
	var found bool
	for _, c := range cs {
		if strings.HasPrefix(c.Name, "looking glass") && strings.Contains(c.Detail, steps.LookingGlassSHM) {
			found = true
		}
	}
	if !found {
		t.Errorf("no /dev/shm backend check in %+v", cs)
	}
}
