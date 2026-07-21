// Package domain renders the libvirt domain XML and assembles the `orthogonals vm` step list.
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

	// MinRAMGiB is the minimum guest RAM in GiB.
	MinRAMGiB = 8
	// HostReserveRAMGiB is the RAM default sizing leaves the host.
	HostReserveRAMGiB  = 8
	MinVCPUs           = 4
	DefaultDiskSizeGiB = 100
	// DefaultWidth and DefaultHeight are the default maximum guest resolution.
	DefaultWidth  = 3840
	DefaultHeight = 2160
	// MaxDimension is the maximum per-axis resolution.
	MaxDimension = 16384

	// ivshmemOverhead is the Looking Glass header size added to the two frames.
	ivshmemOverhead = 10 * mib

	// WideAddressWidthBits is the IOMMU address-width threshold for the maxphysaddr fix.
	WideAddressWidthBits = 40
)

// DefaultGuestRAMGiB is the default guest RAM in GiB for a host of hostBytes.
func DefaultGuestRAMGiB(hostBytes uint64) int {
	memGiB := int((hostBytes + gib - 1) / gib)
	return memGiB - HostReserveRAMGiB
}

// Options are the user-tunable domain knobs; zero values pick host-derived defaults.
type Options struct {
	VMName        string
	RAMGiB        int
	DiskPath      string
	DiskSizeGiB   int
	Width, Height int
	Win11ISO      string
	VirtioISO     string
	ProvisionISO  string
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
	Name            string
	RAMMiB          uint64
	VCPUs           int
	Cores           int
	ThreadsPerCore  int
	VCPUPins        []Pin
	EmulatorPin     string
	IOThreadPin     string
	MaxPhysAddrBits int
	IVSHMEMMiB      uint64
	Width, Height   int
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
	VideoNone       bool
	UUID            string
}

// NewProfile derives the domain profile from a detect result.
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

	if p.GPU, err = parseBDF(nvidia.Address); err != nil {
		return Profile{}, err
	}
	if nvidia.Audio != nil {
		b, err := parseBDF(nvidia.Audio.Address)
		if err != nil {
			return Profile{}, err
		}
		p.Audio = &b
	}
	return p, nil
}

// AssignableVCPUs is how many P-core threads reserve assigns to the guest.
func AssignableVCPUs(c hw.CPU) int {
	vcpu, _, _, _, err := reserve(c)
	if err != nil {
		return 0
	}
	return len(vcpu)
}

// pinning is reserve plus the MinVCPUs floor.
func pinning(c hw.CPU) (vcpu, emu, iot []int, tpc int, err error) {
	vcpu, emu, iot, tpc, err = reserve(c)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	if len(vcpu) < MinVCPUs {
		return nil, nil, nil, 0, fmt.Errorf("%d assignable vCPUs is below the minimum of %d", len(vcpu), MinVCPUs)
	}
	if len(vcpu)%tpc != 0 {
		tpc = 1
	}
	return vcpu, emu, iot, tpc, nil
}

// reserve keeps the first physical P-core for the host and assigns the rest.
// TODO(refactor): assumes sibling threads are adjacent in the kernel cpulists.
func reserve(c hw.CPU) (vcpu, emu, iot []int, tpc int, err error) {
	phys := c.Cores - len(c.ECores)
	if phys <= 0 || len(c.PCores) < phys {
		return nil, nil, nil, 0, fmt.Errorf("cannot derive CPU topology (%d cores, %d threads)", c.Cores, c.Threads)
	}
	tpc = len(c.PCores) / phys
	vcpu = c.PCores[tpc:]
	switch {
	case len(c.ECores) >= 2:
		half := len(c.ECores) / 2
		emu, iot = c.ECores[:half], c.ECores[half:]
	case len(c.ECores) == 1:
		emu, iot = c.ECores, c.ECores
	default:
		emu, iot = c.PCores[:tpc], c.PCores[:tpc]
	}
	return vcpu, emu, iot, tpc, nil
}

// IVSHMEMMiB sizes the Looking Glass frame buffer in MiB for a w×h maximum.
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

// xmlPath is where apply writes the domain XML.
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

// XMLEscape makes s safe as XML element text.
func XMLEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// GuestConfig is the per-VM guest provisioning config carried in the domain XML metadata.
type GuestConfig struct {
	User       string `xml:"metadata>guest>user"`
	Password   string `xml:"metadata>guest>password"`
	Locale     string `xml:"metadata>guest>locale"`
	Resolution string `xml:"metadata>guest>resolution"`
	Win11ISO   string `xml:"metadata>guest>win11-iso"`
}

// ReadGuestConfig loads the guest config from the VM's registry XML under root.
func ReadGuestConfig(root, name string) GuestConfig {
	var g GuestConfig
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return g
	}
	_ = xml.Unmarshal(b, &g)
	return g
}

// DomainXMLID and the other ID funcs return journal step IDs for a VM's domain steps.
func DomainXMLID(vm string) string      { return "vm-domain-xml-" + vm }
func DiskImageID(vm string) string      { return "vm-disk-image-" + vm }
func DiskFcontextID(vm string) string   { return "vm-disk-fcontext-" + vm }
func DiskRestoreconID(vm string) string { return "vm-disk-restorecon-" + vm }
func DefineStepID(vm string) string     { return "vm-define-" + vm }

// Stage is the domain's position in the install pipeline.
type Stage string

const (
	// StageInstall is the install stage: emulated display + installer cdroms.
	StageInstall Stage = "install"
	// StageNoVideo is the post-provisioning stage: no emulated display.
	StageNoVideo Stage = "novideo"
	// StageFinal is the verified stage: installer cdroms removed.
	StageFinal Stage = "final"
)

// Stages lists the stages in pipeline order.
var Stages = []Stage{StageInstall, StageNoVideo, StageFinal}

// CurrentStage reads the domain's stage back from its registry XML under root.
func CurrentStage(root, name string) Stage {
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return StageInstall
	}
	xml := string(b)
	switch {
	case !strings.Contains(xml, "<model type='none'/>"):
		return StageInstall
	case strings.Contains(xml, "device='cdrom'"):
		return StageNoVideo
	default:
		return StageFinal
	}
}

// ApplyStage folds a pipeline stage into the profile.
func (p *Profile) ApplyStage(s Stage) {
	p.VideoNone = s != StageInstall
	if s == StageFinal {
		p.Win11ISO, p.VirtioISO, p.ProvisionISO = "", "", ""
	}
}

// JournaledDisk reports the disk image path and size from the vm's journaled create-volume op.
func JournaledDisk(m *steps.Manifest, vm string) (string, int, bool) {
	args := m.OpArgs(DiskImageID(vm))
	size, err := strconv.Atoi(args["size-gib"])
	if args["path"] == "" || err != nil {
		return "", 0, false
	}
	return args["path"], size, true
}

// Steps assembles the `vm define` step list: domain XML, disk image, SELinux label, define.
func Steps(p Profile) ([]steps.Step, error) {
	xml, err := render(p)
	if err != nil {
		return nil, err
	}
	return []steps.Step{
		{
			ID: DomainXMLID(p.Name), Kind: steps.KindWriteFile,
			Path: xmlPath(p.Name), Content: xml, Mode: 0o600,
		},
		{
			ID: DiskImageID(p.Name), Kind: steps.KindOp, Data: true,
			Op:       steps.OpCreateVolume,
			Args:     map[string]string{"path": p.DiskPath, "size-gib": strconv.Itoa(p.DiskSizeGiB)},
			UndoOp:   steps.OpRemoveFile,
			UndoArgs: map[string]string{"path": p.DiskPath},
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
			ID: DefineStepID(p.Name), Kind: steps.KindOp,
			Op:       steps.OpDefineDomain,
			Args:     map[string]string{"name": p.Name, "xml": xmlPath(p.Name)},
			Input:    xml,
			UndoOp:   steps.OpUndefineDomain,
			UndoArgs: map[string]string{"name": p.Name},
		},
	}, nil
}
