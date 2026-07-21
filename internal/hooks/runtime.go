package hooks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/sysd"
)

// vmNote builds a Windows-VM desktop notification bound for user's session.
func vmNote(user, body string, urgent bool) notify.Notification {
	return notify.Notification{Title: "Windows VM", Icon: "video-display", Urgent: urgent, User: user, Body: body}
}

// Runtime seams swapped by tests.
var (
	// DeleteModule unloads a kernel module non-blocking.
	DeleteModule = func(name string) error { return unix.DeleteModule(name, unix.O_NONBLOCK) }
	// deviceDriver reads a PCI device's bound driver.
	deviceDriver = hw.DeviceDriver
	// RemoveSettle and RescanSettle are the PCI reset settle windows.
	RemoveSettle = time.Second
	RescanSettle = 2 * time.Second
)

// govSaveFile holds the pre-boost CPU governor.
const govSaveFile = "/run/orthogonals-governor"

// Detach evicts the passthrough GPU to vfio-pci.
func Detach(root, user string, sd sysd.Client) error {
	log := hookLog(root, "gpu-detach")
	gpu, devs, err := nvidiaDevices(root)
	if err != nil {
		return err
	}
	if deviceDriver(root, gpu) == "vfio-pci" {
		log("GPU already on vfio-pci — nothing to do")
		boostGovernor(root, log)
		return nil
	}
	log("handover start: %s", strings.Join(devs, " "))

	_ = sd.StopUnit(hostcfg.UnitPersistenced)

	if holders := nvidiaHolders(root); len(holders) > 0 {
		apps := holderApps(holders)
		log("GPU busy — refusing handover, holders: %s", apps)
		notify.Send(vmNote(user, "GPU is busy — close these apps, then start the VM again:\n"+apps, false))
		return fmt.Errorf("GPU busy — close these apps first: %s", apps)
	}
	log("holder gate passed")
	notify.Send(vmNote(user, "VM is starting — the GPU is being handed over, first screen in ~20 seconds.", false))

	for _, m := range NVIDIAUnloadOrder {
		if hw.ModuleLoaded(root, m) {
			if err := DeleteModule(m); err != nil {
				return abort(root, user, log, "unload %s: %v", m, err)
			}
		}
	}
	log("nvidia modules unloaded")

	if out, err := exec.Command("modprobe", "vfio-pci").CombinedOutput(); err != nil {
		return abort(root, user, log, "modprobe vfio-pci: %v\n%s", err, bytes.TrimSpace(out))
	}
	for _, d := range devs {
		if err := hw.SetDriverOverride(root, d, "vfio-pci"); err != nil {
			return abort(root, user, log, "override %s: %v", d, err)
		}
		if err := hw.UnbindDevice(root, d); err != nil {
			return abort(root, user, log, "unbind %s: %v", d, err)
		}
		if err := hw.ProbeDevice(root, d); err != nil {
			return abort(root, user, log, "probe %s: %v", d, err)
		}
	}
	log("bound to vfio-pci")

	for _, d := range devs {
		if drv := deviceDriver(root, d); drv != "vfio-pci" {
			return abort(root, user, log, "%s ended on %q, not vfio-pci", d, drv)
		}
	}
	_ = sd.TryRestartUnit(hostcfg.UnitSwitcheroo)
	boostGovernor(root, log)
	log("GPU handed to vfio-pci")
	return nil
}

// Reattach returns the passthrough GPU to the NVIDIA driver.
func Reattach(root, user string, sd sysd.Client) error {
	log := hookLog(root, "gpu-reattach")
	gpu, devs, err := nvidiaDevices(root)
	if err != nil {
		return err
	}
	restoreGovernor(root, log)
	if deviceDriver(root, gpu) != "vfio-pci" {
		log("GPU not on vfio-pci (failed/refused start) — nothing to undo")
		return nil
	}
	log("reattach start: %s", strings.Join(devs, " "))

	for _, d := range devs {
		_ = hw.SetDriverOverride(root, d, "")
		_ = hw.UnbindDevice(root, d)
	}
	if err := reloadNVIDIA(root, devs, sd); err != nil {
		log("reload: %v", err)
	}
	if err := HealthCheck(root); err == nil {
		log("GPU back on host, healthy")
		return nil
	}
	log("nvidia-smi failed — trying PCI remove + rescan")
	if err := Reenumerate(root, devs, sd); err != nil {
		log("re-enumerate: %v", err)
	}
	if err := HealthCheck(root); err == nil {
		log("GPU back on host after PCI rescan, healthy")
		return nil
	}
	log("nvidia-smi failed after reattach")
	notify.Send(vmNote(user, "GPU reattach failed — run: sudo orthogonals recover --yes (see "+filepath.Join(root, LogPath)+")", true))
	return errors.New("GPU reattach failed — run: sudo orthogonals recover --yes")
}

// Reenumerate resets the passthrough GPU via PCI remove + rescan and reloads the driver.
func Reenumerate(root string, devs []string, sd sysd.Client) error {
	for _, d := range slices.Backward(devs) {
		_ = hw.RemoveDevice(root, d)
	}
	time.Sleep(RemoveSettle)
	if err := hw.RescanPCI(root); err != nil {
		return err
	}
	time.Sleep(RescanSettle)
	return reloadNVIDIA(root, devs, sd)
}

// HealthCheck runs nvidia-smi under a timeout.
func HealthCheck(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.used", "--format=csv,noheader").CombinedOutput()
	hookLog(root, "gpu-reattach")("nvidia-smi: %s", bytes.TrimSpace(out))
	return err
}

// reloadNVIDIA loads the NVIDIA stack in dependency order and probes each device.
func reloadNVIDIA(root string, devs []string, sd sysd.Client) error {
	for _, m := range NVIDIAReloadOrder {
		if out, err := exec.Command("modprobe", m).CombinedOutput(); err != nil {
			return fmt.Errorf("modprobe %s: %w\n%s", m, err, bytes.TrimSpace(out))
		}
	}
	for _, d := range devs {
		_ = hw.ProbeDevice(root, d)
	}
	_ = sd.TryRestartUnit(hostcfg.UnitSwitcheroo)
	return nil
}

// abort logs and notifies once, then returns the error.
func abort(root, user string, log logFunc, format string, a ...any) error {
	err := fmt.Errorf(format, a...)
	log("failed — %v", err)
	notify.Send(vmNote(user, "GPU handover failed — VM not started. See: "+filepath.Join(root, LogPath), false))
	return err
}

// nvidiaDevices re-detects the sole passthrough GPU at runtime.
func nvidiaDevices(root string) (gpu string, devs []string, err error) {
	gpus, err := hw.ScanGPUs(root)
	if err != nil {
		return "", nil, err
	}
	nvidia, err := gpus.SoleNVIDIA()
	if err != nil {
		return "", nil, err
	}
	devs = []string{nvidia.Address}
	if nvidia.Audio != nil {
		devs = append(devs, nvidia.Audio.Address)
	}
	return nvidia.Address, devs, nil
}

type holder struct {
	Comm string
}

// nvidiaHolders lists processes holding /dev/nvidia* open.
func nvidiaHolders(root string) []holder {
	entries, err := os.ReadDir(filepath.Join(root, "proc"))
	if err != nil {
		return nil
	}
	var holders []holder
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		procPid := filepath.Join(root, "proc", e.Name())
		if pidHoldsNVIDIA(procPid) {
			holders = append(holders, holder{Comm: readComm(procPid)})
		}
	}
	return holders
}

func pidHoldsNVIDIA(procPid string) bool {
	fds, _ := os.ReadDir(filepath.Join(procPid, "fd"))
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join(procPid, "fd", fd.Name())); err == nil &&
			strings.HasPrefix(target, "/dev/nvidia") {
			return true
		}
	}
	if b, err := os.ReadFile(filepath.Join(procPid, "maps")); err == nil &&
		strings.Contains(string(b), "/dev/nvidia") {
		return true
	}
	return false
}

func readComm(procPid string) string {
	b, err := os.ReadFile(filepath.Join(procPid, "comm"))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// holderApps is the deduped, space-joined command names for the notification.
func holderApps(holders []holder) string {
	var apps []string
	seen := map[string]bool{}
	for _, h := range holders {
		if !seen[h.Comm] {
			seen[h.Comm] = true
			apps = append(apps, h.Comm)
		}
	}
	return strings.Join(apps, " ")
}

func governors(root string) []string {
	g, _ := filepath.Glob(filepath.Join(root, "/sys/devices/system/cpu/cpu*/cpufreq/scaling_governor"))
	return g
}

// boostGovernor sets every CPU to the performance governor, saving the current one first.
func boostGovernor(root string, log logFunc) {
	govs := governors(root)
	if len(govs) == 0 {
		return
	}
	save := filepath.Join(root, govSaveFile)
	if _, err := os.Stat(save); err != nil {
		if cur, err := os.ReadFile(govs[0]); err == nil {
			_ = os.MkdirAll(filepath.Dir(save), 0o755)
			_ = os.WriteFile(save, cur, 0o644)
		}
	}
	for _, g := range govs {
		_ = os.WriteFile(g, []byte("performance\n"), 0o644)
	}
	log("cpu governor performance")
}

// restoreGovernor writes the saved governor back and clears the save file.
func restoreGovernor(root string, log logFunc) {
	save := filepath.Join(root, govSaveFile)
	b, err := os.ReadFile(save)
	if err != nil {
		return
	}
	for _, g := range governors(root) {
		_ = os.WriteFile(g, b, 0o644)
	}
	_ = os.Remove(save)
	log("cpu governor restored: %s", strings.TrimSpace(string(b)))
}

type logFunc func(format string, a ...any)

// LogWriter is where hook progress is mirrored.
var LogWriter io.Writer = os.Stderr

// hookLog appends timestamped lines to the hooks log and mirrors them to LogWriter.
func hookLog(root, tag string) logFunc {
	path := filepath.Join(root, LogPath)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintf(f, "%s %s: %s\n", time.Now().Format(time.RFC3339), tag, msg)
			_ = f.Close()
		}
		fmt.Fprintf(LogWriter, "%s: %s\n", tag, msg)
	}
}
