// Package domain renders the libvirt domain XML for the Windows guest from
// detect results and assembles the `orthogonals vm` step list: domain XML,
// qcow2 disk creation with SELinux labeling, and virsh define — all through
// the journaled apply engine. The domain is ported from the working PoC: Q35,
// OVMF Secure Boot, emulated TPM 2.0, video none + SPICE, VirtIO disk with
// iothread, VirtIO net, managed='no' hostdevs, Hyper-V enlightenments, HPET
// off, hypervclock on, IVSHMEM for Looking Glass.
package domain

import (
	"bytes"
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

//go:embed templates
var templateFS embed.FS

const (
	mib = 1 << 20
	gib = 1 << 30

	// Sizing minima and defaults from the plan's Defaults table. Exported
	// because preflight gates on the same values — a host that passes
	// preflight must never fail these limits at `vm define`.
	MinRAMGiB          = 8
	MaxDefaultRAMGiB   = 16
	MinVCPUs           = 4
	DefaultDiskSizeGiB = 100
	// The default buffer maximum is 4K: per Looking Glass docs, oversizing
	// costs only the reserved RAM (128 MiB while the VM runs), and it lets
	// the guest switch between every common resolution without a re-define.
	DefaultWidth  = 3840
	DefaultHeight = 2160
	// MaxDimension bounds --width/--height at the DisplayPort/HDMI per-axis
	// ceiling; it also keeps the IVSHMEM power-of-two sizing in uint64 range.
	MaxDimension = 16384

	// ivshmemOverhead covers the Looking Glass/KVMFR headers on top of the
	// two raw frames.
	ivshmemOverhead = 10 * mib

	// IOMMU address widths below this need the maxphysaddr + fw_cfg fix
	// (see Profile.MaxPhysAddrBits); preflight's address-width check warns
	// on the same threshold.
	WideAddressWidthBits = 40
)

// DefaultGuestRAMGiB is the guest RAM a host gets by default: half the
// installed RAM, capped at MaxDefaultRAMGiB. MemTotal excludes firmware and
// kernel reservations (~15.5 GiB on a 16 GiB host), so it rounds up to the
// installed size first or the documented minimum host would default below
// the MinRAMGiB guest minimum.
func DefaultGuestRAMGiB(hostBytes uint64) int {
	memGiB := (hostBytes + gib - 1) / gib
	return int(min(memGiB/2, MaxDefaultRAMGiB))
}

// Options are the user-tunable knobs from the Defaults table; zero values
// pick defaults derived from the host.
type Options struct {
	VMName        string
	RAMGiB        int
	DiskPath      string
	DiskSizeGiB   int
	Width, Height int // maximum guest resolution, sizes the IVSHMEM frame buffer
	// Installer media, attached as SATA CD-ROMs (startupPolicy=optional, so
	// the VM still starts after the ISOs are deleted post-install).
	Win11ISO     string // user-supplied Windows 11 installation ISO
	VirtioISO    string // cached virtio-win ISO (Setup storage driver + guest tools)
	ProvisionISO string // media-built autounattend/payload ISO
	// Guest provisioning settings, carried in the domain XML's <metadata>
	// block so media rebuilds read back what define applied. Empty values
	// stay empty: media applies its defaults at read time.
	GuestUser     string
	GuestPassword string
	Locale        string
}

// BDF is a PCI address split into the hostdev XML address fields.
type BDF struct{ Domain, Bus, Slot, Function string }

// Pin maps one vCPU to one host CPU.
type Pin struct{ VCPU, CPU int }

// Profile is the fully computed domain description the template renders.
type Profile struct {
	Name           string
	RAMMiB         uint64
	VCPUs          int
	Cores          int
	ThreadsPerCore int
	VCPUPins       []Pin
	EmulatorPin    string
	IOThreadPin    string
	// MaxPhysAddrBits is set when the host IOMMU address width is under 40
	// bits: the guest gets <maxphysaddr> AND the OVMF X-PciMmio64Mb fw_cfg
	// knob (PoC: maxphysaddr alone is ignored by OVMF; fw_cfg is the working
	// fix, maxphysaddr is defense-in-depth). 0 = wide host, neither injected.
	MaxPhysAddrBits int
	IVSHMEMMiB      uint64
	Width, Height   int // maximum guest resolution, recorded in <metadata>
	DiskPath        string
	DiskSizeGiB     int
	Win11ISO        string
	VirtioISO       string
	ProvisionISO    string
	GuestUser       string
	GuestPassword   string
	Locale          string
	GPU             BDF
	Audio           *BDF
}

// NewProfile derives the domain profile from a detect result, validating the
// options against the host (RAM and vCPU minimums from the Defaults table).
func NewProfile(r *hw.Result, o Options) (Profile, error) {
	nvidia, err := r.GPUs.SoleNVIDIA()
	if err != nil {
		return Profile{}, err
	}
	if r.Platform.IOMMUAddressWidth == 0 {
		return Profile{}, errors.New("IOMMU is off or unsupported (run orthogonals preflight)")
	}
	name := o.VMName
	if name == "" {
		name = steps.DefaultVMName
	}
	if err := steps.CheckVMName(name); err != nil {
		return Profile{}, err
	}
	p := Profile{
		Name: name, DiskPath: o.DiskPath, DiskSizeGiB: o.DiskSizeGiB,
		Win11ISO: o.Win11ISO, VirtioISO: o.VirtioISO, ProvisionISO: o.ProvisionISO,
		GuestUser: o.GuestUser, GuestPassword: o.GuestPassword, Locale: o.Locale,
	}
	if p.DiskSizeGiB == 0 {
		p.DiskSizeGiB = DefaultDiskSizeGiB
	}
	if p.DiskSizeGiB < 0 {
		return Profile{}, fmt.Errorf("bad disk size %d GiB", p.DiskSizeGiB)
	}
	if p.DiskPath == "" {
		p.DiskPath = "/var/lib/libvirt/images/" + name + ".qcow2"
	}
	// these land verbatim in XML attributes and root-run argv
	for _, path := range []string{p.DiskPath, p.Win11ISO, p.VirtioISO, p.ProvisionISO} {
		if strings.ContainsAny(path, `<>&'"`) {
			return Profile{}, fmt.Errorf("path %q contains characters unsupported in libvirt XML", path)
		}
	}

	ramGiB := o.RAMGiB
	if ramGiB == 0 {
		ramGiB = DefaultGuestRAMGiB(r.Platform.MemTotalBytes)
	}
	if ramGiB < MinRAMGiB {
		return Profile{}, fmt.Errorf("guest RAM %d GiB is below the 8 GiB minimum (host has %.1f GiB)",
			ramGiB, float64(r.Platform.MemTotalBytes)/gib)
	}
	if r.Platform.MemTotalBytes > 0 && uint64(ramGiB)*gib >= r.Platform.MemTotalBytes {
		return Profile{}, fmt.Errorf("guest RAM %d GiB does not fit in host RAM %.1f GiB",
			ramGiB, float64(r.Platform.MemTotalBytes)/gib)
	}
	p.RAMMiB = uint64(ramGiB) * 1024

	vcpu, emu, iot, tpc, err := pinning(r.CPU)
	if err != nil {
		return Profile{}, err
	}
	p.VCPUs = len(vcpu)
	p.ThreadsPerCore = tpc
	p.Cores = len(vcpu) / tpc
	for i, c := range vcpu {
		p.VCPUPins = append(p.VCPUPins, Pin{VCPU: i, CPU: c})
	}
	p.EmulatorPin = cpuset(emu)
	p.IOThreadPin = cpuset(iot)

	w, h := o.Width, o.Height
	if w == 0 && h == 0 {
		w, h = DefaultWidth, DefaultHeight
	}
	if w <= 0 || h <= 0 {
		return Profile{}, fmt.Errorf("bad resolution %dx%d", w, h)
	}
	if w > MaxDimension || h > MaxDimension {
		return Profile{}, fmt.Errorf("resolution %dx%d exceeds the %d-pixel per-axis maximum", w, h, MaxDimension)
	}
	p.Width, p.Height = w, h
	p.IVSHMEMMiB = IVSHMEMMiB(w, h)

	if aw := r.Platform.IOMMUAddressWidth; aw < WideAddressWidthBits {
		p.MaxPhysAddrBits = aw
	}

	gpu := nvidia
	if p.GPU, err = parseBDF(gpu.Address); err != nil {
		return Profile{}, err
	}
	if gpu.Audio != nil {
		b, err := parseBDF(gpu.Audio.Address)
		if err != nil {
			return Profile{}, err
		}
		p.Audio = &b
	}
	return p, nil
}

// AssignableVCPUs is how many P-core threads remain for the guest after
// reserve's host/emulator reservations (0 when the topology is unusable).
// Preflight gates on it, so a passing host can never fail pinning's minimum.
func AssignableVCPUs(c hw.CPU) int {
	vcpu, _, _, _, err := reserve(c)
	if err != nil {
		return 0
	}
	return len(vcpu)
}

// pinning is reserve plus the MinVCPUs floor the domain refuses to go under.
func pinning(c hw.CPU) (vcpu, emu, iot []int, tpc int, err error) {
	vcpu, emu, iot, tpc, err = reserve(c)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if len(vcpu) < MinVCPUs {
		return nil, nil, nil, 0, fmt.Errorf("%d assignable vCPUs is below the minimum of %d", len(vcpu), MinVCPUs)
	}
	if len(vcpu)%tpc != 0 { // irregular topology: fall back to flat cores
		tpc = 1
	}
	return vcpu, emu, iot, tpc, nil
}

// reserve keeps physical core 0 for the host, gives every remaining P-core
// thread to the guest, and parks emulator and iothread on the E-cores (first
// half / second half). Without E-cores the last P-core is taken back from
// the guest for emulator+iothread instead.
// TODO(refactor): assumes sibling threads are adjacent in the kernel cpulists —
// true for the Intel desktop parts v1 targets.
func reserve(c hw.CPU) (vcpu, emu, iot []int, tpc int, err error) {
	phys := c.Cores - len(c.ECores)
	if phys <= 0 || len(c.PCores) < phys {
		return nil, nil, nil, 0, fmt.Errorf("cannot derive CPU topology (%d cores, %d threads)", c.Cores, c.Threads)
	}
	tpc = len(c.PCores) / phys // >= 1: the guard above ensures len(PCores) >= phys
	vcpu = c.PCores[tpc:]
	switch {
	case len(c.ECores) >= 2:
		half := len(c.ECores) / 2
		emu, iot = c.ECores[:half], c.ECores[half:]
	case len(c.ECores) == 1:
		emu, iot = c.ECores, c.ECores
	default:
		if len(vcpu) <= tpc {
			// every remaining thread goes to the emulator: nothing assignable
			emu, iot, vcpu = vcpu, vcpu, nil
		} else {
			emu = vcpu[len(vcpu)-tpc:]
			iot = emu
			vcpu = vcpu[:len(vcpu)-tpc]
		}
	}
	return vcpu, emu, iot, tpc, nil
}

// IVSHMEMMiB sizes the Looking Glass frame buffer: two frames of W×H BGRA
// plus header overhead, rounded up to a power of two (1080p→32, 4K→128).
// Exported for media's guest-mode filter: a mode is safe to advertise to the
// guest exactly when its region size fits the one sized here.
func IVSHMEMMiB(w, h int) uint64 {
	need := uint64(w)*uint64(h)*4*2 + ivshmemOverhead
	size := uint64(1)
	for size < need {
		size <<= 1
	}
	return size / mib
}

// parseBDF splits "0000:01:00.0" into the hostdev address fields.
func parseBDF(addr string) (BDF, error) {
	rest, fn, ok := strings.Cut(addr, ".")
	parts := strings.Split(rest, ":")
	if !ok || len(parts) != 3 {
		return BDF{}, fmt.Errorf("bad PCI address %q", addr)
	}
	for _, s := range append(parts, fn) {
		if _, err := strconv.ParseUint(s, 16, 32); err != nil {
			return BDF{}, fmt.Errorf("bad PCI address %q", addr)
		}
	}
	return BDF{Domain: parts[0], Bus: parts[1], Slot: parts[2], Function: fn}, nil
}

func cpuset(cpus []int) string {
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = strconv.Itoa(c)
	}
	return strings.Join(s, ",")
}

// xmlPath is where apply writes the domain XML that virsh define reads. The
// file doubles as the VM's registry entry: its presence is what the qemu hook
// dispatcher gates on, so a defined domain is always advertised to the hook.
func xmlPath(name string) string { return steps.VMsDirPath + "/" + name + ".xml" }

// render produces the domain XML for the profile.
func render(p Profile) ([]byte, error) {
	tpl, err := template.New("domain.xml").Funcs(template.FuncMap{"xml": XMLEscape}).
		ParseFS(templateFS, "templates/domain.xml")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render domain XML: %w", err)
	}
	return buf.Bytes(), nil
}

// XMLEscape makes s safe as XML element text; media's templates share it.
func XMLEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s)) // cannot fail on a bytes.Buffer
	return b.String()
}

// GuestConfig is the per-VM guest provisioning config the domain XML carries
// in its <metadata> block — `vm define` writes it, media reads it back so
// rebuilds keep the values the VM was defined with.
type GuestConfig struct {
	User       string `xml:"metadata>guest>user"`
	Password   string `xml:"metadata>guest>password"`
	Locale     string `xml:"metadata>guest>locale"`
	Resolution string `xml:"metadata>guest>resolution"`
}

// ReadGuestConfig loads the metadata block from the VM's registry XML under
// root. Fail-open: an undefined VM (or a pre-metadata XML) reads as empty,
// and the caller falls back to its defaults.
func ReadGuestConfig(root, name string) GuestConfig {
	var g GuestConfig
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return g
	}
	_ = xml.Unmarshal(b, &g)
	return g
}

// Journal step IDs for one VM's domain steps, in apply order. cli's
// undefine ordering and steps guards consume the same constructors Steps
// uses, so an ID rename can never silently break undo.
func DomainXMLID(vm string) string        { return "vm-domain-xml-" + vm }
func DiskImageID(vm string) string        { return "vm-disk-image-" + vm }
func DiskFcontextID(vm string) string     { return "vm-disk-fcontext-" + vm }
func DiskRestoreconID(vm string) string   { return "vm-disk-restorecon-" + vm }
func DefineStepID(vm string) string       { return "vm-define-" + vm }
func InstallVideoStepID(vm string) string { return "vm-install-video-" + vm }

// InstallVideoStep flips the domain's install-time emulated display (defined
// in the domain template — see the comment there) to video=none for Looking
// Glass, once the install pipeline reports provisioning complete. virt-xml
// edits the persistent config; a running guest keeps its display until the
// next boot. No UndoCmd: undo removes the whole domain via the define step's
// paired virsh undefine.
func InstallVideoStep(vm string) steps.Step {
	return steps.Step{
		ID: InstallVideoStepID(vm), Kind: steps.KindRunCmd,
		Cmd: []string{"virt-xml", vm, "--edit", "--video", "clearxml=yes,model=none"},
	}
}

// Steps assembles the `vm define` step list: domain XML, disk image (a data
// step — plain undo keeps it, --purge removes it), SELinux label, define.
// IDs carry the domain name so several VMs coexist in one manifest.
func Steps(p Profile) ([]steps.Step, error) {
	xml, err := render(p)
	if err != nil {
		return nil, err
	}
	return []steps.Step{
		{
			ID: DomainXMLID(p.Name), Kind: steps.KindWriteFile,
			// 0600: the <metadata> block carries the guest password
			Path: xmlPath(p.Name), Content: xml, Mode: 0o600,
		},
		{
			ID: DiskImageID(p.Name), Kind: steps.KindRunCmd, Data: true,
			Cmd:     []string{"qemu-img", "create", "-f", "qcow2", p.DiskPath, fmt.Sprintf("%dG", p.DiskSizeGiB)},
			UndoCmd: []string{"rm", "-f", p.DiskPath},
		},
		{
			ID: DiskFcontextID(p.Name), Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "virt_image_t", p.DiskPath},
			UndoCmd: []string{"semanage", "fcontext", "-d", p.DiskPath},
		},
		{
			ID: DiskRestoreconID(p.Name), Kind: steps.KindRunCmd,
			Cmd: []string{"restorecon", p.DiskPath},
		},
		{
			ID: DefineStepID(p.Name), Kind: steps.KindRunCmd,
			Cmd:     []string{"virsh", "define", xmlPath(p.Name)},
			UndoCmd: []string{"virsh", "undefine", p.Name, "--nvram", "--tpm"},
		},
	}, nil
}
