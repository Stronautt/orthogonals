package orchestrate

import (
	"fmt"
	"io"
	"strings"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/sysd"
)

// Recover re-enumerates the passthrough GPU via PCI remove + rescan after a botched handover.
func Recover(root string, s sysd.Client, yes bool, out io.Writer) error {
	res, err := hw.Detect(root)
	if err != nil {
		return err
	}
	nvidia, err := res.GPUs.SoleNVIDIA()
	if err != nil {
		return err
	}
	devs := []string{nvidia.Address}
	if nvidia.Audio != nil {
		devs = append(devs, nvidia.Audio.Address)
	}
	if !yes {
		fmt.Fprintf(out, "would unload the NVIDIA modules, re-enumerate %s via PCI remove + rescan, and reload the driver\n",
			strings.Join(devs, " "))
		return nil
	}
	for _, m := range hooks.NVIDIAUnloadOrder {
		_ = hooks.DeleteModule(m)
	}
	for _, d := range devs {
		_ = hw.SetDriverOverride(root, d, "")
	}
	if err := hooks.Reenumerate(root, devs, s); err != nil {
		return err
	}
	if err := hooks.HealthCheck(root); err != nil {
		return fmt.Errorf("nvidia-smi still fails — reboot required: %w", err)
	}
	fmt.Fprintf(out, "recovered — %s is back on the host driver\n", devs[0])
	return nil
}
