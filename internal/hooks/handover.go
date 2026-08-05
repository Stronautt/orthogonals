package hooks

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/notify"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/utils"
)

// handoverSaveFile records that a handover started and has not finished. The
// rollback below runs in the aborting process, but a process killed mid-detach
// runs nothing at all, and Reattach arrives later as a separate hook
// invocation — only a file crosses that gap.
const handoverSaveFile = "/run/orthogonals-handover"

// handover carries what a rollback needs across Detach's mutations. Every abort
// path unwinds through h.abort, so a mutation added to Detach is covered by the
// abort that already guards it.
type handover struct {
	root, user string
	devs       []string
	// woken is the devices wakeDevices pinned to D0. A device that reported
	// suspended had power/control=auto, so those are the ones to hand back.
	woken []string
	sd    sysd.Client
	log   logFunc
}

func (h *handover) begin() error {
	path := filepath.Join(h.root, handoverSaveFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}

func handoverStarted(root string) bool {
	// Unreadable /run is not "no handover": erring toward a redundant reattach
	// costs a driver reload, erring the other way strands the GPU.
	ok, err := utils.Exists(filepath.Join(root, handoverSaveFile))
	return ok || err != nil
}

// ClearHandover drops the marker. Recover restores the GPU itself, so without
// this a later Reattach would bounce a healthy card.
func ClearHandover(root string) {
	_ = os.Remove(filepath.Join(root, handoverSaveFile))
}

// abort rolls the handover back, then reports. Reenumerate is deliberately not
// part of it — it sleeps for seconds on libvirt's hook path — so a rollback
// that does not reach a healthy GPU leaves the marker for Reattach's full path.
func (h *handover) abort(format string, a ...any) error {
	err := fmt.Errorf(format, a...)
	h.log("failed — %v", err)
	h.rollback()
	notify.Send(vmNote(h.user, "GPU handover failed — VM not started. See: "+filepath.Join(h.root, LogPath), true))
	return err
}

func (h *handover) rollback() {
	for _, d := range h.devs {
		_ = hw.SetDriverOverride(h.root, d, "")
	}
	if err := reloadNVIDIA(h.root, h.devs, h.sd); err != nil {
		h.log("rollback reload: %v", err)
	}
	for _, d := range h.woken {
		_ = hw.SetPowerControl(h.root, d, "auto")
	}
	if err := HealthCheck(h.root); err != nil {
		h.log("rollback left the GPU unhealthy — leaving it to reattach")
		return
	}
	ClearHandover(h.root)
	h.log("rolled back — GPU back on the host driver")
}
