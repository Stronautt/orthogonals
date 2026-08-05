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

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
	"github.com/stronautt/orthogonals/internal/virt"
)

// launch poll bounds; package vars so tests can shrink them.
var (
	launchTimeout      = 60 * time.Second
	launchPollInterval = time.Second
)

var execProcess = syscall.Exec

func vmLaunch(cfg *Config, c virt.Client, name string, stdout, stderr io.Writer) int {
	title := displayName(cfg.Root, name)
	fail := func(format string, a ...any) int {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintf(stderr, "orthogonals vm launch: %s\n", msg)
		if !utils.IsTerminal(stderr) {
			notify.Send(notify.Notification{Title: title, Icon: "orthogonals", Body: msg})
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
		// Refuse here so the reason reads as a CLI error rather than reaching the
		// user wrapped in a libvirt domain-start failure. The hook gate is what
		// makes it safe; this only makes it legible.
		if err := hooks.CheckIOMMUGroups(cfg.Root); err != nil {
			return fail("%v", err)
		}
		if code, ok := ensureMemory(cfg.Root, c, name, fail); !ok {
			return code
		}
		if err := c.StartDomain(name); err != nil {
			// The hook has already notified for these; print its reason verbatim
			// rather than burying it in "starting <vm>: ...". Matching on text is
			// forced by the hook → libvirtd → RPC boundary it crossed.
			if strings.Contains(err.Error(), "gpu-detach: ") || strings.Contains(err.Error(), hooks.KVMFRErrPrefix) {
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
	// port "0" is the client's unix-socket signal, so host is a path.
	if port == "0" {
		fmt.Fprintf(stdout, "connecting to %s over %s\n", name, host)
	} else {
		fmt.Fprintf(stdout, "connecting to %s at %s:%s\n", name, host, port)
	}
	// Title and app-id are what the shell shows in the dock; the app-id must be
	// the desktop entry's basename or the window falls back to the binary name.
	args := []string{"looking-glass-client", "-F", "-c", host, "-p", port,
		"win:title=" + title, "win:appId=" + hostcfg.DesktopAppID(name)}
	// The client prefers /dev/kvmfr0 whenever it exists, so a module left loaded
	// by an earlier VM would hijack a /dev/shm domain and wait for a host that
	// never arrives. The backend is read from libvirt, not /etc/orthogonals/vms:
	// that copy is 0600 root and launch runs as the desktop user, so its EACCES
	// would read as "not kvmfr" and point the client at an empty buffer.
	desc, err := c.DomainXML(name)
	if err != nil {
		return fail("reading %s XML: %v", name, err)
	}
	if _, ok := domain.KVMFRSizeXML([]byte(desc)); !ok {
		args = append(args, "-f", steps.LookingGlassSHM)
	}
	if err := execProcess(lg, args, os.Environ()); err != nil {
		return fail("exec looking-glass-client: %v", err)
	}
	return 0
}

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

// executablePath refuses a temp-dir path; a var so tests can stand in for a
// binary they did not install.
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
