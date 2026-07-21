package orchestrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest journals a kernel-args step the way apply --yes does.
func writeManifest(t *testing.T, root, args string) {
	t.Helper()
	dir := filepath.Join(root, "var/lib/orthogonals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := `{"records":[{"id":"kernel-args","kind":"op","op":"kernel-args-add","op_args":{"args":"` + args + `"}},{"id":"hook-qemu-dispatcher","kind":"write_file","path":"/etc/libvirt/hooks/qemu"},{"id":"enable-switcheroo-control","kind":"enable_unit","unit":"switcheroo-control.service","enable":true}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rebootedRoot is a fake host after apply + reboot.
func rebootedRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeManifest(t, root, "intel_iommu=on iommu=pt")
	write(t, root, "proc/cmdline", "BOOT_IMAGE=vmlinuz root=/dev/mapper/root rhgb quiet intel_iommu=on iommu=pt\n")
	write(t, root, "sys/kernel/iommu_groups/0/devices/.keep", "")
	write(t, root, "sys/kernel/iommu_groups/1/devices/.keep", "")
	write(t, root, "sys/module/vfio_pci/refcnt", "0\n")
	return root
}

func TestVerifyBootPass(t *testing.T) {
	if err := VerifyBoot(rebootedRoot(t)); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyBootFailures(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(t *testing.T, root string)
		want    string
	}{
		{"missing karg", func(t *testing.T, root string) {
			write(t, root, "proc/cmdline", "BOOT_IMAGE=vmlinuz rhgb quiet intel_iommu=on\n")
		}, "iommu=pt"},
		{"no iommu groups", func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, "sys/kernel/iommu_groups")); err != nil {
				t.Fatal(err)
			}
		}, "IOMMU is not active"},
		{"vfio module missing", func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, "sys/module/vfio_pci")); err != nil {
				t.Fatal(err)
			}
		}, "vfio_pci module is not loaded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := rebootedRoot(t)
			tc.corrupt(t, root)
			err := VerifyBoot(root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestVerifyBootNoManifest(t *testing.T) {
	err := VerifyBoot(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "orthogonals apply") {
		t.Errorf("want no-kernel-args-step error, got %v", err)
	}
}
