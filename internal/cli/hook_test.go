package cli

import (
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
