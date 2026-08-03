package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// synthetic is any non-empty --root: it turns off the ownership walk, covered in
// internal/steps where its seam lives.
const synthetic = "/synthetic-root"

func TestShimStep(t *testing.T) {
	s, err := ShimStep(synthetic, "tester", "/usr/bin/orthogonals")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != DispatcherStepID || s.Path != "/etc/libvirt/hooks/qemu" || s.Mode != 0o755 {
		t.Errorf("shim step = %s %s %o", s.ID, s.Path, s.Mode)
	}
	want := "exec /usr/bin/orthogonals hook --user tester qemu \"$@\"\n"
	if !strings.Contains(string(s.Content), want) {
		t.Errorf("shim content missing %q:\n%s", want, s.Content)
	}
	if !strings.HasPrefix(string(s.Content), "#!/bin/sh\n") {
		t.Errorf("shim must start with a shebang:\n%s", s.Content)
	}
	if got := InstalledPaths(); len(got) != 1 || got[0] != s.Path {
		t.Errorf("InstalledPaths() = %v, want [%s]", got, s.Path)
	}
}

func TestShimStepRefusals(t *testing.T) {
	if _, err := ShimStep(synthetic, "", "/usr/bin/orthogonals"); err == nil {
		t.Error("empty user must be refused")
	}
	if _, err := ShimStep(synthetic, "tester", ""); err == nil {
		t.Error("empty exe must be refused")
	}
	if _, err := ShimStep(synthetic, "tester", "/opt/my app/orthogonals"); err == nil {
		t.Error("exe with a space must be refused, not shell-quoted")
	}
	if _, err := ShimStep(synthetic, "tester", "/usr/bin/orth;rm -rf /"); err == nil {
		t.Error("exe with shell metacharacters must be refused")
	}
}

// libvirtd execs this shim as root, so a binary a non-root user could replace
// must never reach the file.
func TestShimStepRefusesAnUntrustedExeOnARealHost(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "orthogonals")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ShimStep("", "tester", exe); err == nil {
		t.Errorf("shim accepted %s, which is not on a root-owned path", exe)
	}
}
