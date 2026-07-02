package orchestrate

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

// Vars so tests run fast. Windows boots in minutes, not seconds, before the
// guest agent answers; the GPU reattach settles moments after shutdown.
var (
	pingTries        = 90
	pingInterval     = 5 * time.Second
	shutdownTries    = 60
	shutdownInterval = 5 * time.Second
	idleTries        = 6
	idleInterval     = 5 * time.Second
)

// Verify runs the end-to-end checks (plan Task 11): VM starts, guest agent
// answers, the guest driver sees the GPU (nvidia-smi), the display pipeline
// (VDD + Looking Glass host service) is up, the VM shuts down
// cleanly, and the reattached host GPU is idle. Each check reports pass or
// fail; the first failure stops the sequence, since later checks depend on
// earlier ones. Under static binding the GPU stays on vfio-pci by design, so
// the host-idle check (which needs the host NVIDIA driver) is skipped.
// displayCheck asserts the capture path from inside the guest: the VDD device
// node provisioning created (ROOT\MttVDD) is bound and error-free, and the
// Looking Glass host service is running. nvidia-smi cannot see either — a
// healthy render GPU with a dead capture path shows a black Looking Glass
// window. Single quotes only: the script rides as one argv element through
// guest-exec's JSON.
// TODO(refactor): a crash-looping service can transiently report Running; parse the
// LG host log if that ever bites.
var displayCheck = fmt.Sprintf(
	`if (-not (Get-PnpDevice | Where-Object { $_.HardwareID -contains '%[1]s' -and $_.Status -eq 'OK' })) { 'no healthy VDD display adapter (%[1]s)'; exit 1 }; $svc = Get-Service '%[2]s' -ErrorAction SilentlyContinue; if (-not $svc -or $svc.Status -ne 'Running') { '%[2]s service is not running'; exit 1 }`,
	media.VDDHardwareID, media.LGHostServiceName)

func Verify(root, vm string, out io.Writer) error {
	fail := func(name string, err error) error {
		fmt.Fprintf(out, "FAIL %s: %v\n", name, err)
		return fmt.Errorf("check %q failed: %w", name, err)
	}
	pass := func(name string) { fmt.Fprintf(out, "PASS %s\n", name) }

	if _, err := ensureRunning(vm, out); err != nil {
		return fail("vm start", err)
	}
	pass("vm start")

	if err := agentPing(vm); err != nil {
		return fail("guest agent ping", err)
	}
	pass("guest agent ping")

	smiOut, smiErr, code, err := media.GuestExec(vm, `C:\Windows\System32\nvidia-smi.exe`)
	if err != nil {
		return fail("guest nvidia-smi", err)
	}
	if code != 0 {
		return fail("guest nvidia-smi", fmt.Errorf("exit %d — the guest driver does not see the GPU\n%s", code,
			bytes.TrimSpace(append(smiOut, smiErr...))))
	}
	pass("guest nvidia-smi")

	dispOut, dispErr, code, err := media.GuestExec(vm, "powershell.exe", "-NoProfile", "-Command", displayCheck)
	if err != nil {
		return fail("guest display pipeline", err)
	}
	if code != 0 {
		return fail("guest display pipeline", fmt.Errorf("exit %d — %s", code,
			bytes.TrimSpace(append(dispOut, dispErr...))))
	}
	pass("guest display pipeline")

	if err := shutdown(vm, out); err != nil {
		return fail("clean shutdown", err)
	}
	pass("clean shutdown")

	if staticBinding(root) {
		fmt.Fprintln(out, "SKIP host gpu idle (static binding — the GPU stays on vfio-pci)")
	} else {
		if err := hostGPUIdle(); err != nil {
			return fail("host gpu idle", err)
		}
		pass("host gpu idle")
	}
	fmt.Fprintln(out, "verify: all checks passed")
	return nil
}

// staticBinding reads the journaled kernel-args step: vfio-pci.ids= means the
// host was applied with --binding=static.
func staticBinding(root string) bool {
	args, err := manifestKernelArgs(root)
	return err == nil && strings.Contains(args, hostcfg.VFIOIDsPrefix)
}

// agentPing waits for the qemu guest agent, which only answers once Windows
// has booted and the virtio tools service is up.
func agentPing(vm string) error {
	var err error
	for range pingTries {
		if err = media.AgentPing(vm); err == nil {
			return nil
		}
		time.Sleep(pingInterval)
	}
	return fmt.Errorf("guest agent did not answer within %v: %w", time.Duration(pingTries)*pingInterval, err)
}

// shutdown asks the guest to power off and waits — a clean shutdown proves
// the release hook ran and handed the GPU back.
func shutdown(vm string, out io.Writer) error {
	if err := virsh(out, "shutdown", vm); err != nil {
		return err
	}
	for range shutdownTries {
		if steps.DomainState(vm) == "shut off" {
			return nil
		}
		time.Sleep(shutdownInterval)
	}
	return fmt.Errorf("VM did not shut off within %v", time.Duration(shutdownTries)*shutdownInterval)
}

// idleFloorMiB: the driver reserves a megabyte or two on an idle card even
// with no processes attached, so "used == 0" is not a healthy-host invariant.
// A leaked VM or a process still holding the GPU shows up as hundreds of MiB.
const idleFloorMiB = 64

// hostGPUIdle asserts the reattached GPU carries no leftover VM state: host
// nvidia-smi reports no more than the driver's idle reservation. Retries
// because the reattach hook finishes moments after libvirt reports the domain
// off.
func hostGPUIdle() error {
	var last error
	for range idleTries {
		out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used", "--format=csv,noheader,nounits").Output()
		if err != nil {
			last = fmt.Errorf("host nvidia-smi: %w", err)
		} else {
			last = nil
			for _, used := range strings.Fields(strings.TrimSpace(string(out))) {
				mib, err := strconv.Atoi(used)
				if err != nil {
					last = fmt.Errorf("host nvidia-smi reported %q, want a MiB count", used)
					break
				}
				if mib > idleFloorMiB {
					last = fmt.Errorf("host GPU reports %d MiB used — a process may still hold it (fuser -v /dev/nvidia*)", mib)
				}
			}
			if last == nil {
				return nil
			}
		}
		time.Sleep(idleInterval)
	}
	return last
}
