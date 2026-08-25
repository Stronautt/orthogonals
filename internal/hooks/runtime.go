package hooks

import (
	"bytes"
	"cmp"
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
	"github.com/stronautt/orthogonals/internal/utils"
)

func vmNote(user, body string, urgent bool) notify.Notification {
	return notify.Notification{Title: "Windows VM", Icon: "computer", Urgent: urgent, User: user, Body: body}
}

// Runtime seams swapped by tests.
var (
	DeleteModule  = func(name string) error { return unix.DeleteModule(name, unix.O_NONBLOCK) }
	deviceDriver  = hw.DeviceDriver
	runtimeStatus = hw.RuntimeStatus
	RemoveSettle  = time.Second
	RescanSettle  = 2 * time.Second
	WakeSettle    = 50 * time.Millisecond
	WakeTimeout   = 5 * time.Second
)

const govSaveFile = "/run/orthogonals-governor"

// Detach evicts the passthrough GPU to vfio-pci.
func Detach(root, user string, sd sysd.Client) error {
	log := hookLog(root, "gpu-detach")
	gpu, devs, err := nvidiaDevices(root)
	if err != nil {
		return err
	}
	if deviceDriver(root, gpu.Address) == "vfio-pci" {
		log("GPU already on vfio-pci — nothing to do")
		boostGovernor(root, log)
		return nil
	}
	// Before the first mutation: without a group the handover would unload the
	// NVIDIA stack and then strand the card on no driver at all.
	if err := iommuGroupErr(gpu); err != nil {
		log("refusing handover — %v", err)
		notify.Send(vmNote(user, "GPU passthrough needs an IOMMU — the VM cannot start.\nRun: orthogonals status", true))
		return err
	}
	log("handover start: %s", strings.Join(devs, " "))

	_ = sd.StopUnit(hostcfg.UnitPersistenced)
	_ = sd.StopUnit(hostcfg.UnitPowerd)

	if holders := nvidiaHolders(root); len(holders) > 0 {
		apps := holderApps(holders)
		log("GPU busy — refusing handover, holders: %s", apps)
		notify.Send(vmNote(user, "GPU is busy — close these apps, then start the VM again:\n"+apps, true))
		return fmt.Errorf("GPU busy — close these apps first: %s", apps)
	}
	log("holder gate passed")

	// The marker goes after the holder gate: a refusal there mutates nothing, and
	// a marker left behind it would make the release hook unbind a healthy GPU.
	h := &handover{root: root, user: user, devs: devs, sd: sd, log: log}
	if err := h.begin(); err != nil {
		return h.abort("record handover: %v", err)
	}
	woken, err := wakeDevices(root, devs, log)
	h.woken = woken
	if err != nil {
		return h.abort("%v", err)
	}
	// Non-urgent: every failure past this point notifies urgently, so this is
	// never the last word left on screen.
	notify.Send(vmNote(user, "VM is starting — the GPU is being handed over, first screen in ~20 seconds.", false))

	for _, m := range NVIDIAUnloadOrder {
		if hw.ModuleLoaded(root, m) {
			if err := DeleteModule(m); err != nil {
				return h.abort("unload %s: %v", m, err)
			}
		}
	}
	log("nvidia modules unloaded")

	if out, err := exec.Command("modprobe", "vfio-pci").CombinedOutput(); err != nil {
		return h.abort("modprobe vfio-pci: %v\n%s", err, bytes.TrimSpace(out))
	}
	for _, d := range devs {
		if err := hw.SetDriverOverride(root, d, "vfio-pci"); err != nil {
			return h.abort("override %s: %v", d, err)
		}
		if err := hw.UnbindDevice(root, d); err != nil {
			return h.abort("unbind %s: %v", d, err)
		}
		if err := hw.ProbeDevice(root, d); err != nil {
			return h.abort("probe %s: %v", d, err)
		}
	}
	log("bound to vfio-pci")

	for _, d := range devs {
		if drv := deviceDriver(root, d); drv != "vfio-pci" {
			return h.abort("%s ended on %q, not vfio-pci", d, drv)
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
	// The marker, not the bind, is what says a handover happened: a detach killed
	// partway leaves no bind to test, sometimes nothing but unloaded modules, and
	// a bind test calls that "nothing to undo". The bind test stays for a marker
	// lost to anything but a reboot.
	if !handoverStarted(root) && deviceDriver(root, gpu.Address) != "vfio-pci" {
		log("no handover in progress (failed/refused start) — nothing to undo")
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
		restoreRuntimePM(root, devs, log)
		ClearHandover(root)
		log("GPU back on host, healthy")
		return nil
	}
	log("nvidia-smi failed — trying PCI remove + rescan")
	if err := Reenumerate(root, devs, sd); err != nil {
		log("re-enumerate: %v", err)
	}
	if err := HealthCheck(root); err == nil {
		restoreRuntimePM(root, devs, log)
		ClearHandover(root)
		log("GPU back on host after PCI rescan, healthy")
		return nil
	}
	log("nvidia-smi failed after reattach")
	notify.Send(vmNote(user, "GPU reattach failed — run: sudo orthogonals recover --yes (see "+filepath.Join(root, LogPath)+")", true))
	return errors.New("GPU reattach failed — run: sudo orthogonals recover --yes")
}

// wakeDevices resumes a runtime-suspended device to D0 before its driver is
// unbound: unbinding a D3cold device fails. woken names the devices it pinned,
// including on the error return — a partial wake still has to be handed back.
func wakeDevices(root string, devs []string, log logFunc) (woken []string, err error) {
	for _, d := range devs {
		if status := runtimeStatus(root, d); status != "suspended" && status != "suspending" {
			continue
		}
		log("waking %s from runtime suspend", d)
		if err := hw.SetPowerControl(root, d, "on"); err != nil {
			return woken, fmt.Errorf("wake %s: %w", d, err)
		}
		woken = append(woken, d)
		if err := waitActive(root, d); err != nil {
			return woken, err
		}
		log("%s active", d)
	}
	return woken, nil
}

func waitActive(root, d string) error {
	deadline := time.Now().Add(WakeTimeout)
	for {
		if runtimeStatus(root, d) == "active" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not resume from runtime suspend within %s", d, WakeTimeout)
		}
		time.Sleep(WakeSettle)
	}
}

func restoreRuntimePM(root string, devs []string, log logFunc) {
	if !hw.IsLaptopChassis(hw.ChassisType(root)) {
		return
	}
	for _, d := range devs {
		_ = hw.SetPowerControl(root, d, "auto")
	}
	log("runtime power management restored to auto")
}

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

func HealthCheck(root string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.used", "--format=csv,noheader").CombinedOutput()
	hookLog(root, "gpu-reattach")("nvidia-smi: %s", bytes.TrimSpace(out))
	return err
}

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

// iommuGroupErr reports why the passthrough devices cannot reach vfio-pci: it
// binds through an IOMMU group and its probe fails with EINVAL without one.
func iommuGroupErr(gpu hw.DGPU) error {
	ungrouped := gpu.UngroupedDevices()
	if len(ungrouped) == 0 {
		return nil
	}
	return fmt.Errorf("no IOMMU group on %s — the kernel cannot bind it to vfio-pci. "+
		"A package update can regenerate the boot config and drop the kernel arguments: "+
		"run `orthogonals status`, and if the arguments are gone re-run `sudo orthogonals apply --yes` "+
		"and reboot. Otherwise enable VT-d/AMD-Vi in firmware", strings.Join(ungrouped, " "))
}

// CheckIOMMUGroups is iommuGroupErr for callers that have not scanned yet. A
// host it cannot scan reports nil: Detach's gate is the authority, this only
// lets the CLI refuse before libvirt does.
func CheckIOMMUGroups(root string) error {
	gpu, _, err := nvidiaDevices(root)
	if err != nil {
		return nil
	}
	return iommuGroupErr(gpu)
}

func nvidiaDevices(root string) (gpu hw.DGPU, devs []string, err error) {
	gpus, err := hw.ScanGPUs(root)
	if err != nil {
		return hw.DGPU{}, nil, err
	}
	nvidia, err := gpus.SoleNVIDIA()
	if err != nil {
		return hw.DGPU{}, nil, err
	}
	return nvidia, nvidia.Addresses(), nil
}

type holder struct {
	Comm string
}

// nvidiaHolders lists processes holding /dev/nvidia* open. The leading slash on
// "/proc" is load-bearing: filepath.Join drops an empty root, so a bare "proc"
// is relative and the scan reads whatever ./proc happens to be.
func nvidiaHolders(root string) []holder {
	entries, err := os.ReadDir(filepath.Join(root, "/proc"))
	if err != nil {
		return nil
	}
	var holders []holder
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		procPid := filepath.Join(root, "/proc", e.Name())
		if pidHoldsNVIDIA(procPid) {
			// "?" rather than "": a blank reads as a missing entry in the
			// notification listing what holds the GPU.
			holders = append(holders, holder{
				Comm: cmp.Or(utils.ReadTrim(filepath.Join(procPid, "comm")), "?"),
			})
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

// ResetTransientState reverts the governor and the cgroup isolation that a
// crashed VM start leaves behind. Each is a no-op without its /run marker, so
// recover can call it at any time.
func ResetTransientState(root string, sd sysd.Client) {
	log := hookLog(root, "recover")
	restoreGovernor(root, log)
	unisolateCPUs(root, sd)
}

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

// LogWriter is the seam TestMain points at io.Discard; hook progress goes here.
var LogWriter io.Writer = os.Stderr

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
