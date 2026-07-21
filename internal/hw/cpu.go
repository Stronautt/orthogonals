package hw

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CPU is the host CPU topology.
type CPU struct {
	Threads int   `json:"threads"`
	Cores   int   `json:"cores"`
	Hybrid  bool  `json:"hybrid"`
	PCores  []int `json:"p_cores"`
	ECores  []int `json:"e_cores,omitempty"`
}

// detectCPU reads root/sys/devices/system/cpu.
func detectCPU(root string) (CPU, error) {
	present, err := os.ReadFile(filepath.Join(root, "/sys/devices/system/cpu/present"))
	if err != nil {
		return CPU{}, fmt.Errorf("read cpu present: %w", err)
	}
	cpus, err := parseCPUList(strings.TrimSpace(string(present)))
	if err != nil {
		return CPU{}, fmt.Errorf("parse cpu present: %w", err)
	}
	c := CPU{Threads: len(cpus)}

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
		p, errP := parseCPUList(pList)
		e, errE := parseCPUList(eList)
		if errP == nil && errE == nil {
			c.Hybrid, c.PCores, c.ECores = true, p, e
			return c, nil
		}
	}
	c.PCores = cpus
	return c, nil
}

// parseCPUList parses kernel cpulist syntax.
func parseCPUList(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("bad cpulist %q", s)
		}
		b := a
		if isRange {
			b, err = strconv.Atoi(hi)
			if err != nil || b < a {
				return nil, fmt.Errorf("bad cpulist %q", s)
			}
		}
		for n := a; n <= b; n++ {
			out = append(out, n)
		}
	}
	return out, nil
}
