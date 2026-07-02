package orchestrate

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestVerifyAllChecksPass(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "NVIDIA-SMI 580.88   Driver Version: 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 0`)
	var out bytes.Buffer
	if err := Verify(t.TempDir(), "win11", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{
		"PASS vm start", "PASS guest agent ping", "PASS guest nvidia-smi",
		"PASS guest display pipeline", "PASS clean shutdown",
		"PASS host gpu idle", "all checks passed",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// The NVIDIA driver reserves a MiB or two on an idle card with no processes
// attached — treating "not exactly 0" as a leak fails on every healthy host.
func TestVerifyHostGPUIdleAllowsDriverReservation(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "NVIDIA-SMI 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 1`)
	var out bytes.Buffer
	if err := Verify(t.TempDir(), "win11", &out); err != nil {
		t.Fatalf("1 MiB of driver reservation must not fail the idle check: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS host gpu idle") {
		t.Errorf("idle GPU not reported as passing:\n%s", out.String())
	}
}

// A process still holding the card (a leaked VM) keeps hundreds of MiB — that
// must still fail, or the check is worthless.
func TestVerifyHostGPUIdleFailsOnHeldCard(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "NVIDIA-SMI 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 512`)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "host gpu idle") {
		t.Fatalf("want host-gpu-idle failure, got %v", err)
	}
	if !strings.Contains(out.String(), "512 MiB used") {
		t.Errorf("failure must name the leaked memory:\n%s", out.String())
	}
}

func TestVerifyGuestNvidiaSmiFails(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "no devices found", 9)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest nvidia-smi") {
		t.Fatalf("want guest nvidia-smi failure, got %v", err)
	}
	if !strings.Contains(out.String(), "FAIL guest nvidia-smi") {
		t.Errorf("failure not reported:\n%s", out.String())
	}
	// later checks must not run after a failure
	if strings.Contains(out.String(), "clean shutdown") {
		t.Errorf("checks continued past the failure:\n%s", out.String())
	}
}

// nvidia-smi succeeding while the capture path is dead is the exact state the
// display check exists for: the display-check exec (powershell.exe → pid 8)
// fails while the generic guest-exec (nvidia-smi → pid 7) passes.
func TestVerifyDisplayPipelineFails(t *testing.T) {
	fastPolling(t)
	msg := base64.StdEncoding.EncodeToString([]byte("Looking Glass (host) service is not running"))
	// clause order matters: the pid-8 status before the generic status, the
	// powershell exec before the generic exec
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo running ;;
*guest-ping*) echo '{"return":{}}' ;;
*'"pid":8'*) echo '{"return":{"exited":true,"exitcode":1,"out-data":"`+msg+`"}}' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":0,"out-data":""}}' ;;
*powershell.exe*) echo '{"return":{"pid":8}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest display pipeline") {
		t.Fatalf("want display-pipeline failure, got %v", err)
	}
	if !strings.Contains(out.String(), "FAIL guest display pipeline") ||
		!strings.Contains(out.String(), "service is not running") {
		t.Errorf("failure must name the dead service:\n%s", out.String())
	}
	// later checks must not run after a failure
	if strings.Contains(out.String(), "clean shutdown") {
		t.Errorf("checks continued past the failure:\n%s", out.String())
	}
}

func TestVerifyHostGPUBusy(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "ok", 0)
	fakeBin(t, "nvidia-smi", `echo 512`)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "512 MiB") {
		t.Fatalf("want busy-GPU failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "fuser") {
		t.Errorf("busy-GPU error should point at fuser: %v", err)
	}
}

func TestVerifyAgentNeverAnswers(t *testing.T) {
	fastPolling(t)
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo running ;;
*qemu-agent-command*) exit 1 ;;
esac`)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest agent") {
		t.Fatalf("want agent-ping failure, got %v", err)
	}
}

func TestVerifyShutdownTimeout(t *testing.T) {
	fastPolling(t)
	// shutdown accepted but the domain never leaves running
	fakeBin(t, "virsh", `case "$*" in
domstate*) echo running ;;
*guest-ping*) echo '{"return":{}}' ;;
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":0,"out-data":""}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`)
	var out bytes.Buffer
	err := Verify(t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "did not shut off") {
		t.Fatalf("want shutdown timeout, got %v", err)
	}
}

func TestVerifyStaticBindingSkipsHostIdle(t *testing.T) {
	fastPolling(t)
	fakeVirsh(t, "running", "NVIDIA-SMI 580.88", 0)
	// no host nvidia-smi fake: under static binding it must never be called
	root := t.TempDir()
	writeManifest(t, root, "intel_iommu=on iommu=pt vfio-pci.ids=10de:2206,10de:1aef")
	var out bytes.Buffer
	if err := Verify(root, "win11", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "SKIP host gpu idle") {
		t.Errorf("static binding should skip the host-idle check:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "all checks passed") {
		t.Errorf("verify should pass:\n%s", out.String())
	}
}
