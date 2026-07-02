package preflight

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/steps"
)

// Facts are host facts preflight needs beyond the detect result; gathered
// separately so analyzers stay pure functions over injectable inputs.
type Facts struct {
	PersistencedEnabled bool     `json:"nvidia_persistenced_enabled"`
	DefaultNetActive    bool     `json:"libvirt_default_net_active"`
	FreeDiskBytes       uint64   `json:"free_disk_bytes"`
	OrthogonalsManaged  bool     `json:"orthogonals_managed"`
	ForeignVFIO         []string `json:"foreign_vfio,omitempty"`
	SwitcherooEnabled   bool     `json:"switcheroo_enabled"`
	SwitcherooNVIDIA    bool     `json:"switcheroo_nvidia_listed"`
}

// GatherFacts reads the live host (prefixed by root, the test seam).
func GatherFacts(root string) Facts {
	f := Facts{
		PersistencedEnabled: steps.UnitEnabled(root, hostcfg.UnitPersistenced),
		// libvirt writes a live status XML per active network.
		DefaultNetActive: exists(filepath.Join(root, "/var/run/libvirt/network/default.xml")) ||
			exists(filepath.Join(root, "/run/libvirt/network/default.xml")),
		FreeDiskBytes: freeDisk(
			filepath.Join(root, "/var/lib/libvirt/images"),
			filepath.Join(root, "/var/lib"),
			filepath.Join(root, "/"),
		),
		OrthogonalsManaged: exists(steps.ManifestPath(root)),
		ForeignVFIO:        scanForeignVFIO(root),
		SwitcherooEnabled:  steps.UnitEnabled(root, hostcfg.UnitSwitcheroo),
	}
	if f.SwitcherooEnabled {
		f.SwitcherooNVIDIA = switcherooListsNVIDIA()
	}
	return f
}

// scanForeignVFIO finds vfio configuration orthogonals did not write: stray
// modprobe.d options, dracut confs, and live kernel args (research §C4).
// Ownership is decided by the caller via OrthogonalsManaged, not here.
func scanForeignVFIO(root string) []string {
	var found []string
	for _, pattern := range []string{"/etc/modprobe.d/*.conf", "/etc/dracut.conf.d/*.conf"} {
		files, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, path := range files {
			for _, line := range vfioLines(path) {
				found = append(found, strings.TrimPrefix(path, root)+": "+line)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "/proc/cmdline")); err == nil {
		for _, arg := range strings.Fields(string(b)) {
			if strings.Contains(arg, "vfio") {
				found = append(found, "kernel cmdline: "+arg)
			}
		}
	}
	return found
}

// vfioLines returns non-comment lines mentioning vfio.
func vfioLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.Contains(trimmed, "vfio") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// switcherooListsNVIDIA asks the running daemon for its GPU list. The daemon
// enumerates GPUs only at startup, so a list without an NVIDIA offload entry
// means GNOME's "Launch using Discrete Graphics Card" menu is missing/stale.
func switcherooListsNVIDIA() bool {
	out, err := exec.Command("switcherooctl", "list").Output()
	if err != nil {
		return false
	}
	for _, block := range strings.Split(string(out), "Device:") {
		if strings.Contains(block, "NVIDIA") && strings.Contains(block, "Environment:") {
			return true
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freeDisk returns available bytes at the first path statfs accepts, so fake
// roots without the libvirt tree fall back to the root filesystem.
func freeDisk(paths ...string) uint64 {
	for _, p := range paths {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return st.Bavail * uint64(st.Bsize)
		}
	}
	return 0
}
