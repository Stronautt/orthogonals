// Package hwtest builds fake sysfs trees under a temp root for hw and cli tests.
package hwtest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// Dev describes one fixture PCI device.
type Dev struct {
	Addr, Vendor, Device, Class, Driver string
	Group                               int
	Reset                               bool
}

// file is one fixture path and its content.
type file struct{ rel, content string }

// sharedHostFiles are the fixture files identical across every synthetic host.
func sharedHostFiles(cpuVendorID string) []file {
	files := []file{
		{"sys/devices/system/cpu/present", "0-19\n"},
		{"proc/cpuinfo", "processor\t: 0\nvendor_id\t: " + cpuVendorID + "\nmodel name\t: fixture cpu\n"},
		{"sys/devices/cpu_core/cpus", "0-11\n"},
		{"sys/devices/cpu_atom/cpus", "12-19\n"},
		{"proc/meminfo", "MemTotal:       33554432 kB\nMemFree:        20000000 kB\n"},
		{"proc/driver/nvidia/version",
			"NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n" +
				"GCC version:  gcc version 15.0.1 20250418 (Red Hat 15.0.1-0) (GCC)\n"},
		{"sys/module/nvidia_drm/parameters/modeset", "Y\n"},
		{"sys/module/nvidia_drm/parameters/fbdev", "N\n"},
		{"sys/fs/selinux/enforce", "1"},
		{"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", "\x06\x00\x00\x00\x01"},
		{"boot/loader/entries/fedora-6.15.0.conf",
			"title Fedora Linux (6.15.0) 44\nversion 6.15.0\nlinux /vmlinuz-6.15.0\ninitrd /initramfs-6.15.0.img\noptions root=UUID=aaaa ro rhgb quiet\n"},
		{"boot/loader/entries/fedora-6.14.0.conf",
			"title Fedora Linux (6.14.0) 44\nversion 6.14.0\nlinux /vmlinuz-6.14.0\ninitrd /initramfs-6.14.0.img\noptions root=UUID=aaaa ro rhgb quiet\n"},
	}
	coreIDs := []int{0, 0, 4, 4, 8, 8, 12, 12, 16, 16, 20, 20, 24, 25, 26, 27, 28, 29, 30, 31}
	for cpu, id := range coreIDs {
		files = append(files, file{
			fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/core_id", cpu), strconv.Itoa(id) + "\n"})
	}
	return files
}

// VT-d CAP register values; bits 16-21 hold MGAW, the address width minus one.
const (
	capMGAW39 = "d2008c40660462\n"
	capMGAW48 = "d2008c406f0462\n"
)

// secureBootOff is the SecureBoot efivar with the flag byte cleared.
const secureBootOff = "\x06\x00\x00\x00\x00"

// intelDesktop is the VT-d, ACPI, and chassis trio every Intel desktop fixture
// shares; cap selects the IOMMU address width.
func intelDesktop(cap string) []file {
	return []file{
		{"sys/class/iommu/dmar0/intel-iommu/cap", cap},
		{"sys/firmware/acpi/tables/DMAR", ""},
		{"sys/class/dmi/id/chassis_type", "3\n"},
	}
}

// referenceGPUs is the PoC desktop's iGPU + NVIDIA dGPU + HDMI audio trio.
func referenceGPUs() []Dev {
	return []Dev{
		{Addr: "0000:00:02.0", Vendor: "0x8086", Device: "0xa780", Class: "0x030000", Driver: "i915", Group: 0, Reset: true},
		{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 1, Reset: true},
		// No Reset: an HDA audio function publishes no sysfs reset file.
		{Addr: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 1},
	}
}

// referenceDisplay is the reference desktop's boot_vga and DRM connector layout.
func referenceDisplay() []file {
	return []file{
		{"sys/bus/pci/devices/0000:00:02.0/boot_vga", "1\n"},
		{"sys/bus/pci/devices/0000:01:00.0/boot_vga", "0\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/card0/card0-DP-1/status", "connected\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/card0/card0-HDMI-A-1/status", "disconnected\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/renderD128/dev", "226:128\n"},
		{"sys/bus/pci/devices/0000:01:00.0/drm/card1/card1-DP-1/status", "disconnected\n"},
	}
}

// buildHost writes the PCI devices, shared files, and host-specific files under root.
func buildHost(root, cpuVendorID string, devs []Dev, specific []file) error {
	for _, d := range devs {
		if err := addPCI(root, d); err != nil {
			return err
		}
	}
	for _, f := range append(sharedHostFiles(cpuVendorID), specific...) {
		if err := writeFile(root, f.rel, f.content); err != nil {
			return err
		}
	}
	return nil
}

// BuildReferenceRoot writes the PoC reference desktop (i5-13600K + RTX 3080) under root.
func BuildReferenceRoot(root string) error {
	return buildHost(root, "GenuineIntel", referenceGPUs(),
		append(intelDesktop(capMGAW39), referenceDisplay()...))
}

// BuildLaptopRoot writes an Intel + NVIDIA notebook: eDP-1 on the iGPU, a MUXless
// (3D-controller) dGPU runtime-suspended.
func BuildLaptopRoot(root string) error {
	return buildHost(root, "GenuineIntel", []Dev{
		{Addr: "0000:00:02.0", Vendor: "0x8086", Device: "0xa780", Class: "0x030000", Driver: "i915", Group: 0, Reset: true},
		{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030200", Driver: "nvidia", Group: 1, Reset: true},
		{Addr: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 1},
	}, []file{
		{"sys/class/iommu/dmar0/intel-iommu/cap", "d2008c40660462\n"},
		{"sys/firmware/acpi/tables/DMAR", ""},
		{"sys/class/dmi/id/chassis_type", "10\n"},
		{"sys/devices/platform/asus-nb-wmi/gpu_mux_mode", "1\n"},
		{"sys/bus/pci/devices/0000:00:02.0/boot_vga", "1\n"},
		{"sys/bus/pci/devices/0000:01:00.0/boot_vga", "0\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/card0/card0-eDP-1/status", "connected\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/renderD128/dev", "226:128\n"},
		{"sys/bus/pci/devices/0000:01:00.0/power/control", "auto\n"},
		{"sys/bus/pci/devices/0000:01:00.0/power/runtime_status", "suspended\n"},
		{"sys/bus/pci/devices/0000:01:00.1/power/control", "auto\n"},
		{"sys/bus/pci/devices/0000:01:00.1/power/runtime_status", "suspended\n"},
	})
}

// BuildLaptopAMDRoot writes an AMD APU + NVIDIA notebook: AMD-Vi/IVRS IOMMU, an
// AMD iGPU on a high bus, a MUXed dGPU runtime-suspended.
func BuildLaptopAMDRoot(root string) error {
	return buildHost(root, "AuthenticAMD", []Dev{
		{Addr: "0000:05:00.0", Vendor: "0x1002", Device: "0x1638", Class: "0x030000", Driver: "amdgpu", Group: 0, Reset: true},
		{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 1, Reset: true},
		{Addr: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 1},
	}, []file{
		{"sys/class/iommu/ivhd0/name", "ivhd0\n"},
		{"sys/firmware/acpi/tables/IVRS", ""},
		{"sys/class/dmi/id/chassis_type", "10\n"},
		{"sys/bus/pci/devices/0000:05:00.0/boot_vga", "1\n"},
		{"sys/bus/pci/devices/0000:01:00.0/boot_vga", "0\n"},
		{"sys/bus/pci/devices/0000:05:00.0/drm/card0/card0-eDP-1/status", "connected\n"},
		{"sys/bus/pci/devices/0000:05:00.0/drm/renderD128/dev", "226:128\n"},
		{"sys/bus/pci/devices/0000:01:00.0/drm/card1/card1-HDMI-A-1/status", "disconnected\n"},
		{"sys/bus/pci/devices/0000:01:00.0/power/control", "auto\n"},
		{"sys/bus/pci/devices/0000:01:00.0/power/runtime_status", "suspended\n"},
		{"sys/bus/pci/devices/0000:01:00.1/power/control", "auto\n"},
		{"sys/bus/pci/devices/0000:01:00.1/power/runtime_status", "suspended\n"},
	})
}

// BuildDirtyGroupRoot writes a reference desktop whose dGPU shares its IOMMU
// group with an unrelated NIC — the whole-group rule preflight must refuse.
func BuildDirtyGroupRoot(root string) error {
	devs := append(referenceGPUs(),
		Dev{Addr: "0000:02:00.0", Vendor: "0x8086", Device: "0x15f3", Class: "0x020000", Driver: "igc", Group: 1})
	return buildHost(root, "GenuineIntel", devs,
		append(intelDesktop(capMGAW39), referenceDisplay()...))
}

// BuildNoIGPURoot writes a host whose only GPU is the NVIDIA dGPU: it cannot
// both drive the desktop and be passed through.
func BuildNoIGPURoot(root string) error {
	return buildHost(root, "GenuineIntel", []Dev{
		{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 1, Reset: true},
		{Addr: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 1},
	}, append(intelDesktop(capMGAW39),
		file{"sys/bus/pci/devices/0000:01:00.0/boot_vga", "1\n"},
		file{"sys/bus/pci/devices/0000:01:00.0/drm/card0/card0-DP-1/status", "connected\n"},
	))
}

// BuildDualNVIDIARoot writes an iGPU plus two identical RTX 3080s: static
// vfio-pci.ids binding cannot tell them apart.
func BuildDualNVIDIARoot(root string) error {
	devs := append(referenceGPUs(),
		Dev{Addr: "0000:02:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 2, Reset: true},
		Dev{Addr: "0000:02:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 2})
	return buildHost(root, "GenuineIntel", devs,
		append(intelDesktop(capMGAW39),
			append(referenceDisplay(),
				file{"sys/bus/pci/devices/0000:02:00.0/boot_vga", "0\n"})...))
}

// BuildForeignVFIORoot writes a reference desktop carrying vfio configuration
// orthogonals did not write and has no manifest to claim.
func BuildForeignVFIORoot(root string) error {
	return buildHost(root, "GenuineIntel", referenceGPUs(),
		append(intelDesktop(capMGAW39),
			append(referenceDisplay(),
				file{"etc/modprobe.d/vfio-preexisting.conf", "options vfio-pci ids=10de:2206,10de:1aef\n"})...))
}

// BuildNoAudioRoot writes a reference desktop whose dGPU has no HDMI audio
// function, so the passthrough set is the GPU alone.
func BuildNoAudioRoot(root string) error {
	return buildHost(root, "GenuineIntel", referenceGPUs()[:2],
		append(intelDesktop(capMGAW39), referenceDisplay()...))
}

// BuildNoResetRoot writes a reference desktop whose dGPU exposes no sysfs reset
// file, so it cannot be handed back and forth.
func BuildNoResetRoot(root string) error {
	devs := referenceGPUs()
	devs[1].Reset = false
	return buildHost(root, "GenuineIntel", devs,
		append(intelDesktop(capMGAW39), referenceDisplay()...))
}

// BuildWideIOMMURoot writes a 48-bit-IOMMU desktop with Secure Boot off and the
// default network up, clearing the address-width, secure-boot, and
// default-network warnings the reference host raises.
func BuildWideIOMMURoot(root string) error {
	return buildHost(root, "GenuineIntel", referenceGPUs(),
		append(intelDesktop(capMGAW48),
			append(referenceDisplay(),
				file{"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", secureBootOff},
				file{"var/run/libvirt/network/default.xml", "<network><name>default</name></network>\n"})...))
}

// BuildBridgeRoot writes a reference desktop whose dGPU sits behind a PCIe
// bridge sharing its IOMMU group — a bridge is not a stranger.
func BuildBridgeRoot(root string) error {
	devs := append(referenceGPUs(),
		Dev{Addr: "0000:00:01.0", Vendor: "0x8086", Device: "0xa70d", Class: "0x060400", Driver: "pcieport", Group: 1})
	return buildHost(root, "GenuineIntel", devs,
		append(intelDesktop(capMGAW39), referenceDisplay()...))
}

// Roots is every synthetic host by name — the single source of the fixture
// topologies for tests, the fixture command, and the tmt tests.
var Roots = map[string]func(string) error{
	"reference":    BuildReferenceRoot,
	"laptop":       BuildLaptopRoot,
	"laptop-amd":   BuildLaptopAMDRoot,
	"dirty-group":  BuildDirtyGroupRoot,
	"no-igpu":      BuildNoIGPURoot,
	"dual-nvidia":  BuildDualNVIDIARoot,
	"foreign-vfio": BuildForeignVFIORoot,
	"no-audio":     BuildNoAudioRoot,
	"no-reset":     BuildNoResetRoot,
	"wide-iommu":   BuildWideIOMMURoot,
	"bridge":       BuildBridgeRoot,
}

// RootNames lists every fixture in Roots, sorted.
func RootNames() []string {
	names := make([]string, 0, len(Roots))
	for n := range Roots {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ReferenceRoot is BuildReferenceRoot in a temp dir.
func ReferenceRoot(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	if err := BuildReferenceRoot(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// WriteFile writes root/rel, creating parent directories.
func WriteFile(t testing.TB, root, rel, content string) {
	t.Helper()
	if err := writeFile(root, rel, content); err != nil {
		t.Fatal(err)
	}
}

// Symlink creates root/rel pointing at target.
func Symlink(t testing.TB, root, rel, target string) {
	t.Helper()
	if err := symlink(root, rel, target); err != nil {
		t.Fatal(err)
	}
}

// AddPCI writes one device under root/sys/bus/pci/devices.
func AddPCI(t testing.TB, root string, d Dev) {
	t.Helper()
	if err := addPCI(root, d); err != nil {
		t.Fatal(err)
	}
}

// FakeTools creates an executable stub per name and returns the dir.
func FakeTools(t testing.TB, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func writeFile(root, rel, content string) error {
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func symlink(root, rel, target string) error {
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.Symlink(target, path)
}

func addPCI(root string, d Dev) error {
	dir := "sys/bus/pci/devices/" + d.Addr
	for _, f := range []struct{ rel, content string }{
		{dir + "/vendor", d.Vendor + "\n"},
		{dir + "/device", d.Device + "\n"},
		{dir + "/class", d.Class + "\n"},
	} {
		if err := writeFile(root, f.rel, f.content); err != nil {
			return err
		}
	}
	if d.Driver != "" {
		if err := symlink(root, dir+"/driver", "../../../bus/pci/drivers/"+d.Driver); err != nil {
			return err
		}
	}
	if d.Group >= 0 {
		if err := symlink(root, dir+"/iommu_group", "../../../kernel/iommu_groups/"+strconv.Itoa(d.Group)); err != nil {
			return err
		}
	}
	if d.Reset {
		return writeFile(root, dir+"/reset", "")
	}
	return nil
}
