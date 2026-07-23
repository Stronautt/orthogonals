package preflight

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/stronautt/orthogonals/internal/hw"
)

// genPCIDevice draws a device with deliberately hostile values: unknown
// vendors, shared and negative IOMMU groups, addresses that repeat.
func genPCIDevice(t *rapid.T) hw.PCIDevice {
	return hw.PCIDevice{
		Address: rapid.SampledFrom([]string{
			"0000:00:02.0", "0000:01:00.0", "0000:01:00.1", "", "garbage",
		}).Draw(t, "address"),
		Vendor: rapid.SampledFrom([]string{
			hw.VendorIntel, hw.VendorNVIDIA, hw.VendorAMD, "0xdead", "",
		}).Draw(t, "vendor"),
		Device:     rapid.SampledFrom([]string{"0x2206", "0xa780", ""}).Draw(t, "device"),
		Class:      rapid.SampledFrom([]string{"0x030000", "0x030200", "0x040300", "0x060400", ""}).Draw(t, "class"),
		Driver:     rapid.SampledFrom([]string{"nvidia", "i915", "vfio-pci", ""}).Draw(t, "driver"),
		IOMMUGroup: rapid.IntRange(-1, 3).Draw(t, "group"),
		HasReset:   rapid.Bool().Draw(t, "reset"),
		BootVGA:    rapid.Bool().Draw(t, "boot_vga"),
		Connectors: rapid.SliceOfN(rapid.SampledFrom([]string{"DP-1", "HDMI-A-1", "eDP-1"}), 0, 2).Draw(t, "connectors"),
	}
}

func genResult(t *rapid.T) *hw.Result {
	devs := rapid.SliceOfN(rapid.Custom(genPCIDevice), 0, 4).Draw(t, "devices")
	var igpu *hw.PCIDevice
	if rapid.Bool().Draw(t, "has_igpu") {
		d := genPCIDevice(t)
		igpu = &d
	}
	dgpus := rapid.SliceOfN(rapid.Custom(func(t *rapid.T) hw.DGPU {
		d := hw.DGPU{PCIDevice: genPCIDevice(t)}
		if rapid.Bool().Draw(t, "has_audio") {
			a := genPCIDevice(t)
			d.Audio = &a
		}
		return d
	}), 0, 3).Draw(t, "dgpus")

	return &hw.Result{
		Devices: devs,
		GPUs:    hw.GPUs{IGPU: igpu, DGPUs: dgpus},
		CPU: hw.CPU{
			Vendor:  rapid.SampledFrom([]string{hw.CPUVendorIntel, hw.CPUVendorAMD, ""}).Draw(t, "cpu_vendor"),
			Threads: rapid.IntRange(0, 256).Draw(t, "threads"),
			Cores:   rapid.IntRange(0, 128).Draw(t, "cores"),
			Hybrid:  rapid.Bool().Draw(t, "hybrid"),
			PCores:  rapid.SliceOfN(rapid.IntRange(0, 255), 0, 4).Draw(t, "p_cores"),
			ECores:  rapid.SliceOfN(rapid.IntRange(0, 255), 0, 4).Draw(t, "e_cores"),
		},
		Platform: hw.Platform{
			IOMMUAddressWidth: rapid.IntRange(0, 64).Draw(t, "aw"),
			IOMMUTable:        rapid.SampledFrom([]string{hw.IOMMUTableDMAR, hw.IOMMUTableIVRS, ""}).Draw(t, "iommu_table"),
			SELinux:           rapid.SampledFrom([]string{"enforcing", "permissive", "disabled", ""}).Draw(t, "selinux"),
			SecureBoot:        rapid.Bool().Draw(t, "secure_boot"),
			ChassisType:       rapid.IntRange(0, 40).Draw(t, "chassis"),
			GPUMux:            rapid.SampledFrom([]string{"hybrid", "discrete", ""}).Draw(t, "mux"),
			MemTotalBytes:     rapid.Uint64Range(0, 1<<40).Draw(t, "mem"),
			NVIDIA:            hw.NVIDIADriver{Loaded: rapid.Bool().Draw(t, "nvidia_loaded")},
			Tools:             map[string]bool{},
		},
	}
}

func genFacts(t *rapid.T) Facts {
	return Facts{
		PersistencedEnabled: rapid.Bool().Draw(t, "persistenced"),
		DefaultNetActive:    rapid.Bool().Draw(t, "default_net"),
		FreeDiskBytes:       rapid.Uint64Range(0, 1<<42).Draw(t, "free_disk"),
		OrthogonalsManaged:  rapid.Bool().Draw(t, "managed"),
		ForeignVFIO:         rapid.SliceOfN(rapid.SampledFrom([]string{"/etc/modprobe.d/x.conf: vfio"}), 0, 2).Draw(t, "foreign"),
		SwitcherooEnabled:   rapid.Bool().Draw(t, "switcheroo"),
		LibvirtReachable:    rapid.Bool().Draw(t, "libvirt"),
		BLSError:            rapid.SampledFrom([]string{"", "no entries"}).Draw(t, "bls_error"),
	}
}

// TestAnalyzeHoldsForAnyHost guards the code path most exposed to strange real
// hardware: every check must produce a defined status and a usable name, on any
// topology, without panicking.
func TestAnalyzeHoldsForAnyHost(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		checks := Analyze(genResult(rt), genFacts(rt))
		if len(checks) == 0 {
			rt.Fatal("Analyze returned no checks")
		}
		seen := map[string]bool{}
		for _, c := range checks {
			if c.Name == "" {
				rt.Fatalf("check with an empty name: %+v", c)
			}
			if seen[c.Name] {
				rt.Fatalf("duplicate check name %q", c.Name)
			}
			seen[c.Name] = true
			switch c.Status {
			case Pass, Warn, Fail:
			default:
				rt.Fatalf("check %q has undefined status %q", c.Name, c.Status)
			}
			if c.Status != Pass && c.Remedy == "" {
				rt.Fatalf("check %q is %s but offers no remedy", c.Name, c.Status)
			}
		}
		switch overall := Overall(checks); overall {
		case Pass, Warn, Fail:
			if code := overall.ExitCode(); code != 0 && code != 1 && code != 2 {
				rt.Fatalf("overall %q maps to exit code %d", overall, code)
			}
		default:
			rt.Fatalf("undefined overall status %q", overall)
		}
	})
}
