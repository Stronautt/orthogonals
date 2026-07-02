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

// reference is the PoC machine: i5-13600K (P-threads 0-11, E-cores 12-19),
// RTX 3080 + audio, 32 GiB RAM, 39-bit IOMMU address width.
func reference(t *testing.T) *hw.Result {
	t.Helper()
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// noECores is a plain 8-core/16-thread desktop CPU on a 46-bit host —
// exercises the no-E-core pinning fallback and the no-address-fix path.
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

// The guest settings survive the XML round trip — including characters that
// need escaping — so media renders exactly what define was given.
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
		name string
		res  *hw.Result
		opts Options
	}{
		// 39-bit host, hybrid CPU, defaults (4K buffer, 16 GiB, 100 GiB disk),
		// installer media attached as SATA cdroms
		{"reference.xml", reference(t), Options{
			Win11ISO:     "/home/user/Win11.iso",
			VirtioISO:    "/var/lib/orthogonals/cache/virtio-win.iso",
			ProvisionISO: "/var/lib/orthogonals/win11-provision.iso",
		}},
		// 32M IVSHMEM sizing at an explicit 1080p maximum on the same 39-bit
		// reference host; no media (the cdrom blocks must disappear when the
		// ISO paths are empty)
		{"reference-1080p.xml", reference(t), Options{Width: 1920, Height: 1080}},
		// no E-cores + 46-bit host: pinning fallback, no maxphysaddr/fw_cfg
		{"no-ecores-46bit.xml", noECores(), Options{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRender(t, mustProfile(t, tc.res, tc.opts))

			// well-formedness (including the qemu: namespace binding)
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

// TestReferenceProfile pins the computed values the golden file alone would
// silently accept changes to under -update.
func TestReferenceProfile(t *testing.T) {
	p := mustProfile(t, reference(t), Options{})
	if p.Name != "win11" {
		t.Errorf("Name = %q, want win11", p.Name)
	}
	if p.RAMMiB != 16*1024 {
		t.Errorf("RAMMiB = %d, want 16384 (min(host/2, 16 GiB))", p.RAMMiB)
	}
	if p.VCPUs != 10 || p.Cores != 5 || p.ThreadsPerCore != 2 {
		t.Errorf("topology = %d vCPUs %d cores × %d threads, want 10 = 5×2", p.VCPUs, p.Cores, p.ThreadsPerCore)
	}
	// reserve physical core 0 (threads 0-1) for the host: vCPU i → CPU 2+i
	for i, pin := range p.VCPUPins {
		if pin.VCPU != i || pin.CPU != 2+i {
			t.Errorf("pin %d = vcpu %d → cpu %d, want vcpu %d → cpu %d", i, pin.VCPU, pin.CPU, i, 2+i)
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

// TestNoECoresFallback: without E-cores the last P-core is taken back from
// the guest for emulator+iothread.
func TestNoECoresFallback(t *testing.T) {
	p := mustProfile(t, noECores(), Options{})
	if p.VCPUs != 12 || p.Cores != 6 || p.ThreadsPerCore != 2 {
		t.Errorf("topology = %d vCPUs %d cores × %d threads, want 12 = 6×2", p.VCPUs, p.Cores, p.ThreadsPerCore)
	}
	if last := p.VCPUPins[len(p.VCPUPins)-1]; last.CPU != 13 {
		t.Errorf("last vCPU pin = cpu %d, want 13 (cpus 14-15 reserved for emulator)", last.CPU)
	}
	if p.EmulatorPin != "14,15" || p.IOThreadPin != "14,15" {
		t.Errorf("emulator/iothread pins = %q/%q, want 14,15 for both", p.EmulatorPin, p.IOThreadPin)
	}
	if p.MaxPhysAddrBits != 0 {
		t.Errorf("MaxPhysAddrBits = %d, want 0 on a 46-bit host", p.MaxPhysAddrBits)
	}
}

// TestAddressWidthFix: hosts under 40 bits get BOTH maxphysaddr and the OVMF
// fw_cfg knob (PoC: maxphysaddr alone is ignored by OVMF); wide hosts get
// neither.
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

// TestQuirkFixes pins the PoC quirk set: install-time qxl + SPICE,
// managed='no' hostdevs, HPET off, hypervclock on, VirtIO disk on an
// iothread.
func TestQuirkFixes(t *testing.T) {
	got := string(mustRender(t, mustProfile(t, reference(t), Options{})))
	for _, want := range []string{
		// defined with qxl (OOBE crash-loops with zero display adapters);
		// up flips it to video=none after provisioning so the VDD monitor
		// is the guest's only display
		"<model type='qxl'/>",
		"<graphics type='spice'",
		"managed='no'", // hooks own the bind/unbind, not libvirt
		"<timer name='hpet' present='no'/>",
		"<timer name='hypervclock' present='yes'/>",
		"<hyperv mode='custom'>",
		"<target dev='vda' bus='virtio'/>",
		"iothread='1'",
		"<backend type='emulator' version='2.0'/>", // emulated TPM for Win11
		"<shmem name='looking-glass'>",
		"org.qemu.guest_agent.0", // provisioning poller needs the agent channel
		"com.redhat.spice.0",     // spice-vdagent's transport: no channel, no clipboard (in Looking Glass either)
		"<sound model='ich9'>",   // guest audio rides SPICE: the dGPU HDA has no sink on a headless card
		"<audio id='1' type='spice'/>",
		"machine='q35'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("domain missing %q", want)
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
		// 6 P-cores ×2 threads + 8 E-cores: core 0 reserved → 10
		{"hybrid 6P+8E", hw.CPU{Threads: 20, Cores: 14, PCores: pcores(12), ECores: []int{12, 13, 14, 15, 16, 17, 18, 19}}, 10},
		// flat 8 cores, no HT, no E-cores: host core + emulator core → 6
		{"flat 8 cores", hw.CPU{Threads: 8, Cores: 8, PCores: pcores(8)}, 6},
		// 2 cores HT: everything is reserved, nothing assignable
		{"degenerate 2 cores", hw.CPU{Threads: 4, Cores: 2, PCores: pcores(4)}, 0},
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
		{15872 << 20, 8}, // a "16 GiB" host reports ~15.5 GiB; round-up keeps it at the minimum
		{32 << 30, 16},
		{128 << 30, 16}, // capped at MaxDefaultRAMGiB
		{12 << 30, 6},   // below the guest minimum — NewProfile rejects it downstream
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
	small.Platform.MemTotalBytes = 12 << 30 // half = 6 GiB < 8 GiB minimum

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
	if got := strings.Join(disk.Cmd, " "); got != "qemu-img create -f qcow2 /var/lib/libvirt/images/win11.qcow2 100G" {
		t.Errorf("disk cmd = %q", got)
	}
	if got := strings.Join(disk.UndoCmd, " "); got != "rm -f /var/lib/libvirt/images/win11.qcow2" {
		t.Errorf("disk undo = %q (undo --purge must remove the image)", got)
	}
	if got := strings.Join(list[2].Cmd, " "); got != "semanage fcontext -a -t virt_image_t /var/lib/libvirt/images/win11.qcow2" {
		t.Errorf("fcontext cmd = %q", got)
	}
	if got := strings.Join(list[4].Cmd, " "); got != "virsh define /etc/orthogonals/vms/win11.xml" {
		t.Errorf("define cmd = %q", got)
	}
	if got := strings.Join(list[4].UndoCmd, " "); got != "virsh undefine win11 --nvram --tpm" {
		t.Errorf("define undo = %q", got)
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
