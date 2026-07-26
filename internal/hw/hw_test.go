package hw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestDetectReference(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))

	r, err := Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Devices) != 3 {
		t.Errorf("Devices = %d, want 3", len(r.Devices))
	}
	if r.GPUs.IGPU == nil || r.GPUs.IGPU.Address != "0000:00:02.0" {
		t.Errorf("IGPU = %+v, want 0000:00:02.0", r.GPUs.IGPU)
	}
	if len(r.GPUs.DGPUs) != 1 {
		t.Fatalf("DGPUs = %d, want 1", len(r.GPUs.DGPUs))
	}
	dgpu := r.GPUs.DGPUs[0]
	if dgpu.Address != "0000:01:00.0" || dgpu.Driver != "nvidia" {
		t.Errorf("dGPU = %+v, want 0000:01:00.0/nvidia", dgpu.PCIDevice)
	}
	if dgpu.Audio == nil || dgpu.Audio.Address != "0000:01:00.1" {
		t.Errorf("dGPU audio = %+v, want 0000:01:00.1", dgpu.Audio)
	}
	if r.CPU.Threads != 20 || r.CPU.Cores != 14 {
		t.Errorf("CPU = %d cores / %d threads, want 14/20", r.CPU.Cores, r.CPU.Threads)
	}
	if r.Platform.IOMMUAddressWidth != 39 {
		t.Errorf("IOMMUAddressWidth = %d, want 39", r.Platform.IOMMUAddressWidth)
	}
	if r.Platform.IOMMUTable != IOMMUTableDMAR {
		t.Errorf("IOMMUTable = %q, want DMAR (reference root has the ACPI DMAR table)", r.Platform.IOMMUTable)
	}
}

func TestDetectLaptopFixture(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))
	root := t.TempDir()
	if err := hwtest.BuildLaptopRoot(root); err != nil {
		t.Fatal(err)
	}
	r, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLaptopChassis(r.Platform.ChassisType) {
		t.Errorf("ChassisType = %d, want a laptop type", r.Platform.ChassisType)
	}
	if r.CPU.Vendor != CPUVendorIntel {
		t.Errorf("CPU.Vendor = %q, want intel", r.CPU.Vendor)
	}
	if r.GPUs.IGPU == nil || r.GPUs.IGPU.Vendor != VendorIntel {
		t.Errorf("IGPU = %+v, want the Intel iGPU", r.GPUs.IGPU)
	}
	nv, err := r.GPUs.SoleNVIDIA()
	if err != nil {
		t.Fatal(err)
	}
	if nv.Class != "0x030200" {
		t.Errorf("dGPU class = %q, want MUXless 3D controller 0x030200", nv.Class)
	}
	if got := RuntimeStatus(root, nv.Address); got != "suspended" {
		t.Errorf("dGPU runtime_status = %q, want suspended", got)
	}
	if r.Platform.GPUMux != GPUMuxHybrid {
		t.Errorf("GPUMux = %q, want hybrid", r.Platform.GPUMux)
	}
	if r.Platform.IOMMUAddressWidth != 39 {
		t.Errorf("IOMMU width = %d, want 39 (DMAR)", r.Platform.IOMMUAddressWidth)
	}
}

func TestDetectLaptopAMDFixture(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))
	root := t.TempDir()
	if err := hwtest.BuildLaptopAMDRoot(root); err != nil {
		t.Fatal(err)
	}
	r, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !IsLaptopChassis(r.Platform.ChassisType) {
		t.Error("chassis not a laptop")
	}
	if r.CPU.Vendor != CPUVendorAMD {
		t.Errorf("CPU.Vendor = %q, want amd", r.CPU.Vendor)
	}
	if r.GPUs.IGPU == nil || r.GPUs.IGPU.Vendor != VendorAMD || r.GPUs.IGPU.Address != "0000:05:00.0" {
		t.Errorf("IGPU = %+v, want the AMD iGPU at 0000:05:00.0", r.GPUs.IGPU)
	}
	if _, err := r.GPUs.SoleNVIDIA(); err != nil {
		t.Errorf("SoleNVIDIA: %v", err)
	}
	if r.Platform.IOMMUTable != IOMMUTableIVRS {
		t.Errorf("IOMMUTable = %q, want IVRS", r.Platform.IOMMUTable)
	}
	if r.Platform.IOMMUAddressWidth != 48 {
		t.Errorf("IOMMU width = %d, want 48 (AMD-Vi)", r.Platform.IOMMUAddressWidth)
	}
}

func TestSummaryReference(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))

	r, err := Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	s := r.Summary()
	for _, want := range []string{
		"14 cores / 20 threads",
		"0000:00:02.0",
		"0000:01:00.0",
		"0000:01:00.1",
		"firmware primary",
		"displays: DP-1",
		"no displays connected",
		"address width 39",
		"enforcing",
		"desktop",
		"dracut: ok",
		"semanage: MISSING",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}

// TestKVMFRAvailable covers the module-present test the domain render keys on:
// modules.dep rather than /sys/module, since up crosses a reboot and nothing is
// loaded on the second leg.
func TestKVMFRAvailable(t *testing.T) {
	const release = "7.1.4-204.fc44.x86_64"
	cases := []struct {
		name string
		dep  string
		want bool
	}{
		{"dkms places it in extra", "extra/kvmfr.ko.xz:\nkernel/drivers/misc/foo.ko.xz:\n", true},
		{"uncompressed", "extra/kvmfr.ko:\n", true},
		{"upstream location", "kernel/drivers/misc/kvmfr.ko.xz:\n", true},
		{"absent", "extra/nvidia.ko.xz: kernel/drivers/acpi/video.ko.xz\n", false},
		{"a lookalike is not a match", "extra/kvmfr-helper.ko.xz:\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			mustWrite(t, filepath.Join(root, "proc/sys/kernel/osrelease"), release+"\n")
			mustWrite(t, filepath.Join(root, "lib/modules", release, "modules.dep"), tc.dep)
			if got := KVMFRAvailable(root); got != tc.want {
				t.Errorf("KVMFRAvailable = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("no modules.dep", func(t *testing.T) {
		root := t.TempDir()
		mustWrite(t, filepath.Join(root, "proc/sys/kernel/osrelease"), release+"\n")
		if KVMFRAvailable(root) {
			t.Error("reported available without a modules.dep")
		}
	})
	t.Run("unknown kernel release", func(t *testing.T) {
		if KVMFRAvailable(t.TempDir()) {
			t.Error("reported available without an osrelease")
		}
	})
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
