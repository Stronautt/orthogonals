package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestDispatchKnownCommands(t *testing.T) {
	commands := []string{
		"detect", "preflight", "apply", "undo", "vm",
		"media", "verify", "up", "status", "recover", "bundle", "version",
	}
	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			t.Setenv("PATH", hwtest.FakeTools(t, hw.RequiredTools...))
			args, want := []string{cmd}, 0
			switch cmd {
			case "detect":
				// implemented commands read sysfs; keep tests host-independent
				args = append(args, "--root", hwtest.ReferenceRoot(t))
			case "apply":
				// dry-run diffs read the current files, which include
				// root-only paths like /etc/libvirt/hooks on a libvirt host;
				// --user pinned because CI runners export no USER/SUDO_USER
				args = append(args, "--root", hwtest.ReferenceRoot(t), "--user", "tester")
			case "preflight":
				args = append(args, "--root", hwtest.ReferenceRoot(t))
				want = 2 // reference machine warns (39-bit address width)
			case "bundle":
				args = append(args, "--root", hwtest.ReferenceRoot(t),
					filepath.Join(t.TempDir(), "bundle.tar.gz"))
			case "vm":
				args = append(args, "--root", hwtest.ReferenceRoot(t), "--win11-iso", "/isos/Win11.iso", "define")
			case "media":
				iso := filepath.Join(t.TempDir(), "win11.iso")
				if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--root", t.TempDir(), "--win11-iso", iso)
			case "undo":
				// fresh root: "nothing applied" — never the host's real journal
				args = append(args, "--root", t.TempDir())
			case "up":
				// dry run prints the pipeline plan
				args = append(args, "--root", t.TempDir())
			case "status":
				// nothing applied on a fresh root → unhealthy
				args = append(args, "--root", t.TempDir())
				want = 1
			case "recover":
				// dry run only prints the plan
				args = append(args, "--root", hwtest.ReferenceRoot(t))
			case "verify":
				// virsh stub answers agent pings but the in-guest command
				// exits 1, so verify fails fast at the nvidia-smi check;
				// --vm-name pinned so the host's real /etc/orthogonals/vms
				// is never consulted
				args = append(args, "--vm-name", "win11")
				dir := t.TempDir()
				script := `#!/bin/sh
case "$*" in
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":1,"out-data":"","err-data":""}}' ;;
*) echo '{"return":{}}' ;;
esac
`
				if err := os.WriteFile(filepath.Join(dir, "virsh"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				want = 1
			}
			code, _, stderr := run(t, args...)
			if code != want {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, want, stderr)
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr should contain usage, got: %q", stderr)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Fatalf("stderr should name the unknown command, got: %q", stderr)
	}
}

func TestNoCommand(t *testing.T) {
	code, _, stderr := run(t)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Fatalf("stderr should contain usage, got: %q", stderr)
	}
}

func TestVersionOutput(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, Version) {
		t.Fatalf("stdout %q should contain version %q", stdout, Version)
	}
}

func TestGlobalFlags(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	tests := []struct {
		name string
		args []string
	}{
		{"before command", []string{"--json", "--yes", "--root", root, "detect"}},
		{"after command", []string{"detect", "--json", "--yes", "--root", root}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := run(t, tt.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
			}
		})
	}
}

func TestBadFlag(t *testing.T) {
	code, _, _ := run(t, "--no-such-flag", "detect")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
