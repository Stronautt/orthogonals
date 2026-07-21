package steps

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
)

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
