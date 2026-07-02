package cli

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/preflight"
)

func allTools(t *testing.T) string {
	t.Helper()
	return hwtest.FakeTools(t, hw.RequiredTools...)
}

// The reference machine passes every gate but warns (39-bit address width,
// Secure Boot, inactive default network) -> exit 2.
func TestPreflightReferenceWarns(t *testing.T) {
	t.Setenv("PATH", allTools(t))
	code, stdout, stderr := run(t, "preflight", "--root", hwtest.ReferenceRoot(t))
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stdout: %q, stderr: %q)", code, stdout, stderr)
	}
	for _, want := range []string{"WARN", "39", "PASS", "preflight:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestPreflightFailJSON(t *testing.T) {
	t.Setenv("PATH", allTools(t))
	root := hwtest.ReferenceRoot(t)
	// no active IOMMU AND no ACPI DMAR table = VT-d off in firmware, the
	// hard-fail case (inactive-but-capable is only a Warn: apply fixes it)
	if err := os.RemoveAll(filepath.Join(root, "sys/class/iommu")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "sys/firmware/acpi/tables/DMAR")); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ := run(t, "preflight", "--root", root, "--json")
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 with VT-d absent", code)
	}
	var report struct {
		Status preflight.Status  `json:"status"`
		Checks []preflight.Check `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if report.Status != preflight.Fail {
		t.Errorf("status = %v, want fail", report.Status)
	}
	if len(report.Checks) == 0 {
		t.Error("checks array is empty")
	}
}

func TestPreflightDetectError(t *testing.T) {
	code, _, stderr := run(t, "preflight", "--root", t.TempDir())
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for missing sysfs tree", code)
	}
	if !strings.Contains(stderr, "preflight") {
		t.Errorf("stderr should mention preflight, got: %q", stderr)
	}
}

func TestBundleCommand(t *testing.T) {
	t.Setenv("PATH", allTools(t))
	out := filepath.Join(t.TempDir(), "bundle.tar.gz")
	code, stdout, stderr := run(t, "bundle", "--root", hwtest.ReferenceRoot(t), out)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if !strings.Contains(stdout, out) {
		t.Errorf("stdout should name the output file, got: %q", stdout)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gzip.NewReader(bytes.NewReader(data)); err != nil {
		t.Errorf("output is not a gzip file: %v", err)
	}
}

func TestBundleDetectError(t *testing.T) {
	code, _, stderr := run(t, "bundle", "--root", t.TempDir(),
		filepath.Join(t.TempDir(), "bundle.tar.gz"))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for missing sysfs tree", code)
	}
	if !strings.Contains(stderr, "bundle") {
		t.Errorf("stderr should mention bundle, got: %q", stderr)
	}
}
