package hooks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/sysd"
)

const (
	cpusetSaveFile        = "/run/orthogonals-cpuset"
	cgroupControllersPath = "/sys/fs/cgroup/cgroup.controllers"
)

// isolationUnits are the host slices confined to the housekeeping cores while a
// VM runs. user.slice is excluded — the Looking Glass client runs there and
// needs full CPU — and so is machine.slice, where libvirt places the domain.
var isolationUnits = []string{"system.slice", "init.scope"}

// isolateCPUs confines host background daemons to the cores the VM does not pin.
// Every step is log-only: a cgroup tweak must never block a VM start.
func isolateCPUs(root string, sd sysd.Client, vm string) {
	log := hookLog(root, "cpu-isolate")
	if !cpusetControllerAvailable(root) {
		log("cgroup v2 cpuset controller unavailable — isolation skipped")
		return
	}
	present, err := hw.PresentCPUs(root)
	if err != nil {
		log("read present CPUs: %v — isolation skipped", err)
		return
	}
	hk := housekeepingCPUs(root, vm, present)
	if len(hk) == 0 {
		log("no cores reserved for the host — isolation skipped")
		return
	}
	// Save the full present set first so unisolate restores an unrestricted
	// cpuset even if only some units below were confined.
	save := filepath.Join(root, cpusetSaveFile)
	_ = os.MkdirAll(filepath.Dir(save), 0o755)
	_ = os.WriteFile(save, []byte(hw.FormatCPUList(present)), 0o644)
	for _, unit := range isolationUnits {
		if err := sd.SetAllowedCPUs(unit, hk); err != nil {
			log("confine %s: %v", unit, err)
		}
	}
	log("host confined to CPUs %s", hw.FormatCPUList(hk))
}

// unisolateCPUs restores an unrestricted cpuset. No-op without the marker, and
// never blocks teardown.
func unisolateCPUs(root string, sd sysd.Client) {
	log := hookLog(root, "cpu-isolate")
	save := filepath.Join(root, cpusetSaveFile)
	b, err := os.ReadFile(save)
	if err != nil {
		return
	}
	present, err := hw.ParseCPUList(string(b))
	if err != nil || len(present) == 0 {
		_ = os.Remove(save)
		return
	}
	for _, unit := range isolationUnits {
		if err := sd.SetAllowedCPUs(unit, present); err != nil {
			log("release %s: %v", unit, err)
		}
	}
	_ = os.Remove(save)
	log("host cpuset restored")
}

// housekeepingCPUs is the present CPUs minus every core the VM pins. Empty when
// the pinning leaves the host none — a no-E-core host shares cores with the
// emulator.
func housekeepingCPUs(root, vm string, present []int) []int {
	pinned, err := domain.ReadPinnedCPUs(root, vm)
	if err != nil {
		return nil
	}
	pinnedSet := make(map[int]bool, len(pinned))
	for _, c := range pinned {
		pinnedSet[c] = true
	}
	var hk []int
	for _, c := range present {
		if !pinnedSet[c] {
			hk = append(hk, c)
		}
	}
	return hk
}

func cpusetControllerAvailable(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, cgroupControllersPath))
	if err != nil {
		return false
	}
	return slices.Contains(strings.Fields(string(b)), "cpuset")
}
