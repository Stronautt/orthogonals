// Package notify is the single desktop-notification seam.
package notify

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

const autoHideMS = "5000"

// Notification is a best-effort desktop message.
type Notification struct {
	Title, Body, Icon string
	Urgent            bool
	User              string
}

// lookupUser is the passwd seam.
var lookupUser = user.Lookup

// credential resolves the bus address and the credentials to drop to — the hook
// calls Send as root. A nil SysProcAttr means "already that user": switching to
// your own uid needs privileges the caller may not have.
func credential(name string, euid int) (*syscall.SysProcAttr, string, error) {
	u, err := lookupUser(name)
	if err != nil {
		return nil, "", err
	}
	// 31-bit parse: the ids are converted to both int and uint32 below, so the
	// smaller signed range is the one that has to hold.
	uid, err := strconv.ParseUint(u.Uid, 10, 31)
	if err != nil {
		return nil, "", fmt.Errorf("user %q has an unusable uid %q", name, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 31)
	if err != nil {
		return nil, "", fmt.Errorf("user %q has an unusable gid %q", name, u.Gid)
	}
	bus := "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + u.Uid + "/bus"
	if int(uid) == euid {
		return nil, bus, nil
	}
	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}, bus, nil
}

var Send = func(n Notification) {
	urgency, expire := "normal", autoHideMS
	if n.Urgent {
		urgency, expire = "critical", "0"
	}
	cmd := exec.Command("notify-send", "-u", urgency, "-t", expire, "-i", n.Icon, n.Title, n.Body)
	if n.User != "" {
		cred, bus, err := credential(n.User, os.Geteuid())
		if err != nil {
			return
		}
		cmd.Env = append(os.Environ(), bus)
		cmd.SysProcAttr = cred
	}
	_ = cmd.Run()
}
