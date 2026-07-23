package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/virt"
)

// launch poll bounds; package vars so tests can shrink them.
var (
	launchTimeout      = 60 * time.Second
	launchPollInterval = time.Second
)

// execProcess replaces the current process with looking-glass-client.
var execProcess = syscall.Exec

// vmLaunch starts the VM if needed and hands off to looking-glass-client.
func vmLaunch(cfg *Config, c virt.Client, name string, stdout, stderr io.Writer) int {
	displayName := resolveDisplayName(cfg.Root, name, "")
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintf(stderr, "orthogonals vm launch: %s\n", msg)
		if !isTerminal(stderr) {
			notify.Send(notify.Notification{Title: displayName, Icon: "computer", Body: msg})
		}
		return 1
	}

	state, err := c.DomainState(name)
	if err != nil {
		if virt.IsNotFound(err) {
			return fail("no such VM %q — define it first with `orthogonals vm define`", name)
		}
		return fail("query %s (is libvirtd running?): %v", name, err)
	}
	if !virt.Live(state) {
		if code, ok := ensureMemory(cfg.Root, c, name, fail); !ok {
			return code
		}
		if err := c.StartDomain(name); err != nil {
			if strings.Contains(err.Error(), "gpu-detach: ") {
				fmt.Fprintf(stderr, "orthogonals vm launch: %v\n", err)
				return 1
			}
			return fail("starting %s: %v", name, err)
		}
	}

	host, port, err := waitForDisplay(c, name)
	if err != nil {
		return fail("%s has no SPICE display after %s — check `virsh domstate %s`", name, launchTimeout, name)
	}

	lg, err := exec.LookPath("looking-glass-client")
	if err != nil {
		return fail("looking-glass-client not found on PATH — install the looking-glass-client package")
	}
	fmt.Fprintf(stdout, "connecting to %s at %s:%s\n", name, host, port)
	if err := execProcess(lg, []string{"looking-glass-client", "-F", "-c", host, "-p", port}, os.Environ()); err != nil {
		return fail("exec looking-glass-client: %v", err)
	}
	return 0
}

// ensureMemory refuses the start when the host has less free RAM than the guest pins.
func ensureMemory(root string, c virt.Client, name string, fail func(string, ...any) int) (int, bool) {
	needKiB, err := c.DomainMaxMemoryKiB(name)
	if err != nil {
		return fail("reading %s memory config: %v", name, err), false
	}
	availKiB := hw.MeminfoKiB(root, "MemAvailable:")
	if availKiB != 0 && availKiB < needKiB {
		return fail("not enough free memory: %s needs %d GiB, only %d GiB available — close some apps",
			name, needKiB>>20, availKiB>>20), false
	}
	return 0, true
}

// waitForDisplay polls until libvirt reports a SPICE port or the timeout hits.
func waitForDisplay(c virt.Client, name string) (host, port string, err error) {
	deadline := time.Now().Add(launchTimeout)
	for {
		host, port, err = c.DomainDisplay(name)
		if err == nil {
			return host, port, nil
		}
		if !time.Now().Before(deadline) {
			return "", "", err
		}
		time.Sleep(launchPollInterval)
	}
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// executablePath resolves the orthogonals binary path, refusing a temp-dir path.
var executablePath = func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.HasPrefix(exe, os.TempDir()) {
		return "", fmt.Errorf("orthogonals runs from a temporary path (%s) — install it (make install or the RPM) before defining VMs", exe)
	}
	return exe, nil
}
