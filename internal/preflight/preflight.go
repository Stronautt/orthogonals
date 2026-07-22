// Package preflight analyzes detect results and gates apply.
package preflight

import (
	"fmt"
	"strings"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hw"
)

// Status is a check outcome.
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

// cacheBytes is the ISO cache and provision headroom above the default disk image.
const cacheBytes = 15 << 30

// hardTools missing => fail (apply cannot run); other RequiredTools warn.
var hardTools = map[string]bool{
	"dracut": true,
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
		checkMux(r),
		checkGPUMux(r),
		checkTools(r),
		checkCPU(r),
		checkMemory(r),
		checkAddressWidth(r),
		checkSecureBoot(r),
		checkPersistenced(f),
		checkSwitcheroo(f),
		checkLibvirt(f),
		checkDefaultNet(f),
		checkDiskSpace(f),
		checkBLS(f),
	}
}

// checkBLS gates on readable Boot Loader Spec entries.
func checkBLS(f Facts) Check {
	const name = "boot entries"
	if f.BLSError != "" {
		return Check{name, Fail, f.BLSError,
			"convert to Boot Loader Spec (grub2-switch-to-blscfg) so kernel args can be managed per entry"}
	}
	return Check{name, Pass, "boot loader entries are readable", ""}
}

// checkLibvirt gates on a reachable local libvirt daemon.
func checkLibvirt(f Facts) Check {
	const name = "libvirt"
	if !f.LibvirtReachable {
		return Check{name, Warn,
			"libvirt is not reachable on its local socket — the domain and network steps will fail",
			"systemctl start virtqemud.socket (or reboot after installing the orthogonals RPM)"}
	}
	return Check{name, Pass, "libvirt answers on its local socket", ""}
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
	const name = "iommu"
	if r.Platform.IOMMUAddressWidth > 0 {
		return Check{name, Pass, fmt.Sprintf("IOMMU active, host address width %d bits", r.Platform.IOMMUAddressWidth), ""}
	}
	tech, karg, bios := iommuTech(r.CPU.Vendor)
	if r.Platform.IOMMUTable {
		return Check{name, Warn, "IOMMU is not active, but the firmware exposes " + tech,
			fmt.Sprintf("no action needed — apply adds %s (reboot required); re-run preflight after that reboot to validate the GPU IOMMU group", karg)}
	}
	if fw := firmwareIOMMUHint(r.Platform.FirmwareIOMMU); fw != "" {
		bios += "; " + fw
	}
	return Check{name, Fail, "IOMMU is off or unsupported — passthrough is impossible without it", bios}
}

// firmwareIOMMUHint names an OS-visible BIOS attribute controlling the IOMMU.
func firmwareIOMMUHint(attrs []hw.FirmwareAttr) string {
	if len(attrs) == 0 {
		return ""
	}
	a := attrs[0]
	hint := fmt.Sprintf("this BIOS exposes the setting %q (currently %q", a.Name, a.Current)
	if len(a.PossibleValues) > 0 {
		hint += ", options: " + strings.Join(a.PossibleValues, "/")
	}
	return hint + fmt.Sprintf(") — set it via /sys/class/firmware-attributes/%s/attributes/%s/current_value and reboot", a.Driver, a.Name)
}

// iommuTech names the vendor's IOMMU technology, kernel args, and BIOS remedy; unknown gets Intel.
func iommuTech(cpuVendor string) (tech, karg, bios string) {
	if cpuVendor == hw.CPUVendorAMD {
		return "AMD-Vi (ACPI IVRS table present)", "iommu=pt",
			"enable AMD-Vi / IOMMU in the BIOS/UEFI setup"
	}
	return "VT-d (ACPI DMAR table present)", "intel_iommu=on iommu=pt",
		"enable VT-d (Intel Virtualization Technology for Directed I/O) in the BIOS/UEFI setup"
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
			"v1 requires an iGPU for the desktop plus an NVIDIA dGPU for the guest"}
	case r.GPUs.IGPU == nil:
		remedy := "enable the iGPU in the BIOS (often \"iGPU Multi-Monitor\") and connect a display to it"
		if hw.IsLaptopChassis(r.Platform.ChassisType) {
			remedy = "set the BIOS/vendor-tool graphics mode to hybrid (not discrete-only) so the iGPU drives the internal panel"
		}
		return Check{name, Fail, "no iGPU found to drive the host desktop", remedy}
	case len(amd) > 0:
		return Check{name, Fail, fmt.Sprintf("AMD dGPU %s is unsupported in v1 (reset quirks need vendor-reset gating)", amd[0].Address),
			"v1 supports NVIDIA dGPUs only; AMD support is on the roadmap"}
	case len(nvidia) == 0:
		return Check{name, Fail, "no NVIDIA dGPU found to pass through", "v1 requires an NVIDIA dGPU"}
	case len(nvidia) > 1:
		return Check{name, Fail, fmt.Sprintf("found %d NVIDIA dGPUs; v1 supports exactly one NVIDIA dGPU", len(nvidia)),
			"remove or ignore the extra GPU (multi-dGPU selection is on the roadmap)"}
	default:
		return Check{name, Pass, fmt.Sprintf("%s iGPU %s + NVIDIA dGPU %s",
			vendorName(r.GPUs.IGPU.Vendor), r.GPUs.IGPU.Address, nvidia[0].Address), ""}
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

// checkDisplayTopology verifies every monitor is cabled to the iGPU.
func checkDisplayTopology(r *hw.Result) Check {
	const name = "display-topology"
	if r.GPUs.IGPU == nil {
		return Check{name, Pass, "skipped (no iGPU — see gpu-topology)", ""}
	}
	laptop := hw.IsLaptopChassis(r.Platform.ChassisType)
	igpu := r.GPUs.IGPU
	if igpu.DRMCard == "" {
		remedy := "make sure every monitor is plugged into the motherboard video outputs, not the graphics card, then re-run preflight"
		if laptop {
			remedy = "the internal panel runs on the iGPU whose graphics driver looks unloaded — check it, then re-run preflight"
		}
		return Check{name, Warn, "cannot verify display cabling: the iGPU exposes no DRM card (iGPU graphics driver not loaded?)", remedy}
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
		remedy := "move that cable to a motherboard video output"
		if laptop {
			remedy = "that external monitor is on the dGPU and goes dark while the VM runs — use the internal panel or an iGPU-driven port"
		}
		return Check{name, Warn, fmt.Sprintf("a display is also connected to the %s — after apply the desktop session ignores the dGPU, so that monitor stays dark",
			strings.Join(dgpuConnected, " and ")), remedy}
	case len(dgpuConnected) > 0:
		remedy := "shut down and move the monitor cable(s) from the listed connector(s) to the motherboard video outputs — the desktop looks and behaves the same afterwards, and GPU apps still run on the NVIDIA card while no VM is running"
		if laptop {
			remedy = "set the BIOS/vendor-tool graphics mode from discrete to hybrid so the iGPU drives the panel; an external monitor on a dGPU-only port goes dark while the VM runs"
		}
		return Check{name, Fail, fmt.Sprintf("no display is connected to the iGPU; your monitor(s) are on the %s", strings.Join(dgpuConnected, " and ")), remedy}
	default:
		return Check{name, Warn, "no connected display found on any GPU",
			"if a monitor is plugged into the graphics card, move it to the motherboard video outputs (a vfio-bound or non-KMS dGPU cannot report its connectors); headless hosts can ignore this"}
	}
}

// checkBootVGA reports which GPU the firmware lit as primary.
func checkBootVGA(r *hw.Result) Check {
	const name = "boot-vga"
	if r.GPUs.IGPU != nil && r.GPUs.IGPU.BootVGA {
		return Check{name, Pass, "the firmware primary GPU (boot_vga) is the Intel iGPU", ""}
	}
	for _, d := range r.GPUs.DGPUs {
		if d.BootVGA {
			if hw.IsLaptopChassis(r.Platform.ChassisType) {
				return Check{name, Pass, fmt.Sprintf("firmware primary GPU is the dGPU at %s — on a laptop the internal panel is the same screen, so the boot menu stays visible", d.Address), ""}
			}
			return Check{name, Warn, fmt.Sprintf("the firmware primary GPU is the dGPU at %s — the desktop still runs on the iGPU, but GRUB and early boot output render on the dGPU's outputs, invisible once your monitors are on the motherboard", d.Address),
				"optional: set the BIOS primary display to the CPU/integrated graphics (ASUS: Advanced > System Agent > Graphics Configuration > \"Primary Display: CPU Graphics\") so the boot menu stays visible — this matters if you ever need the GRUB recovery steps"}
		}
	}
	return Check{name, Pass, "skipped (no boot_vga marker in sysfs)", ""}
}

// checkIOMMUGroup enforces the whole-group rule for the dGPU's IOMMU group.
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

// checkDuplicateGPUIDs refuses identical NVIDIA GPUs.
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

// checkForeignVFIO refuses vfio configuration orthogonals did not write.
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
	if hw.IsLaptopChassis(r.Platform.ChassisType) {
		return Check{name, Pass,
			fmt.Sprintf("laptop chassis (%s): hybrid-graphics passthrough — the panel stays on the iGPU while the dGPU is handed to the guest", hw.ChassisName(r.Platform.ChassisType)),
			""}
	}
	return Check{name, Pass, fmt.Sprintf("chassis: %s", hw.ChassisName(r.Platform.ChassisType)), ""}
}

// checkGPUMux gates the ASUS display MUX: hybrid keeps the panel on the iGPU.
func checkGPUMux(r *hw.Result) Check {
	const name = "gpu-mux"
	switch r.Platform.GPUMux {
	case hw.GPUMuxHybrid:
		return Check{name, Pass, "ASUS display MUX is in hybrid/Optimus mode (the iGPU drives the panel)", ""}
	case hw.GPUMuxDiscrete:
		return Check{name, Fail,
			"ASUS display MUX is in discrete/Ultimate mode: the dGPU drives the panel and the iGPU is off, so the dGPU cannot be passed to the guest",
			"switch to hybrid — `echo 1 | sudo tee " + hw.GPUMuxPath + "` then reboot, or set the BIOS / Armoury Crate GPU mode to Optimus/Standard"}
	default:
		return Check{name, Pass, "skipped (no ASUS gpu_mux_mode knob)", ""}
	}
}

// checkMux warns when a laptop dGPU is MUXless (a 3D controller with no outputs).
func checkMux(r *hw.Result) Check {
	const name = "mux"
	if !hw.IsLaptopChassis(r.Platform.ChassisType) {
		return Check{name, Pass, "skipped (not a laptop)", ""}
	}
	nvidia := r.GPUs.NVIDIA()
	if len(nvidia) != 1 {
		return Check{name, Pass, "skipped (needs exactly one NVIDIA dGPU)", ""}
	}
	if strings.HasPrefix(nvidia[0].Class, hw.Class3DController) {
		return Check{name, Warn,
			fmt.Sprintf("MUXless laptop: the NVIDIA dGPU %s is a 3D controller with no display outputs", nvidia[0].Address),
			"if the guest shows no image once the NVIDIA driver installs, extract the dGPU vBIOS and pass it with --gpu-rom"}
	}
	return Check{name, Pass, fmt.Sprintf("MUXed laptop: the NVIDIA dGPU %s drives display outputs", nvidia[0].Address), ""}
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
			"install the orthogonals RPM (its dependencies pull them in) and re-run preflight"}
	case len(missingSoft) > 0:
		return Check{name, Warn, "binaries missing (needed by later stages): " + strings.Join(missingSoft, ", "),
			"install them before running apply/up"}
	default:
		return Check{name, Pass, "all required host binaries present", ""}
	}
}

// checkCPU gates on the vCPU count domain's pinning will assign.
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
			fmt.Sprintf("host has %.1f GiB RAM; the default guest RAM works out to %d GiB but needs at least %d GiB",
				gib(r.Platform.MemTotalBytes), assignable, domain.MinRAMGiB),
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

// checkSwitcheroo guards the discrete-graphics launch UX.
func checkSwitcheroo(f Facts) Check {
	const name = "switcheroo"
	switch {
	case !f.SwitcherooEnabled:
		return Check{name, Warn,
			"switcheroo-control.service is not enabled — GNOME's \"Launch using Discrete Graphics Card\" menu needs it",
			"systemctl enable --now switcheroo-control (apply does this too)"}
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
