package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
	"github.com/stronautt/orthogonals/internal/virt"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// TestMain swaps the libvirt and systemd seams for package-wide fakes. The
// swaps precede testscript.Main so a script's command subprocess inherits
// them; Main exits the process itself.
func TestMain(m *testing.M) {
	newVirt = func() virt.Client { return &virttest.Fake{} }
	newSysd = func() sysd.Client { return &sysdtest.Fake{} }
	notify.Send = func(notify.Notification) {}
	executablePath = func() (string, error) { return "/usr/bin/orthogonals", nil }
	hooks.LogWriter = io.Discard
	testscript.Main(m, map[string]func(){
		"orthogonals": func() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr)) },
	})
}

// fakeVirt routes the package's libvirt seam at the given fake for one test.
func fakeVirt(t *testing.T, f *virttest.Fake) *virttest.Fake {
	t.Helper()
	old := newVirt
	newVirt = func() virt.Client { return f }
	t.Cleanup(func() { newVirt = old })
	return f
}

// fakeSysd routes the package's systemd seam at the given fake for one test.
func fakeSysd(t *testing.T, f *sysdtest.Fake) *sysdtest.Fake {
	t.Helper()
	old := newSysd
	newSysd = func() sysd.Client { return f }
	t.Cleanup(func() { newSysd = old })
	return f
}

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
				args = append(args, "--root", hwtest.ReferenceRoot(t))
			case "apply":
				args = append(args, "--root", hwtest.ReferenceRoot(t), "--user", "tester")
			case "preflight":
				args = append(args, "--root", hwtest.ReferenceRoot(t))
				want = 2
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
				args = append(args, "--root", t.TempDir())
			case "up":
				args = append(args, "--root", t.TempDir())
			case "status":
				args = append(args, "--root", t.TempDir())
				want = 1
			case "recover":
				args = append(args, "--root", hwtest.ReferenceRoot(t))
			case "verify":
				args = append(args, "--vm-name", "win11")
				fakeVirt(t, &virttest.Fake{State: "running", Agent: virttest.Responder("", "", 1)})
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
	if !strings.Contains(stderr, "unknown command") {
		t.Fatalf("stderr should report an unknown command, got: %q", stderr)
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

// TestNoCommandIgnoresTheProcessArguments pins Run's nil-args guard: cobra
// falls back to os.Args[1:] when SetArgs is handed nil, so a caller asking for
// "no arguments" would dispatch on whatever the process was started with.
func TestNoCommandIgnoresTheProcessArguments(t *testing.T) {
	old := os.Args
	os.Args = []string{"orthogonals", "--definitely-not-a-flag"}
	t.Cleanup(func() { os.Args = old })

	code, _, stderr := run(t)
	if code != 2 || !strings.Contains(stderr, "Usage:") {
		t.Fatalf("Run(nil) consulted os.Args: exit %d, stderr %q", code, stderr)
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
