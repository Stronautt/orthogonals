// Package hw reads host hardware and platform facts from sysfs. Every path
// is prefixed with root (the --root test seam); root "" means the live host.
package hw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PCI vendor IDs used by GPU classification and preflight vendor gates.
const (
	VendorIntel  = "0x8086"
	VendorNVIDIA = "0x10de"
	VendorAMD    = "0x1002"
)

// PCI class-code prefixes; the sysfs class value is the 0x-prefixed 24-bit
// class/subclass/prog-if code, so prefix matching covers every variant.
const (
	ClassDisplay  = "0x03"   // any display controller
	classHDAAudio = "0x0403" // HD Audio function
	ClassBridge   = "0x0604" // PCI-to-PCI bridge / root port
)

// PCIDevice is one entry from sys/bus/pci/devices.
type PCIDevice struct {
	Address    string `json:"address"`     // 0000:01:00.0
	Vendor     string `json:"vendor"`      // 0x10de
	Device     string `json:"device"`      // 0x2206
	Class      string `json:"class"`       // 0x030000
	Driver     string `json:"driver"`      // empty when unbound
	IOMMUGroup int    `json:"iommu_group"` // -1 when IOMMU is off
	HasReset   bool   `json:"has_reset"`   // sysfs reset file present
}

// VendorDeviceID renders the vendor:device pair in the vfio-pci.ids= form:
// "10de:2206".
func (d PCIDevice) VendorDeviceID() string {
	return strings.TrimPrefix(d.Vendor, "0x") + ":" + strings.TrimPrefix(d.Device, "0x")
}

// ScanPCI enumerates root/sys/bus/pci/devices, resolving driver and
// iommu_group symlinks by basename (they may dangle in fixtures).
func ScanPCI(root string) ([]PCIDevice, error) {
	base := filepath.Join(root, "/sys/bus/pci/devices")
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("scan PCI devices: %w", err)
	}
	devs := make([]PCIDevice, 0, len(entries))
	for _, e := range entries {
		dir := filepath.Join(base, e.Name())
		d := PCIDevice{
			Address:    e.Name(),
			Vendor:     readTrim(filepath.Join(dir, "vendor")),
			Device:     readTrim(filepath.Join(dir, "device")),
			Class:      readTrim(filepath.Join(dir, "class")),
			Driver:     linkBase(filepath.Join(dir, "driver")),
			IOMMUGroup: -1,
		}
		if g := linkBase(filepath.Join(dir, "iommu_group")); g != "" {
			if n, err := strconv.Atoi(g); err == nil {
				d.IOMMUGroup = n
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "reset")); err == nil {
			d.HasReset = true
		}
		devs = append(devs, d)
	}
	return devs, nil
}

// DGPU is a discrete GPU plus its co-slot audio function, if any.
type DGPU struct {
	PCIDevice
	Audio *PCIDevice `json:"audio,omitempty"`
}

// GPUs is the classified GPU topology preflight gates on.
type GPUs struct {
	IGPU  *PCIDevice `json:"igpu,omitempty"`
	DGPUs []DGPU     `json:"dgpus,omitempty"`
}

// NVIDIA returns the discrete NVIDIA GPUs; the profile builders (hostcfg,
// hooks, domain) pass through exactly one — matching the preflight gate,
// which tolerates extra non-NVIDIA dGPUs on the host side.
func (g GPUs) NVIDIA() []DGPU {
	var out []DGPU
	for _, d := range g.DGPUs {
		if d.Vendor == VendorNVIDIA {
			out = append(out, d)
		}
	}
	return out
}

// SoleNVIDIA returns the single discrete NVIDIA GPU the v1 product targets,
// or an error naming how many were found.
func (g GPUs) SoleNVIDIA() (DGPU, error) {
	nvidia := g.NVIDIA()
	if len(nvidia) != 1 {
		return DGPU{}, fmt.Errorf("need exactly one NVIDIA discrete GPU, detect found %d (run orthogonals preflight)", len(nvidia))
	}
	return nvidia[0], nil
}

// SetDriverOverride writes the driver_override attribute of a PCI device;
// an empty driver clears the override.
func SetDriverOverride(root, addr, driver string) error {
	return writeDeviceAttr(root, addr, "driver_override", driver)
}

// RemoveDevice hot-removes a PCI device from the bus (sysfs remove); a
// later RescanPCI re-enumerates it.
func RemoveDevice(root, addr string) error {
	return writeDeviceAttr(root, addr, "remove", "1")
}

// RescanPCI asks the PCI core to re-enumerate the bus.
func RescanPCI(root string) error {
	return os.WriteFile(filepath.Join(root, "/sys/bus/pci/rescan"), []byte("1\n"), 0o644)
}

func writeDeviceAttr(root, addr, attr, val string) error {
	return os.WriteFile(filepath.Join(root, "/sys/bus/pci/devices", addr, attr), []byte(val+"\n"), 0o644)
}

// classifyGPUs splits display-class devices into the Intel iGPU (Intel
// display controller on bus 00) and discrete GPUs of any vendor.
func classifyGPUs(devices []PCIDevice) GPUs {
	var g GPUs
	for _, d := range devices {
		if !strings.HasPrefix(d.Class, ClassDisplay) {
			continue
		}
		d := d
		if d.Vendor == VendorIntel && strings.HasPrefix(d.Address, "0000:00:") {
			g.IGPU = &d
			continue
		}
		g.DGPUs = append(g.DGPUs, DGPU{PCIDevice: d, Audio: audioSibling(devices, d)})
	}
	return g
}

// audioSibling finds the HDA function on the same PCI slot as the GPU.
func audioSibling(devices []PCIDevice, gpu PCIDevice) *PCIDevice {
	dot := strings.LastIndex(gpu.Address, ".")
	if dot < 0 {
		return nil
	}
	slot := gpu.Address[:dot+1]
	for _, d := range devices {
		if d.Address != gpu.Address &&
			strings.HasPrefix(d.Address, slot) &&
			strings.HasPrefix(d.Class, classHDAAudio) {
			d := d
			return &d
		}
	}
	return nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func linkBase(path string) string {
	target, err := os.Readlink(path)
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}
