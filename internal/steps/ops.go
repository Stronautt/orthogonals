package steps

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/virt"
)

// Op names journaled by KindOp steps.
const (
	OpDefineDomain   = "define-domain"
	OpUndefineDomain = "undefine-domain"
	OpNetAutostart   = "net-autostart"
	OpNetActive      = "net-active"
	OpSocketReload   = "libvirt-socket-reload"
	OpRemoveFile     = "remove-file"
	OpCreateVolume   = "create-volume"
	OpKernelArgsAdd  = "kernel-args-add"
	OpKernelArgsRem  = "kernel-args-remove"
	OpDesktopLink    = "desktop-link"
)

// DesktopTrustNote is printed when the shortcut could not be marked trusted.
// Stable text: test/tmt asserts on it to prove the path was taken.
const DesktopTrustNote = "desktop shortcut not marked trusted (no desktop session yet) — GNOME asks once on first launch"

// OpClients hands live libvirt/systemd clients to op functions.
type OpClients struct {
	virt     func() virt.Client
	sysd     func() sysd.Client
	injected bool
	vc       virt.Client
	sc       sysd.Client
}

// Virt returns the libvirt client, dialing on first use.
func (c *OpClients) Virt() virt.Client {
	if c.vc == nil {
		c.vc = c.virt()
	}
	return c.vc
}

// Sysd returns the systemd client, dialing on first use.
func (c *OpClients) Sysd() sysd.Client {
	if c.sc == nil {
		c.sc = c.sysd()
	}
	return c.sc
}

func (c *OpClients) close() {
	if c.vc != nil {
		_ = c.vc.Close()
		c.vc = nil
	}
	if c.sc != nil {
		_ = c.sc.Close()
		c.sc = nil
	}
}

// OpFunc applies or undoes one journaled operation.
type OpFunc func(c *OpClients, root string, out io.Writer, args map[string]string) error

// opEntry pairs an op func with whether it dials a daemon.
type opEntry struct {
	fn    OpFunc
	dials bool
}

var ops = map[string]opEntry{
	OpDefineDomain:   {opDefineDomain, true},
	OpUndefineDomain: {opUndefineDomain, true},
	OpNetAutostart:   {opNetAutostart, true},
	OpNetActive:      {opNetActive, true},
	OpSocketReload:   {opSocketReload, true},
	OpRemoveFile:     {opRemoveFile, false},
	OpCreateVolume:   {opCreateVolume, true},
	OpKernelArgsAdd:  {opKernelArgsAdd, false},
	OpKernelArgsRem:  {opKernelArgsRem, false},
	OpDesktopLink:    {opDesktopLink, false},
}

// opDesktopLink puts the VM shortcut on the desktop user's ~/Desktop and then,
// best effort, marks it trusted. The symlink fails loudly; the trust flag needs
// the desktop user's session bus, which does not exist until that user has
// logged in, so it must not abort a define run from a TTY or over ssh.
func opDesktopLink(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	entry, link, owner := args["entry"], args["link"], args["user"]
	if entry == "" || link == "" || owner == "" {
		return errors.New("desktop-link needs user, entry, and link")
	}

	dir := filepath.Join(root, filepath.Dir(link))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	full := filepath.Join(root, link)
	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// The target stays unprefixed: the shortcut has to resolve on the host, not
	// inside a test's --root tree.
	if err := os.Symlink(entry, full); err != nil {
		return err
	}
	fmt.Fprintf(out, "linked %s\n", link)

	// Under --root the tree is synthetic and may name a user that does not
	// exist, so report rather than refuse.
	uid, gid, err := lookupUser(owner)
	if err != nil {
		if root == "" {
			return err
		}
		fmt.Fprintf(out, "%s: %v — ownership not set under --root\n", link, err)
		return nil
	}
	if err := os.Chown(dir, uid, gid); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if err := os.Lchown(full, uid, gid); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	markTrusted(out, full, uid, gid)
	return nil
}

// lookupUser resolves a desktop user to its numeric ids. CGO_ENABLED=0 makes
// os/user parse /etc/passwd directly, so this stays pure Go.
func lookupUser(name string) (uid, gid int, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("desktop user %q: %w", name, err)
	}
	// 31-bit parse: the ids must survive both the int conversions here
	// (Lchown) and markTrusted's uint32 credential conversions, so cap at
	// the smaller signed range.
	uid64, err := strconv.ParseUint(u.Uid, 10, 31)
	if err != nil {
		return 0, 0, fmt.Errorf("desktop user %q has an unusable uid %q", name, u.Uid)
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 31)
	if err != nil {
		return 0, 0, fmt.Errorf("desktop user %q has an unusable gid %q", name, u.Gid)
	}
	return int(uid64), int(gid64), nil
}

// markTrusted runs gio as the desktop user against their own session bus. gio
// is the vendor API for GNOME's file metadata, so this stays an exec. It
// returns no error: a missing trust flag is cosmetic.
var markTrusted = func(out io.Writer, link string, uid, gid int) {
	bus := fmt.Sprintf("/run/user/%d/bus", uid)
	st, err := os.Stat(bus)
	if err != nil || st.Mode()&fs.ModeSocket == 0 {
		fmt.Fprintln(out, DesktopTrustNote)
		return
	}
	cmd := exec.Command("gio", "set", link, "metadata::trusted", "true")
	cmd.Env = append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	if b, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(out, "%s: %s\n", DesktopTrustNote, strings.TrimSpace(string(b)))
		return
	}
	fmt.Fprintf(out, "marked %s trusted\n", link)
}

// opLine renders "op k=v …" with sorted keys.
func opLine(op string, args map[string]string) string {
	parts := []string{op}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+args[k])
	}
	return strings.Join(parts, " ")
}

// opDefineDomain reads the journaled domain XML and defines it.
func opDefineDomain(c *OpClients, root string, out io.Writer, args map[string]string) error {
	xml, err := os.ReadFile(filepath.Join(root, args["xml"]))
	if err != nil {
		return err
	}
	if err := c.Virt().DefineDomain(string(xml)); err != nil {
		return err
	}
	fmt.Fprintf(out, "defined domain %s\n", args["name"])
	return nil
}

// opUndefineDomain undefines the domain, tolerating an already-gone one.
func opUndefineDomain(c *OpClients, _ string, out io.Writer, args map[string]string) error {
	err := c.Virt().UndefineDomain(args["name"])
	if virt.IsNotFound(err) {
		fmt.Fprintf(out, "domain %s already gone\n", args["name"])
		return nil
	}
	if err == nil {
		fmt.Fprintf(out, "undefined domain %s\n", args["name"])
	}
	return err
}

func opNetAutostart(c *OpClients, _ string, out io.Writer, args map[string]string) error {
	if err := c.Virt().NetworkAutostart(args["network"]); err != nil {
		return err
	}
	fmt.Fprintf(out, "network %s set to autostart\n", args["network"])
	return nil
}

func opNetActive(c *OpClients, _ string, out io.Writer, args map[string]string) error {
	if err := c.Virt().EnsureNetworkActive(args["network"]); err != nil {
		return err
	}
	fmt.Fprintf(out, "network %s active\n", args["network"])
	return nil
}

// opSocketReload reloads systemd and restarts the libvirt sockets.
func opSocketReload(c *OpClients, _ string, out io.Writer, _ map[string]string) error {
	s := c.Sysd()
	if err := s.Reload(); err != nil {
		return err
	}
	for _, sock := range []string{"virtqemud.socket", "virtnetworkd.socket"} {
		if err := s.RestartUnit(sock); err != nil {
			return err
		}
	}
	if err := s.TryRestartUnit("virtqemud.service"); err != nil {
		return err
	}
	fmt.Fprintln(out, "virtqemud socket configuration reloaded")
	return nil
}

// opCreateVolume creates the guest's qcow2 disk.
func opCreateVolume(c *OpClients, _ string, out io.Writer, args map[string]string) error {
	size, err := strconv.Atoi(args["size-gib"])
	if err != nil {
		return fmt.Errorf("create-volume size-gib %q: %w", args["size-gib"], err)
	}
	if err := c.Virt().CreateVolumeQCow2(args["path"], size); err != nil {
		return err
	}
	fmt.Fprintf(out, "created %s (%d GiB qcow2)\n", args["path"], size)
	return nil
}

// opKernelArgsAdd / opKernelArgsRem edit /boot/loader/entries directly.
func opKernelArgsAdd(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	if err := bls.AddArgs(root, args["args"]); err != nil {
		return err
	}
	fmt.Fprintf(out, "kernel args added: %s\n", args["args"])
	return nil
}

func opKernelArgsRem(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	if err := bls.RemoveArgs(root, args["args"]); err != nil {
		return err
	}
	fmt.Fprintf(out, "kernel args removed: %s\n", args["args"])
	return nil
}

// opRemoveFile is the journaled rm -f, respecting --root.
func opRemoveFile(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	full := filepath.Join(root, args["path"])
	err := os.Remove(full)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", args["path"])
	return nil
}
