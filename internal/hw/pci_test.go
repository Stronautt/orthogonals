package hw

import (
	"os"
	"reflect"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestScanPCIReference(t *testing.T) {
	devs, err := ScanPCI(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	want := []PCIDevice{
		{Address: "0000:00:02.0", Vendor: "0x8086", Device: "0xa780", Class: "0x030000", Driver: "i915", IOMMUGroup: 0, HasReset: true,
			BootVGA: true, DRMCard: "card0", Connectors: []string{"DP-1"}},
		{Address: "0000:01:00.0", Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", IOMMUGroup: 1, HasReset: true,
			DRMCard: "card1"},
		{Address: "0000:01:00.1", Vendor: "0x10de", Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", IOMMUGroup: 1, HasReset: true},
	}
	if !reflect.DeepEqual(devs, want) {
		t.Fatalf("devices mismatch:\ngot  %+v\nwant %+v", devs, want)
	}
}

func TestScanPCIUnboundNoIOMMU(t *testing.T) {
	root := t.TempDir()
	hwtest.AddPCI(t, root, hwtest.Dev{Addr: "0000:02:00.0", Vendor: "0x10ec", Device: "0x8168", Class: "0x020000", Group: -1})

	devs, err := ScanPCI(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 1 {
		t.Fatalf("got %d devices, want 1", len(devs))
	}
	d := devs[0]
	if d.Driver != "" {
		t.Errorf("Driver = %q, want empty for unbound device", d.Driver)
	}
	if d.IOMMUGroup != -1 {
		t.Errorf("IOMMUGroup = %d, want -1 when IOMMU is off", d.IOMMUGroup)
	}
	if d.HasReset {
		t.Error("HasReset = true, want false without reset file")
	}
}

func TestScanDRM(t *testing.T) {
	const addr = "0000:02:00.0"
	dev := hwtest.Dev{Addr: addr, Vendor: "0x10de", Device: "0x2206", Class: "0x030000", Driver: "nvidia", Group: 2}
	tests := []struct {
		name      string
		files     map[string]string
		wantCard  string
		wantConns []string
	}{
		{name: "no drm dir", wantCard: "", wantConns: nil},
		{name: "renderD only", files: map[string]string{"drm/renderD129/dev": "226:129\n"}, wantCard: "", wantConns: nil},
		{
			name: "only disconnected connectors",
			files: map[string]string{
				"drm/card2/card2-DP-1/status":     "disconnected\n",
				"drm/card2/card2-HDMI-A-1/status": "disconnected\n",
			},
			wantCard: "card2", wantConns: nil,
		},
		{
			name: "connected connectors sorted",
			files: map[string]string{
				"drm/card2/card2-DP-1/status":     "connected\n",
				"drm/card2/card2-DP-2/status":     "connected\n",
				"drm/card2/card2-HDMI-A-1/status": "disconnected\n",
				"drm/renderD129/dev":              "226:129\n",
			},
			wantCard: "card2", wantConns: []string{"DP-1", "DP-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			hwtest.AddPCI(t, root, dev)
			for rel, content := range tt.files {
				hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+addr+"/"+rel, content)
			}
			card, conns := scanDRM(root + "/sys/bus/pci/devices/" + addr)
			if card != tt.wantCard {
				t.Errorf("card = %q, want %q", card, tt.wantCard)
			}
			if !reflect.DeepEqual(conns, tt.wantConns) {
				t.Errorf("connectors = %v, want %v", conns, tt.wantConns)
			}
		})
	}
}

func TestScanPCIMissingTree(t *testing.T) {
	if _, err := ScanPCI(t.TempDir()); err == nil {
		t.Fatal("want error for missing sys/bus/pci/devices")
	}
}

func TestSoleNVIDIA(t *testing.T) {
	nv := DGPU{PCIDevice: PCIDevice{Address: "0000:01:00.0", Vendor: VendorNVIDIA}}
	nv2 := DGPU{PCIDevice: PCIDevice{Address: "0000:02:00.0", Vendor: VendorNVIDIA}}
	amd := DGPU{PCIDevice: PCIDevice{Address: "0000:03:00.0", Vendor: VendorAMD}}

	got, err := GPUs{DGPUs: []DGPU{nv, amd}}.SoleNVIDIA()
	if err != nil {
		t.Fatalf("one NVIDIA dGPU: %v", err)
	}
	if got.Address != nv.Address {
		t.Errorf("Address = %q, want %q", got.Address, nv.Address)
	}
	if _, err := (GPUs{DGPUs: []DGPU{amd}}).SoleNVIDIA(); err == nil {
		t.Error("want error for zero NVIDIA dGPUs")
	}
	if _, err := (GPUs{DGPUs: []DGPU{nv, nv2}}).SoleNVIDIA(); err == nil {
		t.Error("want error for two NVIDIA dGPUs")
	}
}

func TestVendorDeviceID(t *testing.T) {
	d := PCIDevice{Vendor: "0x10de", Device: "0x2206"}
	if got := d.VendorDeviceID(); got != "10de:2206" {
		t.Errorf("VendorDeviceID = %q, want %q", got, "10de:2206")
	}
}

func TestSysfsDeviceWriters(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	const addr = "0000:01:00.0"

	if err := SetDriverOverride(root, addr, "vfio-pci"); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root+"/sys/bus/pci/devices/"+addr+"/driver_override", "vfio-pci\n")

	if err := SetDriverOverride(root, addr, ""); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root+"/sys/bus/pci/devices/"+addr+"/driver_override", "\n")

	if err := RemoveDevice(root, addr); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root+"/sys/bus/pci/devices/"+addr+"/remove", "1\n")

	if err := RescanPCI(root); err != nil {
		t.Fatal(err)
	}
	assertFile(t, root+"/sys/bus/pci/rescan", "1\n")

	if err := RemoveDevice(root, "0000:ff:00.0"); err == nil {
		t.Error("want error writing to a missing device node")
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", path, b, want)
	}
}

func TestIOMMUActive(t *testing.T) {
	root := t.TempDir()
	active, err := IOMMUActive(root)
	if err != nil || active {
		t.Errorf("empty root: active=%v err=%v, want false, nil", active, err)
	}
	if err := os.MkdirAll(root+"/sys/kernel/iommu_groups/1", 0o755); err != nil {
		t.Fatal(err)
	}
	active, err = IOMMUActive(root)
	if err != nil || !active {
		t.Errorf("with group: active=%v err=%v, want true, nil", active, err)
	}
}

func TestModuleLoaded(t *testing.T) {
	root := t.TempDir()
	if ModuleLoaded(root, "vfio_pci") {
		t.Error("want false for missing module")
	}
	if err := os.MkdirAll(root+"/sys/module/vfio_pci", 0o755); err != nil {
		t.Fatal(err)
	}
	if !ModuleLoaded(root, "vfio_pci") {
		t.Error("want true for present module")
	}
}

func TestClassifyGPUs(t *testing.T) {
	igpu := PCIDevice{Address: "0000:00:02.0", Vendor: "0x8086", Class: "0x030000"}
	nv := PCIDevice{Address: "0000:01:00.0", Vendor: "0x10de", Class: "0x030000"}
	nvAudio := PCIDevice{Address: "0000:01:00.1", Vendor: "0x10de", Class: "0x040300"}
	nv2 := PCIDevice{Address: "0000:02:00.0", Vendor: "0x10de", Class: "0x030000"}
	amd := PCIDevice{Address: "0000:03:00.0", Vendor: "0x1002", Class: "0x030000"}
	arc := PCIDevice{Address: "0000:04:00.0", Vendor: "0x8086", Class: "0x030000"}
	nic := PCIDevice{Address: "0000:05:00.0", Vendor: "0x8086", Class: "0x020000"}

	tests := []struct {
		name      string
		devs      []PCIDevice
		wantIGPU  string
		wantDGPUs []string
		wantAudio map[string]string
	}{
		{
			name:      "reference",
			devs:      []PCIDevice{igpu, nv, nvAudio, nic},
			wantIGPU:  "0000:00:02.0",
			wantDGPUs: []string{"0000:01:00.0"},
			wantAudio: map[string]string{"0000:01:00.0": "0000:01:00.1"},
		},
		{
			name:      "no igpu",
			devs:      []PCIDevice{nv, nvAudio},
			wantDGPUs: []string{"0000:01:00.0"},
			wantAudio: map[string]string{"0000:01:00.0": "0000:01:00.1"},
		},
		{
			name:      "two nvidia dgpus",
			devs:      []PCIDevice{igpu, nv, nvAudio, nv2},
			wantIGPU:  "0000:00:02.0",
			wantDGPUs: []string{"0000:01:00.0", "0000:02:00.0"},
			wantAudio: map[string]string{"0000:01:00.0": "0000:01:00.1", "0000:02:00.0": ""},
		},
		{
			name:      "amd dgpu",
			devs:      []PCIDevice{igpu, amd},
			wantIGPU:  "0000:00:02.0",
			wantDGPUs: []string{"0000:03:00.0"},
			wantAudio: map[string]string{"0000:03:00.0": ""},
		},
		{
			name:      "intel discrete is a dgpu not the igpu",
			devs:      []PCIDevice{arc},
			wantDGPUs: []string{"0000:04:00.0"},
			wantAudio: map[string]string{"0000:04:00.0": ""},
		},
		{
			name: "no gpus at all",
			devs: []PCIDevice{nic},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := classifyGPUs(tt.devs)
			gotIGPU := ""
			if g.IGPU != nil {
				gotIGPU = g.IGPU.Address
			}
			if gotIGPU != tt.wantIGPU {
				t.Errorf("IGPU = %q, want %q", gotIGPU, tt.wantIGPU)
			}
			var gotDGPUs []string
			for _, d := range g.DGPUs {
				gotDGPUs = append(gotDGPUs, d.Address)
			}
			if !reflect.DeepEqual(gotDGPUs, tt.wantDGPUs) && (len(gotDGPUs) != 0 || len(tt.wantDGPUs) != 0) {
				t.Errorf("DGPUs = %v, want %v", gotDGPUs, tt.wantDGPUs)
			}
			for _, d := range g.DGPUs {
				gotAudio := ""
				if d.Audio != nil {
					gotAudio = d.Audio.Address
				}
				if gotAudio != tt.wantAudio[d.Address] {
					t.Errorf("audio of %s = %q, want %q", d.Address, gotAudio, tt.wantAudio[d.Address])
				}
			}
		})
	}
}
