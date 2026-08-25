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
	"github.com/stronautt/orthogonals/internal/testsupport"
)

// realPATH is read before TestMain empties PATH — package vars initialize
// first — for the tier tests that drive the machine's own tools.
var realPATH = os.Getenv("PATH")

func TestMain(m *testing.M) {
	LogWriter = io.Discard
	// An empty PATH, so a binary no test faked cannot resolve to the developer's
	// own nvidia-smi or modprobe. fakeBin prepends, so faking still works; the
	// shebang in its scripts is an absolute path and does not need PATH.
	dir, err := os.MkdirTemp("", "hooks-nopath")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("PATH", dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

const (
	gpuAddr = "0000:01:00.0"
	audAddr = "0000:01:00.1"
)

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
		// Real sysfs always exposes driver_override; seeding it lets a rollback
		// that clears the override land byte-identically on where it started.
		hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+dev.addr+"/driver_override", "\n")
	}
	for _, m := range NVIDIAUnloadOrder {
		hwtest.WriteFile(t, root, "sys/module/"+m+"/refcnt", "0\n")
	}
	return root
}

func driverFromOverride(root, addr string) string {
	b, _ := os.ReadFile(filepath.Join(root, "sys/bus/pci/devices", addr, "driver_override"))
	if strings.Contains(string(b), "vfio-pci") {
		return "vfio-pci"
	}
	return "nvidia"
}

func stubDeviceDriver(t *testing.T, fn func(root, addr string) string) {
	t.Helper()
	testsupport.Swap(t, &deviceDriver, fn)
}

func stubRuntimeStatus(t *testing.T, fn func(root, addr string) string) {
	t.Helper()
	testsupport.Swap(t, &runtimeStatus, fn)
}

// stubRuntimeStatusFromControl reports suspended until power/control is pinned
// "on", simulating the kernel's D3cold→D0 transition without a kernel.
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
	return stubDeleteModuleFunc(t, func(string) error { return err })
}

// stubDeleteModuleFunc is stubDeleteModule for a test that has to fail on one
// named module rather than on all of them.
func stubDeleteModuleFunc(t *testing.T, fn func(name string) error) *[]string {
	t.Helper()
	var got []string
	testsupport.Swap(t, &DeleteModule, func(name string) error {
		got = append(got, name)
		return fn(name)
	})
	return &got
}

func stubNotify(t *testing.T) *[]string {
	t.Helper()
	var got []string
	testsupport.Swap(t, &notify.Send, func(n notify.Notification) {
		urgency := "normal"
		if n.Urgent {
			urgency = "critical"
		}
		got = append(got, urgency+": "+n.Body)
	})
	return &got
}

func fakeBin(t *testing.T, name, extra string) string {
	t.Helper()
	return hwtest.FakeTool(t, hwtest.FakePath(t), name, extra)
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

func stripIOMMUGroups(t *testing.T, root string) {
	t.Helper()
	for _, addr := range []string{gpuAddr, audAddr} {
		if err := os.Remove(filepath.Join(root, "sys/bus/pci/devices", addr, "iommu_group")); err != nil {
			t.Fatal(err)
		}
	}
}

// A host that booted without an IOMMU cannot bind vfio-pci at all, so the
// refusal has to land before the first mutation — including the unit stops,
// which nothing restores.
func TestDetachRefusesWithoutIOMMUGroup(t *testing.T) {
	root := hookRoot(t)
	stripIOMMUGroups(t, root)
	stubDeviceDriver(t, driverFromOverride)
	unloaded := stubDeleteModule(t, nil)
	notes := stubNotify(t)
	fakeBin(t, "modprobe", "")
	sd := &sysdtest.Fake{}

	err := Detach(root, "tester", sd)
	if err == nil || !strings.Contains(err.Error(), "no IOMMU group") {
		t.Fatalf("err = %v, want a refusal naming the missing IOMMU group", err)
	}
	if !strings.Contains(err.Error(), gpuAddr) {
		t.Errorf("refusal does not name the device: %v", err)
	}
	if len(sd.Calls) != 0 {
		t.Errorf("units touched before the gate: %v", sd.Calls)
	}
	if len(*unloaded) != 0 {
		t.Errorf("modules unloaded without an IOMMU: %v", *unloaded)
	}
	if got := read(t, filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "driver_override")); strings.TrimSpace(got) != "" {
		t.Errorf("driver_override = %q, want untouched before the gate", got)
	}
	if len(*notes) == 0 || !strings.Contains((*notes)[0], "needs an IOMMU") {
		t.Errorf("no refusal notification: %v", *notes)
	}
}

func TestCheckIOMMUGroups(t *testing.T) {
	t.Run("grouped host passes", func(t *testing.T) {
		if err := CheckIOMMUGroups(hookRoot(t)); err != nil {
			t.Errorf("CheckIOMMUGroups = %v, want nil", err)
		}
	})
	t.Run("ungrouped host refuses", func(t *testing.T) {
		root := hookRoot(t)
		stripIOMMUGroups(t, root)
		if err := CheckIOMMUGroups(root); err == nil {
			t.Error("CheckIOMMUGroups = nil, want a refusal")
		}
	})
	// An unscannable root is not a pass — it is no answer, and Detach's own gate
	// is the one that refuses.
	t.Run("unscannable root defers", func(t *testing.T) {
		if err := CheckIOMMUGroups(t.TempDir()); err != nil {
			t.Errorf("CheckIOMMUGroups = %v, want nil", err)
		}
	})
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

// A "did not start" notification has to outlive the optimistic "VM is starting"
// one before it: normal urgency auto-hides, leaving only the promise of a screen
// that never came.
func TestAbortNotificationsAreUrgent(t *testing.T) {
	t.Run("holder gate", func(t *testing.T) {
		root := hookRoot(t)
		stubDeviceDriver(t, driverFromOverride)
		stubDeleteModule(t, nil)
		notes := stubNotify(t)
		fakeBin(t, "modprobe", "")
		seedHolder(t, root, 4242, "chrome")

		_ = Detach(root, "tester", &sysdtest.Fake{})
		assertUrgent(t, notes)
	})
	t.Run("handover", func(t *testing.T) {
		root := hookRoot(t)
		stubDeviceDriver(t, driverFromOverride)
		stubDeleteModule(t, unix.EWOULDBLOCK)
		notes := stubNotify(t)
		fakeBin(t, "modprobe", "")

		_ = Detach(root, "tester", &sysdtest.Fake{})
		assertUrgent(t, notes)
	})
}

// assertUrgent checks the last notification, the one left on screen.
func assertUrgent(t *testing.T, notes *[]string) {
	t.Helper()
	if len(*notes) == 0 {
		t.Fatal("the abort sent no notification at all")
	}
	if last := (*notes)[len(*notes)-1]; !strings.HasPrefix(last, "critical: ") {
		t.Errorf("the last notification auto-hides: %q", last)
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
	testsupport.Swap(t, &WakeSettle, time.Millisecond)
	testsupport.Swap(t, &WakeTimeout, 5*time.Millisecond)

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

// A detach killed partway leaves no bind to test — sometimes nothing but
// unloaded modules — so the marker is what tells the release hook there is work
// to do. Without it the GPU stays on no driver until the next reboot.
func TestReattachUndoesInterruptedHandover(t *testing.T) {
	root := hookRoot(t)
	hwtest.WriteFile(t, root, handoverSaveFile, "")
	for _, d := range []string{gpuAddr, audAddr} {
		hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+d+"/driver_override", "vfio-pci\n")
	}
	stubDeviceDriver(t, func(_, _ string) string { return "" })
	stubNotify(t)
	modprobe := fakeBin(t, "modprobe", "")
	fakeBin(t, "nvidia-smi", "")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	for _, d := range []string{gpuAddr, audAddr} {
		if got := read(t, filepath.Join(root, "sys/bus/pci/devices", d, "driver_override")); strings.TrimSpace(got) != "" {
			t.Errorf("%s driver_override = %q, want cleared", d, got)
		}
	}
	if got := read(t, modprobe); got != "nvidia\nnvidia_uvm\nnvidia_drm\n" {
		t.Errorf("reload order = %q, want the nvidia stack reloaded", got)
	}
	if handoverStarted(root) {
		t.Error("marker survived a healthy reattach")
	}
}

// A start that never mutated anything must not cost the desktop its display
// driver: no marker, no undo.
func TestReattachWithoutMarkerDoesNothing(t *testing.T) {
	root := hookRoot(t)
	stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })
	stubNotify(t)
	modprobe := fakeBin(t, "modprobe", "")

	if err := Reattach(root, "tester", &sysdtest.Fake{}); err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if _, err := os.Stat(modprobe); err == nil {
		t.Error("reattach reloaded the driver stack with no handover in progress")
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
	// Shell builtins only: TestMain empties PATH, so no coreutils to call.
	fakeBin(t, "nvidia-smi", "n=0; [ -f '"+counter+"' ] && read n < '"+counter+"'; n=$((n+1)); echo $n > '"+counter+"'; [ $n -ge 2 ] || exit 1")

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

// Pins the leading slash on the /proc join: with no --root a bare "proc" is
// relative, so the scan would read whatever ./proc happened to be, find nothing,
// and wave the handover through. Every other test here passes an absolute temp
// root, which makes the join absolute either way.
func TestNvidiaHoldersIgnoresTheWorkingDirectory(t *testing.T) {
	decoy := t.TempDir()
	seedHolder(t, decoy, 1, "decoy")
	t.Chdir(decoy)

	for _, h := range nvidiaHolders("") {
		if h.Comm == "decoy" {
			t.Fatal("nvidiaHolders read ./proc instead of /proc — a relative join makes the holder gate depend on the caller's working directory")
		}
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

func TestResetTransientState(t *testing.T) {
	root := t.TempDir()
	// governor boosted and cpuset isolated — a crashed start.
	for _, cpu := range []string{"cpu0", "cpu1"} {
		hwtest.WriteFile(t, root, "sys/devices/system/cpu/"+cpu+"/cpufreq/scaling_governor", "performance\n")
	}
	hwtest.WriteFile(t, root, "run/orthogonals-governor", "schedutil")
	hwtest.WriteFile(t, root, "run/orthogonals-cpuset", "0-19")
	sd := &sysdtest.Fake{}

	ResetTransientState(root, sd)

	if got := strings.TrimSpace(read(t, filepath.Join(root, "sys/devices/system/cpu/cpu0/cpufreq/scaling_governor"))); got != "schedutil" {
		t.Errorf("governor = %q, want restored schedutil", got)
	}
	if len(sd.AllowedCPUs) == 0 {
		t.Error("cpuset isolation was not lifted")
	}
	for _, marker := range []string{govSaveFile, cpusetSaveFile} {
		if _, err := os.Stat(filepath.Join(root, marker)); !os.IsNotExist(err) {
			t.Errorf("marker %s must be removed", marker)
		}
	}
}

func seedHolder(t *testing.T, root string, pid int, comm string) {
	t.Helper()
	base := "proc/" + strconv.Itoa(pid)
	hwtest.Symlink(t, root, base+"/fd/3", "/dev/nvidia0")
	hwtest.WriteFile(t, root, base+"/comm", comm+"\n")
}
