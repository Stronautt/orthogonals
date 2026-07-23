package media

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdomanski/iso9660"
)

// requireLoopMount skips unless this process can drive the loop subsystem.
// LOOP_CTL_GET_FREE and the mount(2) that follows both need CAP_SYS_ADMIN in
// the initial user namespace, and iso9660/udf are not FS_USERNS_MOUNT, so no
// unprivileged workaround exists — `make test-vm` is where these actually run.
func requireLoopMount(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("loop-mounting needs root — covered by the VM tier (make test-vm)")
	}
	if _, err := os.Stat("/dev/loop-control"); err != nil {
		t.Skipf("no /dev/loop-control: %v", err)
	}
}

// testISO writes a provision ISO through the real writer.
func testISO(t *testing.T, files []Artifact) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provision.iso")
	if err := BuildISO(files, nil, path, &strings.Builder{}); err != nil {
		t.Fatalf("BuildISO: %v", err)
	}
	return path
}

// TestMountISOWithoutPrivilegeExplainsWhy pins the unprivileged failure, which
// is the path a user hits running `orthogonals media` without sudo. It must
// name root rather than surface a bare EACCES from an ioctl.
func TestMountISOWithoutPrivilegeExplainsWhy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root — this asserts the unprivileged failure")
	}
	_, _, err := mountISOLoop(testISO(t, []Artifact{{Name: "a.txt", Content: []byte("x")}}))
	if err == nil {
		t.Fatal("mountISOLoop succeeded without privileges")
	}
	if !strings.Contains(err.Error(), "root required") {
		t.Errorf("error does not point at the missing privilege: %v", err)
	}
}

// TestMountISORoundTrip puts a BuildISO product through a real kernel
// filesystem driver: everywhere else the writer's bytes are only compared to a
// golden, which cannot say whether the kernel can mount them.
func TestMountISORoundTrip(t *testing.T) {
	requireLoopMount(t)
	want := []Artifact{
		{Name: "autounattend.xml", Content: []byte("<unattend/>\n")},
		{Name: "provision.ps1", Content: []byte("Write-Host hello\n")},
	}
	iso := testISO(t, want)

	mnt, cleanup, err := mountISOLoop(iso)
	if err != nil {
		t.Fatalf("mountISOLoop: %v", err)
	}
	for _, a := range want {
		// ISO9660 without Joliet upper-cases names and may append a version.
		got, err := readISOFile(mnt, a.Name)
		if err != nil {
			t.Fatalf("%s: %v (mount contains %v)", a.Name, err, ls(t, mnt))
		}
		if string(got) != string(a.Content) {
			t.Errorf("%s: content round-tripped as %q, want %q", a.Name, got, a.Content)
		}
	}

	cleanup()
	if _, err := os.Stat(mnt); !os.IsNotExist(err) {
		t.Errorf("cleanup left the mountpoint behind at %s (still mounted?)", mnt)
	}

	// A second attach proves cleanup released the loop device rather than
	// leaking it: the free-device search would otherwise drift device by device.
	mnt2, cleanup2, err := mountISOLoop(iso)
	if err != nil {
		t.Fatalf("second mount failed — the first leaked a loop device: %v", err)
	}
	cleanup2()
	_ = mnt2
}

// TestValidateWin11ISOAgainstARealISO drives the whole validator over a real
// mount instead of the fixture directory the unit tests swap in, so the seam's
// contract is checked against the kernel rather than against itself.
func TestValidateWin11ISOAgainstARealISO(t *testing.T) {
	requireLoopMount(t)

	// BuildISO cannot emit subdirectories (checkISOName rejects "/"), and the
	// validator looks for sources/install.wim, so drive the writer directly.
	dir := t.TempDir()
	wim := filepath.Join(dir, "install.wim")
	writeTestWIM(t, wim, wimXMLProAndHome)

	w, err := iso9660.NewWriter()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Cleanup() }()
	if err := w.AddLocalFile(wim, "sources/install.wim"); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(dir, "win11.iso")
	f, err := os.Create(iso)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteTo(f, "WIN11"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	info, err := ValidateWin11ISO(iso, &out)
	if err != nil {
		t.Fatalf("ValidateWin11ISO over a real mount: %v", err)
	}
	if info.DefaultLanguage != "uk-UA" {
		t.Errorf("default language %q, want uk-UA", info.DefaultLanguage)
	}
	if !strings.Contains(out.String(), Edition) {
		t.Errorf("validator did not report the edition it found: %q", out.String())
	}
}

// readISOFile finds a file under mnt, tolerating the ISO9660 name mangling the
// writer applies without Joliet (upper case, optional ";1" version suffix).
func readISOFile(mnt, name string) ([]byte, error) {
	for _, candidate := range []string{name, strings.ToUpper(name), strings.ToUpper(name) + ";1"} {
		if b, err := os.ReadFile(filepath.Join(mnt, candidate)); err == nil {
			return b, nil
		}
	}
	return nil, os.ErrNotExist
}

func ls(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"<unreadable: " + err.Error() + ">"}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
