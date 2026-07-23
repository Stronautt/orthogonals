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

// CPU-isolation paths and the pre-VM restore marker.
const (
	cpusetSaveFile        = "/run/orthogonals-cpuset"
	cgroupControllersPath = "/sys/fs/cgroup/cgroup.controllers"
	presentCPUsPath       = "/sys/devices/system/cpu/present"
)

// isolationUnits are the host slices confined to the housekeeping cores while a
// VM runs. user.slice is deliberately excluded — the Looking Glass client runs
// there and needs full CPU. machine.slice (QEMU) is a separate top-level sibling
// and is never touched; libvirt places orthogonals domains there.
var isolationUnits = []string{"system.slice", "init.scope"}

// isolateCPUs confines host background daemons to the cores the VM does not pin,
// keeping the guest's vCPU/emulator/iothread cores free of host jitter. Every
// step is best-effort and log-only: a cgroup tweak must never block a VM start.
func isolateCPUs(root string, sd sysd.Client, vm string) {
	log := hookLog(root, "cpu-isolate")
	if !cpusetControllerAvailable(root) {
		log("cgroup v2 cpuset controller unavailable — isolation skipped")
		return
	}
	present, err := readPresentCPUs(root)
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

// unisolateCPUs lifts a prior isolation by restoring an unrestricted cpuset.
// No-op when the marker is absent. Best-effort; never blocks teardown.
func unisolateCPUs(root string, sd sysd.Client) {
	log := hookLog(root, "cpu-isolate")
	save := filepath.Join(root, cpusetSaveFile)
	b, err := os.ReadFile(save)
	if err != nil {
		return
	}
	present, err := domain.ParseCPUSet(string(b))
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

// housekeepingCPUs is the present CPUs minus every core the VM pins to guest
// threads — what is left for the host. Empty when the pinning leaves the host no
// dedicated core (e.g. a no-E-core host sharing cores with the emulator).
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

// cpusetControllerAvailable reports cgroup v2 with the cpuset controller present.
func cpusetControllerAvailable(root string) bool {
	b, err := os.ReadFile(filepath.Join(root, cgroupControllersPath))
	if err != nil {
		return false
	}
	return slices.Contains(strings.Fields(string(b)), "cpuset")
}

// readPresentCPUs parses the host's set of populated CPUs.
func readPresentCPUs(root string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(root, presentCPUsPath))
	if err != nil {
		return nil, err
	}
	return domain.ParseCPUSet(string(b))
}
