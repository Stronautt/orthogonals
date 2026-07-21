package hw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RequiredTools are host binaries later stages shell out to.
var RequiredTools = []string{
	"dracut", "semanage", "restorecon", "nvidia-smi",
}

// Platform holds host facts that gate or shape the passthrough setup.
type Platform struct {
	IOMMUAddressWidth int `json:"iommu_address_width"`
	// IOMMUTable reports that the firmware exposes an IOMMU via an ACPI table
	// (Intel DMAR or AMD IVRS).
	IOMMUTable  bool   `json:"iommu_table"`
	SELinux     string `json:"selinux"`
	SecureBoot  bool   `json:"secure_boot"`
	ChassisType int    `json:"chassis_type"`
	// GPUMux is the ASUS display MUX mode ("hybrid"/"discrete"/"").
	GPUMux string `json:"gpu_mux,omitempty"`
	// FirmwareIOMMU are BIOS attributes controlling the IOMMU, when exposed.
	FirmwareIOMMU []FirmwareAttr  `json:"firmware_iommu,omitempty"`
	MemTotalBytes uint64          `json:"mem_total_bytes"`
	NVIDIA        NVIDIADriver    `json:"nvidia"`
	Tools         map[string]bool `json:"tools"`
}

// NVIDIADriver describes the loaded NVIDIA kernel module.
type NVIDIADriver struct {
	Loaded  bool   `json:"loaded"`
	Version string `json:"version,omitempty"`
	Flavor  string `json:"flavor,omitempty"`
	Modeset string `json:"modeset,omitempty"`
	Fbdev   string `json:"fbdev,omitempty"`
}

// detectPlatform gathers platform facts.
func detectPlatform(root string) Platform {
	p := Platform{
		IOMMUAddressWidth: iommuAddressWidth(root),
		IOMMUTable:        iommuTablePresent(root),
		SELinux:           selinuxMode(root),
		SecureBoot:        secureBootEnabled(root),
		MemTotalBytes:     memTotalBytes(root),
		NVIDIA:            DetectNVIDIA(root),
		Tools:             map[string]bool{},
	}
	p.ChassisType = ChassisType(root)
	p.GPUMux = gpuMux(root)
	p.FirmwareIOMMU = firmwareIOMMUAttrs(root)
	for _, tool := range RequiredTools {
		_, err := exec.LookPath(tool)
		p.Tools[tool] = err == nil
	}
	return p
}

// iommuTablePresent stats the firmware IOMMU ACPI table: Intel DMAR or AMD IVRS.
func iommuTablePresent(root string) bool {
	for _, table := range []string{"DMAR", "IVRS"} {
		if _, err := os.Stat(filepath.Join(root, "/sys/firmware/acpi/tables", table)); err == nil {
			return true
		}
	}
	return false
}

// iommuAddressWidth decodes the host DMA address width from the VT-d CAP register.
// ponytail: an AMD-Vi ivhd unit ⇒ 48; parse IVRS if a sub-40-bit AMD host appears.
func iommuAddressWidth(root string) int {
	caps, _ := filepath.Glob(filepath.Join(root, "/sys/class/iommu/dmar*/intel-iommu/cap"))
	width := 0
	for _, f := range caps {
		reg, err := strconv.ParseUint(readTrim(f), 16, 64)
		if err != nil {
			continue
		}
		w := int((reg>>16)&0x3f) + 1
		if width == 0 || w < width {
			width = w
		}
	}
	if width == 0 {
		if ivhd, _ := filepath.Glob(filepath.Join(root, "/sys/class/iommu/ivhd*")); len(ivhd) > 0 {
			return 48
		}
	}
	return width
}

// MeminfoKiB reads a "Key:" field from /proc/meminfo in KiB, 0 when absent or unreadable.
func MeminfoKiB(root, key string) uint64 {
	b, err := os.ReadFile(filepath.Join(root, "/proc/meminfo"))
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, key)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0
		}
		return kb
	}
	return 0
}

// memTotalBytes is the host's total RAM from /proc/meminfo.
func memTotalBytes(root string) uint64 { return MeminfoKiB(root, "MemTotal:") * 1024 }

// nvidiaVersionRe matches a driver version token like 570.153.02.
var nvidiaVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// KernelVersion is the running kernel release.
func KernelVersion(root string) string {
	return readTrim(filepath.Join(root, "/proc/sys/kernel/osrelease"))
}

// DetectNVIDIA reads the loaded NVIDIA module's flavor and version.
func DetectNVIDIA(root string) NVIDIADriver {
	var d NVIDIADriver
	b, err := os.ReadFile(filepath.Join(root, "/proc/driver/nvidia/version"))
	if err != nil {
		return d
	}
	d.Loaded = true
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "NVRM version:")
		if !ok {
			continue
		}
		if strings.Contains(rest, "Open Kernel Module") {
			d.Flavor = "open"
		} else {
			d.Flavor = "proprietary"
		}
		for _, f := range strings.Fields(rest) {
			if nvidiaVersionRe.MatchString(f) {
				d.Version = f
				break
			}
		}
		break
	}
	d.Modeset = readTrim(filepath.Join(root, "/sys/module/nvidia_drm/parameters/modeset"))
	d.Fbdev = readTrim(filepath.Join(root, "/sys/module/nvidia_drm/parameters/fbdev"))
	return d
}

func selinuxMode(root string) string {
	switch readTrim(filepath.Join(root, "/sys/fs/selinux/enforce")) {
	case "1":
		return "enforcing"
	case "0":
		return "permissive"
	default:
		return "disabled"
	}
}

// secureBootEnabled reads the SecureBoot efivar.
func secureBootEnabled(root string) bool {
	b, err := os.ReadFile(filepath.Join(root,
		"/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"))
	return err == nil && len(b) == 5 && b[4] == 1
}

// ChassisType reads the SMBIOS chassis type from sysfs, 0 when absent.
func ChassisType(root string) int {
	n, _ := strconv.Atoi(readTrim(filepath.Join(root, "/sys/class/dmi/id/chassis_type")))
	return n
}

// laptopChassisTypes are the portable SMBIOS chassis types.
var laptopChassisTypes = map[int]bool{
	8: true, 9: true, 10: true, 11: true, 14: true, 30: true, 31: true, 32: true,
}

// IsLaptopChassis reports whether an SMBIOS chassis type is a portable machine.
func IsLaptopChassis(t int) bool {
	return laptopChassisTypes[t]
}

// ChassisName maps the SMBIOS chassis type to a human label.
func ChassisName(t int) string {
	names := map[int]string{
		3: "desktop", 4: "low-profile desktop", 6: "mini tower", 7: "tower",
		9: "laptop", 10: "notebook", 13: "all-in-one", 14: "sub notebook",
		30: "tablet", 31: "convertible", 32: "detachable",
	}
	if n, ok := names[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", t)
}
