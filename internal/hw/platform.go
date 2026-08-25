package hw

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/stronautt/orthogonals/internal/utils"
)

// RequiredTools are host binaries later stages shell out to.
var RequiredTools = []string{
	"dracut", "semanage", "restorecon", "nvidia-smi", "systemd-tmpfiles",
	// dkms builds and installs kvmfr on a Secure Boot host. usermod puts the
	// desktop user in the libvirt group. Both are apply steps and not options.
	"dkms", "usermod",
}

type Platform struct {
	IOMMUAddressWidth int `json:"iommu_address_width"`
	// IOMMUTable names the ACPI table the firmware exposes its IOMMU through:
	// "DMAR" (Intel VT-d), "IVRS" (AMD-Vi), or "" when there is none.
	IOMMUTable  string `json:"iommu_table,omitempty"`
	SELinux     string `json:"selinux"`
	SecureBoot  bool   `json:"secure_boot"`
	ChassisType int    `json:"chassis_type"`
	// GPUMux is the ASUS display MUX mode ("hybrid"/"discrete"/"").
	GPUMux        string          `json:"gpu_mux,omitempty"`
	FirmwareIOMMU []FirmwareAttr  `json:"firmware_iommu,omitempty"`
	MemTotalBytes uint64          `json:"mem_total_bytes"`
	NVIDIA        NVIDIADriver    `json:"nvidia"`
	Tools         map[string]bool `json:"tools"`
}

type NVIDIADriver struct {
	Loaded  bool   `json:"loaded"`
	Version string `json:"version,omitempty"`
	Flavor  string `json:"flavor,omitempty"`
	Modeset string `json:"modeset,omitempty"`
	Fbdev   string `json:"fbdev,omitempty"`
}

func detectPlatform(root string) Platform {
	p := Platform{
		IOMMUAddressWidth: iommuAddressWidth(root),
		IOMMUTable:        iommuTable(root),
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

const (
	IOMMUTableDMAR = "DMAR"
	IOMMUTableIVRS = "IVRS"
)

func iommuTable(root string) string {
	for _, table := range []string{IOMMUTableDMAR, IOMMUTableIVRS} {
		if _, err := os.Stat(filepath.Join(root, "/sys/firmware/acpi/tables", table)); err == nil {
			return table
		}
	}
	return ""
}

// iommuAddressWidth decodes the host DMA address width from the VT-d CAP register.
// ponytail: an AMD-Vi ivhd unit ⇒ 48; parse IVRS if a sub-40-bit AMD host appears.
func iommuAddressWidth(root string) int {
	caps, _ := filepath.Glob(filepath.Join(root, "/sys/class/iommu/dmar*/intel-iommu/cap"))
	width := 0
	for _, f := range caps {
		reg, err := strconv.ParseUint(utils.ReadTrim(f), 16, 64)
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

func memTotalBytes(root string) uint64 { return MeminfoKiB(root, "MemTotal:") * utils.BytesPerKiB }

var nvidiaVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

func KernelVersion(root string) string {
	return utils.ReadTrim(filepath.Join(root, "/proc/sys/kernel/osrelease"))
}

// KVMFRAvailable reports whether the kvmfr module is built for the running
// kernel — whether it exists, never whether it is loaded: up crosses a reboot
// between apply and vm define, so a loaded-state test would put every host on
// /dev/shm for the second leg. modules.dep is what modprobe consults, so the
// answer holds whichever directory dkms used and whatever compression suffix
// the distro applies.
func KVMFRAvailable(root string) bool {
	release := KernelVersion(root)
	if release == "" {
		return false
	}
	// Read whole rather than scan: a dependency line runs past bufio.Scanner's
	// default limit, and a truncated read silently downgrades the backend.
	b, err := os.ReadFile(filepath.Join(root, "/lib/modules", release, "modules.dep"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(b)) {
		path, _, ok := strings.Cut(line, ":")
		if ok && strings.HasPrefix(filepath.Base(path), "kvmfr.ko") {
			return true
		}
	}
	return false
}

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
	d.Modeset = utils.ReadTrim(filepath.Join(root, "/sys/module/nvidia_drm/parameters/modeset"))
	d.Fbdev = utils.ReadTrim(filepath.Join(root, "/sys/module/nvidia_drm/parameters/fbdev"))
	return d
}

func selinuxMode(root string) string {
	switch utils.ReadTrim(filepath.Join(root, "/sys/fs/selinux/enforce")) {
	case "1":
		return "enforcing"
	case "0":
		return "permissive"
	default:
		return "disabled"
	}
}

func secureBootEnabled(root string) bool {
	b, err := os.ReadFile(filepath.Join(root,
		"/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"))
	// The flag comes after the attribute mask of efivarfs, so the whole read is
	// five bytes and not one.
	return err == nil && len(b) == utils.EFIVarAttrLen+1 && b[utils.EFIVarAttrLen] == 1
}

// ChassisType reads the SMBIOS chassis type from sysfs, 0 when absent.
func ChassisType(root string) int {
	n, _ := strconv.Atoi(utils.ReadTrim(filepath.Join(root, "/sys/class/dmi/id/chassis_type")))
	return n
}

var laptopChassisTypes = map[int]bool{
	8: true, 9: true, 10: true, 11: true, 14: true, 30: true, 31: true, 32: true,
}

func IsLaptopChassis(t int) bool {
	return laptopChassisTypes[t]
}

var chassisNames = map[int]string{
	3: "desktop", 4: "low-profile desktop", 6: "mini tower", 7: "tower",
	9: "laptop", 10: "notebook", 13: "all-in-one", 14: "sub notebook",
	30: "tablet", 31: "convertible", 32: "detachable",
}

func ChassisName(t int) string {
	if n, ok := chassisNames[t]; ok {
		return n
	}
	return fmt.Sprintf("type %d", t)
}
