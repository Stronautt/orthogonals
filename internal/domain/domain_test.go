package domain

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

var update = flag.Bool("update", false, "rewrite golden files")

// reference is the PoC reference machine fixture.
func reference(t *testing.T) *hw.Result {
	t.Helper()
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// noECores is a plain 8-core/16-thread desktop CPU on a 46-bit host.
func noECores() *hw.Result {
	cpus := make([]int, 16)
	for i := range cpus {
		cpus[i] = i
	}
	return &hw.Result{
		CPU: hw.CPU{Threads: 16, Cores: 8, PCores: cpus},
		Platform: hw.Platform{
			IOMMUAddressWidth: 46,
			MemTotalBytes:     64 << 30,
		},
		GPUs: hw.GPUs{DGPUs: []hw.DGPU{{
			PCIDevice: hw.PCIDevice{Address: "0000:03:00.0", Vendor: "0x10de", Device: "0x2206"},
			Audio:     &hw.PCIDevice{Address: "0000:03:00.1", Vendor: "0x10de", Device: "0x1aef"},
		}}},
	}
}

func mustProfile(t *testing.T, res *hw.Result, o Options) Profile {
	t.Helper()
	p, err := NewProfile(res, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRender(t *testing.T, p Profile) []byte {
	t.Helper()
	out, err := render(p)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGuestConfigRoundTrip(t *testing.T) {
	root := t.TempDir()
	p := mustProfile(t, reference(t), Options{
		GuestUser: "user", GuestPassword: `p&<>"'w`, Locale: "uk-UA",
		Width: 1920, Height: 1080,
	})
	path := filepath.Join(root, xmlPath(p.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mustRender(t, p), 0o600); err != nil {
		t.Fatal(err)
	}
	got := ReadGuestConfig(root, p.Name)
	want := GuestConfig{User: "user", Password: `p&<>"'w`, Locale: "uk-UA", Resolution: "1920x1080"}
	if got != want {
		t.Errorf("ReadGuestConfig = %+v, want %+v", got, want)
	}
	if empty := ReadGuestConfig(root, "missing"); empty != (GuestConfig{}) {
		t.Errorf("undefined VM must read empty, got %+v", empty)
	}
}

func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name        string
		res         *hw.Result
		opts        Options
		provisioned bool
	}{
		{"reference.xml", reference(t), Options{
			Win11ISO:     "/home/user/Win11.iso",
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}, false},
		{"reference-1080p.xml", reference(t), Options{Width: 1920, Height: 1080}, false},
		{"reference-romfile.xml", reference(t), Options{
			Win11ISO:     "/home/user/Win11.iso",
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
			ROMFile:      "/var/lib/orthogonals/vbios/win11.rom",
			ROMContent:   []byte{0x55, 0xaa, 0x01, 0x02},
		}, false},
		{"no-ecores-46bit.xml", noECores(), Options{}, false},
		{"provisioned.xml", reference(t), Options{
			Win11ISO:     "/home/user/Win11.iso",
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, tc.res, tc.opts)
			if tc.provisioned {
				p.ApplyStage(StageFinal)
			}
			got := mustRender(t, p)

			d := xml.NewDecoder(bytes.NewReader(got))
			for {
				_, err := d.Token()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("rendered XML does not parse: %v\n%s", err, got)
				}
			}

			golden := filepath.Join("testdata", "golden", tc.name)
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%s: %v (run go test -update)", golden, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("rendered XML differs from %s:\n%s", golden, got)
			}
		})
	}
}

// TestReferenceProfile pins the computed values -update would silently accept.
func TestReferenceProfile(t *testing.T) {
	p := mustProfile(t, reference(t), Options{})
	if p.Name != "win11" {
		t.Errorf("Name = %q, want win11", p.Name)
	}
	if p.RAMMiB != 24*1024 {
		t.Errorf("RAMMiB = %d, want 24576 (host minus the 8 GiB reserve)", p.RAMMiB)
	}
	if p.VCPUs != 10 || p.Cores != 5 || p.ThreadsPerCore != 2 {
		t.Errorf("topology = %d vCPUs %d cores × %d threads, want 10 = 5×2", p.VCPUs, p.Cores, p.ThreadsPerCore)
	}
	for i, pin := range p.VCPUPins {
		if pin.VCPU != i || pin.CPU != i+2 {
			t.Errorf("pin %d = vcpu %d → cpu %d, want vcpu %d → cpu %d", i, pin.VCPU, pin.CPU, i, i+2)
		}
	}
	if p.EmulatorPin != "12,13,14,15" {
		t.Errorf("EmulatorPin = %q, want first half of E-cores 12,13,14,15", p.EmulatorPin)
	}
	if p.IOThreadPin != "16,17,18,19" {
		t.Errorf("IOThreadPin = %q, want second half of E-cores 16,17,18,19", p.IOThreadPin)
	}
	if p.MaxPhysAddrBits != 39 {
		t.Errorf("MaxPhysAddrBits = %d, want 39 (host address width)", p.MaxPhysAddrBits)
	}
	if p.IVSHMEMMiB != 128 {
		t.Errorf("IVSHMEMMiB = %d, want 128 for the default 4K maximum", p.IVSHMEMMiB)
	}
	if p.DiskPath != "/var/lib/libvirt/images/win11.qcow2" {
		t.Errorf("DiskPath = %q", p.DiskPath)
	}
	if p.DiskSizeGiB != 100 {
		t.Errorf("DiskSizeGiB = %d, want 100", p.DiskSizeGiB)
	}
	if p.GPU != (BDF{"0000", "01", "00", "0"}) {
		t.Errorf("GPU = %+v", p.GPU)
	}
	if p.Audio == nil || *p.Audio != (BDF{"0000", "01", "00", "1"}) {
		t.Errorf("Audio = %+v", p.Audio)
	}
}

// TestNoECoresFallback pins the no-E-core pinning fallback.
func TestNoECoresFallback(t *testing.T) {
	p := mustProfile(t, noECores(), Options{})
	if p.VCPUs != 14 || p.Cores != 7 || p.ThreadsPerCore != 2 {
		t.Errorf("topology = %d vCPUs %d cores × %d threads, want 14 = 7×2", p.VCPUs, p.Cores, p.ThreadsPerCore)
	}
	if last := p.VCPUPins[len(p.VCPUPins)-1]; last.CPU != 15 {
		t.Errorf("last vCPU pin = cpu %d, want 15 (only cpus 0-1 reserved)", last.CPU)
	}
	if p.EmulatorPin != "0,1" || p.IOThreadPin != "0,1" {
		t.Errorf("emulator/iothread pins = %q/%q, want 0,1 for both", p.EmulatorPin, p.IOThreadPin)
	}
	if p.MaxPhysAddrBits != 0 {
		t.Errorf("MaxPhysAddrBits = %d, want 0 on a 46-bit host", p.MaxPhysAddrBits)
	}
}

// TestAddressWidthFix pins the address-width fix on narrow and wide hosts.
func TestAddressWidthFix(t *testing.T) {
	narrow := string(mustRender(t, mustProfile(t, reference(t), Options{})))
	for _, want := range []string{
		"<maxphysaddr mode='emulate' bits='39'/>",
		"opt/ovmf/X-PciMmio64Mb,string=65536",
		"xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'",
	} {
		if !strings.Contains(narrow, want) {
			t.Errorf("39-bit domain missing %q", want)
		}
	}
	wide := string(mustRender(t, mustProfile(t, noECores(), Options{})))
	for _, stray := range []string{"maxphysaddr", "fw_cfg", "qemu:"} {
		if strings.Contains(wide, stray) {
			t.Errorf("46-bit domain must not contain %q", stray)
		}
	}
}

// TestQuirkFixes pins the PoC quirk set.
func TestQuirkFixes(t *testing.T) {
	got := string(mustRender(t, mustProfile(t, reference(t), Options{})))
	for _, want := range []string{
		"<model type='qxl'/>",
		"<graphics type='spice'",
		"managed='no'",
		"<timer name='hpet' present='no'/>",
		"<timer name='hypervclock' present='yes'/>",
		"<hyperv mode='custom'>",
		"<target dev='vda' bus='virtio'/>",
		"iothread='1'",
		"<backend type='emulator' version='2.0'/>",
		"<shmem name='looking-glass'>",
		"org.qemu.guest_agent.0",
		"com.redhat.spice.0",
		"<sound model='ich9'>",
		"<audio id='1' type='spice'/>",
		"machine='q35'",
		"<libosinfo:os id='http://microsoft.com/win/11'/>",
		"<input type='mouse' bus='virtio'/>",
		"<input type='keyboard' bus='virtio'/>",
		"<direct state='on'/>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("domain missing %q", want)
		}
	}
}

func TestGPURomRendersAndPersists(t *testing.T) {
	const romPath = "/var/lib/orthogonals/vbios/win11.rom"
	p := mustProfile(t, reference(t), Options{
		Win11ISO:   "/home/user/Win11.iso",
		ROMFile:    romPath,
		ROMContent: []byte{0x55, 0xaa, 0x01},
	})
	got := string(mustRender(t, p))
	for _, want := range []string{
		"<rom file='" + romPath + "'/>",
		"<orthogonals:gpu-rom>" + romPath + "</orthogonals:gpu-rom>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("domain missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<rom bar='off'/>") {
		t.Error("rom bar='off' must not render when a vBIOS file is set")
	}

	// The metadata line round-trips through ReadGuestConfig.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/orthogonals/vms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/orthogonals/vms/win11.xml"), []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	if g := ReadGuestConfig(root, "win11"); g.GPURom != romPath {
		t.Errorf("ReadGuestConfig GPURom = %q, want %q", g.GPURom, romPath)
	}
}

func TestGPURomSteps(t *testing.T) {
	const romPath = "/var/lib/orthogonals/vbios/win11.rom"
	withROM := mustProfile(t, reference(t), Options{Win11ISO: "/i.iso", ROMFile: romPath, ROMContent: []byte{0x55, 0xaa}})
	list, err := Steps(withROM)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]steps.Step{}
	for _, s := range list {
		byID[s.ID] = s
	}
	rom, ok := byID[ROMFileID("win11")]
	if !ok || rom.Path != romPath || string(rom.Content) != "\x55\xaa" {
		t.Errorf("rom write step = %+v", rom)
	}
	if _, ok := byID[ROMFcontextID("win11")]; !ok {
		t.Error("missing rom fcontext step")
	}
	if _, ok := byID[ROMRestoreconID("win11")]; !ok {
		t.Error("missing rom restorecon step")
	}

	// No ROM → no ROM steps.
	plain, err := Steps(mustProfile(t, reference(t), Options{Win11ISO: "/i.iso"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plain {
		if s.ID == ROMFileID("win11") {
			t.Error("rom step emitted without a vBIOS")
		}
	}
}

func TestAssignableVCPUs(t *testing.T) {
	pcores := func(n int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = i
		}
		return s
	}
	cases := []struct {
		name string
		cpu  hw.CPU
		want int
	}{
		{"hybrid 6P+8E", hw.CPU{Threads: 20, Cores: 14, PCores: pcores(12), ECores: []int{12, 13, 14, 15, 16, 17, 18, 19}}, 10},
		{"flat 8 cores", hw.CPU{Threads: 8, Cores: 8, PCores: pcores(8)}, 7},
		{"degenerate 2 cores", hw.CPU{Threads: 4, Cores: 2, PCores: pcores(4)}, 2},
		{"unusable topology", hw.CPU{}, 0},
	}
	for _, tc := range cases {
		if got := AssignableVCPUs(tc.cpu); got != tc.want {
			t.Errorf("%s: AssignableVCPUs = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestDefaultGuestRAMGiB(t *testing.T) {
	cases := []struct {
		host uint64
		want int
	}{
		{15872 << 20, 8},
		{32 << 30, 24},
		{128 << 30, 120},
		{12 << 30, 4},
	}
	for _, tc := range cases {
		if got := DefaultGuestRAMGiB(tc.host); got != tc.want {
			t.Errorf("DefaultGuestRAMGiB(%d) = %d, want %d", tc.host, got, tc.want)
		}
	}
}

func TestIVSHMEMSizing(t *testing.T) {
	cases := []struct {
		w, h int
		want uint64
	}{
		{1920, 1080, 32},
		{2560, 1440, 64},
		{3840, 2160, 128},
	}
	for _, tc := range cases {
		if got := IVSHMEMMiB(tc.w, tc.h); got != tc.want {
			t.Errorf("IVSHMEMMiB(%d, %d) = %d, want %d", tc.w, tc.h, got, tc.want)
		}
	}
}

func TestNewProfileErrors(t *testing.T) {
	small := reference(t)
	small.Platform.MemTotalBytes = 12 << 30

	tiny := noECores()
	tiny.CPU = hw.CPU{Threads: 4, Cores: 2, PCores: []int{0, 1, 2, 3}}

	narrow := reference(t)
	narrow.Platform.IOMMUAddressWidth = 0

	badBDF := noECores()
	badBDF.GPUs.DGPUs[0].Address = "junk"

	cases := []struct {
		name string
		res  *hw.Result
		opts Options
		want string
	}{
		{"no dGPU", &hw.Result{Platform: hw.Platform{IOMMUAddressWidth: 46}}, Options{}, "discrete GPU"},
		{"IOMMU off", narrow, Options{}, "IOMMU"},
		{"host RAM too small", small, Options{}, "8 GiB"},
		{"RAM flag below minimum", reference(t), Options{RAMGiB: 4}, "8 GiB"},
		{"RAM exceeds host", reference(t), Options{RAMGiB: 64}, "host"},
		{"too few vCPUs", tiny, Options{}, "vCPU"},
		{"bad resolution", reference(t), Options{Width: 1920, Height: -1}, "resolution"},
		{"resolution above per-axis max", reference(t), Options{Width: MaxDimension + 1, Height: 2160}, "per-axis maximum"},
		{"negative disk size", reference(t), Options{DiskSizeGiB: -5}, "disk size"},
		{"path with XML metachars", reference(t), Options{DiskPath: `/tank/a'b.qcow2`}, "libvirt XML"},
		{"bad GPU address", badBDF, Options{}, "PCI address"},
		{"rom without option-ROM signature", reference(t), Options{ROMFile: "/v/win11.rom", ROMContent: []byte{0x00, 0x01}}, "0x55 0xAA"},
		{"rom path with XML metachars", reference(t), Options{ROMFile: `/v/a'b.rom`, ROMContent: []byte{0x55, 0xaa}}, "libvirt XML"},
		{"rom content without a path", reference(t), Options{ROMContent: []byte{0x55, 0xaa}}, "without a path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewProfile(tc.res, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestApplyStage(t *testing.T) {
	cases := []struct {
		stage         Stage
		wantVideoNone bool
		wantISOs      [3]string
	}{
		{StageInstall, false, [3]string{"w.iso", "v.iso", "p.iso"}},
		{StageNoVideo, true, [3]string{"w.iso", "v.iso", "p.iso"}},
		{StageFinal, true, [3]string{"", "", ""}},
	}
	for _, tc := range cases {
		t.Run(string(tc.stage), func(t *testing.T) {
			p := Profile{Name: "win11", Win11ISO: "w.iso", VirtioISO: "v.iso", ProvisionISO: "p.iso"}
			p.ApplyStage(tc.stage)
			if p.VideoNone != tc.wantVideoNone {
				t.Errorf("VideoNone = %v, want %v", p.VideoNone, tc.wantVideoNone)
			}
			if got := [3]string{p.Win11ISO, p.VirtioISO, p.ProvisionISO}; got != tc.wantISOs {
				t.Errorf("ISOs = %v, want %v", got, tc.wantISOs)
			}
		})
	}
}

// TestStageRoundTrip pins CurrentStage reading back the render's stage.
func TestStageRoundTrip(t *testing.T) {
	for _, stage := range Stages {
		t.Run(string(stage), func(t *testing.T) {
			p := mustProfile(t, reference(t), Options{
				Win11ISO:     "/isos/Win11.iso",
				VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
				ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
			})
			p.ApplyStage(stage)
			root := t.TempDir()
			path := filepath.Join(root, "etc/orthogonals/vms/win11.xml")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, mustRender(t, p), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := CurrentStage(root, "win11"); got != stage {
				t.Errorf("CurrentStage = %s, want %s", got, stage)
			}
		})
	}
	if got := CurrentStage(t.TempDir(), "win11"); got != StageInstall {
		t.Errorf("missing XML must read as the install stage, got %s", got)
	}
}

func TestJournaledDisk(t *testing.T) {
	record := func(args map[string]string) *steps.Manifest {
		return &steps.Manifest{Records: []steps.Record{{ID: DiskImageID("win11"), OpArgs: args}}}
	}
	cases := []struct {
		name     string
		m        *steps.Manifest
		wantPath string
		wantSize int
		wantOK   bool
	}{
		{"journaled", record(map[string]string{"path": "/tank/win11.qcow2", "size-gib": "200"}), "/tank/win11.qcow2", 200, true},
		{"not journaled", &steps.Manifest{}, "", 0, false},
		{"missing path", record(map[string]string{"size-gib": "200"}), "", 0, false},
		{"unparseable size", record(map[string]string{"path": "/x.qcow2", "size-gib": "big"}), "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, size, ok := JournaledDisk(tc.m, "win11")
			if path != tc.wantPath || size != tc.wantSize || ok != tc.wantOK {
				t.Errorf("got (%q, %d, %v), want (%q, %d, %v)", path, size, ok, tc.wantPath, tc.wantSize, tc.wantOK)
			}
		})
	}
}

func TestSteps(t *testing.T) {
	list, err := Steps(mustProfile(t, reference(t), Options{}))
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"vm-domain-xml-win11", "vm-disk-image-win11", "vm-disk-fcontext-win11", "vm-disk-restorecon-win11", "vm-define-win11"}
	if len(list) != len(wantIDs) {
		t.Fatalf("got %d steps, want %d", len(list), len(wantIDs))
	}
	for i, id := range wantIDs {
		if list[i].ID != id {
			t.Errorf("step %d = %s, want %s", i, list[i].ID, id)
		}
	}
	if list[0].Kind != steps.KindWriteFile || list[0].Path != "/etc/orthogonals/vms/win11.xml" {
		t.Errorf("xml step = %+v", list[0])
	}
	disk := list[1]
	if !disk.Data {
		t.Error("disk image step must be a data step (plain undo keeps it)")
	}
	if disk.Kind != steps.KindOp || disk.Op != steps.OpCreateVolume ||
		disk.Args["path"] != "/var/lib/libvirt/images/win11.qcow2" || disk.Args["size-gib"] != "100" {
		t.Errorf("disk step = %+v", disk)
	}
	if disk.UndoOp != steps.OpRemoveFile || disk.UndoArgs["path"] != "/var/lib/libvirt/images/win11.qcow2" {
		t.Errorf("disk undo = %s %v (undo --purge must remove the image)", disk.UndoOp, disk.UndoArgs)
	}
	if got := strings.Join(list[2].Cmd, " "); got != "semanage fcontext -a -t virt_image_t /var/lib/libvirt/images/win11.qcow2" {
		t.Errorf("fcontext cmd = %q", got)
	}
	define := list[4]
	if define.Kind != steps.KindOp || define.Op != steps.OpDefineDomain ||
		define.Args["name"] != "win11" || define.Args["xml"] != "/etc/orthogonals/vms/win11.xml" {
		t.Errorf("define step = %+v", define)
	}
	if !bytes.Equal(define.Input, list[0].Content) {
		t.Error("define step Input must be the rendered domain XML")
	}
	if define.UndoOp != steps.OpUndefineDomain || define.UndoArgs["name"] != "win11" {
		t.Errorf("define undo = %s %v", define.UndoOp, define.UndoArgs)
	}
}

func TestOptionsOverrides(t *testing.T) {
	p := mustProfile(t, reference(t), Options{
		VMName: "gamer", RAMGiB: 12, DiskPath: "/tank/vm.qcow2", DiskSizeGiB: 200,
	})
	if p.Name != "gamer" || p.RAMMiB != 12*1024 || p.DiskPath != "/tank/vm.qcow2" || p.DiskSizeGiB != 200 {
		t.Errorf("overrides not honored: %+v", p)
	}
	list, err := Steps(p)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Path != "/etc/orthogonals/vms/gamer.xml" {
		t.Errorf("xml path = %q", list[0].Path)
	}
}
