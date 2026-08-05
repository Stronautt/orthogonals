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

type OpClients struct {
	virt     func() virt.Client
	sysd     func() sysd.Client
	injected bool
	vc       virt.Client
	sc       sysd.Client
}

func (c *OpClients) Virt() virt.Client {
	if c.vc == nil {
		c.vc = c.virt()
	}
	return c.vc
}

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
	// The desktop user controls this directory. os.Chown follows a symlink, so
	// a ~/Desktop pointed at /etc would hand them /etc; O_NOFOLLOW refuses
	// that, and chowning the descriptor leaves no swap window.
	dirFD, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("open shortcut directory %s (a symlink there is refused): %w", dir, err)
	}
	defer func() { _ = dirFD.Close() }()

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
	uid, gid, home, err := LookupUser(owner)
	if err != nil {
		if root == "" {
			return err
		}
		fmt.Fprintf(out, "%s: %v — ownership not set under --root\n", link, err)
		return nil
	}
	if err := dirFD.Chown(uid, gid); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	if err := os.Lchown(full, uid, gid); err != nil && !errors.Is(err, fs.ErrPermission) {
		return err
	}
	markTrusted(out, full, home, uid, gid)
	return nil
}

// LookupUser resolves a desktop user to its numeric ids and home. CGO_ENABLED=0
// makes os/user parse /etc/passwd directly, so this stays pure Go.
func LookupUser(name string) (uid, gid int, home string, err error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, "", fmt.Errorf("desktop user %q: %w", name, err)
	}
	// 31-bit parse: the ids must survive both the int conversions here
	// (Lchown) and markTrusted's uint32 credential conversions, so cap at
	// the smaller signed range.
	uid64, err := strconv.ParseUint(u.Uid, 10, 31)
	if err != nil {
		return 0, 0, "", fmt.Errorf("desktop user %q has an unusable uid %q", name, u.Uid)
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 31)
	if err != nil {
		return 0, 0, "", fmt.Errorf("desktop user %q has an unusable gid %q", name, u.Gid)
	}
	return int(uid64), int(gid64), u.HomeDir, nil
}

// desktopEnv is env re-pointed at the desktop user: their home, runtime dir and
// session bus **replace** the inherited entries rather than being appended to
// them — sudo hands the op root's HOME, getenv takes the first match, and gvfs
// would then try to write its metadata tree under /root as the dropped user.
func desktopEnv(env []string, home, bus string, uid int) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "HOME="),
			strings.HasPrefix(kv, "XDG_RUNTIME_DIR="),
			strings.HasPrefix(kv, "XDG_DATA_HOME="),
			strings.HasPrefix(kv, "DBUS_SESSION_BUS_ADDRESS="):
			continue
		}
		out = append(out, kv)
	}
	return append(out, "HOME="+home,
		fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid),
		"DBUS_SESSION_BUS_ADDRESS=unix:path="+bus)
}

// trustCmd runs gio as the desktop user against their own session bus: the op
// runs as root, and without the credential gio writes into a home as root.
func trustCmd(link, bus, home string, uid, gid int) *exec.Cmd {
	cmd := exec.Command("gio", "set", link, "metadata::trusted", "true")
	cmd.Env = desktopEnv(os.Environ(), home, bus, uid)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)},
	}
	return cmd
}

// markTrusted stays an exec: gio is the vendor API for GNOME's file metadata.
// It returns no error — a missing trust flag is cosmetic.
var markTrusted = func(out io.Writer, link, home string, uid, gid int) {
	bus := fmt.Sprintf("/run/user/%d/bus", uid)
	st, err := os.Stat(bus)
	if err != nil || st.Mode()&fs.ModeSocket == 0 {
		fmt.Fprintln(out, DesktopTrustNote)
		return
	}
	if b, err := trustCmd(link, bus, home, uid, gid).CombinedOutput(); err != nil {
		fmt.Fprintf(out, "%s: %s\n", DesktopTrustNote, strings.TrimSpace(string(b)))
		return
	}
	fmt.Fprintf(out, "marked %s trusted\n", link)
}

// recordLine and stepLine render a record and a step by their own kind, so a
// refusal names both sides whichever kinds they are.
func recordLine(rec *Record) string {
	switch rec.Kind {
	case KindWriteFile:
		return rec.Path
	case KindRunCmd:
		return strings.Join(rec.Cmd, " ")
	case KindEnableUnit:
		return rec.Unit
	case KindOp:
		return opLine(rec.Op, rec.OpArgs)
	}
	return string(rec.Kind)
}

func stepLine(s Step) string {
	switch s.Kind {
	case KindWriteFile:
		return s.Path
	case KindRunCmd:
		return strings.Join(s.Cmd, " ")
	case KindEnableUnit:
		return s.Unit
	case KindOp:
		return opLine(s.Op, s.Args)
	}
	return string(s.Kind)
}

// opLine sorts the keys: the line is compared in golden output, so it cannot
// depend on map order.
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

// opUndefineDomain tolerates an already-gone domain: the user may have
// undefined it by hand.
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

func opKernelArgsAdd(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	if err := bls.AddArgs(root, args["args"]); err != nil {
		return err
	}
	fmt.Fprintf(out, "kernel args added: %s\n", args["args"])
	return nil
}

// opKernelArgsRem takes the whole args map: apply journals what it added to each
// boot-config target separately, so undo can strip exactly that and leave alone
// whatever a target carried beforehand.
func opKernelArgsRem(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	if err := bls.RemoveArgs(root, args); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s\n", opLine("kernel args removed:", args))
	return nil
}

func opRemoveFile(_ *OpClients, root string, out io.Writer, args map[string]string) error {
	full := filepath.Join(root, args["path"])
	err := os.Remove(full)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fmt.Fprintf(out, "removed %s\n", args["path"])
	return nil
}
