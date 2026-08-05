package preflight

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	godbus "github.com/godbus/dbus/v5"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
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
	// BLSUnreadable marks that directory as root-only (0700 on Fedora): an
	// unprivileged preflight cannot judge the boot entries either way.
	BLSUnreadable bool          `json:"bls_unreadable,omitempty"`
	Signing       ModuleSigning `json:"module_signing"`
	// GrubError is why /etc/default/grub cannot be edited, "" when it can. Kept
	// apart from BLSError: the remedy is one line in one file, not a boot-config
	// conversion.
	GrubError string `json:"grub_error,omitempty"`
	// KernelArgs is what became of the args a previous apply journaled, and
	// KernelArgsMissing the tokens that state is about.
	KernelArgs        KernelArgsState `json:"kernel_args_state,omitempty"`
	KernelArgsMissing []string        `json:"kernel_args_missing,omitempty"`
}

// KernelArgsState is what preflight knows about the kernel args a previous apply
// recorded. Every inactive-IOMMU remedy turns on it, and "apply will fix this"
// is true in exactly one of them.
type KernelArgsState string

const (
	// KernelArgsUnknown is an unprivileged run: the manifest is root-only, so
	// this preflight cannot tell whether apply has ever run. Reporting it as
	// never-applied is what told a host that had lost its args to run the apply
	// it had already run. It is the zero value on purpose — a Facts nobody
	// filled in knows nothing, and saying so is the one answer never wrong.
	KernelArgsUnknown KernelArgsState = ""
	KernelArgsNever   KernelArgsState = "never-applied"
	// KernelArgsLostBoot is journaled args the boot config no longer carries —
	// what a regeneration from /etc/default/grub leaves behind.
	KernelArgsLostBoot KernelArgsState = "missing-from-boot-config"
	KernelArgsPending  KernelArgsState = "pending-reboot"
	KernelArgsLive     KernelArgsState = "live"
)

// GatherFacts reads the live host (prefixed by root, the test seam). Every
// probe is best-effort: an unreadable path is reported as the absent fact it
// looks like, and preflight's checks phrase their remedies as suggestions.
func GatherFacts(root string) Facts {
	netActive, _ := utils.Exists(filepath.Join(root, "/var/run/libvirt/network/default.xml"))
	if !netActive {
		netActive, _ = utils.Exists(filepath.Join(root, "/run/libvirt/network/default.xml"))
	}
	managed, _ := utils.Exists(steps.ManifestPath(root))
	f := Facts{
		PersistencedEnabled: steps.UnitEnabled(root, hostcfg.UnitPersistenced),
		DefaultNetActive:    netActive,
		FreeDiskBytes: freeDisk(
			filepath.Join(root, domain.ImagesDir),
			filepath.Join(root, "/var/lib"),
			filepath.Join(root, "/"),
		),
		OrthogonalsManaged: managed,
		ForeignVFIO:        scanForeignVFIO(root),
		SwitcherooEnabled:  steps.UnitEnabled(root, hostcfg.UnitSwitcheroo),
		LibvirtReachable:   libvirtReachable(root),
		Signing:            gatherSigning(root),
	}
	if f.SwitcherooEnabled {
		f.SwitcherooNVIDIA = switcherooListsNVIDIA(root)
	}
	f.classifyBoot(bls.CheckAccess(root))
	f.KernelArgs, f.KernelArgsMissing = kernelArgsState(root)
	return f
}

// classifyBoot routes a boot-config read error to the check that owns it: a
// root-only entries directory is not a broken host, and a grub line this build
// will not edit is not a BLS-entry problem.
func (f *Facts) classifyBoot(err error) {
	var ge *bls.GrubError
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrPermission):
		f.BLSUnreadable = true
	case errors.As(err, &ge):
		f.GrubError = ge.Error()
	default:
		f.BLSError = err.Error()
	}
}

// kernelArgsState answers what became of the args a previous apply journaled. A
// manifest it cannot read reports unknown, never never-applied: folding "cannot
// read" into "was never there" is the same mistake as folding EACCES into
// "absent", and here it prints "apply will add them" to a host that has.
func kernelArgsState(root string) (KernelArgsState, []string) {
	args, err := hostcfg.JournaledKernelArgs(root)
	switch {
	case errors.Is(err, hostcfg.ErrNoKernelArgsStep):
		return KernelArgsNever, nil
	case err != nil:
		return KernelArgsUnknown, nil
	}
	// A boot config that will not parse is already reported: CheckAccess runs the
	// same parse. Falling through to the live check keeps the remedy at "reboot"
	// rather than inventing a state out of a file nobody could read.
	if w, err := bls.Wanted(root, args); err == nil && len(w.Missing) > 0 {
		return KernelArgsLostBoot, w.Missing
	}
	// An unreadable /proc/cmdline is not proof the args are live. Reporting
	// them all missing keeps the remedy at "reboot", where reading it as
	// live would blame the firmware for a state never observed.
	missing, err := bls.MissingLive(root, args)
	if err != nil {
		missing = strings.Fields(args)
	}
	if len(missing) > 0 {
		return KernelArgsPending, missing
	}
	return KernelArgsLive, nil
}

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

func libvirtReachable(root string) bool {
	if root != "" {
		return true
	}
	c := virt.New()
	defer func() { _ = c.Close() }()
	return c.Ping() == nil
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
