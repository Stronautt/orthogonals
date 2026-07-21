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
	"github.com/stronautt/orthogonals/internal/virt"
)

// Vars so tests run fast.
var (
	pingTries        = 90
	pingInterval     = 5 * time.Second
	shutdownTries    = 60
	shutdownInterval = 5 * time.Second
	idleTries        = 6
	idleInterval     = 5 * time.Second
)

// displayCheck asserts the guest capture path: VDD bound and the LG host service running.
// TODO(refactor): a crash-looping service can transiently report Running.
var displayCheck = fmt.Sprintf(
	`if (-not (Get-PnpDevice | Where-Object { $_.HardwareID -contains '%[1]s' -and $_.Status -eq 'OK' })) { 'no healthy VDD display adapter (%[1]s)'; exit 1 }; $svc = Get-Service '%[2]s' -ErrorAction SilentlyContinue; if (-not $svc -or $svc.Status -ne 'Running') { '%[2]s service is not running'; exit 1 }`,
	media.VDDHardwareID, media.LGHostServiceName)

func Verify(c virt.Client, root, vm string, out io.Writer) error {
	fail := func(name string, err error) error {
		fmt.Fprintf(out, "FAIL %s: %v\n", name, err)
		return fmt.Errorf("check %q failed: %w", name, err)
	}
	pass := func(name string) { fmt.Fprintf(out, "PASS %s\n", name) }

	if _, err := ensureRunning(c, vm, out); err != nil {
		return fail("vm start", err)
	}
	pass("vm start")

	if err := agentPing(c, vm); err != nil {
		return fail("guest agent ping", err)
	}
	pass("guest agent ping")

	smiOut, smiErr, code, err := media.GuestExec(c, vm, `C:\Windows\System32\nvidia-smi.exe`)
	if err != nil {
		return fail("guest nvidia-smi", err)
	}
	if code != 0 {
		return fail("guest nvidia-smi", fmt.Errorf("exit %d — the guest driver does not see the GPU\n%s", code,
			bytes.TrimSpace(append(smiOut, smiErr...))))
	}
	pass("guest nvidia-smi")

	dispOut, dispErr, code, err := media.GuestExec(c, vm, "powershell.exe", "-NoProfile", "-Command", displayCheck)
	if err != nil {
		return fail("guest display pipeline", err)
	}
	if code != 0 {
		return fail("guest display pipeline", fmt.Errorf("exit %d — %s", code,
			bytes.TrimSpace(append(dispOut, dispErr...))))
	}
	pass("guest display pipeline")

	if err := shutdown(c, vm, out); err != nil {
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

// staticBinding reports whether the host was applied with --binding=static.
func staticBinding(root string) bool {
	args, err := manifestKernelArgs(root)
	return err == nil && strings.Contains(args, hostcfg.VFIOIDsPrefix)
}

// agentPing waits for the qemu guest agent.
func agentPing(c virt.Client, vm string) error {
	var err error
	for range pingTries {
		if err = media.AgentPing(c, vm); err == nil {
			return nil
		}
		time.Sleep(pingInterval)
	}
	return fmt.Errorf("guest agent did not answer within %v: %w", time.Duration(pingTries)*pingInterval, err)
}

// shutdown asks the guest to power off and waits.
func shutdown(c virt.Client, vm string, out io.Writer) error {
	if err := c.ShutdownDomain(vm); err != nil {
		return err
	}
	fmt.Fprintf(out, "domain %s shutdown requested\n", vm)
	for range shutdownTries {
		if state, err := c.DomainState(vm); err == nil && state == "shut off" {
			return nil
		}
		time.Sleep(shutdownInterval)
	}
	return fmt.Errorf("VM did not shut off within %v", time.Duration(shutdownTries)*shutdownInterval)
}

// idleFloorMiB is the driver's idle memory reservation floor.
const idleFloorMiB = 64

// hostGPUIdle asserts the reattached GPU carries no leftover VM state.
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
					last = fmt.Errorf("host GPU reports %d MiB used — a process may still hold /dev/nvidia* open", mib)
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
