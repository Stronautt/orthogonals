package hw

import (
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestIOMMUAddressWidth(t *testing.T) {
	t.Run("reference 39-bit", func(t *testing.T) {
		if w := iommuAddressWidth(hwtest.ReferenceRoot(t)); w != 39 {
			t.Errorf("width = %d, want 39", w)
		}
	})
	t.Run("min across units wins", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "sys/class/iommu/dmar0/intel-iommu/cap", "2d0462\n")
		hwtest.WriteFile(t, root, "sys/class/iommu/dmar1/intel-iommu/cap", "260462\n")
		if w := iommuAddressWidth(root); w != 39 {
			t.Errorf("width = %d, want 39 (min of 46 and 39)", w)
		}
	})
	t.Run("no iommu", func(t *testing.T) {
		if w := iommuAddressWidth(t.TempDir()); w != 0 {
			t.Errorf("width = %d, want 0 without dmar units", w)
		}
	})
	t.Run("amd ivhd unit assumes 48-bit", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "sys/class/iommu/ivhd0/name", "ivhd0\n")
		if w := iommuAddressWidth(root); w != 48 {
			t.Errorf("width = %d, want 48 for an active AMD-Vi unit", w)
		}
	})
	t.Run("garbage cap ignored", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "sys/class/iommu/dmar0/intel-iommu/cap", "not-hex\n")
		if w := iommuAddressWidth(root); w != 0 {
			t.Errorf("width = %d, want 0 for unparseable cap", w)
		}
	})
}

func TestIOMMUTable(t *testing.T) {
	for _, table := range []string{IOMMUTableDMAR, IOMMUTableIVRS} {
		t.Run(table, func(t *testing.T) {
			root := t.TempDir()
			hwtest.WriteFile(t, root, "sys/firmware/acpi/tables/"+table, "")
			if got := iommuTable(root); got != table {
				t.Errorf("iommuTable = %q, want %q", got, table)
			}
		})
	}
	if got := iommuTable(t.TempDir()); got != "" {
		t.Errorf("iommuTable on empty root = %q, want empty", got)
	}
}

func TestSELinuxMode(t *testing.T) {
	tests := []struct {
		name, enforce, want string
	}{
		{"enforcing", "1", "enforcing"},
		{"permissive", "0", "permissive"},
		{"disabled", "", "disabled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.enforce != "" {
				hwtest.WriteFile(t, root, "sys/fs/selinux/enforce", tt.enforce)
			}
			if got := selinuxMode(root); got != tt.want {
				t.Errorf("selinuxMode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSecureBoot(t *testing.T) {
	const efivar = "sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"
	t.Run("enabled", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, efivar, "\x06\x00\x00\x00\x01")
		if !secureBootEnabled(root) {
			t.Error("want Secure Boot enabled")
		}
	})
	t.Run("present but off", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, efivar, "\x06\x00\x00\x00\x00")
		if secureBootEnabled(root) {
			t.Error("want Secure Boot disabled")
		}
	})
	t.Run("no efivar", func(t *testing.T) {
		if secureBootEnabled(t.TempDir()) {
			t.Error("want Secure Boot false on non-UEFI host")
		}
	})
}

func TestChassisName(t *testing.T) {
	tests := []struct {
		typ  int
		want string
	}{
		{3, "desktop"},
		{9, "laptop"},
		{10, "notebook"},
		{99, "type 99"},
	}
	for _, tt := range tests {
		if got := ChassisName(tt.typ); got != tt.want {
			t.Errorf("ChassisName(%d) = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

func TestChassisType(t *testing.T) {
	if got := ChassisType(t.TempDir()); got != 0 {
		t.Errorf("ChassisType with no dmi node = %d, want 0", got)
	}
	root := t.TempDir()
	hwtest.WriteFile(t, root, "sys/class/dmi/id/chassis_type", "10\n")
	if got := ChassisType(root); got != 10 {
		t.Errorf("ChassisType = %d, want 10", got)
	}
}

func TestIsLaptopChassis(t *testing.T) {
	for _, typ := range []int{8, 9, 10, 11, 14, 30, 31, 32} {
		if !IsLaptopChassis(typ) {
			t.Errorf("IsLaptopChassis(%d) = false, want true", typ)
		}
	}
	for _, typ := range []int{0, 3, 4, 6, 7, 13} {
		if IsLaptopChassis(typ) {
			t.Errorf("IsLaptopChassis(%d) = true, want false", typ)
		}
	}
}

func TestMemTotalBytes(t *testing.T) {
	t.Run("reference 32 GiB", func(t *testing.T) {
		if got := memTotalBytes(hwtest.ReferenceRoot(t)); got != 32<<30 {
			t.Errorf("memTotalBytes = %d, want %d", got, uint64(32)<<30)
		}
	})
	t.Run("missing meminfo", func(t *testing.T) {
		if got := memTotalBytes(t.TempDir()); got != 0 {
			t.Errorf("memTotalBytes = %d, want 0", got)
		}
	})
}

func TestKernelVersion(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "proc/sys/kernel/osrelease", "6.14.9-200.fc42.x86_64\n")
	if got := KernelVersion(root); got != "6.14.9-200.fc42.x86_64" {
		t.Errorf("KernelVersion = %q, want 6.14.9-200.fc42.x86_64", got)
	}
	if got := KernelVersion(t.TempDir()); got != "" {
		t.Errorf("KernelVersion on empty root = %q, want empty", got)
	}
}

func TestDetectNVIDIA(t *testing.T) {
	const versionFile = "proc/driver/nvidia/version"
	t.Run("not loaded", func(t *testing.T) {
		n := DetectNVIDIA(t.TempDir())
		if n.Loaded {
			t.Error("Loaded = true, want false without /proc/driver/nvidia/version")
		}
	})
	t.Run("proprietary", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, versionFile,
			"NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n"+
				"GCC version:  gcc version 15.0.1 20250418 (Red Hat 15.0.1-0) (GCC)\n")
		n := DetectNVIDIA(root)
		if !n.Loaded {
			t.Fatal("Loaded = false, want true")
		}
		if n.Flavor != "proprietary" {
			t.Errorf("Flavor = %q, want proprietary", n.Flavor)
		}
		if n.Version != "570.153.02" {
			t.Errorf("Version = %q, want 570.153.02", n.Version)
		}
		if n.Modeset != "" || n.Fbdev != "" {
			t.Errorf("Modeset/Fbdev = %q/%q, want empty without nvidia_drm params", n.Modeset, n.Fbdev)
		}
	})
	t.Run("open", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, versionFile,
			"NVRM version: NVIDIA UNIX Open Kernel Module for x86_64  575.64.03  Release Build  (dvs-builder)  Mon Jun 16 08:03:23 UTC 2025\n")
		n := DetectNVIDIA(root)
		if n.Flavor != "open" {
			t.Errorf("Flavor = %q, want open", n.Flavor)
		}
		if n.Version != "575.64.03" {
			t.Errorf("Version = %q, want 575.64.03", n.Version)
		}
	})
	t.Run("nvidia_drm params", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, versionFile,
			"NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n")
		hwtest.WriteFile(t, root, "sys/module/nvidia_drm/parameters/modeset", "Y\n")
		hwtest.WriteFile(t, root, "sys/module/nvidia_drm/parameters/fbdev", "N\n")
		n := DetectNVIDIA(root)
		if n.Modeset != "Y" {
			t.Errorf("Modeset = %q, want Y", n.Modeset)
		}
		if n.Fbdev != "N" {
			t.Errorf("Fbdev = %q, want N", n.Fbdev)
		}
	})
	t.Run("unparseable version file", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, versionFile, "garbage\n")
		n := DetectNVIDIA(root)
		if !n.Loaded {
			t.Error("Loaded = false, want true (file exists)")
		}
		if n.Version != "" || n.Flavor != "" {
			t.Errorf("Version/Flavor = %q/%q, want empty for unparseable NVRM line", n.Version, n.Flavor)
		}
	})
}

func TestDetectPlatformReference(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))

	p := detectPlatform(hwtest.ReferenceRoot(t))
	if p.IOMMUAddressWidth != 39 {
		t.Errorf("IOMMUAddressWidth = %d, want 39", p.IOMMUAddressWidth)
	}
	if p.SELinux != "enforcing" {
		t.Errorf("SELinux = %q, want enforcing", p.SELinux)
	}
	if !p.SecureBoot {
		t.Error("SecureBoot = false, want true")
	}
	if p.ChassisType != 3 {
		t.Errorf("ChassisType = %d, want 3", p.ChassisType)
	}
	if !p.Tools["dracut"] {
		t.Errorf("Tools = %v, want dracut present", p.Tools)
	}
	if p.MemTotalBytes != 32<<30 {
		t.Errorf("MemTotalBytes = %d, want 32 GiB", p.MemTotalBytes)
	}
	if !p.NVIDIA.Loaded || p.NVIDIA.Flavor != "proprietary" || p.NVIDIA.Version != "570.153.02" {
		t.Errorf("NVIDIA = %+v, want loaded proprietary 570.153.02", p.NVIDIA)
	}
	if p.NVIDIA.Modeset != "Y" || p.NVIDIA.Fbdev != "N" {
		t.Errorf("NVIDIA modeset/fbdev = %q/%q, want Y/N", p.NVIDIA.Modeset, p.NVIDIA.Fbdev)
	}
}
