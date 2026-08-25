package hooks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd"
)

func inhibitUnit(vm string) string { return "libvirt-nosleep-" + vm + ".service" }

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
		// Before the handover: detaching first would leave the desktop without
		// its dGPU for a VM that is not going to start. Only for a domain that
		// names the device — a /dev/shm domain must not pin the buffer.
		if sizeMiB, ok := domain.KVMFRSizeMiB(root, vm); ok {
			if err := EnsureKVMFR(root, user, sizeMiB); err != nil {
				log("%v", err)
				notify.Send(vmNote(user, "Looking Glass cannot start — "+
					strings.TrimPrefix(err.Error(), KVMFRErrPrefix), true))
				return err
			}
		}
		if err := Detach(root, user, sd); err != nil {
			return fmt.Errorf("GPU handover to vfio-pci failed — VM start aborted. Details: %s: %w",
				filepath.Join(root, LogPath), err)
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
// hardening on top of it, and log-only because the VM is already running.
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
