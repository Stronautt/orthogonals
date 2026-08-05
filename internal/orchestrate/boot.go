package orchestrate

import (
	"errors"
	"fmt"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
)

func VerifyBoot(root string) error {
	if err := KargsLive(root); err != nil {
		return err
	}
	if err := iommuActive(root); err != nil {
		return err
	}
	return vfioModuleLoaded(root)
}

// KargsLive reports whether the kernel args apply journaled are on the running
// kernel — the difference between "reboot pending" and "the firmware is off".
func KargsLive(root string) error {
	want, err := hostcfg.JournaledKernelArgs(root)
	if err != nil {
		return err
	}
	missing, err := bls.MissingLive(root, want)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("kernel argument %q is not active on the running kernel — reboot, or re-run `orthogonals apply --yes`", missing[0])
	}
	return nil
}

func iommuActive(root string) error {
	active, err := hw.IOMMUActive(root)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("IOMMU is not active (no /sys/kernel/iommu_groups entries) — check that VT-d is enabled in firmware")
	}
	return nil
}

func vfioModuleLoaded(root string) error {
	if !hw.ModuleLoaded(root, "vfio_pci") {
		return errors.New("vfio_pci module is not loaded — the regenerated initramfs may not be in use yet (reboot, or re-run `orthogonals apply --yes`)")
	}
	return nil
}
