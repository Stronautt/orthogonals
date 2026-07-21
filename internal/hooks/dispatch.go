package hooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd"
)

// inhibitUnit is the transient sleep-inhibitor unit for a running VM.
func inhibitUnit(vm string) string { return "libvirt-nosleep-" + vm + ".service" }

// Dispatch is the qemu hook body: prepare/begin detaches the GPU, release/end reattaches it.
func Dispatch(root string, sd sysd.Client, vm, op, subop, user, exe string) error {
	if _, err := os.Stat(filepath.Join(steps.VMsDir(root), vm+".xml")); err != nil {
		return nil
	}
	log := hookLog(root, "qemu")
	switch op + "/" + subop {
	case "prepare/begin":
		if err := oneVMAtATime(root, vm); err != nil {
			return err
		}
		if err := Detach(root, user, sd); err != nil {
			return fmt.Errorf("GPU handover to vfio-pci failed — VM start aborted. Details: %s: %w",
				filepath.Join(root, LogPath), err)
		}
		unit := inhibitUnit(vm)
		_ = sd.ResetFailedUnit(unit)
		if err := sd.StartTransientUnit(unit, []string{exe, "hook", "inhibit", vm}); err != nil {
			log("sleep inhibitor not started: %v", err)
		}
	case "release/end":
		_ = sd.StopUnit(inhibitUnit(vm))
		if err := Reattach(root, user, sd); err != nil {
			return fmt.Errorf("GPU reattach to the host driver failed — run: sudo orthogonals recover --yes. Details: %s: %w",
				filepath.Join(root, LogPath), err)
		}
	}
	return nil
}

// oneVMAtATime refuses the start while another managed domain holds the dGPU.
func oneVMAtATime(root, vm string) error {
	for _, other := range steps.VMNames(root) {
		if other == vm {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, steps.LibvirtRunDir, other+".xml")); err == nil {
			return fmt.Errorf("VM %s is running and holds the dGPU — one VM at a time. Shut it down first: virsh shutdown %s", other, other)
		}
	}
	return nil
}
