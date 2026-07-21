package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	godbus "github.com/godbus/dbus/v5"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/virt"
)

// Facts are host facts preflight needs beyond the detect result.
type Facts struct {
	PersistencedEnabled bool     `json:"nvidia_persistenced_enabled"`
	DefaultNetActive    bool     `json:"libvirt_default_net_active"`
	FreeDiskBytes       uint64   `json:"free_disk_bytes"`
	OrthogonalsManaged  bool     `json:"orthogonals_managed"`
	ForeignVFIO         []string `json:"foreign_vfio,omitempty"`
	SwitcherooEnabled   bool     `json:"switcheroo_enabled"`
	SwitcherooNVIDIA    bool     `json:"switcheroo_nvidia_listed"`
	LibvirtReachable    bool     `json:"libvirt_reachable"`
	// BLSError is the error message from reading /boot/loader/entries.
	BLSError string `json:"bls_error,omitempty"`
}

// GatherFacts reads the live host (prefixed by root, the test seam).
func GatherFacts(root string) Facts {
	f := Facts{
		PersistencedEnabled: steps.UnitEnabled(root, hostcfg.UnitPersistenced),
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
		LibvirtReachable:   libvirtReachable(root),
	}
	if f.SwitcherooEnabled {
		f.SwitcherooNVIDIA = switcherooListsNVIDIA(root)
	}
	if _, err := bls.Tokens(root); err != nil {
		f.BLSError = err.Error()
	}
	return f
}

// scanForeignVFIO finds vfio configuration orthogonals did not write.
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

// switcherooListsNVIDIA reports whether the daemon lists the NVIDIA GPU for offload.
var switcherooListsNVIDIA = func(root string) bool {
	if root != "" {
		return false
	}
	conn, err := godbus.SystemBus()
	if err != nil {
		return false
	}
	obj := conn.Object("net.hadess.SwitcherooControl", "/net/hadess/SwitcherooControl")
	v, err := obj.GetProperty("net.hadess.SwitcherooControl.GPUs")
	if err != nil {
		return false
	}
	gpus, ok := v.Value().([]map[string]godbus.Variant)
	if !ok {
		return false
	}
	for _, g := range gpus {
		name, _ := g["Name"].Value().(string)
		env, _ := g["Environment"].Value().([]string)
		if strings.Contains(name, "NVIDIA") && len(env) > 0 {
			return true
		}
	}
	return false
}

// libvirtReachable probes the local libvirt socket.
var libvirtReachable = func(root string) bool {
	if root != "" {
		return true
	}
	c := virt.New()
	defer func() { _ = c.Close() }()
	return c.Ping() == nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// freeDisk returns available bytes at the first path statfs accepts.
func freeDisk(paths ...string) uint64 {
	for _, p := range paths {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err == nil {
			return st.Bavail * uint64(st.Bsize)
		}
	}
	return 0
}
