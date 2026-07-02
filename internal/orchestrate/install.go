package orchestrate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

// Tunables are vars so tests run fast; defaults sized for a real unattended
// Windows install plus driver provisioning on an SSD.
var (
	// installTimeout bounds the whole install+provision poll loop.
	installTimeout    = 2 * time.Hour
	installInterval   = 15 * time.Second
	heartbeatInterval = time.Minute
	cdPromptWindow    = 3 * time.Minute
	cdPromptInterval  = 500 * time.Millisecond
	// a failed provision-status.json survives in the guest until the logon
	// task's re-run overwrites it, so a resumed Install would read the previous
	// attempt's verdict and fail before the retry gets a chance — tolerate a
	// failed status this long (boot + autologon + script start) before
	// treating it as this run's result
	provisionFailGrace = 5 * time.Minute
)

// setupWritingBytes: a disk this far past a fresh qcow2's ~300 KiB of
// metadata means Windows setup booted and is writing — the CD prompt is
// behind us and further keypresses could answer a real dialog.
const setupWritingBytes = 64 << 20

// Install boots the VM and polls until provisioning reports done, handling
// the Windows-setup quirks (plan Task 11): the domain can power off instead
// of rebooting at setup's first reboot — restart it; the guest agent is
// silent until the virtio tools land in provisioning stage 1 — agent errors
// are polling noise, not failures, until the overall timeout.
func Install(vm string, out io.Writer) error {
	start := time.Now()
	deadline := start.Add(installTimeout)
	fmt.Fprintf(out, "the install runs on a temporary emulated display — watch live with: virt-viewer %s\n", vm)
	wasRunning, err := ensureRunning(vm, out)
	if err != nil {
		return err
	}
	// a domain left running by a failed attempt sits in OVMF's boot manager,
	// past the CD prompt with an empty disk; keypresses cannot rewind it
	if wasRunning && diskPhysBytes(vm) <= setupWritingBytes {
		fmt.Fprintln(out, "VM is running but setup never started — rebooting it to retry the boot-from-CD prompt")
		if err := virsh(out, "destroy", vm); err != nil {
			return err
		}
		if err := virsh(out, "start", vm); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "unattended install + provisioning (up to 2 h) — a heartbeat prints every minute")
	answerCDPrompt(vm, out)
	lastStage := ""
	lastBeat := start
	var failedSince time.Time
	for time.Now().Before(deadline) {
		st, err := media.ProvisionStatus(vm)
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
		// read status first: a guest that finished and powered itself off is
		// caught by Done() above, so only an un-done shut-off VM is the
		// Windows-setup power-off quirk that needs a restart
		if steps.DomainState(vm) == "shut off" {
			fmt.Fprintln(out, "VM powered off (Windows setup quirk) — restarting")
			if err := virsh(out, "start", vm); err != nil {
				return err
			}
		}
		if time.Since(lastBeat) >= heartbeatInterval {
			phase := "Windows setup running (guest agent not up yet)"
			if lastStage != "" {
				phase = "stage " + lastStage + " in progress"
			}
			beat := fmt.Sprintf("  … %s — %d min elapsed", phase, int(time.Since(start).Minutes()))
			if gib := diskWritten(vm); gib != "" {
				beat += ", " + gib + " written"
			}
			fmt.Fprintln(out, beat)
			lastBeat = time.Now()
		}
		time.Sleep(installInterval)
	}
	return fmt.Errorf("the Windows install/provisioning did not finish within %v — inspect the guest console (virt-manager) for a stuck setup, then re-run `orthogonals up --yes` to resume", installTimeout)
}

// ensureRunning starts the domain if needed, reporting whether it was already
// running — a caller that did not boot it cannot assume the guest is at the
// start of its boot sequence.
func ensureRunning(vm string, out io.Writer) (wasRunning bool, err error) {
	if steps.DomainState(vm) == "running" {
		return true, nil
	}
	return false, virsh(out, "start", vm)
}

// virshOut captures virsh output under LC_ALL=C — callers parse English
// tokens and virsh localizes.
func virshOut(args ...string) (string, error) {
	cmd := exec.Command("virsh", args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	return string(b), err
}

// answerCDPrompt presses a key for the guest at Microsoft's "Press any key to
// boot from CD or DVD" prompt. Retail media waits ~5 s and otherwise falls
// through to the (empty) disk, leaving OVMF parked in its boot manager — and
// the guest is defined with video=none, so no human can answer it. Keys stop
// the moment setup starts writing, so none can reach a real dialog.
func answerCDPrompt(vm string, out io.Writer) {
	if diskPhysBytes(vm) > setupWritingBytes {
		return
	}
	fmt.Fprintln(out, "answering the boot-from-CD prompt on the guest's behalf")
	deadline := time.Now().Add(cdPromptWindow)
	for time.Now().Before(deadline) {
		if diskPhysBytes(vm) > setupWritingBytes {
			fmt.Fprintln(out, "Windows setup is running")
			return
		}
		_ = exec.Command("virsh", "send-key", vm, "KEY_ENTER").Run()
		time.Sleep(cdPromptInterval)
	}
}

// diskPhysBytes reports the domain disk's physical allocation — during
// Windows setup it is the only progress signal visible from the host.
func diskPhysBytes(vm string) uint64 {
	out, err := virshOut("domblkinfo", vm, "vda")
	if err != nil {
		return 0
	}
	for line := range strings.SplitSeq(out, "\n") {
		if val, ok := strings.CutPrefix(line, "Physical:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

func diskWritten(vm string) string {
	n := diskPhysBytes(vm)
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}

func virsh(out io.Writer, args ...string) error {
	b, err := exec.Command("virsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("virsh %s: %w\n%s", strings.Join(args, " "), err, bytes.TrimSpace(b))
	}
	fmt.Fprintf(out, "virsh %s\n", strings.Join(args, " "))
	return nil
}
