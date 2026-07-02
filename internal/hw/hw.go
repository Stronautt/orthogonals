package hw

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Result is the full detect output. Its JSON form is the input contract for
// preflight and every later stage.
type Result struct {
	Devices  []PCIDevice `json:"devices"`
	GPUs     GPUs        `json:"gpus"`
	CPU      CPU         `json:"cpu"`
	Platform Platform    `json:"platform"`
}

// Detect scans PCI, CPU, and platform facts under root.
func Detect(root string) (*Result, error) {
	devs, err := ScanPCI(root)
	if err != nil {
		return nil, err
	}
	cpu, err := detectCPU(root)
	if err != nil {
		return nil, err
	}
	return &Result{
		Devices:  devs,
		GPUs:     classifyGPUs(devs),
		CPU:      cpu,
		Platform: detectPlatform(root),
	}, nil
}

// IOMMUActive reports whether the running kernel has IOMMU groups. The error
// is non-nil only when sys/kernel/iommu_groups exists but cannot be read
// (e.g. permissions), so callers can tell "off" from "unreadable".
func IOMMUActive(root string) (bool, error) {
	groups, err := os.ReadDir(filepath.Join(root, "/sys/kernel/iommu_groups"))
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read iommu groups: %w", err)
	}
	return len(groups) > 0, nil
}

// ModuleLoaded reports whether a kernel module is loaded (sys/module entry).
func ModuleLoaded(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, "/sys/module", name))
	return err == nil
}

// Summary renders the human-readable detect report.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "CPU: %d cores / %d threads", r.CPU.Cores, r.CPU.Threads)
	if r.CPU.Hybrid {
		fmt.Fprintf(&b, " (%d P-threads, %d E-cores)", len(r.CPU.PCores), len(r.CPU.ECores))
	}
	b.WriteString("\n")
	if r.Platform.MemTotalBytes > 0 {
		fmt.Fprintf(&b, "RAM: %.1f GiB\n", float64(r.Platform.MemTotalBytes)/(1<<30))
	}

	if r.GPUs.IGPU != nil {
		fmt.Fprintf(&b, "iGPU: %s\n", devLine(*r.GPUs.IGPU))
	} else {
		b.WriteString("iGPU: none\n")
	}
	if len(r.GPUs.DGPUs) == 0 {
		b.WriteString("dGPU: none\n")
	}
	for _, d := range r.GPUs.DGPUs {
		fmt.Fprintf(&b, "dGPU: %s\n", devLine(d.PCIDevice))
		if d.Audio != nil {
			fmt.Fprintf(&b, "  audio: %s\n", devLine(*d.Audio))
		}
	}

	if r.Platform.IOMMUAddressWidth > 0 {
		fmt.Fprintf(&b, "IOMMU: on, host address width %d\n", r.Platform.IOMMUAddressWidth)
	} else if r.Platform.DMARTable {
		b.WriteString("IOMMU: off (firmware exposes VT-d — apply enables it)\n")
	} else {
		b.WriteString("IOMMU: off or unsupported\n")
	}
	fmt.Fprintf(&b, "SELinux: %s\n", r.Platform.SELinux)
	secureBoot := "disabled"
	if r.Platform.SecureBoot {
		secureBoot = "enabled"
	}
	fmt.Fprintf(&b, "Secure Boot: %s\n", secureBoot)
	fmt.Fprintf(&b, "Chassis: %s\n", ChassisName(r.Platform.ChassisType))
	if n := r.Platform.NVIDIA; n.Loaded {
		drm := "nvidia_drm not loaded"
		if n.Modeset != "" {
			drm = "nvidia_drm modeset=" + n.Modeset
			if n.Fbdev != "" {
				drm += " fbdev=" + n.Fbdev
			}
		}
		fmt.Fprintf(&b, "NVIDIA driver: %s (%s), %s\n", n.Version, n.Flavor, drm)
	} else {
		b.WriteString("NVIDIA driver: not loaded\n")
	}
	for _, tool := range RequiredTools {
		state := "MISSING"
		if r.Platform.Tools[tool] {
			state = "ok"
		}
		fmt.Fprintf(&b, "Tool %s: %s\n", tool, state)
	}
	return b.String()
}

func devLine(d PCIDevice) string {
	driver := d.Driver
	if driver == "" {
		driver = "no driver"
	}
	group := "no IOMMU group"
	if d.IOMMUGroup >= 0 {
		group = fmt.Sprintf("IOMMU group %d", d.IOMMUGroup)
	}
	return fmt.Sprintf("%s %s (%s, %s)", d.Address, d.VendorDeviceID(), driver, group)
}
