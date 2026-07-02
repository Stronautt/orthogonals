package orchestrate

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/steps"
)

// fastPolling shrinks every poll knob so tests never sleep for real.
func fastPolling(t *testing.T) {
	t.Helper()
	saved := []any{installTimeout, installInterval, pingTries, pingInterval, shutdownTries, shutdownInterval, idleTries, idleInterval, cdPromptWindow, cdPromptInterval, provisionFailGrace}
	installTimeout, installInterval = 200*time.Millisecond, time.Millisecond
	provisionFailGrace = 10 * time.Millisecond
	pingTries, pingInterval = 3, time.Millisecond
	shutdownTries, shutdownInterval = 5, time.Millisecond
	idleTries, idleInterval = 2, time.Millisecond
	cdPromptWindow, cdPromptInterval = 20*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		installTimeout = saved[0].(time.Duration)
		installInterval = saved[1].(time.Duration)
		pingTries, pingInterval = saved[2].(int), saved[3].(time.Duration)
		shutdownTries, shutdownInterval = saved[4].(int), saved[5].(time.Duration)
		idleTries, idleInterval = saved[6].(int), saved[7].(time.Duration)
		cdPromptWindow, cdPromptInterval = saved[8].(time.Duration), saved[9].(time.Duration)
		provisionFailGrace = saved[10].(time.Duration)
	})
}

// fakeBin installs an executable stub on a fresh PATH dir that logs its argv
// and then runs extra shell. Returns the log path.
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

// writingDisk is a domblkinfo answer past setupWritingBytes: setup has booted,
// so Install must not send boot-from-CD keypresses.
const writingDisk = `domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     9663676416\nPhysical:       9663676416\n' ;;`

// fakeVirsh answers domstate with the contents of a state file (start writes
// "running", shutdown writes "shut off") and qemu-agent-command with a
// guest-exec/guest-exec-status pair returning agentStdout + agentExit.
func fakeVirsh(t *testing.T, initialState, agentStdout string, agentExit int) string {
	t.Helper()
	stateFile := filepath.Join(t.TempDir(), "domstate")
	if err := os.WriteFile(stateFile, []byte(initialState), 0o644); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(agentStdout))
	extra := `case "$*" in
domstate*) cat "` + stateFile + `" ;;
` + writingDisk + `
start*) printf 'running' > "` + stateFile + `" ;;
shutdown*) printf 'shut off' > "` + stateFile + `" ;;
*guest-ping*) echo '{"return":{}}' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":` + strconv.Itoa(agentExit) + `,"out-data":"` + b64 + `"}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`
	return fakeBin(t, "virsh", extra)
}

func TestInstallCompletes(t *testing.T) {
	fastPolling(t)
	log := fakeVirsh(t, "shut off", `{"stage":"done","ok":true,"error":""}`, 0)
	var out bytes.Buffer
	if err := Install("win11", &out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "start win11") {
		t.Errorf("Install must start the VM:\n%s", b)
	}
	if !strings.Contains(out.String(), "provisioning complete") {
		t.Errorf("missing completion line:\n%s", out.String())
	}
}

// A failed provisioning stage fails Install before the caller gets to remove
// the install-time display — the operator can still look at the guest console
// and a re-run can resume where it stopped.
func TestInstallFailsOnStageFailure(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", `{"stage":"nvidia-driver","ok":false,"error":"installer exit 5"}`, 0)
	if err := Install("win11", &bytes.Buffer{}); err == nil {
		t.Fatal("want stage failure")
	}
}

// A failed status left by a previous attempt survives in the guest until the
// logon task's re-run overwrites it — Install must outwait it instead of
// failing the resume on the first read.
func TestInstallOutwaitsStaleFailedStatus(t *testing.T) {
	fastPolling(t)
	countFile := filepath.Join(t.TempDir(), "count")
	stale := base64.StdEncoding.EncodeToString([]byte(`{"stage":"virtio-guest-tools","ok":false,"error":"stale"}`))
	done := base64.StdEncoding.EncodeToString([]byte(`{"stage":"done","ok":true,"error":""}`))
	extra := `case "$*" in
domstate*) printf 'running' ;;
` + writingDisk + `
*guest-exec-status*) echo x >> "` + countFile + `"
  if [ "$(wc -l < "` + countFile + `")" -ge 2 ]; then
    echo '{"return":{"exited":true,"exitcode":0,"out-data":"` + done + `"}}'
  else
    echo '{"return":{"exited":true,"exitcode":0,"out-data":"` + stale + `"}}'
  fi ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`
	fakeBin(t, "virsh", extra)
	var out bytes.Buffer
	if err := Install("win11", &out); err != nil {
		t.Fatalf("stale failed status must be superseded by the re-run: %v", err)
	}
	if !strings.Contains(out.String(), "waiting") {
		t.Errorf("missing grace notice:\n%s", out.String())
	}
}

// Windows setup can power the domain off instead of rebooting at its first
// reboot — Install restarts it and keeps polling.
func TestInstallRestartsPoweredOffVM(t *testing.T) {
	fastPolling(t)
	// starts running, agent has no status yet (exit 1 = type finds no file);
	// flip the domain to shut off after the first domstate poll
	stateFile := filepath.Join(t.TempDir(), "domstate")
	if err := os.WriteFile(stateFile, []byte("running"), 0o644); err != nil {
		t.Fatal(err)
	}
	countFile := stateFile + ".count"
	extra := `case "$*" in
domstate*) cat "` + stateFile + `"; echo x >> "` + countFile + `"
  [ "$(wc -l < "` + countFile + `")" -ge 2 ] || printf 'shut off' > "` + stateFile + `" ;;
` + writingDisk + `
start*) printf 'running' > "` + stateFile + `" ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`
	log := fakeBin(t, "virsh", extra)
	var out bytes.Buffer
	err := Install("win11", &out)
	// provisioning never reports done in this fake, so the timeout fires —
	// the point is the restart happened
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("want timeout, got %v", err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "start win11") {
		t.Errorf("powered-off VM was not restarted:\n%s", b)
	}
	if !strings.Contains(out.String(), "restarting") {
		t.Errorf("restart not reported:\n%s", out.String())
	}
}

func TestInstallHeartbeat(t *testing.T) {
	fastPolling(t)
	saved := heartbeatInterval
	heartbeatInterval = time.Millisecond
	t.Cleanup(func() { heartbeatInterval = saved })
	// running VM, agent never answers (Windows setup phase), domblkinfo grows
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo "running" ;;
domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     8724152320\nPhysical:       8724152320\n' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	err := Install("win11", &out)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("want timeout, got %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "Windows setup running (guest agent not up yet)") {
		t.Errorf("missing pre-agent heartbeat:\n%s", s)
	}
	if !strings.Contains(s, "8.1 GiB written") {
		t.Errorf("missing disk-growth proxy:\n%s", s)
	}
	if !strings.Contains(s, "elapsed") {
		t.Errorf("missing elapsed time:\n%s", s)
	}
}

// Microsoft's retail media waits ~5 s for a keypress at "Press any key to
// boot from CD or DVD" and otherwise falls through to the empty disk, where
// OVMF parks in the boot manager. The guest has no display, so nobody can
// press it — Install must.
func TestInstallAnswersCDPrompt(t *testing.T) {
	fastPolling(t)
	// disk stays empty: the prompt window must be worked for its full length
	log := fakeBin(t, "virsh", `case "$*" in
domstate*) echo "running" ;;
domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     335872\nPhysical:       335872\n' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	_ = Install("win11", &out)
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "send-key win11 KEY_ENTER") {
		t.Errorf("no keypress sent to answer the CD prompt:\n%s", b)
	}
}

// Resuming onto a domain left running by a failed attempt: it is parked in
// OVMF's boot manager, past the CD prompt, with an empty disk. Keypresses
// cannot rewind it — only a fresh boot can.
func TestInstallRebootsVMParkedPastCDPrompt(t *testing.T) {
	fastPolling(t)
	log := fakeBin(t, "virsh", `case "$*" in
domstate*) echo "running" ;;
domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     335872\nPhysical:       335872\n' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	_ = Install("win11", &out)
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "destroy win11") || !strings.Contains(string(b), "start win11") {
		t.Errorf("parked VM was not rebooted:\n%s", b)
	}
	if !strings.Contains(string(b), "send-key win11 KEY_ENTER") {
		t.Errorf("CD prompt not answered after the reboot:\n%s", b)
	}
}

// A running domain whose disk shows setup already writing must be left alone —
// destroying it would throw away an install in progress.
func TestInstallLeavesWritingVMRunning(t *testing.T) {
	fastPolling(t)
	log := fakeVirsh(t, "running", `{"stage":"nvidia-driver","ok":true,"error":""}`, 0)
	var out bytes.Buffer
	_ = Install("win11", &out)
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "destroy win11") {
		t.Errorf("destroyed a VM that was mid-install:\n%s", b)
	}
}

func TestInstallStopsKeysOnceSetupWrites(t *testing.T) {
	fastPolling(t)
	saved := cdPromptInterval
	cdPromptInterval = time.Millisecond
	t.Cleanup(func() { cdPromptInterval = saved })
	// disk already well past the threshold: setup is writing, so a stray
	// ENTER could answer a real dialog — send none
	log := fakeBin(t, "virsh", `case "$*" in
domstate*) echo "running" ;;
domblkinfo*) printf 'Capacity:       107374182400\nAllocation:     9663676416\nPhysical:       9663676416\n' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	_ = Install("win11", &out)
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "send-key") {
		t.Errorf("keypress sent although setup had already written to disk:\n%s", b)
	}
}

func TestDomStateForcesCLocale(t *testing.T) {
	// a localized virsh answers "вимкнути", the poller compares "shut off"
	fakeBin(t, "virsh", `case "$*" in
domstate*) if [ "$LC_ALL" = "C" ]; then echo "shut off"; else echo "вимкнути"; fi ;;
esac`)
	if got := steps.DomainState("win11"); got != "shut off" {
		t.Errorf("DomainState = %q, want \"shut off\" regardless of host locale", got)
	}
}

func TestInstallProvisionStageFailure(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", `{"stage":"nvidia-driver","ok":false,"error":"installer exit 5"}`, 0)
	err := Install("win11", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "nvidia-driver") || !strings.Contains(err.Error(), "installer exit 5") {
		t.Errorf("want stage failure naming stage and error, got %v", err)
	}
}

func TestInstallTimeoutGuidance(t *testing.T) {
	fastPolling(t)
	// agent never answers (exit 1 from virsh itself), domain stays running
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo running ;;
*qemu-agent-command*) exit 1 ;;
esac`)
	err := Install("win11", &bytes.Buffer{})
	if err == nil {
		t.Fatal("want timeout error")
	}
	for _, want := range []string{"did not finish", "resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout guidance missing %q: %v", want, err)
		}
	}
}

func TestInstallStartFails(t *testing.T) {
	fastPolling(t)
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo "shut off" ;;
start*) echo "error: domain not found" >&2; exit 1 ;;
esac`)
	err := Install("win11", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "virsh start") {
		t.Errorf("want start failure, got %v", err)
	}
}
