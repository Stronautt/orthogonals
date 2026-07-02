package preflight

import (
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
)

// refResult mirrors the PoC reference machine (i5-13600K + RTX 3080) but with
// a 46-bit address width and Secure Boot off so the base case is all-pass.
func refResult() *hw.Result {
	igpu := hw.PCIDevice{Address: "0000:00:02.0", Vendor: hw.VendorIntel, Device: "0xa780", Class: "0x030000", Driver: "i915", IOMMUGroup: 0, HasReset: true}
	gpu := hw.PCIDevice{Address: "0000:01:00.0", Vendor: hw.VendorNVIDIA, Device: "0x2206", Class: "0x030000", Driver: "nvidia", IOMMUGroup: 1, HasReset: true}
	audio := hw.PCIDevice{Address: "0000:01:00.1", Vendor: hw.VendorNVIDIA, Device: "0x1aef", Class: "0x040300", Driver: "snd_hda_intel", IOMMUGroup: 1, HasReset: true}
	tools := map[string]bool{}
	for _, tool := range hw.RequiredTools {
		tools[tool] = true
	}
	return &hw.Result{
		Devices: []hw.PCIDevice{igpu, gpu, audio},
		GPUs:    hw.GPUs{IGPU: &igpu, DGPUs: []hw.DGPU{{PCIDevice: gpu, Audio: &audio}}},
		CPU: hw.CPU{Threads: 20, Cores: 14, Hybrid: true,
			PCores: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, ECores: []int{12, 13, 14, 15, 16, 17, 18, 19}},
		Platform: hw.Platform{
			IOMMUAddressWidth: 46, DMARTable: true, SELinux: "enforcing", SecureBoot: false,
			ChassisType: 3, MemTotalBytes: 32 << 30, Tools: tools,
		},
	}
}

func goodFacts() Facts {
	return Facts{DefaultNetActive: true, FreeDiskBytes: 500 << 30,
		SwitcherooEnabled: true, SwitcherooNVIDIA: true}
}

func findCheck(t *testing.T, checks []Check, name string) Check {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q in %+v", name, checks)
	return Check{}
}

func TestAnalyzeReferencePasses(t *testing.T) {
	checks := Analyze(refResult(), goodFacts())
	if got := Overall(checks); got != Pass {
		t.Errorf("Overall = %v, want pass; checks: %+v", got, checks)
	}
}

func TestAnalyzers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*hw.Result, *Facts)
		check  string
		want   Status
		has    []string // substrings expected in Message + Remedy
	}{
		{
			// no DMAR table at all = VT-d off in the BIOS or unsupported
			name: "iommu off fails",
			mutate: func(r *hw.Result, _ *Facts) {
				r.Platform.IOMMUAddressWidth = 0
				r.Platform.DMARTable = false
			},
			check: "iommu", want: Fail, has: []string{"BIOS"},
		},
		{
			// DMAR table present but translation off = virgin host before
			// apply adds the kernel args; must NOT block apply (it fixes this)
			name:   "iommu inactive with dmar table warns",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.IOMMUAddressWidth = 0 },
			check:  "iommu", want: Warn, has: []string{"apply", "reboot"},
		},
		{
			name: "single gpu fails",
			mutate: func(r *hw.Result, _ *Facts) {
				r.GPUs.IGPU = nil
				r.Devices = r.Devices[1:]
			},
			check: "gpu-topology", want: Fail, has: []string{"single"},
		},
		{
			name:   "no nvidia dgpu fails",
			mutate: func(r *hw.Result, _ *Facts) { r.GPUs.DGPUs = nil },
			check:  "gpu-topology", want: Fail, has: []string{"NVIDIA"},
		},
		{
			name: "no igpu with dgpus fails with bios remedy",
			mutate: func(r *hw.Result, _ *Facts) {
				second := hw.DGPU{PCIDevice: hw.PCIDevice{Address: "0000:02:00.0", Vendor: hw.VendorNVIDIA, Class: "0x030000", IOMMUGroup: 2, HasReset: true}}
				r.GPUs.DGPUs = append(r.GPUs.DGPUs, second)
				r.GPUs.IGPU = nil
			},
			check: "gpu-topology", want: Fail, has: []string{"iGPU", "BIOS"},
		},
		{
			name: "two nvidia dgpus fail",
			mutate: func(r *hw.Result, _ *Facts) {
				second := hw.DGPU{PCIDevice: hw.PCIDevice{Address: "0000:02:00.0", Vendor: hw.VendorNVIDIA, Class: "0x030000", IOMMUGroup: 2, HasReset: true}}
				r.GPUs.DGPUs = append(r.GPUs.DGPUs, second)
			},
			check: "gpu-topology", want: Fail, has: []string{"one NVIDIA"},
		},
		{
			name: "amd dgpu fails",
			mutate: func(r *hw.Result, _ *Facts) {
				amd := hw.DGPU{PCIDevice: hw.PCIDevice{Address: "0000:02:00.0", Vendor: hw.VendorAMD, Class: "0x030000", IOMMUGroup: 2, HasReset: true}}
				r.GPUs.DGPUs = append(r.GPUs.DGPUs, amd)
			},
			check: "gpu-topology", want: Fail, has: []string{"AMD"},
		},
		{
			name:   "laptop chassis fails",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.ChassisType = 10 },
			check:  "chassis", want: Fail, has: []string{"laptop"},
		},
		{
			name: "stranger in gpu iommu group fails with acs refusal",
			mutate: func(r *hw.Result, _ *Facts) {
				nvme := hw.PCIDevice{Address: "0000:01:00.2", Vendor: "0x144d", Class: "0x010802", IOMMUGroup: 1, HasReset: true}
				r.Devices = append(r.Devices, nvme)
			},
			check: "iommu-group", want: Fail,
			has: []string{"0000:01:00.2", "ACS override", "BIOS", "slot", "kernel"},
		},
		{
			name: "bridge in gpu iommu group is fine",
			mutate: func(r *hw.Result, _ *Facts) {
				bridge := hw.PCIDevice{Address: "0000:00:01.0", Vendor: hw.VendorIntel, Class: "0x060400", IOMMUGroup: 1}
				r.Devices = append(r.Devices, bridge)
			},
			check: "iommu-group", want: Pass,
		},
		{
			name:   "missing reset file fails",
			mutate: func(r *hw.Result, _ *Facts) { r.GPUs.DGPUs[0].HasReset = false },
			check:  "gpu-reset", want: Fail, has: []string{"reset"},
		},
		{
			name:   "missing xorriso fails",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.Tools["xorriso"] = false },
			check:  "tools", want: Fail, has: []string{"xorriso"},
		},
		{
			name:   "missing virsh fails",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.Tools["virsh"] = false },
			check:  "tools", want: Fail, has: []string{"virsh"},
		},
		{
			name:   "missing lsof only warns",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.Tools["lsof"] = false },
			check:  "tools", want: Warn, has: []string{"lsof"},
		},
		{
			name: "too few assignable vcpus fails",
			mutate: func(r *hw.Result, _ *Facts) {
				r.CPU = hw.CPU{Threads: 4, Cores: 2, PCores: []int{0, 1, 2, 3}}
			},
			check: "cpu", want: Fail, has: []string{"vCPU"},
		},
		{
			// non-hybrid 3c/6t: 2 threads/core, host takes one core and the
			// emulator+iothread take a second (no E-cores), leaving only 2
			// vCPUs — domain.pinning refuses this, so the gate must too, or the
			// host gets mutated + rebooted before `vm define` fails.
			name: "non-hybrid small cpu is refused to match pinning",
			mutate: func(r *hw.Result, _ *Facts) {
				r.CPU = hw.CPU{Threads: 6, Cores: 3,
					PCores: []int{0, 1, 2, 3, 4, 5}}
			},
			check: "cpu", want: Fail, has: []string{"vCPU"},
		},
		{
			name:   "low ram fails",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.MemTotalBytes = 8 << 30 },
			check:  "memory", want: Fail, has: []string{"8 GiB"},
		},
		{
			// a real 16 GiB host reports ~15.5 GiB MemTotal after firmware
			// reservations; the documented minimum must still pass
			name:   "16 GiB host with firmware reservations passes",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.MemTotalBytes = 15872 << 20 },
			check:  "memory", want: Pass,
		},
		{
			name:   "39-bit address width warns about auto-remediation",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.IOMMUAddressWidth = 39 },
			check:  "address-width", want: Warn, has: []string{"39", "automatic"},
		},
		{
			name:   "secure boot with nvidia warns about module signing",
			mutate: func(r *hw.Result, _ *Facts) { r.Platform.SecureBoot = true },
			check:  "secure-boot", want: Warn, has: []string{"sign"},
		},
		{
			name:   "nvidia-persistenced enabled warns",
			mutate: func(_ *hw.Result, f *Facts) { f.PersistencedEnabled = true },
			check:  "persistenced", want: Warn, has: []string{"persistenced"},
		},
		{
			name:   "inactive default network warns",
			mutate: func(_ *hw.Result, f *Facts) { f.DefaultNetActive = false },
			check:  "default-network", want: Warn, has: []string{"apply"},
		},
		{
			name:   "low disk space warns",
			mutate: func(_ *hw.Result, f *Facts) { f.FreeDiskBytes = 20 << 30 },
			check:  "disk-space", want: Warn, has: []string{"GiB"},
		},
		{
			name:   "undetectable disk space warns instead of a bogus 0 GiB pass",
			mutate: func(_ *hw.Result, f *Facts) { f.FreeDiskBytes = 0 },
			check:  "disk-space", want: Warn, has: []string{"could not determine"},
		},
		{
			name: "foreign vfio config fails",
			mutate: func(_ *hw.Result, f *Facts) {
				f.ForeignVFIO = []string{"/etc/modprobe.d/vfio.conf: options vfio-pci ids=10de:2206"}
			},
			check: "foreign-vfio", want: Fail, has: []string{"vfio.conf", "remove", "adopt"},
		},
		{
			name: "orthogonals-managed vfio config passes",
			mutate: func(_ *hw.Result, f *Facts) {
				f.ForeignVFIO = []string{"kernel cmdline: vfio-pci.ids=10de:2206"}
				f.OrthogonalsManaged = true
			},
			check: "foreign-vfio", want: Pass,
		},
		{
			name:   "switcheroo disabled warns",
			mutate: func(_ *hw.Result, f *Facts) { f.SwitcherooEnabled = false; f.SwitcherooNVIDIA = false },
			check:  "switcheroo", want: Warn,
			has: []string{"switcheroo-control", "dnf install switcheroo-control", "systemctl enable --now switcheroo-control"},
		},
		{
			name:   "switcheroo missing nvidia gpu warns",
			mutate: func(_ *hw.Result, f *Facts) { f.SwitcherooNVIDIA = false },
			check:  "switcheroo", want: Warn, has: []string{"NVIDIA", "restart"},
		},
		{
			name: "duplicate nvidia vendor:device ids fail",
			mutate: func(r *hw.Result, _ *Facts) {
				twin := hw.PCIDevice{Address: "0000:02:00.0", Vendor: hw.VendorNVIDIA, Device: "0x2206",
					Class: "0x030000", Driver: "nvidia", IOMMUGroup: 2, HasReset: true}
				r.Devices = append(r.Devices, twin)
				r.GPUs.DGPUs = append(r.GPUs.DGPUs, hw.DGPU{PCIDevice: twin})
			},
			check: "duplicate-gpu-ids", want: Fail,
			has: []string{"10de:2206", "0000:01:00.0", "0000:02:00.0", "static"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, f := refResult(), goodFacts()
			tt.mutate(r, &f)
			c := findCheck(t, Analyze(r, f), tt.check)
			if c.Status != tt.want {
				t.Fatalf("%s status = %v, want %v (%+v)", tt.check, c.Status, tt.want, c)
			}
			text := c.Message + " " + c.Remedy
			for _, want := range tt.has {
				if !strings.Contains(text, want) {
					t.Errorf("%s text missing %q: %q", tt.check, want, text)
				}
			}
		})
	}
}

// The ACS override must be refused outright, never offered as a fallback.
func TestACSOverrideNeverOffered(t *testing.T) {
	r, f := refResult(), goodFacts()
	r.Devices = append(r.Devices, hw.PCIDevice{Address: "0000:01:00.2", Class: "0x010802", IOMMUGroup: 1})
	c := findCheck(t, Analyze(r, f), "iommu-group")
	if c.Status != Fail {
		t.Fatalf("status = %v, want fail", c.Status)
	}
	if !strings.Contains(c.Remedy, "never") {
		t.Errorf("remedy must state the ACS override is never enabled: %q", c.Remedy)
	}
}

func TestOverallAndExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		want   Status
		code   int
	}{
		{"all pass", []Check{{Status: Pass}, {Status: Pass}}, Pass, 0},
		{"warn wins over pass", []Check{{Status: Pass}, {Status: Warn}}, Warn, 2},
		{"fail wins over warn", []Check{{Status: Warn}, {Status: Fail}}, Fail, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Overall(tt.checks)
			if got != tt.want {
				t.Errorf("Overall = %v, want %v", got, tt.want)
			}
			if got.ExitCode() != tt.code {
				t.Errorf("ExitCode = %d, want %d", got.ExitCode(), tt.code)
			}
		})
	}
}
