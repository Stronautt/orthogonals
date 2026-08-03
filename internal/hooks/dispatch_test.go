package hooks

import (
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
)

// currentUser is the one account these tests can chown to without privileges.
func currentUser(t *testing.T) string {
	t.Helper()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return u.Username
}

// registerVM writes the minimal XML the hook reads back: the <memory> element
// the hugepage reservation sizes from.
func registerVM(t *testing.T, root, vm string) {
	t.Helper()
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/"+vm+".xml",
		"<domain><memory unit='MiB'>24576</memory></domain>")
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

func shortSpiceWait(t *testing.T) {
	t.Helper()
	oldSettle, oldTimeout := SpiceSettle, SpiceTimeout
	SpiceSettle, SpiceTimeout = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { SpiceSettle, SpiceTimeout = oldSettle, oldTimeout })
}

func spiceSocket(t *testing.T, root, vm string) string {
	t.Helper()
	path := filepath.Join(root, steps.SpiceSocketPath(vm))
	if err := os.MkdirAll(filepath.Dir(path), 0o730); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// QEMU leaves it group- and world-readable; the whole point is to narrow it.
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDispatchStartedNarrowsTheSpiceSocket(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	shortSpiceWait(t)
	path := spiceSocket(t, root, "win11")

	owner := currentUser(t)
	if err := Dispatch(root, &sysdtest.Fake{}, "win11", "started", "begin", owner, "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode = %04o, want 0600", fi.Mode().Perm())
	}
}

func TestDispatchStartedSurvivesAMissingSocket(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	shortSpiceWait(t)

	if err := Dispatch(root, &sysdtest.Fake{}, "win11", "started", "begin", currentUser(t), "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("a missing socket must not fail the start: %v", err)
	}
	log, err := os.ReadFile(filepath.Join(root, LogPath))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "did not appear") {
		t.Errorf("missing socket was not reported:\n%s", log)
	}
}

func TestDispatchStartedLeavesANonSocketAlone(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	shortSpiceWait(t)
	path := filepath.Join(root, steps.SpiceSocketPath("win11"))
	if err := os.MkdirAll(filepath.Dir(path), 0o730); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a socket"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Dispatch(root, &sysdtest.Fake{}, "win11", "started", "begin", currentUser(t), "/usr/bin/orthogonals"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("a plain file was chmodded to %04o", fi.Mode().Perm())
	}
}

func TestDispatchPrepareStartsInhibitor(t *testing.T) {
	root := hookRoot(t)
	registerVM(t, root, "win11")
	seedHugepages(t, root, "0")
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

// This arm fires after Detach has already pulled the GPU off the host driver, so
// it has to name the cause rather than let QEMU surface an opaque out-of-memory.
func TestDispatchPrepareFailsOnUnreadableGuestRAM(t *testing.T) {
	root := hookRoot(t)
	// Registered, so the hook answers for it — but with no <memory> to size the
	// hugepage pool from.
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/win11.xml", "<domain/>")
	stubNotify(t)
	stubDeleteModule(t, nil)
	stubDeviceDriver(t, driverFromOverride)
	fakeBin(t, "modprobe", "")

	err := Dispatch(root, &sysdtest.Fake{}, "win11", "prepare", "begin", "tester", "/usr/bin/orthogonals")
	if err == nil || !strings.Contains(err.Error(), "hugepage reservation") {
		t.Fatalf("err = %v, want a guest-RAM read failure naming the hugepage reservation", err)
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
