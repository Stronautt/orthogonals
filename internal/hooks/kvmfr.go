package hooks

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/utils"
)

// KVMFRErrPrefix marks a failure `vm launch` prints verbatim. The error crosses
// hook → libvirtd → RPC → CLI, where only the text survives, so errors.Is cannot
// reach across it.
const KVMFRErrPrefix = "kvmfr: "

// kvmfrSizeFile records the MiB the loaded module was given: static_size_mb is a
// module_param_array with mode 0000, so sysfs never exposes it, and a character
// device has no length to stat. Under /run, since note and module both go at
// reboot.
const kvmfrSizeFile = "/run/orthogonals-kvmfr-size"

// KVMFRLabel lets qemu open the node: svirt_image_t is the type libvirt puts on
// devices it hands a domain. Exported so the desk tier can put it through the
// running policy's own validator.
const KVMFRLabel = "system_u:object_r:svirt_image_t:s0"

// Device-node poll bounds; vars so tests can shrink them.
var (
	KVMFRTimeout = 5 * time.Second
	KVMFRSettle  = 50 * time.Millisecond
)

// EnsureKVMFR loads the kvmfr module sized for this domain and hands the device
// node to the desktop user and qemu. On demand rather than at boot: the size
// must match the domain being started.
func EnsureKVMFR(root, owner string, sizeMiB uint64) error {
	log := hookLog(root, "kvmfr")
	if sizeMiB == 0 || sizeMiB > math.MaxInt32 {
		return fmt.Errorf("%srefusing a %d MiB buffer", KVMFRErrPrefix, sizeMiB)
	}
	if err := loadKVMFR(root, log, sizeMiB); err != nil {
		return err
	}
	dev := filepath.Join(root, steps.KVMFRDevice)
	if err := waitForDevice(dev); err != nil {
		return err
	}
	// udev stamps its own device_t while processing the add event, and that write
	// can land after ours, leaving qemu denied by svirt with only an AVC to show.
	// The queue has to be drained, not raced.
	if out, err := exec.Command("udevadm", "settle", "--timeout=5").CombinedOutput(); err != nil {
		log("udevadm settle: %v %s", err, bytes.TrimSpace(out))
	}
	return chownKVMFR(root, dev, owner, log)
}

func loadKVMFR(root string, log logFunc, sizeMiB uint64) error {
	if loaded, _ := utils.Exists(filepath.Join(root, "/sys/module/kvmfr")); loaded {
		if have := loadedSizeMiB(root); have >= sizeMiB {
			log("module already loaded with %d MiB", have)
			return nil
		}
		log("reloading: resident module is smaller than the %d MiB this domain needs", sizeMiB)
		if err := DeleteModule("kvmfr"); err != nil {
			return fmt.Errorf("%scannot resize the buffer, the module is in use (%w) — close the Looking Glass client and start again",
				KVMFRErrPrefix, err)
		}
	}
	arg := "static_size_mb=" + strconv.FormatUint(sizeMiB, 10)
	if out, err := exec.Command("modprobe", "kvmfr", arg).CombinedOutput(); err != nil {
		// The notification quotes this text, so the remedy belongs in it.
		return fmt.Errorf("%sthe module is not available for kernel %s (modprobe: %v) — run `sudo orthogonals up` to fall back to %s\n%s",
			KVMFRErrPrefix, hw.KernelVersion(root), err, steps.LookingGlassSHM, bytes.TrimSpace(out))
	}
	log("loaded with %s", arg)
	// Best effort: a lost note costs one reload.
	_ = os.WriteFile(filepath.Join(root, kvmfrSizeFile), []byte(strconv.FormatUint(sizeMiB, 10)), 0o644)
	return nil
}

// loadedSizeMiB is 0 on any doubt, which forces a reload rather than trusting a
// possibly undersized buffer.
func loadedSizeMiB(root string) uint64 {
	n, _ := utils.ReadUint(filepath.Join(root, kvmfrSizeFile))
	return n
}

func waitForDevice(dev string) error {
	deadline := time.Now().Add(KVMFRTimeout)
	for {
		fi, err := os.Stat(dev)
		switch {
		case err == nil && fi.Mode()&os.ModeCharDevice != 0:
			return nil
		case err == nil:
			// qemu opens the backend with O_CREAT, so a start without the module
			// leaves a plain file here; mapping it would leave the guest writing
			// frames nobody reads.
			return fmt.Errorf("%s%s is not a character device — delete it and reload the module", KVMFRErrPrefix, dev)
		case !errors.Is(err, fs.ErrNotExist):
			// Only absence is worth waiting out; spinning the timeout on EACCES
			// or EIO would spend it to report the wrong cause.
			return fmt.Errorf("%sstat %s: %w", KVMFRErrPrefix, dev, err)
		case !time.Now().Before(deadline):
			return fmt.Errorf("%s%s did not appear within %s", KVMFRErrPrefix, dev, KVMFRTimeout)
		}
		time.Sleep(KVMFRSettle)
	}
}

// chownKVMFR gives the node to the desktop user and the qemu group, then labels
// it for svirt. Here rather than in a udev rule so undo has nothing to take back.
func chownKVMFR(root, dev, owner string, log logFunc) error {
	uid, _, _, err := steps.LookupUser(owner)
	if err != nil {
		return fmt.Errorf("%s%w", KVMFRErrPrefix, err)
	}
	gid, err := qemuGID()
	if err != nil {
		return fmt.Errorf("%s%w", KVMFRErrPrefix, err)
	}
	if err := os.Chown(dev, uid, gid); err != nil {
		return fmt.Errorf("%schown %s: %w", KVMFRErrPrefix, dev, err)
	}
	if err := os.Chmod(dev, 0o660); err != nil {
		return fmt.Errorf("%schmod %s: %w", KVMFRErrPrefix, dev, err)
	}
	// udev may still be processing the add event; confirm ours stuck.
	if fi, err := os.Stat(dev); err == nil && fi.Mode().Perm() != 0o660 {
		log("udev reset the mode to %o — retrying", fi.Mode().Perm())
		if err := os.Chmod(dev, 0o660); err != nil {
			return fmt.Errorf("%schmod %s: %w", KVMFRErrPrefix, dev, err)
		}
	}
	selinux, _ := utils.Exists(filepath.Join(root, "/sys/fs/selinux/enforce"))
	if err := setxattr(dev, "security.selinux", []byte(KVMFRLabel), 0); err != nil {
		// A host without SELinux cannot take the label and does not need it;
		// one with SELinux would have qemu denied, so that must be loud.
		if selinux {
			return fmt.Errorf("%slabel %s as %s: %w", KVMFRErrPrefix, dev, KVMFRLabel, err)
		}
		log("no SELinux label set (%v) — SELinux is not active", err)
		return nil
	}
	// Read back rather than trust the write: a relabel after ours denies qemu at
	// map time with only an AVC as evidence.
	if selinux {
		if got := currentLabel(dev); got != KVMFRLabel {
			return fmt.Errorf("%s%s is labelled %q, not %q — something relabelled it after the hook did",
				KVMFRErrPrefix, dev, got, KVMFRLabel)
		}
	}
	return nil
}

func currentLabel(path string) string {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, "security.selinux", buf)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(buf[:n]), "\x00")
}

// setxattr is a seam: a unit test cannot label a file it does not own.
var setxattr = unix.Lsetxattr

func qemuGID() (int, error) {
	g, err := user.LookupGroup("qemu")
	if err != nil {
		return 0, fmt.Errorf("group qemu: %w", err)
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 31)
	if err != nil {
		return 0, fmt.Errorf("group qemu has an unusable gid %q", g.Gid)
	}
	return int(gid), nil
}
