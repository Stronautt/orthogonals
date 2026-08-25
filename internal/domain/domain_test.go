package domain

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/testsupport"
	"github.com/stronautt/orthogonals/internal/utils"
)

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

func TestSettingsRoundTrip(t *testing.T) {
	full := Settings{
		DisplayName: "Work PC", User: "pavlo", RAMGiB: 12,
		Disk: "/tank/vm.qcow2", DiskSizeGiB: 200, Resolution: "1920x1080",
		GuestUser: "user", GuestPassword: `p&<>"'w`, Locale: "uk-UA",
		Win11ISO: "/home/user/Win11.iso", GPUROM: "/var/lib/orthogonals/vbios/win11.rom",
		Shares: []string{"/home/user/Documents", "/srv/media"},
	}
	// Walked, not counted: a field count is satisfied by editing the number,
	// which round-trips the new knob vacuously.
	v := reflect.ValueOf(full)
	for _, f := range reflect.VisibleFields(v.Type()) {
		switch {
		case !f.IsExported():
			t.Errorf("Settings.%s is unexported: encoding/xml drops it and Over cannot set it", f.Name)
		case v.FieldByIndex(f.Index).IsZero():
			t.Errorf("Settings.%s has no value here, so the round-trip below proves nothing about it", f.Name)
		}
	}
	root := t.TempDir()
	p := mustProfile(t, reference(t), Options{Settings: full, ROMContent: []byte{0x55, 0xaa}})
	path := filepath.Join(root, xmlPath(p.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mustRender(t, p), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSettings(root, p.Name)
	if err != nil || !reflect.DeepEqual(got, full) {
		t.Errorf("ReadSettings = %+v, %v, want %+v", got, err, full)
	}
	empty, err := ReadSettings(root, "missing")
	if err != nil || !reflect.DeepEqual(empty, Settings{}) {
		t.Errorf("undefined VM must read empty and not error, got %+v, %v", empty, err)
	}
	// A registry that exists but will not parse must not read as "undefined":
	// converging on a half-read record silently resets every knob past the break.
	if err := os.WriteFile(path, []byte("<domain><metadata><settings><ram-gib>8"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := ReadSettings(root, p.Name); err == nil {
		t.Errorf("a truncated registry must be an error, got %+v", s)
	}

	bare := mustProfile(t, reference(t), Options{}).Settings
	want := Settings{RAMGiB: 20, Disk: "/var/lib/libvirt/images/win11.qcow2",
		DiskSizeGiB: DefaultDiskSizeGiB, Resolution: "3840x2160"}
	if !reflect.DeepEqual(bare, want) {
		t.Errorf("resolved defaults = %+v, want %+v", bare, want)
	}
}

func TestSettingsOver(t *testing.T) {
	prev := Settings{RAMGiB: 12, Locale: "uk-UA", Shares: []string{"/srv/docs"}}
	got := Settings{RAMGiB: 8, Shares: []string{""}}.Over(prev)
	want := Settings{RAMGiB: 8, Locale: "uk-UA", Shares: []string{""}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Over = %+v, want %+v", got, want)
	}
	merged := (Settings{}).Over(prev)
	if !reflect.DeepEqual(merged, prev) {
		t.Errorf("no flags must reproduce the registered record, got %+v", merged)
	}
	// reflect's Set copies a slice header, so the merged record would otherwise
	// share prev's array.
	merged.Shares[0] = "/mutated"
	if prev.Shares[0] != "/srv/docs" {
		t.Errorf("Over aliased the registered slice: prev.Shares = %v", prev.Shares)
	}
}

func TestReadPinnedCPUs(t *testing.T) {
	root := t.TempDir()
	p := mustProfile(t, reference(t), Options{})
	path := filepath.Join(root, xmlPath(p.Name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, mustRender(t, p), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPinnedCPUs(root, p.Name)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	if !slices.Equal(got, want) {
		t.Errorf("ReadPinnedCPUs = %v, want vcpu+emulator+iothread pins %v", got, want)
	}
	if _, err := ReadPinnedCPUs(root, "missing"); err == nil {
		t.Error("ReadPinnedCPUs must error on a missing domain")
	}
}

func TestNewShares(t *testing.T) {
	got, err := NewShares([]string{
		"/home/user/Documents/",
		"/home/user/Windows Shared",
		"/srv/media",
		"/mnt/Документи",
		"/mnt/other/Documents",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Share{
		{Dir: "/home/user/Documents", Tag: "Documents", Drive: "Z:", Service: "VirtioFsSvc"},
		{Dir: "/home/user/Windows Shared", Tag: "Windows-Shared", Drive: "Y:", Service: "VirtioFsSvc-Windows-Shared"},
		{Dir: "/srv/media", Tag: "media", Drive: "X:", Service: "VirtioFsSvc-media"},
		// non-ASCII collapses to the fallback tag; the last collides with the first
		{Dir: "/mnt/Документи", Tag: "share", Drive: "W:", Service: "VirtioFsSvc-share"},
		{Dir: "/mnt/other/Documents", Tag: "Documents-2", Drive: "V:", Service: "VirtioFsSvc-Documents-2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NewShares =\n%+v\nwant\n%+v", got, want)
	}
}

func TestNewSharesRefusals(t *testing.T) {
	cases := map[string][]string{
		"relative path":                     {"home/user/Documents"},
		"same directory":                    {"/srv/media", "/srv/media/"},
		"XML-hostile":                       {"/srv/m<edia"},
		"more than there are drive letters": make([]string, len(shareDrives)+1),
	}
	for name, dirs := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewShares(dirs); err == nil {
				t.Errorf("NewShares(%q) succeeded, want a refusal", dirs)
			}
		})
	}
}

func TestShareTagFitsTheGuestLimit(t *testing.T) {
	long := "/mnt/" + strings.Repeat("d", 3*MaxShareTag)
	shares, err := NewShares([]string{long, long + "x"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range shares {
		if len(s.Tag) > MaxShareTag {
			t.Errorf("tag %q is %d bytes, over the %d the guest service accepts", s.Tag, len(s.Tag), MaxShareTag)
		}
	}
	if shares[0].Tag == shares[1].Tag {
		t.Errorf("truncation collapsed two shares onto tag %q", shares[0].Tag)
	}
}

func TestSharesRenderSharedMemoryAndDevices(t *testing.T) {
	with := string(mustRender(t, mustProfile(t, reference(t), Options{Settings: Settings{Shares: []string{"/home/user/Documents", "/srv/media"}}})))
	for _, want := range []string{
		"<source type='memfd'/>",
		"<access mode='shared'/>",
		"<driver type='virtiofs'/>",
		"<source dir='/home/user/Documents'/>",
		"<target dir='Documents'/>",
		"<source dir='/srv/media'/>",
		"<target dir='media'/>",
		"<share>/home/user/Documents</share>",
	} {
		if !strings.Contains(with, want) {
			t.Errorf("shared domain XML is missing %q", want)
		}
	}
	without := string(mustRender(t, mustProfile(t, reference(t), Options{})))
	for _, banned := range []string{"access mode='shared'", "virtiofs", "<share>"} {
		if strings.Contains(without, banned) {
			t.Errorf("share-less domain XML contains %q", banned)
		}
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
			Settings:     Settings{Win11ISO: "/home/user/Win11.iso"},
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}, false},
		{"reference-1080p.xml", reference(t), Options{Settings: Settings{Resolution: "1920x1080"}}, false},
		{"reference-romfile.xml", reference(t), Options{
			Settings: Settings{
				Win11ISO: "/home/user/Win11.iso",
				GPUROM:   "/var/lib/orthogonals/vbios/win11.rom",
			},
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
			ROMContent:   []byte{0x55, 0xaa, 0x01, 0x02},
		}, false},
		{"no-ecores-46bit.xml", noECores(), Options{}, false},
		{"reference-kvmfr.xml", reference(t), Options{
			Settings:     Settings{Win11ISO: "/home/user/Win11.iso"},
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
			KVMFR:        true,
		}, false},
		{"provisioned.xml", reference(t), Options{
			Settings:     Settings{Win11ISO: "/home/user/Win11.iso"},
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}, true},
		{"reference-shares.xml", reference(t), Options{
			Settings: Settings{
				Win11ISO: "/home/user/Win11.iso",
				Shares:   []string{"/home/user/Windows Shared", "/srv/media"},
			},
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}, false},
	}
	// render is the per-case work: build the profile, apply the stage, render,
	// and prove the XML parses before anything compares it.
	render := func(t *testing.T, res *hw.Result, opts Options, provisioned bool) []byte {
		t.Helper()
		p := mustProfile(t, res, opts)
		if provisioned {
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
		return got
	}

	// The reference domain is goldened in full and every other case as its
	// departure from it. The cases are the reference with one option changed,
	// so a full golden apiece rewrites all seven whenever the template moves —
	// `Switch the guest disk to writeback caching` was 7 files for one line.
	if cases[0].name != "reference.xml" {
		t.Fatal("cases[0] is the baseline every other case departs from")
	}
	baseline := render(t, cases[0].res, cases[0].opts, cases[0].provisioned)
	testsupport.Golden(t, cases[0].name, baseline)

	for _, tc := range cases[1:] {
		t.Run(tc.name, func(t *testing.T) {
			got := render(t, tc.res, tc.opts, tc.provisioned)
			name := strings.TrimSuffix(tc.name, ".xml") + ".delta"
			if testsupport.GoldenDelta(t, name, baseline, got) {
				t.Errorf("%s renders byte-identically to reference.xml, so it "+
					"proves nothing; give it an option the reference lacks", tc.name)
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
	if p.RAMMiB != 20*1024 {
		t.Errorf("RAMMiB = %d, want 20480 (5/8 of the 32 GiB host)", p.RAMMiB)
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
		"cache='writeback'",
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
		Settings:   Settings{Win11ISO: "/home/user/Win11.iso", GPUROM: romPath},
		ROMContent: []byte{0x55, 0xaa, 0x01},
	})
	got := string(mustRender(t, p))
	for _, want := range []string{
		"<rom file='" + romPath + "'/>",
		"<gpu-rom>" + romPath + "</gpu-rom>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("domain missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<rom bar='off'/>") {
		t.Error("rom bar='off' must not render when a vBIOS file is set")
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/orthogonals/vms"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/orthogonals/vms/win11.xml"), []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	if g, err := ReadSettings(root, "win11"); err != nil || g.GPUROM != romPath {
		t.Errorf("ReadSettings GPUROM = %q, %v, want %q", g.GPUROM, err, romPath)
	}
}

func TestGPURomSteps(t *testing.T) {
	const romPath = "/var/lib/orthogonals/vbios/win11.rom"
	withROM := mustProfile(t, reference(t), Options{Settings: Settings{Win11ISO: "/i.iso", GPUROM: romPath}, ROMContent: []byte{0x55, 0xaa}})
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

	plain, err := Steps(mustProfile(t, reference(t), Options{Settings: Settings{Win11ISO: "/i.iso"}}))
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
		name    string
		cpu     hw.CPU
		want    int
		wantErr bool
	}{
		{"hybrid 6P+8E", hw.CPU{Threads: 20, Cores: 14, PCores: pcores(12), ECores: []int{12, 13, 14, 15, 16, 17, 18, 19}}, 10, false},
		{"flat 8 cores", hw.CPU{Threads: 8, Cores: 8, PCores: pcores(8)}, 7, false},
		{"degenerate 2 cores", hw.CPU{Threads: 4, Cores: 2, PCores: pcores(4)}, 2, false},
		// An unreadable topology must not read as a small CPU — preflight quotes
		// this error instead of telling the user to buy more cores.
		{"unusable topology", hw.CPU{}, 0, true},
	}
	for _, tc := range cases {
		got, err := AssignableVCPUs(tc.cpu)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: AssignableVCPUs error = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("%s: AssignableVCPUs = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestDefaultGuestRAMGiB(t *testing.T) {
	cases := []struct {
		host uint64
		want int
	}{
		{15872 << 20, 10}, // ~15.5 GiB rounds up to 16 → 10
		{16 << 30, 10},
		{24 << 30, 15},
		{32 << 30, 20},
		{64 << 30, 40},
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

func TestKVMFRFits(t *testing.T) {
	const host = 32 * utils.BytesPerGiB
	cases := []struct {
		name    string
		sizeMiB uint64
		host    uint64
		want    bool
	}{
		{"4K on a 32 GiB host", 128, host, true},
		{"exactly one sixteenth", 2048, host, true},
		{"one MiB over", 2049, host, false},
		{"16384x16384 sizes at 4 GiB", IVSHMEMMiB(MaxDimension, MaxDimension), host, false},
		{"unknown host size is taken at its word", 4096, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KVMFRFits(tc.sizeMiB, tc.host); got != tc.want {
				t.Errorf("KVMFRFits(%d, %d) = %v, want %v", tc.sizeMiB, tc.host, got, tc.want)
			}
		})
	}
}

// The cap has to reach the rendered XML, not just the profile field.
func TestKVMFRDeclinedRendersSHM(t *testing.T) {
	p := mustProfile(t, reference(t), Options{
		Settings: Settings{Resolution: fmt.Sprintf("%dx%d", MaxDimension, MaxDimension)}, KVMFR: true,
	})
	if p.KVMFR {
		t.Fatalf("profile kept kvmfr for a %d MiB buffer", p.IVSHMEMMiB)
	}
	got := string(mustRender(t, p))
	if !strings.Contains(got, "<shmem name='looking-glass'>") {
		t.Error("declined kvmfr did not fall back to the shmem element")
	}
	if strings.Contains(got, steps.KVMFRDevice) {
		t.Errorf("declined kvmfr still names %s", steps.KVMFRDevice)
	}
}

func TestKVMFRRenderSize(t *testing.T) {
	p := mustProfile(t, reference(t), Options{KVMFR: true})
	if !p.KVMFR {
		t.Fatal("reference profile declined kvmfr")
	}
	// The literal, not p.IVSHMEMMiB restated: this is the number qemu maps and
	// the hook passes modprobe.
	const wantMiB = 128 // 3840x2160 BGRA, double-buffered, plus header, to a power of two
	if p.IVSHMEMMiB != wantMiB || p.IVSHMEMBytes() != wantMiB*utils.BytesPerMiB {
		t.Errorf("IVSHMEM = %d MiB / %d bytes, want %d MiB", p.IVSHMEMMiB, p.IVSHMEMBytes(), wantMiB)
	}
	got := string(mustRender(t, p))
	for _, want := range []string{
		`"mem-path":"/dev/kvmfr0"`,
		fmt.Sprintf(`"size":%d`, wantMiB*utils.BytesPerMiB),
		`"bus":"pci.2"`,
		`<controller type='pci' index='1' model='pcie-root-port'/>`,
		`<controller type='pci' index='2' model='pcie-to-pci-bridge'>`,
		`<address type='pci' domain='0x0000' bus='0x01' slot='0x00' function='0x0'/>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered XML lacks %s:\n%s", want, got)
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
		{"RAM flag below minimum", reference(t), Options{Settings: Settings{RAMGiB: 4}}, "8 GiB"},
		{"RAM exceeds host", reference(t), Options{Settings: Settings{RAMGiB: 64}}, "host"},
		{"too few vCPUs", tiny, Options{}, "vCPU"},
		{"bad resolution", reference(t), Options{Settings: Settings{Resolution: "1920x-1"}}, "resolution"},
		{"resolution above per-axis max", reference(t), Options{Settings: Settings{Resolution: fmt.Sprintf("%dx2160", MaxDimension+1)}}, "per-axis maximum"},
		{"negative disk size", reference(t), Options{Settings: Settings{DiskSizeGiB: -5}}, "disk size"},
		{"path with XML metachars", reference(t), Options{Settings: Settings{Disk: `/tank/a'b.qcow2`}}, "libvirt XML"},
		{"bad GPU address", badBDF, Options{}, "PCI address"},
		{"rom without option-ROM signature", reference(t), Options{Settings: Settings{GPUROM: "/v/win11.rom"}, ROMContent: []byte{0x00, 0x01}}, "0x55 0xAA"},
		{"rom path with XML metachars", reference(t), Options{Settings: Settings{GPUROM: `/v/a'b.rom`}, ROMContent: []byte{0x55, 0xaa}}, "libvirt XML"},
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

func TestStageRoundTrip(t *testing.T) {
	for _, stage := range Stages {
		t.Run(string(stage), func(t *testing.T) {
			p := mustProfile(t, reference(t), Options{
				Settings:     Settings{Win11ISO: "/isos/Win11.iso"},
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
		VMName:   "gamer",
		Settings: Settings{RAMGiB: 12, Disk: "/tank/vm.qcow2", DiskSizeGiB: 200},
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

func TestKVMFRSizeMiB(t *testing.T) {
	write := func(t *testing.T, p Profile) string {
		t.Helper()
		root := t.TempDir()
		path := filepath.Join(root, xmlPath(p.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, mustRender(t, p), 0o600); err != nil {
			t.Fatal(err)
		}
		return root
	}

	t.Run("kvmfr domain reports its buffer", func(t *testing.T) {
		p := mustProfile(t, reference(t), Options{KVMFR: true})
		got, ok := KVMFRSizeMiB(write(t, p), p.Name)
		if !ok {
			t.Fatal("kvmfr domain not recognised")
		}
		if got != p.IVSHMEMMiB {
			t.Errorf("size = %d MiB, want %d", got, p.IVSHMEMMiB)
		}
	})

	t.Run("shm domain must not load the module", func(t *testing.T) {
		p := mustProfile(t, reference(t), Options{})
		if _, ok := KVMFRSizeMiB(write(t, p), p.Name); ok {
			t.Error("a /dev/shm domain reported a kvmfr buffer")
		}
	})

	t.Run("undefined VM", func(t *testing.T) {
		if _, ok := KVMFRSizeMiB(t.TempDir(), "missing"); ok {
			t.Error("undefined VM reported a kvmfr buffer")
		}
	})
}
