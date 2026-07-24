package cli

import (
	"os"
	"testing"

	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
)

func TestHookUnmanagedPassesThrough(t *testing.T) {
	root := t.TempDir()
	sd := fakeSysd(t, &sysdtest.Fake{})
	code, _, stderr := run(t, "hook", "--root", root, "--user", "tester", "qemu", "ghost", "prepare", "begin", "-")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (unmanaged pass-through)\n%s", code, stderr)
	}
	if len(sd.Calls) != 0 {
		t.Errorf("unmanaged hook dialed systemd: %v", sd.Calls)
	}
}

// TestHookDefaultsAnEmptyPATH: libvirtd invokes hooks with a minimal
// environment, and modprobe, nvidia-smi and notify-send must resolve during
// the handover. The failure would land halfway through moving the GPU.
func TestHookDefaultsAnEmptyPATH(t *testing.T) {
	t.Setenv("PATH", "")
	fakeSysd(t, &sysdtest.Fake{})
	// Unmanaged domain: dispatch returns at once, so only the PreRun is under test.
	if code, _, stderr := run(t, "hook", "--root", t.TempDir(), "qemu", "ghost", "prepare", "begin"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := os.Getenv("PATH"); got != "/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("PATH = %q, want the hook's fallback", got)
	}
}

// TestHookKeepsAnInheritedPATH: the fallback must not override a PATH libvirtd
// did pass, or a host with tools outside the default directories breaks.
func TestHookKeepsAnInheritedPATH(t *testing.T) {
	t.Setenv("PATH", "/opt/custom/bin")
	fakeSysd(t, &sysdtest.Fake{})
	if code, _, stderr := run(t, "hook", "--root", t.TempDir(), "qemu", "ghost", "prepare", "begin"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := os.Getenv("PATH"); got != "/opt/custom/bin" {
		t.Errorf("PATH = %q, want the inherited value", got)
	}
}

func TestHookUsageErrors(t *testing.T) {
	cases := [][]string{
		{"hook"},
		{"hook", "qemu", "win11"},
		{"hook", "inhibit"},
		{"hook", "frobnicate"},
	}
	for _, args := range cases {
		t.Run(args[len(args)-1], func(t *testing.T) {
			if code, _, _ := run(t, args...); code != 2 {
				t.Errorf("%v: exit %d, want 2", args, code)
			}
		})
	}
}
