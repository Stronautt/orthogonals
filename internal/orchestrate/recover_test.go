package orchestrate

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func fastRecover(t *testing.T) {
	t.Helper()
	savedRemove, savedRescan := removeSettle, rescanSettle
	removeSettle, rescanSettle = time.Millisecond, time.Millisecond
	t.Cleanup(func() { removeSettle, rescanSettle = savedRemove, savedRescan })
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRecoverSequence(t *testing.T) {
	fastRecover(t)
	root := hwtest.ReferenceRoot(t)
	modprobe := fakeBin(t, "modprobe", "")
	systemctl := fakeBin(t, "systemctl", "")
	smi := fakeBin(t, "nvidia-smi", "")

	var out bytes.Buffer
	if err := Recover(root, true, &out); err != nil {
		t.Fatalf("Recover: %v\n%s", err, out.String())
	}

	// unload/reload orders shared with the hook scripts (hooks.NVIDIA*Order)
	wantModprobe := "-r nvidia_drm\n-r nvidia_modeset\n-r nvidia_uvm\n-r nvidia\nnvidia\nnvidia_uvm\nnvidia_drm\n"
	if got := readFile(t, modprobe); got != wantModprobe {
		t.Errorf("modprobe calls = %q, want %q", got, wantModprobe)
	}
	for _, dev := range []string{"0000:01:00.0", "0000:01:00.1"} {
		base := filepath.Join(root, "sys/bus/pci/devices", dev)
		if got := readFile(t, filepath.Join(base, "driver_override")); got != "\n" {
			t.Errorf("%s driver_override = %q, want cleared", dev, got)
		}
		if got := readFile(t, filepath.Join(base, "remove")); got != "1\n" {
			t.Errorf("%s remove = %q, want \"1\\n\"", dev, got)
		}
	}
	if got := readFile(t, filepath.Join(root, "sys/bus/pci/rescan")); got != "1\n" {
		t.Errorf("rescan = %q, want \"1\\n\"", got)
	}
	if got := readFile(t, systemctl); !strings.Contains(got, "try-restart switcheroo-control.service") {
		t.Errorf("systemctl calls = %q, want try-restart switcheroo-control.service", got)
	}
	if got := readFile(t, smi); got == "" {
		t.Error("nvidia-smi health check was never run")
	}
	if !strings.Contains(out.String(), "recovered — 0000:01:00.0") {
		t.Errorf("output = %q, want recovery confirmation", out.String())
	}
}

func TestRecoverStillBroken(t *testing.T) {
	fastRecover(t)
	root := hwtest.ReferenceRoot(t)
	fakeBin(t, "modprobe", "")
	fakeBin(t, "systemctl", "")
	fakeBin(t, "nvidia-smi", "exit 1")

	err := Recover(root, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "reboot required") {
		t.Fatalf("err = %v, want reboot-required failure", err)
	}
}

func TestRecoverDryRun(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	var out bytes.Buffer
	if err := Recover(root, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would") ||
		!strings.Contains(out.String(), "0000:01:00.0 0000:01:00.1") {
		t.Errorf("dry run output = %q, want plan with both device addresses", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "sys/bus/pci/devices/0000:01:00.0/remove")); err == nil {
		t.Error("dry run wrote a sysfs remove attribute")
	}
}

func TestRecoverNeedsOneGPU(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	hwtest.AddPCI(t, root, hwtest.Dev{
		Addr: "0000:02:00.0", Vendor: "0x10de", Device: "0x2206",
		Class: "0x030000", Driver: "nvidia", Group: 2, Reset: true,
	})
	err := Recover(root, true, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v, want exactly-one-GPU refusal", err)
	}
}
