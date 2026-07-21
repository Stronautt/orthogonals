package hooks

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
)

// TestMain silences the hook progress mirror.
func TestMain(m *testing.M) {
	LogWriter = io.Discard
	os.Exit(m.Run())
}

const (
	gpuAddr = "0000:01:00.0"
	audAddr = "0000:01:00.1"
)

// hookRoot is a reference host wired for the runtime hooks.
func hookRoot(t *testing.T) string {
	t.Helper()
	root := hwtest.ReferenceRoot(t)
	for _, dev := range []struct{ addr, drv string }{{gpuAddr, "nvidia"}, {audAddr, "snd_hda_intel"}} {
		link := filepath.Join(root, "sys/bus/pci/devices", dev.addr, "driver")
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../drivers/"+dev.drv, link); err != nil {
			t.Fatal(err)
		}
		hwtest.WriteFile(t, root, "sys/bus/pci/drivers/"+dev.drv+"/unbind", "")
	}
	for _, m := range NVIDIAUnloadOrder {
		hwtest.WriteFile(t, root, "sys/module/"+m+"/refcnt", "0\n")
	}
	return root
}

// driverFromOverride reflects what Detach wrote to driver_override.
func driverFromOverride(root, addr string) string {
	b, _ := os.ReadFile(filepath.Join(root, "sys/bus/pci/devices", addr, "driver_override"))
	if strings.Contains(string(b), "vfio-pci") {
		return "vfio-pci"
	}
	return "nvidia"
}

func stubDeviceDriver(t *testing.T, fn func(root, addr string) string) {
	t.Helper()
	old := deviceDriver
	deviceDriver = fn
	t.Cleanup(func() { deviceDriver = old })
}

func stubRuntimeStatus(t *testing.T, fn func(root, addr string) string) {
	t.Helper()
	old := runtimeStatus
	runtimeStatus = fn
	t.Cleanup(func() { runtimeStatus = old })
}

// stubRuntimeStatusFromControl reports a device suspended until its power/control
// is pinned "on", simulating the kernel's D3cold→D0 transition without a kernel.
func stubRuntimeStatusFromControl(t *testing.T) {
	t.Helper()
	stubRuntimeStatus(t, func(root, addr string) string {
		b, _ := os.ReadFile(filepath.Join(root, "sys/bus/pci/devices", addr, "power/control"))
		if strings.TrimSpace(string(b)) == "on" {
			return "active"
		}
		return "suspended"
	})
}

func stubDeleteModule(t *testing.T, err error) *[]string {
	t.Helper()
	var got []string
	old := DeleteModule
	DeleteModule = func(name string) error {
		got = append(got, name)
		return err
	}
	t.Cleanup(func() { DeleteModule = old })
	return &got
}

func stubNotify(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := notify.Send
	notify.Send = func(n notify.Notification) {
		urgency := "normal"
		if n.Urgent {
			urgency = "critical"
		}
		got = append(got, urgency+": "+n.Body)
	}
	t.Cleanup(func() { notify.Send = old })
	return &got
}

// fakeBin installs an argv-logging stub on PATH and returns its log path.
func fakeBin(t *testing.T, name, extra string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\n" + extra + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDetachSuccess(t *testing.T) {
	root := hookRoot(t)
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	stubNotify(t)
	modprobe := fakeBin(t, "modprobe", "")
	sd := &sysdtest.Fake{}

	if err := Detach(root, "tester", sd); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := strings.Join(*unloaded, " "); got != "nvidia_drm nvidia_modeset nvidia_uvm nvidia" {
		t.Errorf("unload order = %q", got)
	}
	if !strings.Contains(read(t, modprobe), "vfio-pci") {
		t.Error("modprobe vfio-pci not called")
	}
	for _, d := range []string{gpuAddr, audAddr} {
		ov := read(t, filepath.Join(root, "sys/bus/pci/devices", d, "driver_override"))
		if strings.TrimSpace(ov) != "vfio-pci" {
			t.Errorf("%s driver_override = %q", d, ov)
		}
	}
	if got := read(t, filepath.Join(root, "sys/bus/pci/drivers_probe")); strings.TrimSpace(got) == "" {
		t.Error("no device was probed onto vfio-pci")
	}
	if !sd.Logged("stop nvidia-persistenced.service") {
		t.Errorf("persistenced not stopped: %v", sd.Calls)
	}
	if !sd.Logged("stop nvidia-powerd.service") {
		t.Errorf("nvidia-powerd not stopped: %v", sd.Calls)
	}
	if !sd.Logged("try-restart switcheroo-control.service") {
		t.Errorf("switcheroo not restarted: %v", sd.Calls)
	}
}

func TestDetachPersistencedStoppedBeforeHolderGate(t *testing.T) {
	root := hookRoot(t)
	stubDeviceDriver(t, driverFromOverride)
	stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	seedHolder(t, root, 4242, "steam")
	sd := &sysdtest.Fake{}

	if err := Detach(root, "tester", sd); err == nil {
		t.Fatal("expected holder-gate refusal")
	}
	if len(sd.Calls) == 0 || sd.Calls[0] != "stop nvidia-persistenced.service" {
		t.Errorf("persistenced stop must come first: %v", sd.Calls)
	}
}

func TestDetachHolderRefusal(t *testing.T) {
	root := hookRoot(t)
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	notes := stubNotify(t)
	fakeBin(t, "modprobe", "")
	seedHolder(t, root, 4242, "chrome")

	err := Detach(root, "tester", &sysdtest.Fake{})
	if err == nil || !strings.Contains(err.Error(), "chrome") {
		t.Fatalf("err = %v, want a refusal naming chrome", err)
	}
	if len(*unloaded) != 0 {
		t.Errorf("modules unloaded despite a busy GPU: %v", *unloaded)
	}
	if len(*notes) == 0 || !strings.Contains((*notes)[0], "close these apps") {
		t.Errorf("no refusal notification: %v", *notes)
	}
}

func TestDetachBusyModuleAborts(t *testing.T) {
	root := hookRoot(t)
	stubDeviceDriver(t, driverFromOverride)
	stubDeleteModule(t, unix.EWOULDBLOCK)
	stubNotify(t)
	fakeBin(t, "modprobe", "")

	err := Detach(root, "tester", &sysdtest.Fake{})
	if err == nil || !strings.Contains(err.Error(), "nvidia_drm") {
		t.Fatalf("err = %v, want a busy-module abort", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "driver_override")); err == nil {
		if strings.Contains(read(t, filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "driver_override")), "vfio-pci") {
			t.Error("bound to vfio-pci despite the busy-module abort")
		}
	}
}

func TestDetachAlreadyVfio(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	stubNotify(t)
	hwtest.WriteFile(t, root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor", "powersave\n")

	if err := Detach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if len(*unloaded) != 0 {
		t.Errorf("unloaded modules on the already-vfio short-circuit: %v", *unloaded)
	}
	if got := read(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")); strings.TrimSpace(got) != "performance" {
		t.Errorf("governor = %q, want performance", got)
	}
}

func TestDetachVerifyFailureAborts(t *testing.T) {
	root := hookRoot(t)
	stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	// bind never takes: driver stays nvidia even after the override write
	stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })

	err := Detach(root, "tester", &sysdtest.Fake{})
	if err == nil || !strings.Contains(err.Error(), "not vfio-pci") {
		t.Fatalf("err = %v, want a verify-failure abort", err)
	}
}

func TestDetachWakesSuspendedDevice(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/power/control", "auto\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+audAddr+"/power/control", "auto\n")
	stubRuntimeStatusFromControl(t)
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")

	if err := Detach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	for _, d := range []string{gpuAddr, audAddr} {
		if got := strings.TrimSpace(read(t, filepath.Join(root, "sys/bus/pci/devices", d, "power/control"))); got != "on" {
			t.Errorf("%s power/control = %q, want on (woken before unbind)", d, got)
		}
	}
	if len(*unloaded) == 0 {
		t.Error("modules never unloaded — the wake blocked the handover")
	}
}

func TestDetachWakeTimeoutAborts(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/power/control", "auto\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+audAddr+"/power/control", "auto\n")
	stubRuntimeStatus(t, func(_, _ string) string { return "suspended" })
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	oldSettle, oldTimeout := WakeSettle, WakeTimeout
	WakeSettle, WakeTimeout = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { WakeSettle, WakeTimeout = oldSettle, oldTimeout })

	err := Detach(root, "tester", &sysdtest.Fake{})
	if err == nil || !strings.Contains(err.Error(), "resume from runtime suspend") {
		t.Fatalf("err = %v, want a wake-timeout abort", err)
	}
	if len(*unloaded) != 0 {
		t.Errorf("modules unloaded despite the wake failure: %v", *unloaded)
	}
}

func TestDetachDesktopSkipsWake(t *testing.T) {
	root := hookRoot(t) // reference desktop: no power/runtime_status nodes
	stubDeviceDriver(t, driverFromOverride)
	stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")

	if err := Detach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "power/control")); err == nil {
		t.Error("power/control written on a desktop with no runtime PM")
	}
}

func TestReattachLaptopRestoresRuntimePM(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/class/dmi/id/chassis_type", "10\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/power/control", "on\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+audAddr+"/power/control", "on\n")
	stubDeviceDriver(t, driverFromOverride)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	fakeBin(t, "nvidia-smi", "")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	for _, d := range []string{gpuAddr, audAddr} {
		if got := strings.TrimSpace(read(t, filepath.Join(root, "sys/bus/pci/devices", d, "power/control"))); got != "auto" {
			t.Errorf("%s power/control = %q, want auto (runtime PM restored)", d, got)
		}
	}
}

func TestReattachDesktopLeavesRuntimePM(t *testing.T) {
	root := hookRoot(t) // chassis 3 (desktop)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/power/control", "on\n")
	stubDeviceDriver(t, driverFromOverride)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	fakeBin(t, "nvidia-smi", "")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if got := strings.TrimSpace(read(t, filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "power/control"))); got != "on" {
		t.Errorf("desktop reattach touched power/control: got %q, want on untouched", got)
	}
}

func TestReattachGovernorRestoredBeforeGuard(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "run/orthogonals-governor", "schedutil\n")
	hwtest.WriteFile(t, root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor", "performance\n")
	stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })
	stubNotify(t)

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if got := read(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")); strings.TrimSpace(got) != "schedutil" {
		t.Errorf("governor = %q, want restored schedutil", got)
	}
	if _, err := os.Stat(filepath.Join(root, "run/orthogonals-governor")); err == nil {
		t.Error("governor save file survived restore")
	}
}

func TestReattachHealthy(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	stubDeviceDriver(t, driverFromOverride)
	stubNotify(t)
	modprobe := fakeBin(t, "modprobe", "")
	fakeBin(t, "nvidia-smi", "")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if got := read(t, modprobe); got != "nvidia\nnvidia_uvm\nnvidia_drm\n" {
		t.Errorf("reload order = %q", got)
	}
}

func TestReattachFallbackThenHealthy(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	stubDeviceDriver(t, driverFromOverride)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	RemoveSettle, RescanSettle = time.Millisecond, time.Millisecond
	counter := filepath.Join(t.TempDir(), "n")
	fakeBin(t, "nvidia-smi", "n=$(cat '"+counter+"' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '"+counter+"'; [ $n -ge 2 ] || exit 1")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if got := read(t, filepath.Join(root, "sys/bus/pci/rescan")); strings.TrimSpace(got) != "1" {
		t.Errorf("PCI rescan not triggered by the fallback: %q", got)
	}
}

func TestReattachFinalFailureNotifies(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+gpuAddr+"/driver_override", "vfio-pci\n")
	stubDeviceDriver(t, driverFromOverride)
	notes := stubNotify(t)
	fakeBin(t, "modprobe", "")
	RemoveSettle, RescanSettle = time.Millisecond, time.Millisecond
	fakeBin(t, "nvidia-smi", "exit 1")

	err := Reattach(root, "tester", &sysdtest.Fake{})
	if err == nil || !strings.Contains(err.Error(), "orthogonals recover") {
		t.Fatalf("err = %v, want a recover-hint failure", err)
	}
	if len(*notes) == 0 || !strings.Contains((*notes)[len(*notes)-1], "recover --yes") {
		t.Errorf("no recover-hint notification: %v", *notes)
	}
}

func TestNvidiaHolders(t *testing.T) {
	root := t.TempDir()
	seedHolder(t, root, 100, "steam")
	hwtest.WriteFile(t, root, "proc/200/comm", "bash\n")
	hwtest.Symlink(t, root, "proc/200/fd/1", "/dev/pts/0")

	holders := nvidiaHolders(root)
	if len(holders) != 1 || holders[0].Comm != "steam" {
		t.Fatalf("holders = %v, want just steam", holders)
	}
}

func TestGovernorRoundTrip(t *testing.T) {
	root := t.TempDir()
	for _, cpu := range []string{"cpu0", "cpu1"} {
		hwtest.WriteFile(t, root, "sys/devices/system/cpu/"+cpu+"/cpufreq/scaling_governor", "ondemand\n")
	}
	log := hookLog(root, "test")
	boostGovernor(root, log)
	if got := read(t, filepath.Join(root, "sys/devices/system/cpu/cpu1/cpufreq/scaling_governor")); strings.TrimSpace(got) != "performance" {
		t.Errorf("cpu1 governor = %q, want performance", got)
	}
	restoreGovernor(root, log)
	if got := read(t, filepath.Join(root, "sys/devices/system/cpu/cpu1/cpufreq/scaling_governor")); strings.TrimSpace(got) != "ondemand" {
		t.Errorf("cpu1 governor = %q, want restored ondemand", got)
	}
}

// seedHolder makes pid look like it holds /dev/nvidia0 open, with comm.
func seedHolder(t *testing.T, root string, pid int, comm string) {
	t.Helper()
	base := "proc/" + strconv.Itoa(pid)
	hwtest.Symlink(t, root, base+"/fd/3", "/dev/nvidia0")
	hwtest.WriteFile(t, root, base+"/comm", comm+"\n")
}
