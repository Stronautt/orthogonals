// Package domain renders the libvirt domain XML and assembles the `orthogonals vm` step list.
package domain

import (
	"bytes"
	"embed"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	MinRAMGiB = 8
	// DefaultRAMNum/DefaultRAMDen size guest RAM as a fraction of host RAM. The
	// rest stays free for the host desktop and for the overhead of QEMU.
	DefaultRAMNum = 5
	DefaultRAMDen = 8
	// MinHostRAMGiB is the smallest host DefaultGuestRAMGiB clears MinRAMGiB on.
	MinHostRAMGiB      = (MinRAMGiB*DefaultRAMDen + DefaultRAMNum - 1) / DefaultRAMNum
	MinVCPUs           = 4
	DefaultDiskSizeGiB = 100
	// ImagesDir is libvirt's default storage pool.
	ImagesDir     = "/var/lib/libvirt/images"
	DefaultWidth  = 3840
	DefaultHeight = 2160
	MaxDimension  = 16384

	// bytesPerPixel is Looking Glass's BGRA frame format; lgFrames is its
	// double buffer. ivshmemOverhead is the header added to the two frames.
	bytesPerPixel   = 4
	lgFrames        = 2
	ivshmemOverhead = 10 * utils.BytesPerMiB

	// KVMFRRAMDivisor caps the kvmfr buffer at 1/16 of host RAM: kvmfr vmallocs
	// the whole region at load, where a /dev/shm file populates only the pages
	// Looking Glass touches.
	KVMFRRAMDivisor = 16

	// WideAddressWidthBits is the IOMMU address-width threshold for the maxphysaddr fix.
	WideAddressWidthBits = 40

	// MaxShareTag is the longest virtiofs mount tag the guest service accepts.
	MaxShareTag = 36
)

// shareDrives counts down from Z to keep clear of C and the install CDs.
const shareDrives = "ZYXWVUTSRQPONMLKJIHGFE"

func DefaultGuestRAMGiB(hostBytes uint64) int {
	memGiB := int((hostBytes + utils.BytesPerGiB - 1) / utils.BytesPerGiB)
	return memGiB * DefaultRAMNum / DefaultRAMDen
}

// Options build a Profile: the registered Settings plus the inputs the caller
// derives.
type Options struct {
	VMName string
	// Settings.Shares must arrive absolute and already checked to exist.
	Settings Settings
	// VirtioISO and ProvisionISO are cache paths, not knobs: moving them into
	// Settings would make them sticky and journal them.
	VirtioISO    string
	ProvisionISO string
	// ROMContent is the vBIOS installed at Settings.GPUROM; both empty renders
	// <rom bar='off'/>.
	ROMContent []byte
	// KVMFR asks for the kvmfr backend; NewProfile still refuses a buffer too
	// large to vmalloc, so compare with Profile.KVMFR to detect the downgrade.
	KVMFR bool
}

// Share is one host directory exported to the guest over virtiofs.
type Share struct {
	Dir   string // host path <source dir=> names
	Tag   string // virtiofs mount tag, and the volume name the guest shows
	Drive string // guest drive letter, "Z:" form
	// Service mounts this share. VirtioFsSvc holds one filesystem object and
	// reads its tag from the command line, never its registry key, so extra
	// shares need clones of it.
	Service string
}

// stockShareService is the service virtio-win installs.
const stockShareService = "VirtioFsSvc"

// NewShares derives tag, drive letter and service name from the order given, so
// a converge re-reading the registered directories reproduces the same letters.
func NewShares(dirs []string) ([]Share, error) {
	if len(dirs) > len(shareDrives) {
		return nil, fmt.Errorf("%d shared directories is more than the %d drive letters a guest has for them",
			len(dirs), len(shareDrives))
	}
	out := make([]Share, 0, len(dirs))
	seen := map[string]bool{}
	tags := map[string]bool{}
	for i, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("shared directory %q is not an absolute path", dir)
		}
		dir = filepath.Clean(dir)
		if strings.ContainsAny(dir, `<>&'"`) {
			return nil, fmt.Errorf("shared directory %q contains characters unsupported in libvirt XML", dir)
		}
		if seen[dir] {
			return nil, fmt.Errorf("shared directory %q is listed twice", dir)
		}
		seen[dir] = true
		tag := shareTag(dir, tags)
		tags[tag] = true
		svc := stockShareService
		if i > 0 {
			svc = stockShareService + "-" + tag
		}
		out = append(out, Share{Dir: dir, Tag: tag, Drive: shareDrives[i:i+1] + ":", Service: svc})
	}
	return out, nil
}

// shareTag builds a mount tag from a directory's base name, ASCII only: the tag
// crosses the QEMU command line, the domain XML and a wide-string compare in the
// guest service.
func shareTag(dir string, taken map[string]bool) string {
	var b strings.Builder
	for _, r := range filepath.Base(dir) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	tag := strings.Trim(b.String(), "-.")
	if tag == "" {
		tag = "share"
	}
	base := truncateTag(tag, MaxShareTag)
	tag = base
	for n := 2; taken[tag]; n++ {
		suffix := "-" + strconv.Itoa(n)
		tag = truncateTag(base, MaxShareTag-len(suffix)) + suffix
	}
	return tag
}

func truncateTag(tag string, max int) string {
	if len(tag) <= max {
		return tag
	}
	return strings.Trim(tag[:max], "-.")
}

// BDF is a PCI address split into the hostdev XML address fields.
type BDF struct{ Domain, Bus, Slot, Function string }

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
	// KVMFR renders the buffer through <qemu:commandline>: libvirt's <shmem> can
	// only name a file under /dev/shm.
	KVMFR         bool
	Width, Height int
	DiskPath      string
	DiskSizeGiB   int
	Win11ISO      string
	VirtioISO     string
	ProvisionISO  string
	SpiceSocket   string
	GPU           BDF
	Audio         *BDF
	VideoNone     bool
	UUID          string
	ROMFile       string
	ROMContent    []byte
	// Shares are the virtiofs exports; any at all force shared memory backing.
	Shares []Share
	// Settings is the record this profile was resolved from, defaults filled in.
	Settings Settings
}

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
	s := o.Settings
	p := Profile{
		Name: name, DiskPath: s.Disk, DiskSizeGiB: s.DiskSizeGiB,
		Win11ISO: s.Win11ISO, VirtioISO: o.VirtioISO, ProvisionISO: o.ProvisionISO,
		SpiceSocket: steps.SpiceSocketPath(name),
		ROMFile:     s.GPUROM, ROMContent: o.ROMContent,
	}
	if err := checkROM(s.GPUROM, o.ROMContent); err != nil {
		return Profile{}, err
	}
	if p.Shares, err = NewShares(s.Shares); err != nil {
		return Profile{}, err
	}
	if p.DiskSizeGiB == 0 {
		p.DiskSizeGiB = DefaultDiskSizeGiB
	}
	if p.DiskSizeGiB < 0 {
		return Profile{}, fmt.Errorf("bad disk size %d GiB", p.DiskSizeGiB)
	}
	if p.DiskPath == "" {
		p.DiskPath = filepath.Join(ImagesDir, name+".qcow2")
	}
	for _, path := range []string{p.DiskPath, p.Win11ISO, p.VirtioISO, p.ProvisionISO, p.SpiceSocket} {
		if strings.ContainsAny(path, `<>&'"`) {
			return Profile{}, fmt.Errorf("path %q contains characters unsupported in libvirt XML", path)
		}
	}

	ramGiB := s.RAMGiB
	if ramGiB == 0 {
		ramGiB = DefaultGuestRAMGiB(r.Platform.MemTotalBytes)
	}
	if ramGiB < MinRAMGiB {
		return Profile{}, fmt.Errorf("guest RAM %d GiB is below the 8 GiB minimum (host has %.1f GiB)",
			ramGiB, utils.GiB(r.Platform.MemTotalBytes))
	}
	// Divide rather than multiply: uint64(ramGiB)*BytesPerGiB wraps to 0 at
	// 2^34 GiB, and the wrapped --ram then sails past this check.
	if r.Platform.MemTotalBytes > 0 && uint64(ramGiB) >= r.Platform.MemTotalBytes/utils.BytesPerGiB {
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

	w, h, err := ParseResolution(s.Resolution)
	if err != nil {
		return Profile{}, err
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

	// Defaults go back into the record: the next define re-reads them rather
	// than re-deriving from a host that may have changed.
	s.RAMGiB = ramGiB
	s.Disk, s.DiskSizeGiB = p.DiskPath, p.DiskSizeGiB
	s.Resolution = fmt.Sprintf("%dx%d", p.Width, p.Height)
	p.Settings = s
	return p, nil
}

// AssignableVCPUs is how many P-core threads reserve assigns to the guest. The
// error is reserve's own: an unreadable topology and a small CPU both yield no
// threads, and only reserve can tell preflight which it was.
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
// the hook cannot drift.
func (Profile) KVMFRDevice() string { return steps.KVMFRDevice }

// IVSHMEMMiB sizes the Looking Glass frame buffer in MiB for a w×h maximum.
func IVSHMEMMiB(w, h int) uint64 {
	need := uint64(w)*uint64(h)*bytesPerPixel*lgFrames + ivshmemOverhead
	size := uint64(1)
	for size < need {
		size <<= 1
	}
	return size / utils.BytesPerMiB
}

// romMagic is the PCI expansion-ROM signature.
var romMagic = [2]byte{0x55, 0xaa}

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

// ROMPath is where a supplied --gpu-rom is installed.
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

func xmlPath(name string) string { return steps.VMsDirPath + "/" + name + ".xml" }

var domainTpl = template.Must(template.New("domain.xml").
	Funcs(template.FuncMap{"xml": utils.XMLEscape}).
	ParseFS(templateFS, "templates/domain.xml"))

func render(p Profile) ([]byte, error) {
	var buf bytes.Buffer
	if err := domainTpl.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("render domain XML: %w", err)
	}
	return buf.Bytes(), nil
}

// KVMFRSizeMiB returns the buffer a VM's registry XML asks kvmfr for, and
// whether that VM uses the kvmfr backend at all. Root-only — the registry copy
// is 0600 because it carries the guest password; an unprivileged caller takes
// KVMFRSizeXML over libvirt's copy.
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
		// The arg is the qemu -object JSON that the template renders. A JSON
		// parse reads it, and a text match does not, so neither the field order
		// nor the spacing can change the answer.
		var backend struct {
			MemPath string `json:"mem-path"`
			Size    uint64 `json:"size"`
		}
		if err := json.Unmarshal([]byte(arg.Value), &backend); err != nil {
			continue
		}
		if backend.MemPath != steps.KVMFRDevice {
			continue
		}
		// Round up, or the module sizes below the backend; divide before adding,
		// since size+BytesPerMiB-1 wraps within a MiB of MaxUint64.
		n := backend.Size / utils.BytesPerMiB
		if backend.Size%utils.BytesPerMiB != 0 {
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

// ReadPinnedCPUs returns the sorted union of host CPUs the VM's XML pins to
// guest threads (vcpu, emulator, iothread).
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

// DomainXMLID and the other ID funcs return journal step IDs.
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
	// StageInstall renders the emulated display and the installer cdroms.
	StageInstall Stage = "install"
	// StageNoVideo drops the emulated display.
	StageNoVideo Stage = "novideo"
	// StageFinal drops the installer cdroms too.
	StageFinal Stage = "final"
)

// Stages lists the stages in pipeline order.
var Stages = []Stage{StageInstall, StageNoVideo, StageFinal}

// CurrentStage reads the stage back from the domain's registry XML under root.
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

func (p *Profile) ApplyStage(s Stage) {
	p.VideoNone = s != StageInstall
	if s == StageFinal {
		p.Win11ISO, p.VirtioISO, p.ProvisionISO = "", "", ""
	}
}

// JournaledDisk reports the disk path and size from the vm's create-volume op.
func JournaledDisk(m *steps.Manifest, vm string) (string, int, bool) {
	args := m.OpArgs(DiskImageID(vm))
	size, err := strconv.Atoi(args["size-gib"])
	if args["path"] == "" || err != nil {
		return "", 0, false
	}
	return args["path"], size, true
}

// imageLabelSteps is the semanage-fcontext + restorecon pair every labeled
// image file needs.
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

// Steps assembles the `vm define` step list.
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
