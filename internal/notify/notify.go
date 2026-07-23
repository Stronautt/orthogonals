// Package notify is the single desktop-notification seam.
package notify

import (
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// autoHideMS is how long a normal banner shows before it auto-dismisses.
const autoHideMS = "5000"

// Notification is a best-effort desktop message.
type Notification struct {
	Title, Body, Icon string
	Urgent            bool
	User              string
}

// Send delivers n via notify-send.
var Send = func(n Notification) {
	urgency, expire := "normal", autoHideMS
	if n.Urgent {
		urgency, expire = "critical", "0"
	}
	cmd := exec.Command("notify-send", "-u", urgency, "-t", expire, "-i", n.Icon, n.Title, n.Body)
	if n.User != "" {
		u, err := user.Lookup(n.User)
		if err != nil {
			return
		}
		cmd.Env = append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/"+u.Uid+"/bus")
		// 31-bit parse: the ids get converted to both int and uint32 below.
		uid, uidErr := strconv.ParseUint(u.Uid, 10, 31)
		gid, gidErr := strconv.ParseUint(u.Gid, 10, 31)
		if uidErr != nil || gidErr != nil {
			return
		}
		if int(uid) != os.Geteuid() {
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}}
		}
	}
	_ = cmd.Run()
}
