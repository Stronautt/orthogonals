//go:build desk

// detect, preflight, and status against the machine this is running on, via
// `make test-desk`. Read-only by construction — nothing here writes outside
// t.TempDir(), and no test calls apply.
//
// TestFixtureAttributesExistOnRealHardware is what this suite exists for: every
// other golden derives from the hand-written hwtest.Roots, so nothing else can
// catch a fixture modelling an attribute sysfs does not publish.
package desk

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/preflight"
	"github.com/stronautt/orthogonals/internal/steps"
)

// realRoot is the running machine — the one place in the suite that passes no
// --root prefix.
const realRoot = "/"

func detectHost(t *testing.T) *hw.Result {
	t.Helper()
	r, err := hw.Detect(realRoot)
	if err != nil {
		t.Fatalf("detect on real hardware: %v", err)
	}
	return r
}

// TestJSONContractOnRealHardware exists because a schema that only ever sees
// synthetic input encodes the fixture's assumptions instead of the format's.
func TestJSONContractOnRealHardware(t *testing.T) {
	res := detectHost(t)
	tests := []struct {
		name   string
		schema string
		doc    any
	}{
		{"detect", "detect", res},
		{"preflight", "preflight", preflightReport(res)},
		{"status", "status", orchestrate.Status(realRoot)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validateSchema(t, tt.schema, encode(t, tt.doc))
		})
	}
}

// preflightReport mirrors runPreflight's --json envelope (internal/cli/preflight.go).
func preflightReport(res *hw.Result) any {
	checks := preflight.Analyze(res, preflight.GatherFacts(realRoot))
	return struct {
		Status preflight.Status  `json:"status"`
		Checks []preflight.Check `json:"checks"`
	}{preflight.Overall(checks), checks}
}

// TestPreflightContractHoldsOnRealHardware asserts what must hold for any host,
// not just the ones with a golden file.
func TestPreflightContractHoldsOnRealHardware(t *testing.T) {
	checks := preflight.Analyze(detectHost(t), preflight.GatherFacts(realRoot))
	if len(checks) == 0 {
		t.Fatal("preflight produced no checks")
	}
	seen := map[string]bool{}
	for _, c := range checks {
		if seen[c.Name] {
			t.Errorf("duplicate check name %q — names are the scripting contract", c.Name)
		}
		seen[c.Name] = true
		if !isKebab(c.Name) {
			t.Errorf("check name %q is not kebab-case", c.Name)
		}
		if c.Message == "" {
			t.Errorf("check %q has no message", c.Name)
		}
		if c.Status == preflight.Fail && c.Remedy == "" {
			t.Errorf("check %q fails without telling the user what to do", c.Name)
		}
	}
	overall := preflight.Overall(checks)
	if code := overall.ExitCode(); code != 0 && code != 1 && code != 2 {
		t.Errorf("overall %q maps to exit code %d, want 0, 1, or 2", overall, code)
	}
	t.Logf("preflight on this host: %s (exit %d)", overall, overall.ExitCode())
	for _, c := range checks {
		if c.Status != preflight.Pass {
			t.Logf("  %s %s: %s", strings.ToUpper(string(c.Status)), c.Name, c.Message)
		}
	}
}

// TestSpiceSocketTypeIsUsableOnThisHost: semanage refuses a type the shipped
// policy does not define, which would abort apply halfway through. Appearing
// in file_contexts proves the type exists and is valid as a file context.
// svirt_var_run_t, the obvious guess by analogy, is not defined anywhere.
func TestSpiceSocketTypeIsUsableOnThisHost(t *testing.T) {
	const fileContexts = "/etc/selinux/targeted/contexts/files/file_contexts"
	b, err := os.ReadFile(fileContexts)
	if err != nil {
		t.Skipf("no targeted policy on this host: %v", err)
	}
	if !strings.Contains(string(b), "qemu_var_run_t") {
		t.Errorf("%s never mentions qemu_var_run_t — apply's fcontext rule for %s would be refused",
			fileContexts, steps.RunDirPath)
	}
}

// TestKVMFRLabelIsUsableOnThisHost: a type the running policy does not define
// would fail every VM start with a denial that names nothing.
//
// file_contexts is the wrong oracle — it maps paths to types, and no path
// defaults to svirt_image_t, which libvirt applies dynamically to the devices it
// hands a domain. /sys/fs/selinux/context is the kernel's own validator.
func TestKVMFRLabelIsUsableOnThisHost(t *testing.T) {
	const validator = "/sys/fs/selinux/context"
	if _, err := os.Stat(validator); err != nil {
		t.Skipf("no SELinux on this host: %v", err)
	}
	if err := os.WriteFile(validator, []byte(hooks.KVMFRLabel), 0o644); err != nil {
		t.Errorf("the running policy rejects %q, so the hook could not label %s: %v",
			hooks.KVMFRLabel, steps.KVMFRDevice, err)
	}
	// Prove the validator would have caught a bad type, so a pass means something.
	if err := os.WriteFile(validator, []byte("system_u:object_r:orthogonals_not_a_type_t:s0"), 0o644); err == nil {
		t.Error("the context validator accepted a type that cannot exist")
	}
}

func isKebab(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// hostConditional lists fixture paths whose absence says something about how
// this host is configured rather than that the fixture is wrong. Every other
// path the reference fixture writes must exist here.
var hostConditional = map[string]string{
	"sys/class/iommu/dmar0/intel-iommu/cap":                                    "no VT-d, or the IOMMU is off in firmware",
	"sys/firmware/acpi/tables/DMAR":                                            "AMD hosts expose IVRS instead",
	"sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c": "BIOS boot, or efivarfs not mounted",
	"sys/devices/cpu_core/cpus":                                                "not a hybrid (P-core/E-core) CPU",
	"sys/devices/cpu_atom/cpus":                                                "not a hybrid (P-core/E-core) CPU",
	"proc/driver/nvidia/version":                                               "the NVIDIA driver is not loaded",
	"sys/module/nvidia_drm/parameters/modeset":                                 "nvidia_drm is not loaded",
	"sys/module/nvidia_drm/parameters/fbdev":                                   "nvidia_drm is not loaded",
	hwtest.MokListRTPath:                                                       "BIOS boot, or a kernel signed into db with no shim",
	"var/lib/dkms/mok.pub":                                                     "dkms has built nothing on this host",
	"var/lib/dkms/mok.key":                                                     "dkms has built nothing on this host",
}

// TestFixtureAttributesExistOnRealHardware requires every file the reference
// fixture synthesizes to correspond to something this machine publishes. PCI
// attributes are matched by device class, not by address — sysfs attribute
// names come from the bus and the driver core, not from the vendor, so any real
// display-class device answers for a fixture display-class device. Everything
// outside sys/bus/pci is checked at its literal path.
func TestFixtureAttributesExistOnRealHardware(t *testing.T) {
	fixture := t.TempDir()
	if err := hwtest.BuildReferenceRoot(fixture); err != nil {
		t.Fatalf("build the reference fixture: %v", err)
	}
	real, err := hw.ScanPCI(realRoot)
	if err != nil {
		t.Fatalf("scan real PCI devices: %v", err)
	}

	const pciPrefix = "sys/bus/pci/devices/"
	seenAttrs := map[string]bool{} // class prefix + "/" + attr, deduped across devices
	err = filepath.WalkDir(fixture, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixture, path)
		if err != nil || rel == "." {
			return err
		}
		switch {
		case strings.HasPrefix(rel, pciPrefix):
			// Only the first entry below the device directory is meaningful:
			// deeper paths (drm/card0/card0-DP-1/status) carry card and
			// connector names that legitimately differ per machine.
			parts := strings.SplitN(strings.TrimPrefix(rel, pciPrefix), "/", 3)
			if len(parts) < 2 {
				return nil
			}
			addr, attr := parts[0], parts[1]
			class := readFixtureClass(t, fixture, addr)
			if key := class + "/" + attr; !seenAttrs[key] {
				seenAttrs[key] = true
				checkPCIAttr(t, real, addr, class, attr)
			}
			if len(parts) == 3 {
				return fs.SkipDir
			}
		case d.IsDir():
		default:
			checkHostPath(t, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the fixture: %v", err)
	}
}

func readFixtureClass(t *testing.T, fixture, addr string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixture, "sys/bus/pci/devices", addr, "class"))
	if err != nil {
		t.Fatalf("fixture device %s has no class attribute: %v", addr, err)
	}
	return strings.TrimSpace(string(b))
}

// checkPCIAttr passes as soon as any real device of the same class publishes attr.
func checkPCIAttr(t *testing.T, real []hw.PCIDevice, addr, class, attr string) {
	t.Helper()
	// The class prefix is the pair of hex digits naming the base class: 0x03
	// display, 0x04 multimedia. Matching the full six digits would demand a
	// host with the exact same programming interface.
	base := class[:min(len(class), 4)]
	var peers []string
	for _, d := range real {
		if !strings.HasPrefix(d.Class, base) {
			continue
		}
		peers = append(peers, d.Address)
		if _, err := os.Lstat(filepath.Join("/sys/bus/pci/devices", d.Address, attr)); err == nil {
			return
		}
	}
	switch {
	case len(peers) == 0:
		t.Logf("skipped %s on %s: this host has no class-%s device", attr, addr, base)
	case attr == "iommu_group" && !iommuOn(t):
		t.Logf("skipped iommu_group: this host booted without an active IOMMU")
	default:
		t.Errorf("fixture device %s (class %s) has %q, but no real class-%s device does (%s) — "+
			"the fixture models something sysfs does not publish",
			addr, class, attr, base, strings.Join(peers, ", "))
	}
}

func checkHostPath(t *testing.T, rel string) {
	t.Helper()
	// BLS entry filenames carry the kernel version, so only the directory is
	// a fixed claim about the host.
	if dir := filepath.Dir(rel); dir == "boot/loader/entries" {
		rel = dir
	}
	switch _, err := os.Lstat(filepath.Join(realRoot, rel)); {
	case err == nil:
		return
	case errors.Is(err, fs.ErrPermission):
		// /etc/libvirt is 0700, so an unprivileged run cannot tell absent from
		// unreadable — the same limit preflight records as Facts.BLSUnreadable.
		t.Logf("skipped /%s: unreadable without root", rel)
		return
	}
	if why, ok := hostConditional[rel]; ok {
		t.Logf("skipped /%s: %s", rel, why)
		return
	}
	t.Errorf("the fixture writes /%s, which does not exist on this host — "+
		"either the fixture is fiction or the path belongs in hostConditional", rel)
}

func iommuOn(t *testing.T) bool {
	t.Helper()
	active, err := hw.IOMMUActive(realRoot)
	if err != nil {
		t.Fatalf("read iommu groups: %v", err)
	}
	return active
}

// TestRealAttributesTheFixturesNeverModel is advisory and never fails: hardware
// differs, so an unmodelled attribute is not a defect, only a candidate.
func TestRealAttributesTheFixturesNeverModel(t *testing.T) {
	fixture := t.TempDir()
	if err := hwtest.BuildReferenceRoot(fixture); err != nil {
		t.Fatalf("build the reference fixture: %v", err)
	}
	res := detectHost(t)
	modelled := map[string]bool{}
	for _, addr := range []string{"0000:00:02.0", "0000:01:00.0", "0000:01:00.1"} {
		entries, err := os.ReadDir(filepath.Join(fixture, "sys/bus/pci/devices", addr))
		if err != nil {
			continue
		}
		for _, e := range entries {
			modelled[e.Name()] = true
		}
	}
	for _, d := range res.Devices {
		if !strings.HasPrefix(d.Class, "0x03") {
			continue
		}
		entries, err := os.ReadDir(filepath.Join("/sys/bus/pci/devices", d.Address))
		if err != nil {
			continue
		}
		var extra []string
		for _, e := range entries {
			if !modelled[e.Name()] {
				extra = append(extra, e.Name())
			}
		}
		t.Logf("%s (%s) publishes %d attributes the fixtures do not model: %s",
			d.Address, d.VendorDeviceID(), len(extra), strings.Join(extra, " "))
	}
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func validateSchema(t *testing.T, name string, doc []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "schema", name+".schema.json")
	sch, err := jsonschema.NewCompiler().Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", path, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if err := sch.Validate(inst); err != nil {
		t.Fatalf("real-hardware output violates %s:\n%v", path, err)
	}
}

// TestMOKListAgreesWithMokutil is where this code proves the EFI_SIGNATURE_LIST
// layout. Every other signing test builds its own bytes, so all of them agree
// with the same misreading of UEFI 2.10 §32.4.1.
func TestMOKListAgreesWithMokutil(t *testing.T) {
	if _, err := exec.LookPath("mokutil"); err != nil {
		t.Skipf("mokutil not installed: %v", err)
	}
	f := preflight.GatherFacts(realRoot)
	if f.Signing.DKMS.Cert == "" {
		t.Skip("dkms has built nothing on this host, so there is no key to compare")
	}
	// mokutil exits 1 when the key IS enrolled, so only the output can be read.
	out, _ := exec.Command("mokutil", "--test-key", f.Signing.DKMS.Cert).Output()
	want := strings.Contains(string(out), "is already enrolled")
	if f.Signing.DKMS.Enrolled != want {
		t.Errorf("MokListRT parse says enrolled=%v for %s; mokutil says %v",
			f.Signing.DKMS.Enrolled, f.Signing.DKMS.Cert, want)
	}
}
