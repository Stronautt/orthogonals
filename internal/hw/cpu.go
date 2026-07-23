package hw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CPU vendor tokens from /proc/cpuinfo vendor_id.
const (
	CPUVendorIntel = "intel"
	CPUVendorAMD   = "amd"
)

// CPU is the host CPU topology.
type CPU struct {
	// Vendor is "intel", "amd", or "".
	Vendor  string `json:"vendor,omitempty"`
	Threads int    `json:"threads"`
	Cores   int    `json:"cores"`
	Hybrid  bool   `json:"hybrid"`
	PCores  []int  `json:"p_cores"`
	ECores  []int  `json:"e_cores,omitempty"`
}

// detectCPU reads root/sys/devices/system/cpu.
func detectCPU(root string) (CPU, error) {
	present, err := os.ReadFile(filepath.Join(root, "/sys/devices/system/cpu/present"))
	if err != nil {
		return CPU{}, fmt.Errorf("read cpu present: %w", err)
	}
	cpus, err := ParseCPUList(strings.TrimSpace(string(present)))
	if err != nil {
		return CPU{}, fmt.Errorf("parse cpu present: %w", err)
	}
	c := CPU{Threads: len(cpus), Vendor: cpuVendor(root)}

	coreIDs := map[string]bool{}
	for _, n := range cpus {
		id := readTrim(filepath.Join(root, "/sys/devices/system/cpu",
			fmt.Sprintf("cpu%d/topology/core_id", n)))
		if id != "" {
			coreIDs[id] = true
		}
	}
	c.Cores = len(coreIDs)
	if c.Cores == 0 {
		c.Cores = c.Threads
	}

	pList := readTrim(filepath.Join(root, "/sys/devices/cpu_core/cpus"))
	eList := readTrim(filepath.Join(root, "/sys/devices/cpu_atom/cpus"))
	if pList != "" && eList != "" {
		p, errP := ParseCPUList(pList)
		e, errE := ParseCPUList(eList)
		if errP == nil && errE == nil {
			c.Hybrid, c.PCores, c.ECores = true, p, e
			return c, nil
		}
	}
	c.PCores = cpus
	return c, nil
}

// cpuVendor maps /proc/cpuinfo vendor_id to a short token, "" when absent or unrecognized.
func cpuVendor(root string) string {
	b, err := os.ReadFile(filepath.Join(root, "/proc/cpuinfo"))
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "vendor_id")
		if !ok {
			continue
		}
		_, val, _ := strings.Cut(rest, ":")
		switch strings.TrimSpace(val) {
		case "GenuineIntel":
			return CPUVendorIntel
		case "AuthenticAMD":
			return CPUVendorAMD
		}
		return ""
	}
	return ""
}

// MaxCPUIndex bounds a parsed cpulist. Linux caps CONFIG_NR_CPUS at 8192, and
// the qemu hook parses cpusets from sysfs and from a domain XML a user may have
// edited: an unbounded range would expand to an allocation that kills the hook
// mid-handover, with the GPU already detached.
const MaxCPUIndex = 8191

// ParseCPUList parses kernel/libvirt cpulist syntax ("0-3,7,9-11") into CPU
// indices. The one parser for every cpulist in the tree — sysfs, domain XML,
// and hook state all go through the same bounds.
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil {
			return nil, fmt.Errorf("bad cpulist %q", s)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
				return nil, fmt.Errorf("bad cpulist %q", s)
			}
		}
		if b < a || b > MaxCPUIndex {
			return nil, fmt.Errorf("bad cpulist %q", s)
		}
		for n := a; n <= b; n++ {
			if len(out) > MaxCPUIndex {
				return nil, fmt.Errorf("bad cpulist %q", s)
			}
			out = append(out, n)
		}
	}
	return out, nil
}

// FormatCPUList renders CPU indices as a compact cpulist string ("0,1").
func FormatCPUList(cpus []int) string {
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = strconv.Itoa(c)
	}
	return strings.Join(s, ",")
}
