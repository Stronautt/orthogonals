// Package preflight analyzes detect results and gates apply: hard fails for
// hosts orthogonals cannot support in v1, warns for issues apply fixes
// automatically or the user must be aware of.
package preflight

import (
	"fmt"
	"strings"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw"
)

// Status is a check outcome; Overall reduces a check list to the worst one.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
)

// ExitCode maps a status to the preflight CLI contract: 0 pass, 2 warn, 1 fail.
func (s Status) ExitCode() int {
	switch s {
	case Fail:
		return 1
	case Warn:
		return 2
	default:
		return 0
	}
}

// Check is one analyzer verdict.
type Check struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}

// cacheBytes is the ISO cache + provision payloads headroom on top of the
// default disk image; the sizing gates themselves live in internal/domain.
const cacheBytes = 15 << 30

// hardTools missing => fail (apply cannot run); other RequiredTools warn.
var hardTools = map[string]bool{
	"dracut": true, "grubby": true, "virsh": true, "qemu-img": true, "xorriso": true,
}

// laptopChassis are SMBIOS chassis types refused in v1 (MUX/power handling
// differs per model).
var laptopChassis = map[int]bool{
	8: true, 9: true, 10: true, 11: true, 14: true, 30: true, 31: true, 32: true,
}

// Analyze runs every analyzer over the detect result and gathered facts.
func Analyze(r *hw.Result, f Facts) []Check {
	return []Check{
		checkIOMMU(r),
		checkGPUTopology(r),
		checkDisplayTopology(r),
		checkBootVGA(r),
		checkDuplicateGPUIDs(r),
		checkIOMMUGroup(r),
		checkGPUReset(r),
		checkForeignVFIO(f),
		checkChassis(r),
		checkTools(r),
		checkCPU(r),
		checkMemory(r),
		checkAddressWidth(r),
		checkSecureBoot(r),
		checkPersistenced(f),
		checkSwitcheroo(f),
		checkDefaultNet(f),
		checkDiskSpace(f),
	}
}

// Overall reduces checks to the worst status.
func Overall(checks []Check) Status {
	out := Pass
	for _, c := range checks {
		switch c.Status {
		case Fail:
			return Fail
		case Warn:
			out = Warn
		}
	}
	return out
}

func checkIOMMU(r *hw.Result) Check {
	switch {
	case r.Platform.IOMMUAddressWidth > 0:
		return Check{"iommu", Pass, fmt.Sprintf("IOMMU active, host address width %d bits", r.Platform.IOMMUAddressWidth), ""}
	case r.Platform.DMARTable:
		// virgin host: VT-d exposed by the firmware, kernel args not yet
		// installed. Must not FAIL — apply's kernel-args step is the fix,
		// and a FAIL here would block apply from ever running.
		return Check{"iommu", Warn, "IOMMU is not active, but the firmware exposes VT-d (ACPI DMAR table present)",
			"no action needed — apply adds intel_iommu=on iommu=pt (reboot required); re-run preflight after that reboot to validate the GPU IOMMU group"}
	default:
		return Check{"iommu", Fail, "IOMMU is off or unsupported — passthrough is impossible without it",
			"enable VT-d (Intel Virtualization Technology for Directed I/O) in the BIOS/UEFI setup"}
	}
}

func checkGPUTopology(r *hw.Result) Check {
	const name = "gpu-topology"
	nvidia := r.GPUs.NVIDIA()
	var amd []hw.DGPU
	for _, d := range r.GPUs.DGPUs {
		if d.Vendor == hw.VendorAMD {
			amd = append(amd, d)
		}
	}
	switch {
	case r.GPUs.IGPU == nil && len(r.GPUs.DGPUs) == 1:
		return Check{name, Fail, "single-GPU host: the only GPU cannot both drive the desktop and be passed through",
			"v1 requires an Intel iGPU for the desktop plus an NVIDIA dGPU for the guest"}
	case r.GPUs.IGPU == nil:
		return Check{name, Fail, "no Intel iGPU found to drive the host desktop",
			"enable the iGPU in the BIOS (often \"iGPU Multi-Monitor\") and connect a display to it"}
	case len(amd) > 0:
		return Check{name, Fail, fmt.Sprintf("AMD dGPU %s is unsupported in v1 (reset quirks need vendor-reset gating)", amd[0].Address),
			"v1 supports NVIDIA dGPUs only; AMD support is on the roadmap"}
	case len(nvidia) == 0:
		return Check{name, Fail, "no NVIDIA dGPU found to pass through", "v1 requires an NVIDIA dGPU"}
	case len(nvidia) > 1:
		return Check{name, Fail, fmt.Sprintf("found %d NVIDIA dGPUs; v1 supports exactly one NVIDIA dGPU", len(nvidia)),
			"remove or ignore the extra GPU (multi-dGPU selection is on the roadmap)"}
	default:
		return Check{name, Pass, fmt.Sprintf("Intel iGPU %s + NVIDIA dGPU %s", r.GPUs.IGPU.Address, nvidia[0].Address), ""}
	}
}

func vendorName(vendor string) string {
	switch vendor {
	case hw.VendorIntel:
		return "Intel"
	case hw.VendorNVIDIA:
		return "NVIDIA"
	case hw.VendorAMD:
		return "AMD"
	}
	return vendor
}

// checkDisplayTopology verifies the one physical requirement software cannot
// remove: every monitor must be cabled to the iGPU, because a vfio-bound
// dGPU cannot scan out the host desktop. A GPU without a DRM card reports
// nothing about its connectors (vfio-bound or driverless), so absence of
// evidence never Fails — only a display positively seen on a dGPU while the
// iGPU has none does.
func checkDisplayTopology(r *hw.Result) Check {
	const name = "display-topology"
	if r.GPUs.IGPU == nil {
		return Check{name, Pass, "skipped (no Intel iGPU — see gpu-topology)", ""}
	}
	igpu := r.GPUs.IGPU
	if igpu.DRMCard == "" {
		return Check{name, Warn, "cannot verify display cabling: the iGPU exposes no DRM card (Intel graphics driver not loaded?)",
			"make sure every monitor is plugged into the motherboard video outputs, not the graphics card, then re-run preflight"}
	}
	var dgpuConnected []string
	for _, d := range r.GPUs.DGPUs {
		if len(d.Connectors) > 0 {
			dgpuConnected = append(dgpuConnected,
				fmt.Sprintf("%s dGPU %s (%s)", vendorName(d.Vendor), d.Address, strings.Join(d.Connectors, ", ")))
		}
	}
	switch {
	case len(igpu.Connectors) > 0 && len(dgpuConnected) == 0:
		return Check{name, Pass, fmt.Sprintf("all connected displays are on the iGPU (%s)", strings.Join(igpu.Connectors, ", ")), ""}
	case len(igpu.Connectors) > 0:
		return Check{name, Warn, fmt.Sprintf("a display is also connected to the %s — after apply the desktop session ignores the dGPU, so that monitor stays dark",
			strings.Join(dgpuConnected, " and ")),
			"move that cable to a motherboard video output"}
	case len(dgpuConnected) > 0:
		return Check{name, Fail, fmt.Sprintf("no display is connected to the iGPU; your monitor(s) are on the %s", strings.Join(dgpuConnected, " and ")),
			"shut down and move the monitor cable(s) from the listed connector(s) to the motherboard video outputs — the desktop looks and behaves the same afterwards, and GPU apps still run on the NVIDIA card while no VM is running"}
	default:
		return Check{name, Warn, "no connected display found on any GPU",
			"if a monitor is plugged into the graphics card, move it to the motherboard video outputs (a vfio-bound or non-KMS dGPU cannot report its connectors); headless hosts can ignore this"}
	}
}

// checkBootVGA reports which GPU the firmware lit as primary. A dGPU that is
// boot_vga still works — the udev mutter-ignore rule keeps the session on
// the iGPU — but GRUB and early boot render on the dGPU's outputs, which is
// invisible once the monitors are on the motherboard. That matters for the
// GRUB escape-hatch recovery flow, so it warns instead of passing silently.
func checkBootVGA(r *hw.Result) Check {
	const name = "boot-vga"
	if r.GPUs.IGPU != nil && r.GPUs.IGPU.BootVGA {
		return Check{name, Pass, "the firmware primary GPU (boot_vga) is the Intel iGPU", ""}
	}
	for _, d := range r.GPUs.DGPUs {
		if d.BootVGA {
			return Check{name, Warn, fmt.Sprintf("the firmware primary GPU is the dGPU at %s — the desktop still runs on the iGPU, but GRUB and early boot output render on the dGPU's outputs, invisible once your monitors are on the motherboard", d.Address),
				"optional: set the BIOS primary display to the CPU/integrated graphics (ASUS: Advanced > System Agent > Graphics Configuration > \"Primary Display: CPU Graphics\") so the boot menu stays visible — this matters if you ever need the GRUB recovery steps"}
		}
	}
	return Check{name, Pass, "skipped (no boot_vga marker in sysfs)", ""}
}

// checkIOMMUGroup enforces the whole-group rule: everything in the dGPU's
// IOMMU group must go to the guest, so only the GPU, its audio function, and
// PCIe bridges/root ports (class 0x0604, never bound to vfio) may share it.
func checkIOMMUGroup(r *hw.Result) Check {
	const name = "iommu-group"
	nvidia := r.GPUs.NVIDIA()
	if len(nvidia) != 1 || nvidia[0].IOMMUGroup < 0 {
		return Check{name, Pass, "skipped (needs exactly one NVIDIA dGPU in an IOMMU group)", ""}
	}
	gpu := nvidia[0]
	var strangers []string
	for _, d := range r.Devices {
		if d.IOMMUGroup != gpu.IOMMUGroup || d.Address == gpu.Address {
			continue
		}
		if gpu.Audio != nil && d.Address == gpu.Audio.Address {
			continue
		}
		if strings.HasPrefix(d.Class, hw.ClassBridge) {
			continue
		}
		strangers = append(strangers, fmt.Sprintf("%s (class %s)", d.Address, d.Class))
	}
	if len(strangers) == 0 {
		return Check{name, Pass, fmt.Sprintf("IOMMU group %d contains only the GPU, its audio function, and bridges", gpu.IOMMUGroup), ""}
	}
	return Check{name, Fail,
		fmt.Sprintf("dGPU IOMMU group %d also contains %s — the whole group must be handed to the guest",
			gpu.IOMMUGroup, strings.Join(strangers, ", ")),
		"orthogonals never enables the ACS override patch: it fakes isolation and lets the guest DMA into those devices. " +
			"Instead: move the GPU to a CPU-attached PCIe slot, look for a BIOS update, or try a newer kernel with better ACS support"}
}

// checkDuplicateGPUIDs refuses identical NVIDIA GPUs: static binding matches
// by vendor:device (`vfio-pci.ids=`), so twins cannot be told apart and both
// would be grabbed (research §D5; address-based binding is post-v1).
func checkDuplicateGPUIDs(r *hw.Result) Check {
	const name = "duplicate-gpu-ids"
	byID := map[string][]string{}
	for _, d := range r.Devices {
		if d.Vendor == hw.VendorNVIDIA && strings.HasPrefix(d.Class, hw.ClassDisplay) {
			id := d.VendorDeviceID()
			byID[id] = append(byID[id], d.Address)
		}
	}
	for id, addrs := range byID {
		if len(addrs) > 1 {
			return Check{name, Fail,
				fmt.Sprintf("NVIDIA GPUs %s share vendor:device %s — static vfio-pci.ids binding cannot tell them apart",
					strings.Join(addrs, " and "), id),
				"remove one of the identical GPUs; address-based binding is on the post-v1 roadmap"}
		}
	}
	return Check{name, Pass, "no duplicate NVIDIA vendor:device IDs", ""}
}

// checkForeignVFIO refuses vfio configuration orthogonals did not write —
// mixing foreign modprobe/dracut/karg binding with orthogonals' own leads to
// unpredictable driver binding. A present manifest means orthogonals owns
// (has adopted) the config, so re-runs over an applied host stay clean.
func checkForeignVFIO(f Facts) Check {
	const name = "foreign-vfio"
	switch {
	case f.OrthogonalsManaged:
		return Check{name, Pass, "existing vfio configuration is orthogonals-managed (manifest present)", ""}
	case len(f.ForeignVFIO) > 0:
		return Check{name, Fail,
			"pre-existing vfio configuration found: " + strings.Join(f.ForeignVFIO, "; "),
			"remove or let orthogonals adopt it: delete these entries and reboot so apply starts from a clean slate; " +
				"if they are leftovers from an orthogonals install whose /var/lib/orthogonals state was deleted, " +
				"restore that state (or remove the leftovers) before re-running apply"}
	default:
		return Check{name, Pass, "no pre-existing vfio configuration", ""}
	}
}

func checkGPUReset(r *hw.Result) Check {
	const name = "gpu-reset"
	nvidia := r.GPUs.NVIDIA()
	if len(nvidia) != 1 {
		return Check{name, Pass, "skipped (needs exactly one NVIDIA dGPU)", ""}
	}
	if !nvidia[0].HasReset {
		return Check{name, Fail,
			fmt.Sprintf("dGPU %s has no sysfs reset file — the device cannot be reset between host and guest", nvidia[0].Address),
			"check for a BIOS update; a GPU without function-level reset cannot be passed through reliably"}
	}
	return Check{name, Pass, "dGPU exposes a sysfs reset file", ""}
}

func checkChassis(r *hw.Result) Check {
	const name = "chassis"
	if laptopChassis[r.Platform.ChassisType] {
		return Check{name, Fail,
			fmt.Sprintf("laptop/portable chassis (%s) is unsupported in v1", hw.ChassisName(r.Platform.ChassisType)),
			"laptop hybrid graphics (MUX switches, power gating) differ per model; v1 targets desktop machines"}
	}
	return Check{name, Pass, fmt.Sprintf("chassis: %s", hw.ChassisName(r.Platform.ChassisType)), ""}
}

func checkTools(r *hw.Result) Check {
	const name = "tools"
	var missingHard, missingSoft []string
	for _, tool := range hw.RequiredTools {
		if r.Platform.Tools[tool] {
			continue
		}
		if hardTools[tool] {
			missingHard = append(missingHard, tool)
		} else {
			missingSoft = append(missingSoft, tool)
		}
	}
	switch {
	case len(missingHard) > 0:
		return Check{name, Fail, "required binaries missing: " + strings.Join(append(missingHard, missingSoft...), ", "),
			"install them (dnf install libvirt qemu-kvm xorriso grubby dracut) and re-run preflight"}
	case len(missingSoft) > 0:
		return Check{name, Warn, "binaries missing (needed by later stages): " + strings.Join(missingSoft, ", "),
			"install them before running apply/up"}
	default:
		return Check{name, Pass, "all required host binaries present", ""}
	}
}

// checkCPU gates on the vCPU count domain's pinning will actually assign, so
// a host that passes here can never hard-fail later at `vm define`.
func checkCPU(r *hw.Result) Check {
	const name = "cpu"
	assignable := domain.AssignableVCPUs(r.CPU)
	if assignable < domain.MinVCPUs {
		return Check{name, Fail,
			fmt.Sprintf("only %d assignable vCPU threads (P-core threads minus reserved host/emulator cores); need at least %d", assignable, domain.MinVCPUs),
			"v1 needs enough performance cores for 4 vCPU threads after reserving host/emulator cores"}
	}
	return Check{name, Pass, fmt.Sprintf("%d assignable vCPU threads", assignable), ""}
}

func checkMemory(r *hw.Result) Check {
	const name = "memory"
	assignable := domain.DefaultGuestRAMGiB(r.Platform.MemTotalBytes)
	if assignable < domain.MinRAMGiB {
		return Check{name, Fail,
			fmt.Sprintf("host has %.1f GiB RAM; the guest gets all but %d GiB = %d GiB but needs at least %d GiB",
				gib(r.Platform.MemTotalBytes), domain.HostReserveRAMGiB, assignable, domain.MinRAMGiB),
			"Windows 11 needs 8 GiB minimum; v1 requires a 16 GiB host"}
	}
	return Check{name, Pass, fmt.Sprintf("%.1f GiB host RAM, %d GiB assignable", gib(r.Platform.MemTotalBytes), assignable), ""}
}

func checkAddressWidth(r *hw.Result) Check {
	const name = "address-width"
	w := r.Platform.IOMMUAddressWidth
	switch {
	case w == 0:
		return Check{name, Pass, "skipped (IOMMU off)", ""}
	case w < domain.WideAddressWidthBits:
		return Check{name, Warn,
			fmt.Sprintf("host IOMMU address width is %d bits: 64-bit GPU BARs can map above the DMA limit and stall the guest", w),
			"no action needed — VM definition applies the maxphysaddr + OVMF PciMmio64 fix automatically"}
	default:
		return Check{name, Pass, fmt.Sprintf("host IOMMU address width %d bits", w), ""}
	}
}

func checkSecureBoot(r *hw.Result) Check {
	const name = "secure-boot"
	if r.Platform.SecureBoot && len(r.GPUs.NVIDIA()) > 0 {
		return Check{name, Warn,
			"Secure Boot is enabled: the NVIDIA kernel module must be signed or it will not load after the setup reboot",
			"if the NVIDIA driver works today you are fine (akmods MOK is enrolled); otherwise enroll it before apply"}
	}
	return Check{name, Pass, "no Secure Boot signing concern", ""}
}

func checkPersistenced(f Facts) Check {
	const name = "persistenced"
	if f.PersistencedEnabled {
		return Check{name, Warn,
			"nvidia-persistenced.service is enabled; it keeps the GPU open and blocks dynamic unbinding",
			"apply disables it (undo restores the previous state)"}
	}
	return Check{name, Pass, "nvidia-persistenced not enabled", ""}
}

// checkSwitcheroo guards the primary dGPU-launch UX (research §A): GNOME's
// "Launch using Discrete Graphics Card" menu needs switcheroo-control running
// and listing the NVIDIA GPU with its offload environment.
func checkSwitcheroo(f Facts) Check {
	const name = "switcheroo"
	switch {
	case !f.SwitcherooEnabled:
		return Check{name, Warn,
			"switcheroo-control.service is not enabled — GNOME's \"Launch using Discrete Graphics Card\" menu needs it",
			"dnf install switcheroo-control && systemctl enable --now switcheroo-control (apply does this too)"}
	case !f.SwitcherooNVIDIA:
		return Check{name, Warn,
			"switcherooctl does not list the NVIDIA GPU with an offload environment — the daemon enumerates GPUs only at startup",
			"systemctl restart switcheroo-control"}
	default:
		return Check{name, Pass, "switcheroo-control lists the NVIDIA GPU for discrete-graphics launch", ""}
	}
}

func checkDefaultNet(f Facts) Check {
	const name = "default-network"
	if !f.DefaultNetActive {
		return Check{name, Warn, "libvirt default NAT network is inactive",
			"apply activates it and marks it autostart"}
	}
	return Check{name, Pass, "libvirt default network active", ""}
}

func checkDiskSpace(f Facts) Check {
	const name = "disk-space"
	const need = uint64(domain.DefaultDiskSizeGiB<<30 + cacheBytes)
	if f.FreeDiskBytes == 0 {
		// freeDisk returns 0 only when every statfs probe failed
		return Check{name, Warn, "could not determine free disk space where the guest disk lives",
			fmt.Sprintf("ensure ~%.0f GiB is free for the disk image and ISO cache", gib(need))}
	}
	if f.FreeDiskBytes < need {
		return Check{name, Warn,
			fmt.Sprintf("%.0f GiB free where the guest disk lives; ~%.0f GiB recommended for the disk image and ISO cache",
				gib(f.FreeDiskBytes), gib(need)),
			"free up space or point --disk-path/--disk-size at a larger filesystem"}
	}
	return Check{name, Pass, fmt.Sprintf("%.0f GiB free disk space", gib(f.FreeDiskBytes)), ""}
}

func gib(b uint64) float64 { return float64(b) / (1 << 30) }
