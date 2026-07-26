package orchestrate

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
)

// Check is one status or verify result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Healthy reports whether every check passed.
func Healthy(cs []Check) bool {
	for _, c := range cs {
		if !c.OK {
			return false
		}
	}
	return true
}

// Status is the health check behind orthogonals status.
func Status(root string) []Check {
	m, err := steps.Load(root)
	if err != nil {
		return []Check{{Name: "manifest", Detail: err.Error()}}
	}
	if len(m.Records) == 0 {
		return []Check{{Name: "applied", Detail: "nothing applied — run `orthogonals apply`"}}
	}
	var out []Check
	add := func(name string, err error) {
		c := Check{Name: name, OK: err == nil}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
	}

	if want, err := manifestKernelArgs(root); err == nil {
		add("kernel arguments", kargsLive(root, want))
		add("iommu", iommuActive(root))
		add("vfio module", vfioModuleLoaded(root))
	} else {
		add("kernel arguments", err)
	}

	devs, err := hw.ScanPCI(root)
	if err != nil {
		add("gpu scan", fmt.Errorf("cannot read PCI devices: %w", err))
	}
	for _, d := range devs {
		if d.Vendor != hw.VendorNVIDIA || !strings.HasPrefix(d.Class, hw.ClassDisplay) {
			continue
		}
		var err error
		switch d.Driver {
		case "nvidia", "vfio-pci":
		case "":
			err = fmt.Errorf("no driver bound — run `orthogonals recover --yes`")
		default:
			err = fmt.Errorf("bound to unexpected driver %s", d.Driver)
		}
		c := Check{Name: "gpu binding " + d.Address, OK: err == nil, Detail: "bound to " + d.Driver}
		if err != nil {
			c.Detail = err.Error()
		}
		out = append(out, c)
	}

	if m.Has(hooks.DispatcherStepID) {
		var missing, unreadable []string
		for _, p := range hooks.InstalledPaths() {
			// An unreadable path is not a missing one: re-running apply is the
			// wrong advice when the answer is that this process cannot look.
			switch ok, statErr := utils.Exists(filepath.Join(root, p)); {
			case statErr != nil:
				unreadable = append(unreadable, p)
			case !ok:
				missing = append(missing, p)
			}
		}
		var err error
		if len(unreadable) > 0 {
			err = fmt.Errorf("cannot read %s — run as root to check", strings.Join(unreadable, ", "))
		}
		if len(missing) > 0 {
			err = fmt.Errorf("missing %s — re-run `orthogonals apply --yes`", strings.Join(missing, ", "))
		}
		add("libvirt hooks", err)
	}

	if m.Has(hostcfg.SwitcherooStepID) {
		var err error
		if !steps.UnitEnabled(root, hostcfg.UnitSwitcheroo) {
			err = fmt.Errorf("%s is not enabled — GNOME's dGPU launch menu will be missing", hostcfg.UnitSwitcheroo)
		}
		add("switcheroo-control", err)
	}
	out = append(out, lookingGlassBackend(root)...)
	rpmnew, _ := utils.Exists(filepath.Join(root, steps.QemuConfPath+".rpmnew"))
	if m.Has(hostcfg.DeviceACLStepID) && rpmnew {
		add("libvirt device acl", fmt.Errorf(
			"libvirt shipped a new %s (.rpmnew): its default device list may have grown past ours — re-run `orthogonals apply --yes`",
			steps.QemuConfPath))
	}
	return out
}

// lookingGlassBackend reports which frame-buffer backend each defined VM uses,
// and fails when a domain wants kvmfr on a host that no longer has the module
// built — the state in which the qemu hook refuses the start.
func lookingGlassBackend(root string) []Check {
	names, err := filepath.Glob(filepath.Join(steps.VMsDir(root), "*.xml"))
	if err != nil {
		return nil
	}
	var out []Check
	for _, path := range names {
		vm := strings.TrimSuffix(filepath.Base(path), ".xml")
		size, kvmfr := domain.KVMFRSizeMiB(root, vm)
		switch {
		case !kvmfr:
			out = append(out, Check{Name: "looking glass " + vm, OK: true,
				Detail: "frame buffer on " + steps.LookingGlassSHM})
		case hw.KVMFRAvailable(root):
			out = append(out, Check{Name: "looking glass " + vm, OK: true,
				Detail: fmt.Sprintf("frame buffer on %s, %d MiB (DMABUF)", steps.KVMFRDevice, size)})
		default:
			out = append(out, Check{Name: "looking glass " + vm, Detail: fmt.Sprintf(
				"%s wants %s but the kvmfr module is not built for kernel %s — the VM will refuse to start; run `sudo orthogonals up` to fall back to %s",
				vm, steps.KVMFRDevice, hw.KernelVersion(root), steps.LookingGlassSHM)})
		}
	}
	return out
}
