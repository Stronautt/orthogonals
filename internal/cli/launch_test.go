package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/notify"
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

func TestVMLaunchRunningDomainExecs(t *testing.T) {
	fakeVirt(t, &virttest.Fake{State: "running", DisplayHost: "127.0.0.1", DisplayPort: "5901"})
	argv := captureExec(t)
	fakeBinDir(t, []string{"looking-glass-client"})
	root := launchRoot(t, "")
	code, out, stderr := run(t, "vm", "--root", root, "--vm-name", "win11", "launch")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	want := []string{"looking-glass-client", "-F", "-c", "127.0.0.1", "-p", "5901"}
	if strings.Join(*argv, " ") != strings.Join(want, " ") {
		t.Errorf("exec argv = %v, want %v", *argv, want)
	}
	if !strings.Contains(out, "127.0.0.1:5901") {
		t.Errorf("missing connect line:\n%s", out)
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
