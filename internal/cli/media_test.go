package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/media/mediatest"
)

func TestMediaRequiresISO(t *testing.T) {
	code, _, stderr := run(t, "media")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(stderr, "--win11-iso") {
		t.Errorf("usage should name --win11-iso: %s", stderr)
	}
}

func TestMediaDryRun(t *testing.T) {
	root := t.TempDir()
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "media", "--root", root, "--win11-iso", iso)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"would validate " + iso,
		"virtio-win ISO",
		"NVIDIA Windows driver",
		"Looking Glass host",
		"would build provision ISO at",
		"win11-provision.iso",
		"dry run — re-run with --yes to apply",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("dry-run output missing %q\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "var/lib/orthogonals")); err == nil {
		t.Error("dry run created state under root")
	}
}

// writeGuestMeta registers a VM whose domain XML carries the given guest metadata.
func writeGuestMeta(t *testing.T, root, vm, user, password, locale, resolution string) {
	t.Helper()
	xml := "<domain type='kvm'>\n  <name>" + vm + "</name>\n  <metadata>\n" +
		"    <orthogonals:guest xmlns:orthogonals='https://github.com/stronautt/orthogonals'>\n" +
		"      <orthogonals:user>" + user + "</orthogonals:user>\n" +
		"      <orthogonals:password>" + password + "</orthogonals:password>\n" +
		"      <orthogonals:locale>" + locale + "</orthogonals:locale>\n" +
		"      <orthogonals:resolution>" + resolution + "</orthogonals:resolution>\n" +
		"    </orthogonals:guest>\n  </metadata>\n</domain>\n"
	path := filepath.Join(root, "etc/orthogonals/vms", vm+".xml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(xml), 0o600); err != nil {
		t.Fatal(err)
	}
}

// media turns a VM locale the media cannot display into a loud failure.
func TestMediaLocaleNotOnMedia(t *testing.T) {
	mediatest.InstallFixture(t, mediatest.WimXMLProUkrainian)
	root := t.TempDir()
	writeGuestMeta(t, root, "win11", "user", "pw", "en-US", "3840x2160")
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, _, stderr := run(t, "media", "--root", root, "--yes", "--win11-iso", iso)
	if code != 1 {
		t.Fatalf("exit %d, want 1\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "cannot display") ||
		!strings.Contains(stderr, "en-US") || !strings.Contains(stderr, "uk-UA") {
		t.Errorf("error must name the rejected locale and the media's languages, got: %s", stderr)
	}
}

// fakeDownloads points every pin at a local server with test-computed hashes.
func fakeDownloads(t *testing.T) []artifacts.Download {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload for " + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	var fakes []artifacts.Download
	for _, d := range artifacts.Downloads() {
		sum := sha256.Sum256([]byte("payload for /" + d.File))
		d.URL = srv.URL + "/" + d.File
		d.SHA256 = hex.EncodeToString(sum[:])
		fakes = append(fakes, d)
	}
	return fakes
}

func TestMediaYesEndToEnd(t *testing.T) {
	fakes := fakeDownloads(t)
	prev := downloads
	downloads = func() []artifacts.Download { return fakes }
	t.Cleanup(func() { downloads = prev })

	mediatest.InstallFixture(t, mediatest.WimXMLProUkrainian)
	root := t.TempDir()
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "media", "--root", root, "--yes", "--win11-iso", iso)
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "provision ISO ready") {
		t.Errorf("missing success message:\n%s", stdout)
	}
	for _, d := range fakes {
		if _, err := os.Stat(filepath.Join(media.CacheDir(root), d.File)); err != nil {
			t.Errorf("cache is missing %s", d.File)
		}
	}
	st, err := os.Stat(media.ISOPath(root, "win11"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("ISO mode = %04o, want 0600", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "etc/orthogonals")); err == nil {
		t.Error("media wrote under /etc/orthogonals")
	}
}

func TestMediaNvidiaInstallerShownInDryRun(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "media", "--root", t.TempDir(), "--win11-iso", iso,
		"--nvidia-installer", "/home/user/driver.exe")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "user-supplied /home/user/driver.exe") {
		t.Errorf("dry run should show the user-supplied installer:\n%s", stdout)
	}
}
