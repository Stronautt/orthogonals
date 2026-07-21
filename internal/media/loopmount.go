package media

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// MountISO loop-mounts an ISO read-only and returns the mountpoint plus a cleanup.
var MountISO = mountISOLoop

// loopAttachTries bounds the GET_FREE→configure race.
const loopAttachTries = 3

func mountISOLoop(path string) (string, func(), error) {
	iso, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	loop, loopPath, err := attachLoop(iso)
	if err != nil {
		_ = iso.Close()
		return "", nil, fmt.Errorf("loop-attach %s (root required): %w", path, err)
	}
	mnt, err := os.MkdirTemp("", "orthogonals-iso-")
	if err != nil {
		_ = loop.Close()
		_ = iso.Close()
		return "", nil, err
	}
	err = unix.Mount(loopPath, mnt, "udf", unix.MS_RDONLY, "")
	if err != nil {
		err = unix.Mount(loopPath, mnt, "iso9660", unix.MS_RDONLY, "")
	}
	if err != nil {
		_ = loop.Close()
		_ = iso.Close()
		_ = os.RemoveAll(mnt)
		return "", nil, fmt.Errorf("mount %s (root required): %w", path, err)
	}
	cleanup := func() {
		_ = unix.Unmount(mnt, 0)
		_ = loop.Close()
		_ = iso.Close()
		_ = os.RemoveAll(mnt)
	}
	return mnt, cleanup, nil
}

// attachLoop binds the ISO to a free loop device, read-only with autoclear.
func attachLoop(iso *os.File) (*os.File, string, error) {
	ctl, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = ctl.Close() }()
	var lastErr error
	for range loopAttachTries {
		n, err := unix.IoctlRetInt(int(ctl.Fd()), unix.LOOP_CTL_GET_FREE)
		if err != nil {
			return nil, "", err
		}
		dev := fmt.Sprintf("/dev/loop%d", n)
		loop, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err != nil {
			lastErr = err
			continue
		}
		err = unix.IoctlLoopConfigure(int(loop.Fd()), &unix.LoopConfig{
			Fd:   uint32(iso.Fd()),
			Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_READ_ONLY | unix.LO_FLAGS_AUTOCLEAR},
		})
		if err == nil {
			return loop, dev, nil
		}
		_ = loop.Close()
		lastErr = err
		if !errors.Is(err, unix.EBUSY) {
			break
		}
	}
	return nil, "", lastErr
}
