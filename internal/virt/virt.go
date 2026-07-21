// Package virt is the libvirt seam orthogonals drives the hypervisor through.
package virt

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// Client is what orthogonals needs from libvirt.
type Client interface {
	DefineDomain(xml string) error
	// UndefineDomain removes the persistent config plus its NVRAM and TPM state.
	UndefineDomain(name string) error
	StartDomain(name string) error
	DestroyDomain(name string) error
	ShutdownDomain(name string) error
	DomainState(name string) (string, error)
	DomainUUID(name string) (string, error)
	DomainBlockPhysical(name, dev string) (uint64, error)
	// DomainMaxMemoryKiB is the domain's configured maximum memory in KiB.
	DomainMaxMemoryKiB(name string) (uint64, error)
	// DomainDisplay returns the SPICE host and port from the live domain XML.
	DomainDisplay(name string) (host, port string, err error)
	SendKeyEnter(name string) error
	// AgentCommand sends one qemu-guest-agent request and returns the raw JSON reply.
	AgentCommand(name, cmdJSON string) (string, error)
	NetworkAutostart(name string) error
	// EnsureNetworkActive starts a defined network unless it is already active.
	EnsureNetworkActive(name string) error
	// CreateVolumeQCow2 creates a qcow2 volume at path.
	CreateVolumeQCow2(path string, sizeGiB int) error
	// Ping reports whether the hypervisor is reachable at all.
	Ping() error
	Close() error
}

// New returns a lazily connecting client for the local system libvirt.
func New() Client { return &client{} }

// Live reports whether a domain state means the guest still holds its resources.
func Live(state string) bool {
	switch state {
	case "running", "paused", "pmsuspended", "in shutdown":
		return true
	}
	return false
}

// IsNotFound reports a libvirt "no such domain/network/pool/volume" error.
func IsNotFound(err error) bool {
	var le libvirt.Error
	if !errors.As(err, &le) {
		return false
	}
	switch libvirt.ErrorNumber(le.Code) {
	case libvirt.ErrNoDomain, libvirt.ErrNoNetwork, libvirt.ErrNoStoragePool, libvirt.ErrNoStorageVol:
		return true
	}
	return false
}

// sockets to try in dial order.
var sockets = []string{
	"/var/run/libvirt/virtqemud-sock",
	"/var/run/libvirt/libvirt-sock",
}

type client struct {
	l *libvirt.Libvirt
}

func (c *client) ensure() (*libvirt.Libvirt, error) {
	if c.l != nil {
		return c.l, nil
	}
	var lastErr error
	for _, sock := range sockets {
		l := libvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(sock)))
		if err := l.ConnectToURI(libvirt.QEMUSystem); err != nil {
			lastErr = err
			continue
		}
		c.l = l
		return l, nil
	}
	return nil, fmt.Errorf("connect to libvirt (%s): %w", strings.Join(sockets, ", "), lastErr)
}

// do runs one RPC, redialing once on a transport error.
func (c *client) do(f func(l *libvirt.Libvirt) error) error {
	l, err := c.ensure()
	if err != nil {
		return err
	}
	err = f(l)
	var le libvirt.Error
	if err == nil || errors.As(err, &le) {
		return err
	}
	_ = c.l.Disconnect()
	c.l = nil
	l, err2 := c.ensure()
	if err2 != nil {
		return err
	}
	return f(l)
}

func (c *client) withDomain(op, name string, f func(l *libvirt.Libvirt, d libvirt.Domain) error) error {
	err := c.do(func(l *libvirt.Libvirt) error {
		d, err := l.DomainLookupByName(name)
		if err != nil {
			return err
		}
		return f(l, d)
	})
	if err != nil {
		return fmt.Errorf("%s domain %s: %w", op, name, err)
	}
	return nil
}

func (c *client) DefineDomain(xml string) error {
	err := c.do(func(l *libvirt.Libvirt) error {
		_, err := l.DomainDefineXML(xml)
		return err
	})
	if err != nil {
		return fmt.Errorf("define domain: %w", err)
	}
	return nil
}

func (c *client) UndefineDomain(name string) error {
	return c.withDomain("undefine", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		return l.DomainUndefineFlags(d, libvirt.DomainUndefineNvram|libvirt.DomainUndefineTpm)
	})
}

func (c *client) StartDomain(name string) error {
	return c.withDomain("start", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		return l.DomainCreate(d)
	})
}

func (c *client) DestroyDomain(name string) error {
	return c.withDomain("destroy", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		return l.DomainDestroy(d)
	})
}

func (c *client) ShutdownDomain(name string) error {
	return c.withDomain("shutdown", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		return l.DomainShutdown(d)
	})
}

// stateWords maps libvirt domain states to virsh's vocabulary.
var stateWords = map[int32]string{
	0: "no state", 1: "running", 2: "idle", 3: "paused",
	4: "in shutdown", 5: "shut off", 6: "crashed", 7: "pmsuspended",
}

func (c *client) DomainState(name string) (string, error) {
	var state int32
	err := c.withDomain("state of", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		s, _, err := l.DomainGetState(d, 0)
		state = s
		return err
	})
	if err != nil {
		return "", err
	}
	if w, ok := stateWords[state]; ok {
		return w, nil
	}
	return fmt.Sprintf("state %d", state), nil
}

func (c *client) DomainUUID(name string) (string, error) {
	var u libvirt.UUID
	err := c.withDomain("uuid of", name, func(_ *libvirt.Libvirt, d libvirt.Domain) error {
		u = d.UUID
		return nil
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

func (c *client) DomainBlockPhysical(name, dev string) (uint64, error) {
	var phys uint64
	err := c.withDomain("block info of", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		_, _, p, err := l.DomainGetBlockInfo(d, dev, 0)
		phys = p
		return err
	})
	return phys, err
}

func (c *client) DomainMaxMemoryKiB(name string) (uint64, error) {
	var mem uint64
	err := c.withDomain("memory info of", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		_, maxMem, _, _, _, err := l.DomainGetInfo(d)
		mem = maxMem
		return err
	})
	return mem, err
}

// ErrNoDisplay means the domain has no resolved SPICE port yet.
var ErrNoDisplay = errors.New("no graphics display port yet")

func (c *client) DomainDisplay(name string) (host, port string, err error) {
	var desc string
	if e := c.withDomain("display of", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		x, err := l.DomainGetXMLDesc(d, 0)
		desc = x
		return err
	}); e != nil {
		return "", "", e
	}
	return parseSpiceDisplay(desc)
}

// parseSpiceDisplay pulls the SPICE host:port out of a live domain XML.
func parseSpiceDisplay(desc string) (host, port string, err error) {
	var doc struct {
		Graphics []struct {
			Type    string `xml:"type,attr"`
			Port    string `xml:"port,attr"`
			Listen  string `xml:"listen,attr"`
			Listens []struct {
				Address string `xml:"address,attr"`
			} `xml:"listen"`
		} `xml:"devices>graphics"`
	}
	if err := xml.Unmarshal([]byte(desc), &doc); err != nil {
		return "", "", err
	}
	for _, g := range doc.Graphics {
		if g.Type != "spice" {
			continue
		}
		if g.Port == "" || g.Port == "-1" {
			return "", "", ErrNoDisplay
		}
		host = g.Listen
		for _, l := range g.Listens {
			if l.Address != "" {
				host = l.Address
			}
		}
		if host == "" {
			host = "127.0.0.1"
		}
		return host, g.Port, nil
	}
	return "", "", ErrNoDisplay
}

// enterKeycode is KEY_ENTER in the linux keycode set.
const enterKeycode = 28

func (c *client) SendKeyEnter(name string) error {
	return c.withDomain("send key to", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		return l.DomainSendKey(d, uint32(libvirt.KeycodeSetLinux), 0, []uint32{enterKeycode}, 0)
	})
}

func (c *client) AgentCommand(name, cmdJSON string) (string, error) {
	var reply string
	err := c.withDomain("agent command to", name, func(l *libvirt.Libvirt, d libvirt.Domain) error {
		res, err := l.QEMUDomainAgentCommand(d, cmdJSON, -1, 0)
		if err != nil {
			return err
		}
		if len(res) == 0 {
			return errors.New("agent returned no reply")
		}
		reply = res[0]
		return nil
	})
	return reply, err
}

func (c *client) NetworkAutostart(name string) error {
	err := c.do(func(l *libvirt.Libvirt) error {
		n, err := l.NetworkLookupByName(name)
		if err != nil {
			return err
		}
		return l.NetworkSetAutostart(n, 1)
	})
	if err != nil {
		return fmt.Errorf("autostart network %s: %w", name, err)
	}
	return nil
}

func (c *client) EnsureNetworkActive(name string) error {
	err := c.do(func(l *libvirt.Libvirt) error {
		n, err := l.NetworkLookupByName(name)
		if err != nil {
			return err
		}
		active, err := l.NetworkIsActive(n)
		if err != nil || active == 1 {
			return err
		}
		return l.NetworkCreate(n)
	})
	if err != nil {
		return fmt.Errorf("start network %s: %w", name, err)
	}
	return nil
}

// CreateVolumeQCow2 creates the qcow2 through a transient dir pool over the target directory.
func (c *client) CreateVolumeQCow2(path string, sizeGiB int) error {
	poolXML := fmt.Sprintf("<pool type='dir'><name>orthogonals-vol</name><target><path>%s</path></target></pool>",
		xmlEscape(filepath.Dir(path)))
	volXML := fmt.Sprintf("<volume><name>%s</name><capacity unit='GiB'>%d</capacity><target><format type='qcow2'/></target></volume>",
		xmlEscape(filepath.Base(path)), sizeGiB)
	err := c.do(func(l *libvirt.Libvirt) error {
		pool, err := l.StoragePoolCreateXML(poolXML, 0)
		if err != nil {
			return err
		}
		defer func() { _ = l.StoragePoolDestroy(pool) }()
		_, err = l.StorageVolCreateXML(pool, volXML, 0)
		return err
	})
	if err != nil {
		return fmt.Errorf("create volume %s: %w", path, err)
	}
	return nil
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", `"`, "&quot;")

func xmlEscape(s string) string { return xmlEscaper.Replace(s) }

func (c *client) Ping() error {
	_, err := c.ensure()
	return err
}

func (c *client) Close() error {
	if c.l == nil {
		return nil
	}
	l := c.l
	c.l = nil
	return l.Disconnect()
}
