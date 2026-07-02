package orchestrate

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
)

// Vars so tests run fast. The PCI core needs a beat between remove and
// rescan, and the re-enumerated card another before the driver loads.
var (
	removeSettle = time.Second
	rescanSettle = 2 * time.Second
)

// Recover re-enumerates the passthrough GPU via PCI remove + rescan after a
// botched handover — the escape hatch when nvidia-smi fails after VM
// shutdown. Runtime repair only: it changes no configuration, so nothing is
// journaled.
func Recover(root string, yes bool, out io.Writer) error {
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
		// a module that is busy or already gone is fine — the PCI remove
		// below is the actual reset
		_ = exec.Command("modprobe", "-r", m).Run()
	}
	// device nodes can be half-gone after the botched handover, so the
	// per-device writes are best-effort
	for _, d := range devs {
		_ = hw.SetDriverOverride(root, d, "")
	}
	// the GPU (function 0) goes last, after its sibling functions
	for _, d := range slices.Backward(devs) {
		_ = hw.RemoveDevice(root, d)
	}
	time.Sleep(removeSettle)
	if err := hw.RescanPCI(root); err != nil {
		return err
	}
	time.Sleep(rescanSettle)
	for _, m := range hooks.NVIDIAReloadOrder {
		if b, err := exec.Command("modprobe", m).CombinedOutput(); err != nil {
			return fmt.Errorf("modprobe %s: %w\n%s", m, err, bytes.TrimSpace(b))
		}
	}
	// switcheroo-control enumerates GPUs only at startup; restart it so the
	// desktop's dGPU launch menu reflects the recovered card
	_ = exec.Command("systemctl", "try-restart", hostcfg.UnitSwitcheroo).Run()
	if b, err := exec.Command("nvidia-smi").CombinedOutput(); err != nil {
		return fmt.Errorf("nvidia-smi still fails — reboot required: %w\n%s", err, bytes.TrimSpace(b))
	}
	fmt.Fprintf(out, "recovered — %s is back on the host driver\n", devs[0])
	return nil
}
