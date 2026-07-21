package orchestrate

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

func TestVerifyAllChecksPass(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "NVIDIA-SMI 580.88   Driver Version: 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 0`)
	var out bytes.Buffer
	if err := Verify(f, t.TempDir(), "win11", &out); err != nil {
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

// A MiB or two of driver reservation on an idle card must not fail the check.
func TestVerifyHostGPUIdleAllowsDriverReservation(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "NVIDIA-SMI 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 1`)
	var out bytes.Buffer
	if err := Verify(f, t.TempDir(), "win11", &out); err != nil {
		t.Fatalf("1 MiB of driver reservation must not fail the idle check: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS host gpu idle") {
		t.Errorf("idle GPU not reported as passing:\n%s", out.String())
	}
}

// A held card keeping hundreds of MiB must still fail.
func TestVerifyHostGPUIdleFailsOnHeldCard(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "NVIDIA-SMI 580.88", 0)
	fakeBin(t, "nvidia-smi", `echo 512`)
	var out bytes.Buffer
	err := Verify(f, t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "host gpu idle") {
		t.Fatalf("want host-gpu-idle failure, got %v", err)
	}
	if !strings.Contains(out.String(), "512 MiB used") {
		t.Errorf("failure must name the leaked memory:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "/dev/nvidia") {
		t.Errorf("host-gpu-idle error should point at the held device: %v", err)
	}
}

func TestVerifyGuestNvidiaSmiFails(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "no devices found", 9)
	var out bytes.Buffer
	err := Verify(f, t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest nvidia-smi") {
		t.Fatalf("want guest nvidia-smi failure, got %v", err)
	}
	if !strings.Contains(out.String(), "FAIL guest nvidia-smi") {
		t.Errorf("failure not reported:\n%s", out.String())
	}
	if strings.Contains(out.String(), "clean shutdown") {
		t.Errorf("checks continued past the failure:\n%s", out.String())
	}
}

// nvidia-smi passing while the capture path is dead is what the display check catches.
func TestVerifyDisplayPipelineFails(t *testing.T) {
	fastPolling(t)
	msg := base64.StdEncoding.EncodeToString([]byte("Looking Glass (host) service is not running"))
	f := &virttest.Fake{State: "running", Phys: writingDisk, Agent: func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, `"pid":8`):
			return `{"return":{"exited":true,"exitcode":1,"out-data":"` + msg + `"}}`, nil
		case strings.Contains(cmd, "guest-exec-status"):
			return `{"return":{"exited":true,"exitcode":0,"out-data":""}}`, nil
		case strings.Contains(cmd, "powershell.exe"):
			return `{"return":{"pid":8}}`, nil
		case strings.Contains(cmd, "guest-exec"):
			return `{"return":{"pid":7}}`, nil
		default:
			return `{"return":{}}`, nil
		}
	}}
	var out bytes.Buffer
	err := Verify(f, t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest display pipeline") {
		t.Fatalf("want display-pipeline failure, got %v", err)
	}
	if !strings.Contains(out.String(), "FAIL guest display pipeline") ||
		!strings.Contains(out.String(), "service is not running") {
		t.Errorf("failure must name the dead service:\n%s", out.String())
	}
	if strings.Contains(out.String(), "clean shutdown") {
		t.Errorf("checks continued past the failure:\n%s", out.String())
	}
}

func TestVerifyAgentNeverAnswers(t *testing.T) {
	fastPolling(t)
	f := &virttest.Fake{State: "running", Phys: writingDisk}
	var out bytes.Buffer
	err := Verify(f, t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "guest agent") {
		t.Fatalf("want agent-ping failure, got %v", err)
	}
}

func TestVerifyShutdownTimeout(t *testing.T) {
	fastPolling(t)
	f := &virttest.Fake{State: "running", Phys: writingDisk, Agent: virttest.Responder("", "", 0)}
	f.OnState = func() (string, error) { return "running", nil }
	var out bytes.Buffer
	err := Verify(f, t.TempDir(), "win11", &out)
	if err == nil || !strings.Contains(err.Error(), "did not shut off") {
		t.Fatalf("want shutdown timeout, got %v", err)
	}
}

func TestVerifyStaticBindingSkipsHostIdle(t *testing.T) {
	fastPolling(t)
	f := fakeVM("running", "NVIDIA-SMI 580.88", 0)
	root := t.TempDir()
	writeManifest(t, root, "intel_iommu=on iommu=pt vfio-pci.ids=10de:2206,10de:1aef")
	var out bytes.Buffer
	if err := Verify(f, root, "win11", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "SKIP host gpu idle") {
		t.Errorf("static binding should skip the host-idle check:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "all checks passed") {
		t.Errorf("verify should pass:\n%s", out.String())
	}
}
