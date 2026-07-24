package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd"
)

// inhibitUnit is the transient sleep-inhibitor unit for a running VM.
func inhibitUnit(vm string) string { return "libvirt-nosleep-" + vm + ".service" }

// Dispatch is the qemu hook body: prepare/begin detaches the GPU, release/end reattaches it.
func Dispatch(root string, sd sysd.Client, vm, op, subop, user, exe string) error {
	if _, err := os.Stat(filepath.Join(steps.VMsDir(root), vm+".xml")); err != nil {
		// Only true absence means "not ours". Any other stat failure (EACCES on
		// the registry) must not silently no-op the hook: skipping release/end
		// would leave the GPU on vfio-pci.
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("check VM registry for %s: %w", vm, err)
	}
	log := hookLog(root, "qemu")
	switch op + "/" + subop {
	case "prepare/begin":
		if err := oneVMAtATime(root, vm); err != nil {
			return err
		}
		if err := Detach(root, user, sd); err != nil {
			return fmt.Errorf("GPU handover to vfio-pci failed — VM start aborted. Details: %s: %w",
				filepath.Join(root, LogPath), err)
		}
		ramMiB, err := domain.ReadMemoryMiB(root, vm)
		if err != nil {
			return fmt.Errorf("read guest RAM for hugepage reservation: %w", err)
		}
		if err := reserveHugepages(root, user, ramMiB); err != nil {
			return err
		}
		isolateCPUs(root, sd, vm)
		unit := inhibitUnit(vm)
		_ = sd.ResetFailedUnit(unit)
		if err := sd.StartTransientUnit(unit, []string{exe, "hook", "inhibit", vm}); err != nil {
			log("sleep inhibitor not started: %v", err)
		}
	case "started/begin":
		secureSpiceSocket(root, vm, user)
	case "release/end":
		_ = sd.StopUnit(inhibitUnit(vm))
		unisolateCPUs(root, sd)
		freeHugepages(root)
		if err := Reattach(root, user, sd); err != nil {
			return fmt.Errorf("GPU reattach to the host driver failed — run: sudo orthogonals recover --yes. Details: %s: %w",
				filepath.Join(root, LogPath), err)
		}
	}
	return nil
}

// SpiceSettle and SpiceTimeout bound the wait for QEMU to bind the socket.
var (
	SpiceSettle  = 50 * time.Millisecond
	SpiceTimeout = 2 * time.Second
)

// secureSpiceSocket hands the SPICE socket to the desktop user. QEMU binds it
// world-readable; the 0730 per-VM directory is the real gate, so this is
// hardening on top of it — and log-only, because the VM is already running.
func secureSpiceSocket(root, vm, user string) {
	log := hookLog(root, "spice-socket")
	rel := steps.SpiceSocketPath(vm)
	path := filepath.Join(root, rel)

	deadline := time.Now().Add(SpiceTimeout)
	for {
		fi, err := os.Lstat(path)
		if err == nil {
			// Only qemu could plant something else here; do not trust it.
			if fi.Mode()&fs.ModeSocket == 0 {
				log("%s is not a socket — left alone", rel)
				return
			}
			break
		}
		if !time.Now().Before(deadline) {
			log("%s did not appear within %s — the directory mode still restricts it", rel, SpiceTimeout)
			return
		}
		time.Sleep(SpiceSettle)
	}

	uid, gid, _, err := steps.LookupUser(user)
	if err != nil {
		log("%v", err)
		return
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		log("chown %s: %v", rel, err)
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		log("chmod %s: %v", rel, err)
		return
	}
	log("%s handed to %s (0600)", rel, user)
}

// oneVMAtATime refuses the start while another managed domain holds the dGPU.
func oneVMAtATime(root, vm string) error {
	for _, other := range steps.VMNames(root) {
		if other == vm {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, steps.LibvirtRunDir, other+".xml")); err == nil {
			return fmt.Errorf("VM %s is running and holds the dGPU — one VM at a time. Shut it down first: virsh shutdown %s", other, other)
		}
	}
	return nil
}
