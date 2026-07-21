package preflight

import (
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

// fakeSwitcheroo scripts the injectable switcheroo D-Bus probe for one test.
func fakeSwitcheroo(t *testing.T, listsNVIDIA bool) {
	t.Helper()
	old := switcherooListsNVIDIA
	switcherooListsNVIDIA = func(string) bool { return listsNVIDIA }
	t.Cleanup(func() { switcherooListsNVIDIA = old })
}

func TestGatherFacts(t *testing.T) {
	t.Run("bare root", func(t *testing.T) {
		f := GatherFacts(t.TempDir())
		if f.PersistencedEnabled {
			t.Error("PersistencedEnabled = true, want false without wants symlink")
		}
		if f.DefaultNetActive {
			t.Error("DefaultNetActive = true, want false without status file")
		}
		if f.FreeDiskBytes == 0 {
			t.Error("FreeDiskBytes = 0, want statfs fallback on the root itself")
		}
		if f.OrthogonalsManaged {
			t.Error("OrthogonalsManaged = true, want false without manifest")
		}
		if len(f.ForeignVFIO) != 0 {
			t.Errorf("ForeignVFIO = %v, want empty", f.ForeignVFIO)
		}
		if f.SwitcherooEnabled || f.SwitcherooNVIDIA {
			t.Error("switcheroo facts should be false on a bare root")
		}
	})
	t.Run("persistenced enabled and default net active", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "etc/systemd/system/multi-user.target.wants/nvidia-persistenced.service", "")
		hwtest.WriteFile(t, root, "var/run/libvirt/network/default.xml", "<network/>")
		f := GatherFacts(root)
		if !f.PersistencedEnabled {
			t.Error("PersistencedEnabled = false, want true")
		}
		if !f.DefaultNetActive {
			t.Error("DefaultNetActive = false, want true")
		}
	})
	t.Run("foreign vfio traces found", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "etc/modprobe.d/vfio.conf",
			"# bind the GPU early\noptions vfio-pci ids=10de:2206,10de:1aef\n")
		hwtest.WriteFile(t, root, "etc/dracut.conf.d/00-vfio.conf",
			"add_drivers+=\" vfio-pci \"\n")
		hwtest.WriteFile(t, root, "proc/cmdline",
			"BOOT_IMAGE=/vmlinuz root=/dev/mapper/root rhgb quiet vfio-pci.ids=10de:2206 rd.driver.pre=vfio-pci\n")
		f := GatherFacts(root)
		joined := strings.Join(f.ForeignVFIO, "\n")
		for _, want := range []string{
			"/etc/modprobe.d/vfio.conf: options vfio-pci ids=10de:2206,10de:1aef",
			"/etc/dracut.conf.d/00-vfio.conf: add_drivers+=\" vfio-pci \"",
			"kernel cmdline: vfio-pci.ids=10de:2206",
			"kernel cmdline: rd.driver.pre=vfio-pci",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("ForeignVFIO missing %q, got:\n%s", want, joined)
			}
		}
		if f.OrthogonalsManaged {
			t.Error("OrthogonalsManaged = true, want false without manifest")
		}
	})
	t.Run("comments and vfio-free config are ignored", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "etc/modprobe.d/blacklist.conf",
			"# vfio is mentioned only in this comment\nblacklist pcspkr\n")
		hwtest.WriteFile(t, root, "proc/cmdline",
			"BOOT_IMAGE=/vmlinuz root=/dev/mapper/root intel_iommu=on iommu=pt rhgb quiet\n")
		f := GatherFacts(root)
		if len(f.ForeignVFIO) != 0 {
			t.Errorf("ForeignVFIO = %v, want empty", f.ForeignVFIO)
		}
	})
	t.Run("manifest marks the host orthogonals-managed", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "var/lib/orthogonals/manifest.json", "{}")
		f := GatherFacts(root)
		if !f.OrthogonalsManaged {
			t.Error("OrthogonalsManaged = false, want true with manifest present")
		}
	})
	t.Run("switcheroo enabled with nvidia listed", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "etc/systemd/system/multi-user.target.wants/switcheroo-control.service", "")
		fakeSwitcheroo(t, true)
		f := GatherFacts(root)
		if !f.SwitcherooEnabled {
			t.Error("SwitcherooEnabled = false, want true with wants symlink")
		}
		if !f.SwitcherooNVIDIA {
			t.Error("SwitcherooNVIDIA = false, want true when the daemon lists an NVIDIA device with offload env")
		}
	})
	t.Run("switcheroo enabled but daemon unreachable", func(t *testing.T) {
		root := t.TempDir()
		hwtest.WriteFile(t, root, "etc/systemd/system/multi-user.target.wants/switcheroo-control.service", "")
		f := GatherFacts(root)
		if f.SwitcherooNVIDIA {
			t.Error("SwitcherooNVIDIA = true, want false when the daemon is unreachable (fixture root)")
		}
	})
}
