package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"

	libvirt "github.com/digitalocean/go-libvirt"
)

// StandInBus is the PCIe root port the stand-in dGPU sits behind, and the bus
// number test/tmt/vfio.sh looks for. Alone on that port, its two functions get
// an IOMMU group of their own, which is what the whole-group rule needs.
const StandInBus = "0x01"

func diskPath(name string) string { return filepath.Join(WorkDir, name+".qcow2") }

// managementMAC derives a stable MAC from the domain name so `up` can find the
// guest's DHCP lease without guessing. 52:54:00 is the QEMU OUI.
func managementMAC(name string) string {
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", h[0], h[1], h[2])
}

// createOverlay makes the guest's disk: a qcow2 backed by the pristine cloud
// image, so a run never dirties the download. Through libvirt's storage API
// rather than qemu-img, so this command spawns no subprocesses.
func createOverlay(l *libvirt.Libvirt, o options, base string) (string, error) {
	poolXML := fmt.Sprintf(
		"<pool type='dir'><name>orthogonals-vfio</name><target><path>%s</path></target></pool>", WorkDir)
	pool, err := l.StoragePoolCreateXML(poolXML, 0)
	if err != nil {
		// A pool over the same directory may already be defined from an earlier
		// run; reuse it rather than failing.
		if pool, err = l.StoragePoolLookupByName("orthogonals-vfio"); err != nil {
			return "", fmt.Errorf("create the storage pool at %s: %w", WorkDir, err)
		}
	} else {
		defer func() { _ = l.StoragePoolDestroy(pool) }()
	}

	name := o.name + ".qcow2"
	volXML := fmt.Sprintf(`<volume>
  <name>%s</name>
  <capacity unit='GiB'>%d</capacity>
  <target><format type='qcow2'/><permissions><mode>0644</mode></permissions></target>
  <backingStore><path>%s</path><format type='qcow2'/></backingStore>
</volume>`, name, o.diskGiB, base)
	if _, err := l.StorageVolCreateXML(pool, volXML, 0); err != nil {
		return "", fmt.Errorf("create the guest disk: %w", err)
	}
	return diskPath(o.name), nil
}

// domainTmpl is the guest. The non-boilerplate parts:
//
//   - <iommu> with caching_mode makes vfio-pci usable inside the guest;
//     intremap needs the QEMU ioapic, hence <ioapic driver='qemu'/>.
//   - aw_bits sets the VT-d CAP register the address-width check decodes, so 39
//     reproduces the maxphysaddr warning path against a real register.
//   - The CPU is host-passthrough, deliberately: `vm define` needs /dev/kvm
//     (libvirt offers no kvm domain type without it and autoselecting the
//     rendered domain's secure-boot EFI firmware fails), and the guest's
//     kvm_intel/kvm_amd only load on their own silicon with the host's real
//     vendor string and vmx/svm nested in. A named model cannot deliver that
//     on both fleets: KVM nests only the host's own virt extension, and
//     kvm_amd refuses a non-AMD vendor. The suite no longer needs any
//     particular vendor — the kernel-arg choice keys on the ACPI table the
//     emulated IOMMU provides, not on the CPU.
//   - The two virtio-scsi controllers are the stand-in dGPU and its audio
//     function: ordinary virtio-pci devices needing no backing resource, which
//     advertise FLR (so the kernel publishes the `reset` preflight requires) and
//     alone on a root port land in an IOMMU group of their own. Only vendor,
//     device, and class are overlaid on them in the guest; iommu_group,
//     driver_override, unbind, remove and rescan stay the kernel's own.
//   - <video> stands in for the iGPU: genuinely display-class, so it takes
//     boot_vga and a DRM card with no overlay.
var domainTmpl = template.Must(template.New("domain").Parse(`<domain type='kvm'>
  <name>{{.Name}}</name>
  <memory unit='GiB'>{{.MemGiB}}</memory>
  <vcpu>{{.VCPUs}}</vcpu>
  <os>
    <type arch='x86_64' machine='q35'>hvm</type>
    <boot dev='hd'/>
  </os>
  <features>
    <acpi/>
    <apic/>
    <ioapic driver='qemu'/>
  </features>
  <cpu mode='host-passthrough' check='none'>
    <topology sockets='1' cores='{{.Cores}}' threads='2'/>
  </cpu>
  <clock offset='utc'/>
  <on_reboot>restart</on_reboot>
  <devices>
    <iommu model='{{.IOMMU}}'>
      <driver intremap='on' caching_mode='on' aw_bits='{{.AWBits}}'/>
    </iommu>

    <controller type='pci' index='1' model='pcie-root-port'/>
    <controller type='scsi' index='1' model='virtio-scsi'>
      <address type='pci' domain='0x0000' bus='{{.StandInBus}}' slot='0x00' function='0x0' multifunction='on'/>
    </controller>
    <controller type='scsi' index='2' model='virtio-scsi'>
      <address type='pci' domain='0x0000' bus='{{.StandInBus}}' slot='0x00' function='0x1'/>
    </controller>

    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2'/>
      <source file='{{.Disk}}'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='{{.Seed}}'/>
      <target dev='sda' bus='sata'/>
      <readonly/>
    </disk>

    <interface type='network'>
      <source network='default'/>
      <model type='virtio'/>
      <mac address='{{.MAC}}'/>
    </interface>

    <video><model type='virtio'/></video>
    <graphics type='vnc' port='-1' listen='127.0.0.1'/>
    <serial type='pty'><target port='0'/></serial>
    <console type='pty'><target type='serial' port='0'/></console>
    <memballoon model='none'/>
  </devices>
</domain>
`))

func renderDomain(o options, disk, seed, mac string) (string, error) {
	if o.iommu != "intel" && o.iommu != "amd" {
		return "", fmt.Errorf("unknown IOMMU model %q (want intel or amd)", o.iommu)
	}
	// An explicit topology, because the default gives every vCPU its own socket
	// and therefore its own core_id: the profile then reserves the whole first
	// physical core for the host and has nothing left to assign, so preflight
	// fails on `cpu` before apply runs.
	if o.vcpus < 6 || o.vcpus%2 != 0 {
		return "", fmt.Errorf("--vcpus %d: need an even count of at least 6, or the profile has no threads left to assign after reserving a core for the host", o.vcpus)
	}
	// The 5/8 default guest RAM must clear the 8 GiB floor, so the guest needs
	// at least 13 GiB before apply will run at all.
	if o.memGiB*5/8 < 8 {
		return "", fmt.Errorf("--memory %d GiB: the default guest RAM works out to %d GiB, below the 8 GiB minimum",
			o.memGiB, o.memGiB*5/8)
	}
	var b strings.Builder
	err := domainTmpl.Execute(&b, struct {
		Name, IOMMU, Disk, Seed, MAC, StandInBus string
		MemGiB, VCPUs, Cores, AWBits             int
	}{
		Name: o.name, IOMMU: o.iommu,
		Disk: disk, Seed: seed, MAC: mac, StandInBus: StandInBus,
		MemGiB: o.memGiB, VCPUs: o.vcpus, Cores: o.vcpus / 2, AWBits: o.awBits,
	})
	return b.String(), err
}
