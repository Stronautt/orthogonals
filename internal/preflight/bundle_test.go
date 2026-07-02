package preflight

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

const (
	secretPassword = "s3cretpw123"
	fakeSerial     = "SER1AL123XYZ"
	fakeMAC        = "aa:bb:cc:dd:ee:f0"
	fakeUUID       = "123e4567-e89b-12d3-a456-426614174000"
	fakeMachineID  = "abcdef0123456789abcdef0123456789"
)

// fakeBin writes an executable that prints output, for PATH-seam tests.
func fakeBin(t *testing.T, dir, name, output string) {
	t.Helper()
	script := "#!/bin/sh\ncat <<'EOF'\n" + output + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readTarGz(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = string(data)
	}
	return entries
}

func TestWriteBundleRedaction(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "sys/class/dmi/id/product_serial", fakeSerial+"\n")
	hwtest.WriteFile(t, root, "etc/machine-id", fakeMachineID+"\n")
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/win11.xml",
		"<domain type='kvm'>\n  <name>win11</name>\n  <metadata>\n    <orthogonals:guest xmlns:orthogonals='https://github.com/stronautt/orthogonals'>\n"+
			"      <orthogonals:user>user</orthogonals:user>\n      <orthogonals:password>"+secretPassword+"</orthogonals:password>\n"+
			"      <orthogonals:locale>uk-UA</orthogonals:locale>\n      <orthogonals:resolution>3840x2160</orthogonals:resolution>\n"+
			"    </orthogonals:guest>\n  </metadata>\n</domain>\n")
	hostname, _ := os.Hostname()
	hwtest.WriteFile(t, root, "etc/orthogonals/notes.txt",
		"host "+hostname+" mac "+fakeMAC+" uuid "+fakeUUID+" serial "+fakeSerial+" id "+fakeMachineID+"\n")
	hwtest.WriteFile(t, root, "etc/libvirt/hooks/qemu", "#!/bin/bash\n# dispatcher for "+hostname+"\n")
	hwtest.WriteFile(t, root, "var/log/orthogonals/hooks.log",
		"gpu-detach: handover start on "+hostname+"\n")

	bin := t.TempDir()
	fakeBin(t, bin, "lspci", "01:00.0 VGA [0300]: NVIDIA [10de:2206]\nserial "+fakeSerial)
	fakeBin(t, bin, "journalctl", "kernel: vfio-pci 0000:01:00.0 mac "+fakeMAC+" uuid "+fakeUUID)
	t.Setenv("PATH", bin)

	var buf bytes.Buffer
	if err := WriteBundle(&buf, root, refResult()); err != nil {
		t.Fatal(err)
	}
	entries := readTarGz(t, &buf)

	for _, name := range []string{
		"detect.json", "lspci.txt", "journal.txt",
		"configs/etc/orthogonals/vms/win11.xml",
		"configs/etc/orthogonals/notes.txt",
		"configs/etc/libvirt/hooks/qemu",
		"configs/var/log/orthogonals/hooks.log",
	} {
		if _, ok := entries[name]; !ok {
			t.Errorf("bundle missing entry %q (have %v)", name, keys(entries))
		}
	}

	var all strings.Builder
	for _, data := range entries {
		all.WriteString(data)
	}
	secrets := []string{secretPassword, fakeSerial, fakeMAC, fakeUUID, fakeMachineID}
	if len(hostname) >= 2 {
		secrets = append(secrets, hostname)
	}
	for _, s := range secrets {
		if strings.Contains(all.String(), s) {
			t.Errorf("bundle leaks %q", s)
		}
	}

	xml := entries["configs/etc/orthogonals/vms/win11.xml"]
	if strings.Contains(xml, "user</orthogonals:user>") || !strings.Contains(xml, "[redacted]") {
		t.Errorf("guest credentials not stripped from the domain XML: %q", xml)
	}
	if !strings.Contains(xml, "win11") || !strings.Contains(xml, "uk-UA") {
		t.Errorf("domain XML lost non-credential content: %q", xml)
	}
	if !strings.Contains(entries["detect.json"], `"devices"`) {
		t.Error("detect.json missing detect result content")
	}
}

// A host without lspci/journalctl still produces a bundle, noting the gap.
func TestWriteBundleMissingTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var buf bytes.Buffer
	if err := WriteBundle(&buf, t.TempDir(), refResult()); err != nil {
		t.Fatal(err)
	}
	entries := readTarGz(t, &buf)
	if !strings.Contains(entries["lspci.txt"], "unavailable") {
		t.Errorf("lspci.txt should note the missing tool: %q", entries["lspci.txt"])
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
