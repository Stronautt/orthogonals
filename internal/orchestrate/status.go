package orchestrate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
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

// Status is the lightweight health check behind `orthogonals status`
// (research §C5): catches kernel or driver updates silently undoing the
// setup. It only checks what the manifest says orthogonals applied.
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
		// records exist but no kernel-args step: partial apply/undo — say so
		// instead of silently skipping the boot-config checks
		add("kernel arguments", err)
	}

	// nvidia (VM off) and vfio-pci (VM running) are both healthy bindings;
	// no driver means a failed rebind — `orthogonals recover` territory
	devs, err := hw.ScanPCI(root)
	if err != nil {
		// a health check that can't read the PCI tree must report a failure,
		// not silently skip the very binding a driver update would break
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
		// every hook script must exist for dynamic binding to work; the
		// dispatcher alone starts VMs without ever detaching the GPU
		var missing []string
		for _, p := range hooks.InstalledPaths() {
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				missing = append(missing, p)
			}
		}
		var err error
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
	return out
}
