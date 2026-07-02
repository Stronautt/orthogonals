package hw

import (
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestDetectReference(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut", "grubby", "virsh"))

	r, err := Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Devices) != 3 {
		t.Errorf("Devices = %d, want 3", len(r.Devices))
	}
	if r.GPUs.IGPU == nil || r.GPUs.IGPU.Address != "0000:00:02.0" {
		t.Errorf("IGPU = %+v, want 0000:00:02.0", r.GPUs.IGPU)
	}
	if len(r.GPUs.DGPUs) != 1 {
		t.Fatalf("DGPUs = %d, want 1", len(r.GPUs.DGPUs))
	}
	dgpu := r.GPUs.DGPUs[0]
	if dgpu.Address != "0000:01:00.0" || dgpu.Driver != "nvidia" {
		t.Errorf("dGPU = %+v, want 0000:01:00.0/nvidia", dgpu.PCIDevice)
	}
	if dgpu.Audio == nil || dgpu.Audio.Address != "0000:01:00.1" {
		t.Errorf("dGPU audio = %+v, want 0000:01:00.1", dgpu.Audio)
	}
	if r.CPU.Threads != 20 || r.CPU.Cores != 14 {
		t.Errorf("CPU = %d cores / %d threads, want 14/20", r.CPU.Cores, r.CPU.Threads)
	}
	if r.Platform.IOMMUAddressWidth != 39 {
		t.Errorf("IOMMUAddressWidth = %d, want 39", r.Platform.IOMMUAddressWidth)
	}
	if !r.Platform.DMARTable {
		t.Error("DMARTable = false, want true (reference root has the ACPI table)")
	}
}

func TestDetectMissingPCITree(t *testing.T) {
	if _, err := Detect(t.TempDir()); err == nil {
		t.Fatal("want error when sysfs PCI tree is missing")
	}
}

func TestSummaryReference(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut", "grubby"))

	r, err := Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	s := r.Summary()
	for _, want := range []string{
		"14 cores / 20 threads",
		"0000:00:02.0",
		"0000:01:00.0",
		"0000:01:00.1",
		"address width 39",
		"enforcing",
		"desktop",
		"dracut: ok",
		"virsh: MISSING",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}
