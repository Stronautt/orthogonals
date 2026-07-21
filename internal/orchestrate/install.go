package orchestrate

import (
	"fmt"
	"io"
	"time"

	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/virt"
)

// Tunables are vars so tests run fast.
var (
	installTimeout    = 2 * time.Hour
	installInterval   = 15 * time.Second
	heartbeatInterval = time.Minute
	cdPromptWindow    = 3 * time.Minute
	cdPromptInterval  = 500 * time.Millisecond
	// provisionFailGrace tolerates a stale failed status before treating it as this run's result.
	provisionFailGrace = 5 * time.Minute
)

// setupWritingBytes marks that Windows setup has booted and is writing.
const setupWritingBytes = 64 << 20

// Install boots the VM and polls until provisioning reports done.
func Install(c virt.Client, vm string, out io.Writer) error {
	start := time.Now()
	deadline := start.Add(installTimeout)
	fmt.Fprintf(out, "the install runs on a temporary emulated display — watch live with: virt-viewer %s\n", vm)
	wasRunning, err := ensureRunning(c, vm, out)
	if err != nil {
		return err
	}
	if wasRunning && diskPhysBytes(c, vm) <= setupWritingBytes {
		fmt.Fprintln(out, "VM is running but setup never started — rebooting it to retry the boot-from-CD prompt")
		if err := c.DestroyDomain(vm); err != nil {
			return err
		}
		if err := startDomain(c, vm, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "unattended install + provisioning (up to 2 h) — a heartbeat prints every minute")
	answerCDPrompt(c, vm, out)
	lastStage := ""
	lastBeat := start
	var failedSince time.Time
	for time.Now().Before(deadline) {
		st, err := media.ProvisionStatus(c, vm)
		if err == nil {
			if !st.OK {
				if failedSince.IsZero() {
					failedSince = time.Now()
					fmt.Fprintf(out, "guest reports stage %s failed — waiting %v for a provisioning re-run to supersede it\n", st.Stage, provisionFailGrace)
				} else if time.Since(failedSince) >= provisionFailGrace {
					return fmt.Errorf("provisioning failed at stage %s: %s", st.Stage, st.Error)
				}
			} else {
				failedSince = time.Time{}
				if st.Done() {
					fmt.Fprintln(out, "provisioning complete")
					return nil
				}
				if st.Stage != lastStage {
					fmt.Fprintf(out, "provisioning: stage %s\n", st.Stage)
					lastStage = st.Stage
					lastBeat = time.Now()
				}
			}
		}
		if state, err := c.DomainState(vm); err == nil && state == "shut off" {
			fmt.Fprintln(out, "VM powered off (Windows setup quirk) — restarting")
			if err := startDomain(c, vm, out); err != nil {
				return err
			}
		}
		if time.Since(lastBeat) >= heartbeatInterval {
			phase := "Windows setup running (guest agent not up yet)"
			if lastStage != "" {
				phase = "stage " + lastStage + " in progress"
			}
			beat := fmt.Sprintf("  … %s — %d min elapsed", phase, int(time.Since(start).Minutes()))
			if gib := diskWritten(c, vm); gib != "" {
				beat += ", " + gib + " written"
			}
			fmt.Fprintln(out, beat)
			lastBeat = time.Now()
		}
		time.Sleep(installInterval)
	}
	return fmt.Errorf("the Windows install/provisioning did not finish within %v — inspect the guest console (virt-manager) for a stuck setup, then re-run `orthogonals up --yes` to resume", installTimeout)
}

// ensureRunning starts the domain if needed, reporting whether it was already running.
func ensureRunning(c virt.Client, vm string, out io.Writer) (wasRunning bool, err error) {
	if state, err := c.DomainState(vm); err == nil && state == "running" {
		return true, nil
	}
	return false, startDomain(c, vm, out)
}

func startDomain(c virt.Client, vm string, out io.Writer) error {
	if err := c.StartDomain(vm); err != nil {
		return err
	}
	fmt.Fprintf(out, "domain %s started\n", vm)
	return nil
}

// answerCDPrompt presses a key at the guest's boot-from-CD prompt.
func answerCDPrompt(c virt.Client, vm string, out io.Writer) {
	if diskPhysBytes(c, vm) > setupWritingBytes {
		return
	}
	fmt.Fprintln(out, "answering the boot-from-CD prompt on the guest's behalf")
	deadline := time.Now().Add(cdPromptWindow)
	for time.Now().Before(deadline) {
		if diskPhysBytes(c, vm) > setupWritingBytes {
			fmt.Fprintln(out, "Windows setup is running")
			return
		}
		_ = c.SendKeyEnter(vm)
		time.Sleep(cdPromptInterval)
	}
}

// diskPhysBytes reports the domain disk's physical allocation.
func diskPhysBytes(c virt.Client, vm string) uint64 {
	n, err := c.DomainBlockPhysical(vm, "vda")
	if err != nil {
		return 0
	}
	return n
}

func diskWritten(c virt.Client, vm string) string {
	n := diskPhysBytes(c, vm)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
