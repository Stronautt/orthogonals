// Package hw reads host hardware and platform facts from sysfs.
package hw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stronautt/orthogonals/internal/utils"
)

const (
	VendorIntel  = "0x8086"
	VendorNVIDIA = "0x10de"
	VendorAMD    = "0x1002"
)

// PCI class-code prefixes.
const (
	ClassDisplay = "0x03"
	// Class3DController is a display device with no outputs (a MUXless dGPU).
	Class3DController = "0x0302"
	classHDAAudio     = "0x0403"
	ClassBridge       = "0x0604"
)

// PCIDevice is one entry from sys/bus/pci/devices.
type PCIDevice struct {
	Address    string   `json:"address"`
	Vendor     string   `json:"vendor"`
	Device     string   `json:"device"`
	Class      string   `json:"class"`
	Driver     string   `json:"driver"`
	IOMMUGroup int      `json:"iommu_group"`
	HasReset   bool     `json:"has_reset"`
	BootVGA    bool     `json:"boot_vga"`
	DRMCard    string   `json:"drm_card,omitempty"`
	Connectors []string `json:"connectors,omitempty"`
}

func (d PCIDevice) VendorDeviceID() string {
	return strings.TrimPrefix(d.Vendor, "0x") + ":" + strings.TrimPrefix(d.Device, "0x")
}

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
			Vendor:     utils.ReadTrim(filepath.Join(dir, "vendor")),
			Device:     utils.ReadTrim(filepath.Join(dir, "device")),
			Class:      utils.ReadTrim(filepath.Join(dir, "class")),
			Driver:     utils.LinkBase(filepath.Join(dir, "driver")),
			IOMMUGroup: -1,
		}
		if g := utils.LinkBase(filepath.Join(dir, "iommu_group")); g != "" {
			if n, err := strconv.Atoi(g); err == nil {
				d.IOMMUGroup = n
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "reset")); err == nil {
			d.HasReset = true
		}
		d.BootVGA = utils.ReadTrim(filepath.Join(dir, "boot_vga")) == "1"
		d.DRMCard, d.Connectors = scanDRM(dir)
		devs = append(devs, d)
	}
	return devs, nil
}

// DGPU is a discrete GPU plus its co-slot audio function, if any.
type DGPU struct {
	PCIDevice
	Audio *PCIDevice `json:"audio,omitempty"`
}

// Addresses is the GPU's PCI address plus its audio sibling's, if any — the
// device set a passthrough handover moves as one unit.
func (d DGPU) Addresses() []string {
	addrs := []string{d.Address}
	if d.Audio != nil {
		addrs = append(addrs, d.Audio.Address)
	}
	return addrs
}

// UngroupedDevices is the addresses with no iommu_group. vfio-pci binds through
// an IOMMU group and its probe fails with EINVAL without one, so this is a
// precondition of a handover, not a diagnostic.
func (d DGPU) UngroupedDevices() []string {
	var out []string
	if d.IOMMUGroup < 0 {
		out = append(out, d.Address)
	}
	if d.Audio != nil && d.Audio.IOMMUGroup < 0 {
		out = append(out, d.Audio.Address)
	}
	return out
}

// VendorDeviceIDs is the vendor:device pair of the GPU plus its audio
// sibling's, if any — what vfio-pci binds to.
func (d DGPU) VendorDeviceIDs() []string {
	ids := []string{d.VendorDeviceID()}
	if d.Audio != nil {
		ids = append(ids, d.Audio.VendorDeviceID())
	}
	return ids
}

type GPUs struct {
	IGPU  *PCIDevice `json:"igpu,omitempty"`
	DGPUs []DGPU     `json:"dgpus,omitempty"`
}

func (g GPUs) NVIDIA() []DGPU {
	var out []DGPU
	for _, d := range g.DGPUs {
		if d.Vendor == VendorNVIDIA {
			out = append(out, d)
		}
	}
	return out
}

func (g GPUs) SoleNVIDIA() (DGPU, error) {
	nvidia := g.NVIDIA()
	if len(nvidia) != 1 {
		return DGPU{}, fmt.Errorf("need exactly one NVIDIA discrete GPU, detect found %d (run orthogonals preflight)", len(nvidia))
	}
	return nvidia[0], nil
}

func SetDriverOverride(root, addr, driver string) error {
	return writeDeviceAttr(root, addr, "driver_override", driver)
}

func RemoveDevice(root, addr string) error {
	return writeDeviceAttr(root, addr, "remove", "1")
}

func RescanPCI(root string) error {
	return echoSysfs(filepath.Join(root, "/sys/bus/pci/rescan"), "1")
}

func DeviceDriver(root, addr string) string {
	return utils.LinkBase(filepath.Join(root, "/sys/bus/pci/devices", addr, "driver"))
}

// RuntimeStatus reads a PCI device's power/runtime_status, "" without runtime PM.
func RuntimeStatus(root, addr string) string {
	return utils.ReadTrim(filepath.Join(root, "/sys/bus/pci/devices", addr, "power/runtime_status"))
}

// SetPowerControl writes a PCI device's power/control ("on" pins D0, "auto" allows suspend).
func SetPowerControl(root, addr, val string) error {
	return writeDeviceAttr(root, addr, "power/control", val)
}

func UnbindDevice(root, addr string) error {
	if DeviceDriver(root, addr) == "" {
		return nil
	}
	return echoSysfs(filepath.Join(root, "/sys/bus/pci/devices", addr, "driver/unbind"), addr)
}

func ProbeDevice(root, addr string) error {
	return echoSysfs(filepath.Join(root, "/sys/bus/pci/drivers_probe"), addr)
}

func writeDeviceAttr(root, addr, attr, val string) error {
	return echoSysfs(filepath.Join(root, "/sys/bus/pci/devices", addr, attr), val)
}

func echoSysfs(path, val string) error {
	return os.WriteFile(path, []byte(val+"\n"), 0o644)
}

// ScanGPUs skips the CPU and platform probes Detect runs.
func ScanGPUs(root string) (GPUs, error) {
	devs, err := ScanPCI(root)
	if err != nil {
		return GPUs{}, err
	}
	return classifyGPUs(devs), nil
}

// classifyGPUs preserves device order.
func classifyGPUs(devices []PCIDevice) GPUs {
	var g GPUs
	igpuAddr := selectIGPU(devices)
	for _, d := range devices {
		if !strings.HasPrefix(d.Class, ClassDisplay) {
			continue
		}
		if igpuAddr != "" && d.Address == igpuAddr {
			g.IGPU = &d
			continue
		}
		g.DGPUs = append(g.DGPUs, DGPU{PCIDevice: d, Audio: audioSibling(devices, d)})
	}
	return g
}

// selectIGPU returns the lowest-addressed non-NVIDIA display device, "" when every
// display device is NVIDIA.
func selectIGPU(devices []PCIDevice) string {
	igpu := ""
	for _, d := range devices {
		if !strings.HasPrefix(d.Class, ClassDisplay) || d.Vendor == VendorNVIDIA {
			continue
		}
		if igpu == "" || d.Address < igpu {
			igpu = d.Address
		}
	}
	return igpu
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

func scanDRM(devDir string) (card string, connected []string) {
	entries, err := os.ReadDir(filepath.Join(devDir, "drm"))
	if err != nil {
		return "", nil
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "card") && !strings.Contains(e.Name(), "-") {
			card = e.Name()
			break
		}
	}
	if card == "" {
		return "", nil
	}
	conns, err := os.ReadDir(filepath.Join(devDir, "drm", card))
	if err != nil {
		return card, nil
	}
	for _, c := range conns {
		if !strings.HasPrefix(c.Name(), card+"-") {
			continue
		}
		status := utils.ReadTrim(filepath.Join(devDir, "drm", card, c.Name(), "status"))
		if status == "connected" {
			connected = append(connected, strings.TrimPrefix(c.Name(), card+"-"))
		}
	}
	return card, connected
}
