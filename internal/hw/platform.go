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

// RequiredTools are host binaries later stages shell out to; detect reports
// presence, preflight gates on it (hard requirements fail, the rest warn).
var RequiredTools = []string{
	"dracut", "grubby", "virsh", "qemu-img", "xorriso",
	"semanage", "restorecon", "dnf", "lsof", "nvidia-smi",
	"wiminfo", // wimlib-utils: media validates the Win11 ISO edition list
}

// Platform holds host facts that gate or shape the passthrough setup.
type Platform struct {
	IOMMUAddressWidth int `json:"iommu_address_width"` // 0 = IOMMU off or absent
	// DMARTable: the ACPI DMAR table exists, i.e. the BIOS exposes VT-d even
	// when the kernel has not enabled translation (no intel_iommu=on yet) —
	// distinguishes "apply's kernel-args step will fix this" from "no VT-d".
	DMARTable     bool            `json:"dmar_table"`
	SELinux       string          `json:"selinux"` // enforcing|permissive|disabled
	SecureBoot    bool            `json:"secure_boot"`
	ChassisType   int             `json:"chassis_type"` // SMBIOS chassis type
	MemTotalBytes uint64          `json:"mem_total_bytes"`
	NVIDIA        NVIDIADriver    `json:"nvidia"`
	Tools         map[string]bool `json:"tools"`
}

// NVIDIADriver describes the loaded NVIDIA kernel module. Flavor matters:
// the open (GSP) module has documented unbind panics on driver teardown, so
// dynamic-binding bug reports need flavor + version + nvidia_drm state.
type NVIDIADriver struct {
	Loaded  bool   `json:"loaded"`
	Version string `json:"version,omitempty"`
	Flavor  string `json:"flavor,omitempty"`  // open|proprietary
	Modeset string `json:"modeset,omitempty"` // nvidia_drm.modeset Y|N, "" = nvidia_drm not loaded
	Fbdev   string `json:"fbdev,omitempty"`   // nvidia_drm.fbdev Y|N, "" = param absent
}

// detectPlatform gathers platform facts; individual facts degrade to zero
// values when their sysfs sources are absent.
func detectPlatform(root string) Platform {
	p := Platform{
		IOMMUAddressWidth: iommuAddressWidth(root),
		DMARTable:         dmarTablePresent(root),
		SELinux:           selinuxMode(root),
		SecureBoot:        secureBootEnabled(root),
		MemTotalBytes:     memTotalBytes(root),
		NVIDIA:            DetectNVIDIA(root),
		Tools:             map[string]bool{},
	}
	if n, err := strconv.Atoi(readTrim(filepath.Join(root, "/sys/class/dmi/id/chassis_type"))); err == nil {
		p.ChassisType = n
	}
	for _, tool := range RequiredTools {
		_, err := exec.LookPath(tool)
		p.Tools[tool] = err == nil
	}
	return p
}

// dmarTablePresent stats the ACPI DMAR table; the file is root-read-only but
// existence alone answers "is VT-d exposed by the firmware".
func dmarTablePresent(root string) bool {
	_, err := os.Stat(filepath.Join(root, "/sys/firmware/acpi/tables/DMAR"))
	return err == nil
}

// iommuAddressWidth decodes MGAW (bits 21:16, stores width-1) from each VT-d
// unit's CAP register and returns the minimum across units, matching the
// kernel's "DMAR: Host address width N" line. 0 means no active IOMMU.
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
	return width
}

// memTotalBytes parses the MemTotal line from /proc/meminfo (value is in kB).
func memTotalBytes(root string) uint64 {
	b, err := os.ReadFile(filepath.Join(root, "/proc/meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
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
		return kb * 1024
	}
	return 0
}

// nvidiaVersionRe matches a driver version token like 570.153.02.
var nvidiaVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// KernelVersion is the running kernel release (uname -r equivalent), read
// from /proc/sys/kernel/osrelease; empty when absent (fixture roots).
func KernelVersion(root string) string {
	return readTrim(filepath.Join(root, "/proc/sys/kernel/osrelease"))
}

// DetectNVIDIA reads the loaded NVIDIA module's flavor and version from
// /proc/driver/nvidia/version (NVRM line says "Open Kernel Module" for the
// open flavor) and nvidia_drm's modeset/fbdev parameters from sysfs.
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

// secureBootEnabled reads the SecureBoot efivar: 4 attribute bytes + 1 value
// byte; value 1 means enabled. Missing var (BIOS boot) reads as disabled.
func secureBootEnabled(root string) bool {
	b, err := os.ReadFile(filepath.Join(root,
		"/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c"))
	return err == nil && len(b) == 5 && b[4] == 1
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
