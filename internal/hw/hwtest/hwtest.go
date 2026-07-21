// Package hwtest builds fake sysfs trees under a temp root for hw and cli tests.
package hwtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Dev describes one fixture PCI device.
type Dev struct {
	Addr, Vendor, Device, Class, Driver string
	Group                               int
	Reset                               bool
}

// BuildReferenceRoot writes the PoC reference machine under root.
func BuildReferenceRoot(root string) error {
	devs := []Dev{
		{Addr: "0000:00:02.0", Vendor: "0x8086", Device: "0xa780", Class: "0x030000", Driver: "i915", Group: 0, Reset: true},
		{Addr: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 1, Reset: true},
		{Addr: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", Group: 1, Reset: true},
	}
	for _, d := range devs {
		if err := addPCI(root, d); err != nil {
			return err
		}
	}

	files := []struct{ rel, content string }{
		{"sys/devices/system/cpu/present", "0-19\n"},
		{"sys/devices/cpu_core/cpus", "0-11\n"},
		{"sys/devices/cpu_atom/cpus", "12-19\n"},
		{"proc/meminfo", "MemTotal:       33554432 kB\nMemFree:        20000000 kB\n"},
		{"proc/driver/nvidia/version",
			"NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n" +
				"GCC version:  gcc version 15.0.1 20250418 (Red Hat 15.0.1-0) (GCC)\n"},
		{"sys/module/nvidia_drm/parameters/modeset", "Y\n"},
		{"sys/module/nvidia_drm/parameters/fbdev", "N\n"},
		{"sys/class/iommu/dmar0/intel-iommu/cap", "d2008c40660462\n"},
		{"sys/firmware/acpi/tables/DMAR", ""},
		{"sys/fs/selinux/enforce", "1"},
		{"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c", "\x06\x00\x00\x00\x01"},
		{"sys/class/dmi/id/chassis_type", "3\n"},
		{"sys/bus/pci/devices/0000:00:02.0/boot_vga", "1\n"},
		{"sys/bus/pci/devices/0000:01:00.0/boot_vga", "0\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/card0/card0-DP-1/status", "connected\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/card0/card0-HDMI-A-1/status", "disconnected\n"},
		{"sys/bus/pci/devices/0000:00:02.0/drm/renderD128/dev", "226:128\n"},
		{"sys/bus/pci/devices/0000:01:00.0/drm/card1/card1-DP-1/status", "disconnected\n"},
		{"boot/loader/entries/fedora-6.15.0.conf",
			"title Fedora Linux (6.15.0) 44\nversion 6.15.0\nlinux /vmlinuz-6.15.0\ninitrd /initramfs-6.15.0.img\noptions root=UUID=aaaa ro rhgb quiet\n"},
		{"boot/loader/entries/fedora-6.14.0.conf",
			"title Fedora Linux (6.14.0) 44\nversion 6.14.0\nlinux /vmlinuz-6.14.0\ninitrd /initramfs-6.14.0.img\noptions root=UUID=aaaa ro rhgb quiet\n"},
	}
	coreIDs := []int{0, 0, 4, 4, 8, 8, 12, 12, 16, 16, 20, 20, 24, 25, 26, 27, 28, 29, 30, 31}
	for cpu, id := range coreIDs {
		files = append(files, struct{ rel, content string }{
			fmt.Sprintf("sys/devices/system/cpu/cpu%d/topology/core_id", cpu), strconv.Itoa(id) + "\n"})
	}
	for _, f := range files {
		if err := writeFile(root, f.rel, f.content); err != nil {
			return err
		}
	}
	return nil
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
