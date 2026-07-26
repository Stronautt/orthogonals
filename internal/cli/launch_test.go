package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// stubNotify captures desktop notifications instead of running notify-send.
func stubNotify(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := notify.Send
	notify.Send = func(n notify.Notification) { got = append(got, n.Body) }
	t.Cleanup(func() { notify.Send = old })
	return &got
}

// captureExec records execProcess argv instead of exec'ing.
func captureExec(t *testing.T) *[]string {
	t.Helper()
	var got []string
	old := execProcess
	execProcess = func(_ string, argv []string, _ []string) error {
		got = argv
		return nil
	}
	t.Cleanup(func() { execProcess = old })
	return &got
}

// fastPoll shrinks the launch poll bounds so timeout tests do not sleep.
func fastPoll(t *testing.T) {
	t.Helper()
	oldT, oldI := launchTimeout, launchPollInterval
	launchTimeout, launchPollInterval = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() { launchTimeout, launchPollInterval = oldT, oldI })
}

func launchRoot(t *testing.T, memAvailableKiB string) string {
	t.Helper()
	root := t.TempDir()
	if memAvailableKiB != "" {
		p := filepath.Join(root, "proc")
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "meminfo"),
			[]byte("MemTotal:       33554432 kB\nMemAvailable:   "+memAvailableKiB+" kB\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestVMLaunchRunningDomainExecs pins the client argv. `-p 0` is not a
// placeholder: it tells Looking Glass `-c` names a unix socket, so "fixing"
// the zero breaks the launch.
func TestVMLaunchRunningDomainExecs(t *testing.T) {
	const sock = "/run/orthogonals/win11/spice.sock"
	fakeVirt(t, &virttest.Fake{State: "running", DisplayHost: sock, DisplayPort: "0"})
	argv := captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "")
	code, out, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	// Without -f the client would prefer a /dev/kvmfr0 left loaded by an earlier
	// VM and wait forever for a host.
	want := []string{"looking-glass-client", "-F", "-c", sock, "-p", "0", "-f", steps.LookingGlassSHM}
	if strings.Join(*argv, " ") != strings.Join(want, " ") {
		t.Errorf("exec argv = %v, want %v", *argv, want)
	}
	if !strings.Contains(out, "over "+sock) {
		t.Errorf("missing connect line:\n%s", out)
	}
}

// TestVMLaunchTCPDisplayStillWorks: a domain defined before the socket switch
// keeps its address listen until the next converge.
func TestVMLaunchTCPDisplayStillWorks(t *testing.T) {
	fakeVirt(t, &virttest.Fake{State: "running", DisplayHost: "127.0.0.1", DisplayPort: "5901"})
	argv := captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "")
	if code, out, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	want := []string{"looking-glass-client", "-F", "-c", "127.0.0.1", "-p", "5901", "-f", steps.LookingGlassSHM}
	if strings.Join(*argv, " ") != strings.Join(want, " ") {
		t.Errorf("exec argv = %v, want %v", *argv, want)
	}
}

// TestVMLaunchWaitsForTheDisplay: libvirt reports no display until QEMU binds
// the socket, so the poll must outlast that gap.
func TestVMLaunchWaitsForTheDisplay(t *testing.T) {
	const sock = "/run/orthogonals/win11/spice.sock"
	fastPoll(t)
	fakeVirt(t, &virttest.Fake{
		State: "running", DisplayHost: sock, DisplayPort: "0", DisplayAfter: 3,
	})
	argv := captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "")
	if code, out, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	if got := strings.Join(*argv, " "); !strings.Contains(got, sock) {
		t.Errorf("exec argv = %q, want the socket once the display appeared", got)
	}
}

func TestVMLaunchStartsShutOffDomain(t *testing.T) {
	f := fakeVirt(t, &virttest.Fake{State: "shut off", MaxMemKiB: 8 << 20, DisplayHost: "127.0.0.1", DisplayPort: "5900"})
	captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "16000000")
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if !f.Logged("start win11") {
		t.Errorf("shut-off domain was not started: %v", f.Calls)
	}
}

func TestVMLaunchRefusesLowMemory(t *testing.T) {
	fakeVirt(t, &virttest.Fake{State: "shut off", MaxMemKiB: 16 << 20, DisplayPort: "5900"})
	captureExec(t)
	root := launchRoot(t, "2000000")
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "not enough free memory") {
		t.Errorf("missing memory refusal:\n%s", stderr)
	}
}

func TestVMLaunchDisplayTimeout(t *testing.T) {
	fastPoll(t)
	fakeVirt(t, &virttest.Fake{State: "running"})
	captureExec(t)
	root := launchRoot(t, "")
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "no SPICE display") {
		t.Errorf("missing display-timeout error:\n%s", stderr)
	}
}

func TestVMLaunchSuppressesHookNotify(t *testing.T) {
	fakeVirt(t, &virttest.Fake{State: "shut off", StartErr: errors.New("gpu-detach: GPU handover failed")})
	captureExec(t)
	notes := stubNotify(t)
	root := launchRoot(t, "")
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "gpu-detach:") {
		t.Errorf("hook failure not surfaced:\n%s", stderr)
	}
	if len(*notes) != 0 {
		t.Errorf("launch double-notified on a hook failure: %v", *notes)
	}
}

func TestVMLaunchNotifiesOnFailure(t *testing.T) {
	fakeVirt(t, &virttest.Fake{})
	captureExec(t)
	notes := stubNotify(t)
	root := launchRoot(t, "")
	code, _, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "no such VM") {
		t.Errorf("missing not-found error:\n%s", stderr)
	}
	if !strings.Contains(strings.Join(*notes, "\n"), "no such VM") {
		t.Errorf("no desktop notification on failure: %v", *notes)
	}
}

// TestVMLaunchKVMFRDomainOmitsBackendFlag: a kvmfr domain is left to the
// client's own detection, which is what picks the DMABUF path. No registry copy
// is written, so the decision can only come from libvirt — launch runs as the
// desktop user and cannot read the 0600 root one.
func TestVMLaunchKVMFRDomainOmitsBackendFlag(t *testing.T) {
	const sock = "/run/orthogonals/win11/spice.sock"
	xml := `<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>win11</name>
  <qemu:commandline>
    <qemu:arg value='-object'/>
    <qemu:arg value='{"qom-type":"memory-backend-file","id":"looking-glass","mem-path":"` +
		steps.KVMFRDevice + `","size":134217728,"share":true}'/>
  </qemu:commandline>
</domain>`
	fakeVirt(t, &virttest.Fake{State: "running", DisplayHost: sock, DisplayPort: "0", XML: xml})
	argv := captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "")

	if code, out, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	if slices.Contains(*argv, "-f") {
		t.Errorf("kvmfr domain launched with an explicit backend: %v", *argv)
	}
}
