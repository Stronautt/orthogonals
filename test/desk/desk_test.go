//go:build desk

// detect, preflight, and status against the machine this is running on, via
// `make test-desk`. Read-only by construction — nothing here writes outside
// t.TempDir(), and no test calls apply.
//
// TestFixtureAttributesExistOnRealHardware is what this suite exists for: every
// golden in the suite derives from the hand-written hwtest.Roots, so nothing
// else can catch a fixture modelling an attribute sysfs does not publish.
package desk

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/preflight"
)

// realRoot is the running machine. Every other test passes a --root prefix;
// this one is the only place that does not.
const realRoot = "/"

// detectHost is hw.Detect against the real host, shared by the tests below.
func detectHost(t *testing.T) *hw.Result {
	t.Helper()
	r, err := hw.Detect(realRoot)
	if err != nil {
		t.Fatalf("detect on real hardware: %v", err)
	}
	return r
}

// TestJSONContractOnRealHardware validates the published schemas against live
// hardware: a schema that only ever sees synthetic input encodes the fixture's
// assumptions instead of the format's.
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

// TestPreflightContractHoldsOnRealHardware asserts the parts of the preflight
// contract that must hold for any host, not just the ones with a golden file.
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

// isKebab reports whether a check name is lowercase letters, digits, and dashes.
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
}

// TestFixtureAttributesExistOnRealHardware requires every file the reference
// fixture synthesizes to correspond to something this machine publishes.
//
// PCI attributes are matched by device class, not by address — sysfs attribute
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

// readFixtureClass reads one fixture device's class attribute.
func readFixtureClass(t *testing.T, fixture, addr string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixture, "sys/bus/pci/devices", addr, "class"))
	if err != nil {
		t.Fatalf("fixture device %s has no class attribute: %v", addr, err)
	}
	return strings.TrimSpace(string(b))
}

// checkPCIAttr requires some real device of the same class to publish attr.
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

// checkHostPath requires a non-PCI fixture file to exist at the same real path.
func checkHostPath(t *testing.T, rel string) {
	t.Helper()
	// BLS entry filenames carry the kernel version, so only the directory is
	// a fixed claim about the host.
	if dir := filepath.Dir(rel); dir == "boot/loader/entries" {
		rel = dir
	}
	if _, err := os.Lstat(filepath.Join(realRoot, rel)); err == nil {
		return
	}
	if why, ok := hostConditional[rel]; ok {
		t.Logf("skipped /%s: %s", rel, why)
		return
	}
	t.Errorf("the fixture writes /%s, which does not exist on this host — "+
		"either the fixture is fiction or the path belongs in hostConditional", rel)
}

// iommuOn reports whether the running kernel published any IOMMU groups.
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

// validateSchema asserts a document satisfies the published contract.
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
