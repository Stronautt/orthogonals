package orchestrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

// VerifyBoot checks that the applied boot configuration is live on the running kernel.
func VerifyBoot(root string) error {
	want, err := manifestKernelArgs(root)
	if err != nil {
		return err
	}
	if err := kargsLive(root, want); err != nil {
		return err
	}
	if err := iommuActive(root); err != nil {
		return err
	}
	return vfioModuleLoaded(root)
}

// manifestKernelArgs recovers the kargs apply added from the journaled step.
func manifestKernelArgs(root string) (string, error) {
	m, err := steps.Load(root)
	if err != nil {
		return "", err
	}
	if args := m.OpArgs(hostcfg.KernelArgsStepID); args["args"] != "" {
		return args["args"], nil
	}
	return "", errors.New("no journaled kernel-args step — run `orthogonals apply --yes` first")
}

func kargsLive(root, want string) error {
	b, err := os.ReadFile(filepath.Join(root, "/proc/cmdline"))
	if err != nil {
		return fmt.Errorf("read kernel cmdline: %w", err)
	}
	live := strings.Fields(string(b))
	for _, arg := range strings.Fields(want) {
		if !slices.Contains(live, arg) {
			return fmt.Errorf("kernel argument %q is not active on the running kernel — reboot, or re-run `orthogonals apply --yes`", arg)
		}
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
