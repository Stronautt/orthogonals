package hooks

import (
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
)

// registerVM registers a managed VM.
func registerVM(t *testing.T, root, vm string) {
	t.Helper()
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/"+vm+".xml", "<domain/>")
}

func TestDispatchUnmanagedPassesThrough(t *testing.T) {
	root := t.TempDir()
	sd := &sysdtest.Fake{}
	if err := Dispatch(root, sd, "ghost", "prepare", "begin", "tester", "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("unmanaged domain must pass through: %v", err)
	}
	if len(sd.Calls) != 0 {
		t.Errorf("unmanaged dispatch touched systemd: %v", sd.Calls)
	}
}

func TestDispatchOneVMAtATime(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	registerVM(t, root, "gaming")
	hwtest.WriteFile(t, root, "run/libvirt/qemu/gaming.xml", "<domain/>")

	err := Dispatch(root, &sysdtest.Fake{}, "win11", "prepare", "begin", "tester", "/usr/bin/orthogonals")
	if err == nil || !strings.Contains(err.Error(), "gaming is running") {
		t.Fatalf("err = %v, want a one-VM-at-a-time refusal naming gaming", err)
	}
}

func TestDispatchPrepareStartsInhibitor(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	stubDeviceDriver(t, driverFromOverride)
	stubDeleteModule(t, nil)
	stubNotify(t)
	fakeBin(t, "modprobe", "")
	sd := &sysdtest.Fake{}

	if err := Dispatch(root, sd, "win11", "prepare", "begin", "tester", "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	reset := indexOf(sd.Calls, "reset-failed libvirt-nosleep-win11.service")
	start := indexOf(sd.Calls, "start-transient libvirt-nosleep-win11.service /usr/bin/orthogonals hook inhibit win11")
	if reset < 0 || start < 0 || reset > start {
		t.Errorf("inhibitor start sequence wrong: %v", sd.Calls)
	}
}

func TestDispatchPrepareFailureWraps(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	stubNotify(t)
	stubDeleteModule(t, nil)
	fakeBin(t, "modprobe", "")
	stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })

	err := Dispatch(root, &sysdtest.Fake{}, "win11", "prepare", "begin", "tester", "/usr/bin/orthogonals")
	if err == nil || !strings.Contains(err.Error(), "GPU handover to vfio-pci failed") ||
		!strings.Contains(err.Error(), LogPath) {
		t.Fatalf("err = %v, want a wrapped handover failure naming the log", err)
	}
}

func TestDispatchReleaseStopsThenReattaches(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	stubNotify(t)
	stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })
	sd := &sysdtest.Fake{}

	if err := Dispatch(root, sd, "win11", "release", "end", "tester", "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !sd.Logged("stop libvirt-nosleep-win11.service") {
		t.Errorf("release must stop the inhibitor: %v", sd.Calls)
	}
}

func TestDispatchUnknownOpNoop(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	sd := &sysdtest.Fake{}
	if err := Dispatch(root, sd, "win11", "migrate", "begin", "tester", "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("unknown op must be a no-op: %v", err)
	}
	if len(sd.Calls) != 0 {
		t.Errorf("unknown op touched systemd: %v", sd.Calls)
	}
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}
