package hooks

import (
	"strings"
	"testing"
)

func TestShimStep(t *testing.T) {
	s, err := ShimStep("tester", "/usr/bin/orthogonals")
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
	if _, err := ShimStep("", "/usr/bin/orthogonals"); err == nil {
		t.Error("empty user must be refused")
	}
	if _, err := ShimStep("tester", ""); err == nil {
		t.Error("empty exe must be refused")
	}
	if _, err := ShimStep("tester", "/opt/my app/orthogonals"); err == nil {
		t.Error("exe with a space must be refused, not shell-quoted")
	}
	if _, err := ShimStep("tester", "/usr/bin/orth;rm -rf /"); err == nil {
		t.Error("exe with shell metacharacters must be refused")
	}
}
