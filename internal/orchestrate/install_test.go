package orchestrate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/virt/virttest"
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

// fakeBin installs an executable stub that logs its argv and runs extra shell.
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

// writingDisk is a physical allocation past setupWritingBytes.
const writingDisk = 9663676416

// fakeVM scripts a domain: initial state, a written disk, and a guest-exec responder.
func fakeVM(initialState, agentStdout string, agentExit int) *virttest.Fake {
	return &virttest.Fake{State: initialState, Phys: writingDisk, Agent: virttest.Responder(agentStdout, "", agentExit)}
}

func TestInstallCompletes(t *testing.T) {
	fastPolling(t)
	f := fakeVM("shut off", `{"stage":"done","ok":true,"error":""}`, 0)
	var out bytes.Buffer
	if err := Install(f, "win11", &out); err != nil {
		t.Fatal(err)
	}
	if !f.Logged("start win11") {
		t.Errorf("Install must start the VM:\n%v", f.Calls)
	}
	if !strings.Contains(out.String(), "provisioning complete") {
		t.Errorf("missing completion line:\n%s", out.String())
	}
}

// A stale failed status must be superseded by the re-run, not fail the resume.
func TestInstallOutwaitsStaleFailedStatus(t *testing.T) {
	fastPolling(t)
	stale := virttest.Responder(`{"stage":"virtio-guest-tools","ok":false,"error":"stale"}`, "", 0)
	done := virttest.Responder(`{"stage":"done","ok":true,"error":""}`, "", 0)
	statusReads := 0
	f := &virttest.Fake{State: "running", Phys: writingDisk, Agent: func(cmd string) (string, error) {
		if strings.Contains(cmd, "guest-exec-status") {
			statusReads++
			if statusReads >= 2 {
				return done(cmd)
			}
			return stale(cmd)
		}
		return done(cmd)
	}}
	var out bytes.Buffer
	if err := Install(f, "win11", &out); err != nil {
		t.Fatalf("stale failed status must be superseded by the re-run: %v", err)
	}
	if !strings.Contains(out.String(), "waiting") {
		t.Errorf("missing grace notice:\n%s", out.String())
	}
}

// Windows setup can power the domain off; Install restarts it and keeps polling.
func TestInstallRestartsPoweredOffVM(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "", 1)
	polls := 0
	f.OnState = func() (string, error) {
		polls++
		state := f.State
		if polls == 1 {
			f.State = "shut off"
		}
		return state, nil
	}
	var out bytes.Buffer
	err := Install(f, "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Fatalf("want timeout, got %v", err)
	}
	if !f.Logged("start win11") {
		t.Errorf("powered-off VM was not restarted:\n%v", f.Calls)
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
	f := &virttest.Fake{State: "running", Phys: 8724152320}
	var out bytes.Buffer
	err := Install(f, "win11", &out)
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

// A domain parked past the CD prompt with an empty disk must be rebooted, not keyed.
func TestInstallRebootsVMParkedPastCDPrompt(t *testing.T) {
	fastPolling(t)
	f := &virttest.Fake{State: "running", Phys: 335872, Agent: virttest.Responder("", "", 1)}
	var out bytes.Buffer
	_ = Install(f, "win11", &out)
	if !f.Logged("destroy win11") || !f.Logged("start win11") {
		t.Errorf("parked VM was not rebooted:\n%v", f.Calls)
	}
	if !f.Logged("send-key win11") {
		t.Errorf("CD prompt not answered after the reboot:\n%v", f.Calls)
	}
}

// A running domain with setup already writing must be left alone.
func TestInstallLeavesWritingVMRunning(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", `{"stage":"nvidia-driver","ok":true,"error":""}`, 0)
	var out bytes.Buffer
	_ = Install(f, "win11", &out)
	if f.Logged("destroy win11") {
		t.Errorf("destroyed a VM that was mid-install:\n%v", f.Calls)
	}
}

func TestInstallStopsKeysOnceSetupWrites(t *testing.T) {
	fastPolling(t)
	saved := cdPromptInterval
	cdPromptInterval = time.Millisecond
	t.Cleanup(func() { cdPromptInterval = saved })
	f := &virttest.Fake{State: "running", Phys: writingDisk, Agent: virttest.Responder("", "", 1)}
	var out bytes.Buffer
	_ = Install(f, "win11", &out)
	if f.Logged("send-key win11") {
		t.Errorf("keypress sent although setup had already written to disk:\n%v", f.Calls)
	}
}

func TestInstallProvisionStageFailure(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", `{"stage":"nvidia-driver","ok":false,"error":"installer exit 5"}`, 0)
	err := Install(f, "win11", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "nvidia-driver") || !strings.Contains(err.Error(), "installer exit 5") {
		t.Errorf("want stage failure naming stage and error, got %v", err)
	}
}

func TestInstallTimeoutGuidance(t *testing.T) {
	fastPolling(t)
	f := &virttest.Fake{State: "running", Phys: writingDisk}
	err := Install(f, "win11", &bytes.Buffer{})
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
	f := &virttest.Fake{State: "shut off", StartErr: errors.New("domain not found")}
	err := Install(f, "win11", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "start domain") {
		t.Errorf("want start failure, got %v", err)
	}
}
