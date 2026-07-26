// Package domain renders the libvirt domain XML and assembles the `orthogonals vm` step list.
package domain

import (
	"bytes"
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
)

//go:embed templates
var templateFS embed.FS

const (

	// MinRAMGiB is the minimum guest RAM in GiB.
	MinRAMGiB = 8
	// DefaultRAMNum/DefaultRAMDen size the default guest RAM as a fraction of host
	// RAM (5/8 — e.g. 20 GiB on a 32 GiB host), scaling with the host and leaving
	// the rest free so the hook's 2M hugepage pool reserves cleanly. Override with
	// --ram.
	DefaultRAMNum = 5
	DefaultRAMDen = 8
	// MinHostRAMGiB is the smallest host DefaultGuestRAMGiB clears MinRAMGiB on.
	// Derived, so it follows the fraction rather than drifting from it.
	MinHostRAMGiB      = (MinRAMGiB*DefaultRAMDen + DefaultRAMNum - 1) / DefaultRAMNum
	MinVCPUs           = 4
	DefaultDiskSizeGiB = 100
	// DefaultWidth and DefaultHeight are the default maximum guest resolution.
	DefaultWidth  = 3840
	DefaultHeight = 2160
	// MaxDimension is the maximum per-axis resolution.
	MaxDimension = 16384

	// ivshmemOverhead is the Looking Glass header size added to the two frames.
	ivshmemOverhead = 10 * utils.BytesPerMiB

	// KVMFRRAMDivisor caps the kvmfr buffer at 1/16 of host RAM. kvmfr vmallocs
	// the whole region at load, where the /dev/shm file populates only the pages
	// Looking Glass touches, so a 16384x16384 profile (4 GiB) must not reach
	// modprobe.
	KVMFRRAMDivisor = 16

	// WideAddressWidthBits is the IOMMU address-width threshold for the maxphysaddr fix.
	WideAddressWidthBits = 40
)

// DefaultGuestRAMGiB is the default guest RAM in GiB for a host of hostBytes:
// DefaultRAMNum/DefaultRAMDen (5/8) of host RAM, scaling with the host.
func DefaultGuestRAMGiB(hostBytes uint64) int {
	memGiB := int((hostBytes + utils.BytesPerGiB - 1) / utils.BytesPerGiB)
	return memGiB * DefaultRAMNum / DefaultRAMDen
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
	// ROMFile is the vBIOS path rendered as <rom file=>; ROMContent is the bytes
	// installed there. Both empty renders <rom bar='off'/>.
	ROMFile    string
	ROMContent []byte
	// KVMFR asks for the kvmfr backend, the caller having established the module
	// is built for the running kernel. NewProfile still refuses a buffer too
	// large to vmalloc, so compare with Profile.KVMFR to detect the downgrade.
	KVMFR bool
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
	// KVMFR renders the buffer as a memory-backend-file on steps.KVMFRDevice
	// instead of a <shmem> element: libvirt's <shmem> can only name a file under
	// /dev/shm, so kvmfr has to go through <qemu:commandline>.
	KVMFR         bool
	Width, Height int
	DiskPath      string
	DiskSizeGiB   int
	Win11ISO      string
	VirtioISO     string
	ProvisionISO  string
	GuestUser     string
	GuestPassword string
	Locale        string
	SpiceSocket   string
	GPU           BDF
	Audio         *BDF
	VideoNone     bool
	UUID          string
	ROMFile       string
	ROMContent    []byte
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
		SpiceSocket: steps.SpiceSocketPath(name),
		ROMFile:     o.ROMFile, ROMContent: o.ROMContent,
	}
	if err := checkROM(o.ROMFile, o.ROMContent); err != nil {
		return Profile{}, err
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
	for _, path := range []string{p.DiskPath, p.Win11ISO, p.VirtioISO, p.ProvisionISO, p.SpiceSocket} {
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
			ramGiB, utils.GiB(r.Platform.MemTotalBytes))
	}
	if r.Platform.MemTotalBytes > 0 && uint64(ramGiB)*utils.BytesPerGiB >= r.Platform.MemTotalBytes {
		return Profile{}, fmt.Errorf("guest RAM %d GiB does not fit in host RAM %.1f GiB",
			ramGiB, utils.GiB(r.Platform.MemTotalBytes))
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
	p.EmulatorPin = hw.FormatCPUList(emu)
	p.IOThreadPin = hw.FormatCPUList(iot)

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
	p.KVMFR = o.KVMFR && KVMFRFits(p.IVSHMEMMiB, r.Platform.MemTotalBytes)

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
// The error is reserve's own: an unreadable topology and a genuinely small CPU
// both yield no threads, and only reserve can tell preflight which it was.
func AssignableVCPUs(c hw.CPU) (int, error) {
	vcpu, _, _, _, err := reserve(c)
	if err != nil {
		return 0, err
	}
	return len(vcpu), nil
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
// Assumes sibling threads are adjacent in the kernel cpulists.
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

// KVMFRFits reports whether a buffer of sizeMiB is small enough to hand kvmfr.
// A host of unknown size (hostBytes 0, the fixture case) is taken at its word.
func KVMFRFits(sizeMiB, hostBytes uint64) bool {
	return hostBytes == 0 || sizeMiB*utils.BytesPerMiB <= hostBytes/KVMFRRAMDivisor
}

// IVSHMEMBytes is the buffer size the memory-backend-file object declares; it
// must match what the hook passes to modprobe as static_size_mb.
func (p Profile) IVSHMEMBytes() uint64 { return p.IVSHMEMMiB * utils.BytesPerMiB }

// KVMFRDevice is the node the rendered XML names; a method so the template and
// the hook cannot drift apart.
func (Profile) KVMFRDevice() string { return steps.KVMFRDevice }

// IVSHMEMMiB sizes the Looking Glass frame buffer in MiB for a w×h maximum.
func IVSHMEMMiB(w, h int) uint64 {
	need := uint64(w)*uint64(h)*4*2 + ivshmemOverhead
	size := uint64(1)
	for size < need {
		size <<= 1
	}
	return size / utils.BytesPerMiB
}

// romMagic is the PCI expansion-ROM signature (bytes 0x55 0xAA).
var romMagic = [2]byte{0x55, 0xaa}

// checkROM validates a supplied vBIOS: the install path must be XML-safe and the
// content must carry the PCI option-ROM signature.
func checkROM(file string, content []byte) error {
	if file == "" && len(content) == 0 {
		return nil
	}
	if file == "" {
		return errors.New("gpu rom content supplied without a path")
	}
	if strings.ContainsAny(file, `<>&'"`) {
		return fmt.Errorf("gpu rom path %q contains characters unsupported in libvirt XML", file)
	}
	if len(content) < 2 || content[0] != romMagic[0] || content[1] != romMagic[1] {
		return errors.New("gpu rom is not a PCI option ROM (missing 0x55 0xAA signature) — extract the correct VBIOS")
	}
	return nil
}

// ROMPath is the canonical vBIOS location a supplied --gpu-rom is installed to.
func ROMPath(name string) string { return steps.StateDirPath + "/vbios/" + name + ".rom" }

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

// xmlPath is where apply writes the domain XML.
func xmlPath(name string) string { return steps.VMsDirPath + "/" + name + ".xml" }

var domainTpl = template.Must(template.New("domain.xml").
	Funcs(template.FuncMap{"xml": utils.XMLEscape}).
	ParseFS(templateFS, "templates/domain.xml"))

// render produces the domain XML for the profile.
func render(p Profile) ([]byte, error) {
	var buf bytes.Buffer
	if err := domainTpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render domain XML: %w", err)
	}
	return buf.Bytes(), nil
}

// GuestConfig is the per-VM guest provisioning config carried in the domain XML metadata.
type GuestConfig struct {
	User       string `xml:"metadata>guest>user"`
	Password   string `xml:"metadata>guest>password"`
	Locale     string `xml:"metadata>guest>locale"`
	Resolution string `xml:"metadata>guest>resolution"`
	Win11ISO   string `xml:"metadata>guest>win11-iso"`
	GPURom     string `xml:"metadata>guest>gpu-rom"`
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

// ReadMemoryMiB returns the guest RAM in MiB from the VM's registry XML under
// root. The template always renders unit='MiB'; any other unit is refused so a
// caller sizing the hugepage pool can never mis-scale it.
func ReadMemoryMiB(root, name string) (uint64, error) {
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return 0, err
	}
	var doc struct {
		Memory struct {
			Unit  string `xml:"unit,attr"`
			Value string `xml:",chardata"`
		} `xml:"memory"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return 0, fmt.Errorf("parse domain memory: %w", err)
	}
	if doc.Memory.Unit != "MiB" {
		return 0, fmt.Errorf("domain memory unit %q is not MiB", doc.Memory.Unit)
	}
	n, err := strconv.ParseUint(strings.TrimSpace(doc.Memory.Value), 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("bad domain memory %q", doc.Memory.Value)
	}
	return n, nil
}

// KVMFRSizeMiB returns the buffer a VM's registry XML asks kvmfr for, and
// whether that VM uses the kvmfr backend at all. The hook needs both: load the
// module before qemu opens the device, and only for domains that name it.
// Root-only — the registry copy is 0600 because it carries the guest password;
// an unprivileged caller takes KVMFRSizeXML over libvirt's copy.
func KVMFRSizeMiB(root, name string) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return 0, false
	}
	return KVMFRSizeXML(b)
}

// KVMFRSizeXML is the same answer from a domain XML already in hand.
func KVMFRSizeXML(b []byte) (uint64, bool) {
	var doc struct {
		Args []struct {
			Value string `xml:"value,attr"`
		} `xml:"commandline>arg"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return 0, false
	}
	for _, arg := range doc.Args {
		if !strings.Contains(arg.Value, `"mem-path":"`+steps.KVMFRDevice+`"`) {
			continue
		}
		m := backendSizeRe.FindStringSubmatch(arg.Value)
		if m == nil {
			return 0, false
		}
		size, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			return 0, false
		}
		// Round up, since a partial MiB would size the module below the backend,
		// but divide before adding: size+BytesPerMiB-1 wraps within a MiB of MaxUint64.
		n := size / utils.BytesPerMiB
		if size%utils.BytesPerMiB != 0 {
			n++
		}
		// static_size_mb is a C int in the module.
		if n == 0 || n > math.MaxInt32 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

// backendSizeRe pulls the byte count out of the rendered memory-backend-file.
var backendSizeRe = regexp.MustCompile(`"size":(\d+)`)

// ReadPinnedCPUs returns the sorted union of host CPUs the VM's XML pins to guest
// threads (vcpu, emulator, iothread) — the complement of the host's housekeeping
// cores.
func ReadPinnedCPUs(root, name string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(root, xmlPath(name)))
	if err != nil {
		return nil, err
	}
	var doc struct {
		VCPUPin []struct {
			CPUSet string `xml:"cpuset,attr"`
		} `xml:"cputune>vcpupin"`
		EmulatorPin struct {
			CPUSet string `xml:"cpuset,attr"`
		} `xml:"cputune>emulatorpin"`
		IOThreadPin struct {
			CPUSet string `xml:"cpuset,attr"`
		} `xml:"cputune>iothreadpin"`
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse cputune: %w", err)
	}
	seen := map[int]bool{}
	var out []int
	add := func(set string) error {
		cpus, err := hw.ParseCPUList(set)
		if err != nil {
			return err
		}
		for _, c := range cpus {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
		return nil
	}
	for _, v := range doc.VCPUPin {
		if err := add(v.CPUSet); err != nil {
			return nil, err
		}
	}
	if err := add(doc.EmulatorPin.CPUSet); err != nil {
		return nil, err
	}
	if err := add(doc.IOThreadPin.CPUSet); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// DomainXMLID and the other ID funcs return journal step IDs for a VM's domain steps.
func DomainXMLID(vm string) string      { return "vm-domain-xml-" + vm }
func DiskImageID(vm string) string      { return "vm-disk-image-" + vm }
func DiskFcontextID(vm string) string   { return "vm-disk-fcontext-" + vm }
func DiskRestoreconID(vm string) string { return "vm-disk-restorecon-" + vm }
func ROMFileID(vm string) string        { return "vm-gpu-rom-" + vm }
func ROMFcontextID(vm string) string    { return "vm-gpu-rom-fcontext-" + vm }
func ROMRestoreconID(vm string) string  { return "vm-gpu-rom-restorecon-" + vm }
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

// imageLabelSteps is the semanage-fcontext + restorecon pair every labeled
// image file needs — one builder so the disk and ROM pairs cannot drift.
func imageLabelSteps(fcID, rcID, path string) []steps.Step {
	return []steps.Step{
		{
			ID: fcID, Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "virt_image_t", path},
			UndoCmd: []string{"semanage", "fcontext", "-d", path},
		},
		{
			ID: rcID, Kind: steps.KindRunCmd,
			Cmd: []string{"restorecon", path},
		},
	}
}

// Steps assembles the `vm define` step list: domain XML, disk image, SELinux
// label, an optional vBIOS install, then define.
func Steps(p Profile) ([]steps.Step, error) {
	xml, err := render(p)
	if err != nil {
		return nil, err
	}
	list := []steps.Step{
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
	}
	list = append(list, imageLabelSteps(DiskFcontextID(p.Name), DiskRestoreconID(p.Name), p.DiskPath)...)
	if p.ROMFile != "" {
		list = append(list, steps.Step{
			ID: ROMFileID(p.Name), Kind: steps.KindWriteFile,
			Path: p.ROMFile, Content: p.ROMContent, Mode: 0o644,
		})
		list = append(list, imageLabelSteps(ROMFcontextID(p.Name), ROMRestoreconID(p.Name), p.ROMFile)...)
	}
	return append(list, steps.Step{
		ID: DefineStepID(p.Name), Kind: steps.KindOp,
		Op:       steps.OpDefineDomain,
		Args:     map[string]string{"name": p.Name, "xml": xmlPath(p.Name)},
		Input:    xml,
		UndoOp:   steps.OpUndefineDomain,
		UndoArgs: map[string]string{"name": p.Name},
	}), nil
}
