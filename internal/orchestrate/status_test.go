package orchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

const switcherooWants = "etc/systemd/system/multi-user.target.wants/switcheroo-control.service"

// healthyRoot is an applied, rebooted host: an NVIDIA dGPU on the host
// driver plus the live boot state and installed hooks the manifest promises.
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

// failing returns the first non-OK check whose name starts with prefix.
func failing(cs []Check, prefix string) *Check {
	for i := range cs {
		if strings.HasPrefix(cs[i].Name, prefix) && !cs[i].OK {
			return &cs[i]
		}
	}
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
			if failing(cs, tc.check) == nil {
				t.Errorf("check %q did not fail: %+v", tc.check, cs)
			}
		})
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
	c := failing(Status(root), "gpu binding 0000:02:00.0")
	if c == nil {
		t.Fatal("pci-stub-bound GPU must not report healthy")
	}
	if !strings.Contains(c.Detail, "pci-stub") {
		t.Errorf("check should name the unexpected driver, got %q", c.Detail)
	}
}

func TestStatusMissingKernelArgsRecord(t *testing.T) {
	// records exist but no kernel-args step (partial apply/undo): the boot
	// config checks must be reported as failing, not silently skipped
	root := healthyRoot(t)
	m := `{"records":[{"id":"hook-qemu-dispatcher","kind":"write_file","path":"/etc/libvirt/hooks/qemu"}]}`
	write(t, root, "var/lib/orthogonals/manifest.json", m)
	c := failing(Status(root), "kernel arguments")
	if c == nil {
		t.Fatal("missing kernel-args record must surface as a failing check")
	}
	if !strings.Contains(c.Detail, "kernel-args") {
		t.Errorf("detail should point at the missing step, got %q", c.Detail)
	}
}
