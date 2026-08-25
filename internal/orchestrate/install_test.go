package orchestrate

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/testsupport"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

func fastPolling(t *testing.T) {
	t.Helper()
	testsupport.Swap(t, &installTimeout, 200*time.Millisecond)
	testsupport.Swap(t, &installInterval, time.Millisecond)
	testsupport.Swap(t, &provisionFailGrace, 10*time.Millisecond)
	testsupport.Swap(t, &pingTries, 3)
	testsupport.Swap(t, &pingInterval, time.Millisecond)
	testsupport.Swap(t, &shutdownTries, 5)
	testsupport.Swap(t, &shutdownInterval, time.Millisecond)
	testsupport.Swap(t, &idleTries, 2)
	testsupport.Swap(t, &idleInterval, time.Millisecond)
	testsupport.Swap(t, &cdPromptWindow, 20*time.Millisecond)
	testsupport.Swap(t, &cdPromptInterval, time.Millisecond)
}

func fakeBin(t *testing.T, name, extra string) string {
	t.Helper()
	return hwtest.FakeTool(t, hwtest.FakePath(t), name, extra)
}

// writingDisk is a physical allocation past setupWritingBytes.
const writingDisk = 9663676416

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
	testsupport.Swap(t, &heartbeatInterval, time.Millisecond)
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
	testsupport.Swap(t, &cdPromptInterval, time.Millisecond)
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
