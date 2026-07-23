// Command vfiohost boots the guest test/tmt/vfio.sh runs inside, and tears it
// down again.
//
// tmt cannot provision this one: its testcloud plugin has no way to ask for an
// emulated IOMMU (the hardware matrix supports `iommu` on beaker only), which
// is the single thing this guest exists to have. So the domain is defined here
// over libvirt RPC, and tmt attaches with `provision: how: connect`.
//
//	go run ./test/vfiohost up     # prints the guest address on stdout
//	go run ./test/vfiohost down
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"
)

// WorkDir holds the guest's disk, its cloud-init seed, and the throwaway ssh
// key tmt connects with. Under /var/tmp rather than the user's home because
// qemu has to read the disk and ~/.cache is 0700.
const WorkDir = "/var/tmp/orthogonals-vfio"

// KeyPath is where `up` leaves the private key; the Makefile hands it to tmt.
var KeyPath = filepath.Join(WorkDir, "id_ed25519")

// sockets to try in dial order, matching internal/virt.
var sockets = []string{
	"/var/run/libvirt/virtqemud-sock",
	"/var/run/libvirt/libvirt-sock",
}

type options struct {
	name    string
	iommu   string
	awBits  int
	memGiB  int
	vcpus   int
	diskGiB int
	timeout time.Duration
}

func main() {
	var o options
	fs := flag.NewFlagSet("vfiohost", flag.ExitOnError)
	fs.StringVar(&o.name, "name", "orthogonals-vfio", "libvirt domain name")
	fs.StringVar(&o.iommu, "iommu", "intel", "emulated IOMMU model: intel or amd")
	fs.IntVar(&o.awBits, "aw-bits", 39, "IOMMU address width; 39 exercises the maxphysaddr path, 48 does not")
	// 14 GiB is the smallest guest whose own /proc/meminfo clears preflight's
	// memory floor without faking it: the 5/8 default lands on exactly 8 GiB.
	fs.IntVar(&o.memGiB, "memory", 14, "guest RAM in GiB — the hook reserves 8 GiB of hugepages inside it")
	fs.IntVar(&o.vcpus, "vcpus", 8, "guest vCPUs, even and at least 6; two threads per core")
	fs.IntVar(&o.diskGiB, "disk", 20, "guest disk in GiB")
	fs.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long to wait for ssh")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: vfiohost [flags] up|down\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])

	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	if err := run(fs.Arg(0), o); err != nil {
		fmt.Fprintln(os.Stderr, "vfiohost:", err)
		os.Exit(1)
	}
}

func run(cmd string, o options) error {
	l, err := connect()
	if err != nil {
		return err
	}
	defer func() { _ = l.Disconnect() }()

	switch cmd {
	case "up":
		addr, err := up(l, o)
		if err != nil {
			return err
		}
		// stdout carries the address and nothing else: the Makefile captures it.
		fmt.Println(addr)
		return nil
	case "down":
		return down(l, o.name)
	default:
		return fmt.Errorf("unknown command %q (want up or down)", cmd)
	}
}

func connect() (*libvirt.Libvirt, error) {
	var lastErr error
	for _, sock := range sockets {
		l := libvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(sock)))
		if err := l.ConnectToURI(libvirt.QEMUSystem); err != nil {
			lastErr = err
			continue
		}
		return l, nil
	}
	return nil, fmt.Errorf("connect to libvirt: %w", lastErr)
}

// up brings the guest from nothing to an address that answers on port 22.
// Idempotent: an existing domain of the same name is torn down first.
func up(l *libvirt.Libvirt, o options) (string, error) {
	if err := down(l, o.name); err != nil {
		return "", err
	}
	if err := os.MkdirAll(WorkDir, 0o755); err != nil {
		return "", err
	}
	base, err := ensureBaseImage()
	if err != nil {
		return "", err
	}
	pubKey, err := ensureSSHKey()
	if err != nil {
		return "", err
	}
	seed, err := writeSeedISO(o.name, pubKey)
	if err != nil {
		return "", err
	}
	disk, err := createOverlay(l, o, base)
	if err != nil {
		return "", err
	}

	mac := managementMAC(o.name)
	xml, err := renderDomain(o, disk, seed, mac)
	if err != nil {
		return "", err
	}
	dom, err := l.DomainDefineXML(xml)
	if err != nil {
		return "", fmt.Errorf("define %s: %w", o.name, err)
	}
	if err := l.DomainCreate(dom); err != nil {
		return "", fmt.Errorf("start %s: %w", o.name, err)
	}
	logf("started %s (%s IOMMU, aw_bits=%d, %d GiB, %d vCPU)", o.name, o.iommu, o.awBits, o.memGiB, o.vcpus)

	addr, err := waitForSSH(l, mac, o.timeout)
	if err != nil {
		return "", err
	}
	logf("guest reachable at %s — connect with: ssh -i %s root@%s", addr, KeyPath, addr)
	return addr, nil
}

// down destroys and undefines the domain, then releases its storage. Every
// step tolerates absence so a partial `up` can still be cleaned up.
func down(l *libvirt.Libvirt, name string) error {
	dom, err := l.DomainLookupByName(name)
	switch {
	case isNotFound(err):
	case err != nil:
		return fmt.Errorf("look up %s: %w", name, err)
	default:
		// A running domain must be killed before it can be undefined; it may
		// already be off, which is not an error here.
		_ = l.DomainDestroy(dom)
		if err := l.DomainUndefineFlags(dom, libvirt.DomainUndefineNvram); err != nil && !isNotFound(err) {
			return fmt.Errorf("undefine %s: %w", name, err)
		}
		logf("removed domain %s", name)
	}
	// The base image stays: 600 MB, re-downloaded per run otherwise.
	for _, f := range []string{diskPath(name), seedPath(name)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// waitForSSH polls the default network's DHCP leases for the management NIC,
// then waits for that address to accept a TCP connection on port 22.
func waitForSSH(l *libvirt.Libvirt, mac string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var addr string
	for addr == "" {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no DHCP lease for %s within %s — the guest did not boot far enough to ask", mac, timeout)
		}
		var err error
		if addr, err = leaseFor(l, mac); err != nil {
			return "", err
		}
		if addr == "" {
			time.Sleep(2 * time.Second)
		}
	}
	logf("guest took the lease %s", addr)

	for {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(addr, "22"), 3*time.Second)
		if err == nil {
			_ = conn.Close()
			return addr, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s never accepted ssh within %s: %w", addr, timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

// leaseFor returns the address leased to mac, "" while there is none yet.
func leaseFor(l *libvirt.Libvirt, mac string) (string, error) {
	net, err := l.NetworkLookupByName("default")
	if err != nil {
		return "", fmt.Errorf("look up the default network: %w", err)
	}
	leases, _, err := l.NetworkGetDhcpLeases(net, libvirt.OptString{mac}, 1, 0)
	if err != nil {
		return "", fmt.Errorf("read DHCP leases: %w", err)
	}
	for _, lease := range leases {
		if lease.Ipaddr != "" {
			return lease.Ipaddr, nil
		}
	}
	return "", nil
}

func isNotFound(err error) bool {
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

// logf reports progress on stderr, leaving stdout for the guest address alone.
func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "vfiohost: "+format+"\n", a...)
}
