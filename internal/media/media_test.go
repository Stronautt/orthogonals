package media

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"encoding/binary"
	"unicode/utf16"

	"github.com/kdomanski/iso9660"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

var update = flag.Bool("update", false, "rewrite golden files")

func referenceProfile(t *testing.T) Profile {
	t.Helper()
	p, err := NewProfile("user", "s3cretPassw0rd16", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mustRender(t *testing.T, p Profile) map[string][]byte {
	t.Helper()
	arts, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte, len(arts))
	for _, a := range arts {
		out[a.Name] = a.Content
	}
	return out
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s does not match golden file (run go test ./internal/media -update):\n%s", name, got)
	}
}

func wellFormedXML(t *testing.T, b []byte) {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				return
			}
			t.Fatalf("not well-formed XML: %v", err)
		}
	}
}

func TestRenderGolden(t *testing.T) {
	arts := mustRender(t, referenceProfile(t))
	for _, name := range []string{"autounattend.xml", "vdd_settings.xml", "provision.ps1"} {
		if _, ok := arts[name]; !ok {
			t.Fatalf("Render is missing %s", name)
		}
	}
	checkGolden(t, "autounattend.xml", arts["autounattend.xml"])
	checkGolden(t, "vdd_settings.xml", arts["vdd_settings.xml"])
	checkGolden(t, "provision.ps1", arts["provision.ps1"])
}

func TestProvisionTrustsTheVDDPublisher(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	if !strings.Contains(s, "Add-TrustedPublisher $cat.FullName") {
		t.Error("provision.ps1 installs the VDD without trusting its publisher — that hangs on a modal prompt")
	}
	for _, store := range []string{"TrustedPublisher", "Root", "LocalMachine"} {
		if !strings.Contains(s, store) {
			t.Errorf("provision.ps1 does not populate the %s certificate store", store)
		}
	}
}

func TestProvisionStopsWindowsUpdateFromInstallingDrivers(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	for _, want := range []string{
		"SearchOrderConfig",
		"DontSearchWindowsUpdate",
		"ExcludeWUDriversInQualityUpdate",
		"Stop-Service wuauserv",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q — Windows Update will race the pinned driver", want)
		}
	}
	if !strings.Contains(s, "Set-Service wuauserv -StartupType Manual") {
		t.Error("cleanup must hand the update service back, or the guest never gets security updates")
	}
}

func TestProvisionDebloatsTheGuest(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	for _, want := range []string{
		"win11debloat.zip",
		"Win11Debloat.ps1",
		"-Silent -RunDefaults",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q", want)
		}
	}
	debloat := strings.Index(s, "Name = 'debloat'")
	for _, stage := range []string{"Name = 'nvidia-driver'", "Name = 'vdd'", "Name = 'lg-service'"} {
		if i := strings.Index(s, stage); i == -1 || i > debloat {
			t.Errorf("debloat stage must run after %s", stage)
		}
	}
}

func TestProvisionInstallsGuestAgent(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	if !strings.Contains(s, `virtio-win-guest-tools.exe`) {
		t.Error("provision.ps1 never installs the virtio-win guest-tools bundle")
	}
	if !strings.Contains(s, `'spice-agent'`) {
		t.Error("provision.ps1 never checks the SPICE vdagent service — no clipboard without it")
	}
	if !strings.Contains(s, "did not take effect") {
		t.Error("provision.ps1 does not verify a stage reached its done state after running")
	}
	if !strings.Contains(s, `$code -ne 0 -and -not (Get-NvidiaGpu)`) {
		t.Error("provision.ps1 fails the nvidia-driver stage on exit code alone, without checking the adapter")
	}
}

func TestAutounattendContent(t *testing.T) {
	b := mustRender(t, referenceProfile(t))["autounattend.xml"]
	wellFormedXML(t, b)
	s := string(b)

	for _, banned := range []string{"LabConfig", "BypassTPMCheck", "BypassSecureBootCheck", "BypassRAMCheck", "BypassNRO"} {
		if strings.Contains(s, banned) {
			t.Errorf("autounattend.xml contains forbidden bypass key %s", banned)
		}
	}
	for _, want := range []string{
		"<Value>Windows 11 Pro</Value>",
		"<Key>VK7JG-NPHTM-C97JM-9MPGT-3V66T</Key>",
		`viostor\w11\amd64`,
		"ORTHOGONALS",
		"provision.ps1",
		"<Name>user</Name>",
		"<Value>s3cretPassw0rd16</Value>",
		"<InputLocale>en-US</InputLocale>",
		"bcdedit /set {default} recoveryenabled No",
		"bcdedit /set {default} bootstatuspolicy IgnoreAllFailures",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("autounattend.xml is missing %q", want)
		}
	}
}

func TestAutounattendEscaping(t *testing.T) {
	p, err := NewProfile("user", `p<&>"pass`, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	b := mustRender(t, p)["autounattend.xml"]
	wellFormedXML(t, b)
	if !strings.Contains(string(b), "p&lt;&amp;&gt;&#34;pass") {
		t.Errorf("password not XML-escaped:\n%s", b)
	}
}

func TestProvisionContent(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	for _, want := range []string{
		"virtio-win-guest-tools.exe", "/install /quiet /norestart",
		"-s -noreboot",
		"looking-glass-host-setup.exe",
		"'/S'",
		"nefconc.exe", "ROOT\\MttVDD",
		"TrustedPublisher",
		"GPU_FRIENDLY_NAME",
		"C:\\VirtualDisplayDriver",
		"Looking Glass (host)",
		"C:\\Windows\\Panther\\unattend.xml",
		"C:\\Windows\\System32\\Sysprep\\unattend.xml",
		"provision-status.json",
		"orthogonals-provision",
		"'user'",
		"AutoAdminLogon",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q", want)
		}
	}
	for _, d := range artifacts.ProvisionPayloads() {
		if !strings.Contains(s, d.File) {
			t.Errorf("provision.ps1 never references payload %s", d.File)
		}
	}
	if strings.Contains(s, "s3cretPassw0rd16") {
		t.Error("provision.ps1 must not embed the guest password")
	}
}

func TestProvisionUserEscaping(t *testing.T) {
	p, err := NewProfile("o'user", "pw", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := string(mustRender(t, p)["provision.ps1"])
	if !strings.Contains(s, "'o''user'") {
		t.Error("user name not escaped for PowerShell single quotes")
	}
}

// TestProvisionPwshParse parse-checks the rendered script when pwsh is installed.
func TestProvisionPwshParse(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not installed")
	}
	b := mustRender(t, referenceProfile(t))["provision.ps1"]
	path := filepath.Join(t.TempDir(), "provision.ps1")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	check := fmt.Sprintf(`$errs = $null; [void][System.Management.Automation.Language.Parser]::ParseFile('%s', [ref]$null, [ref]$errs); if ($errs.Count) { $errs | Out-String | Write-Output; exit 1 }`, path)
	out, err := exec.Command("pwsh", "-NoProfile", "-Command", check).CombinedOutput()
	if err != nil {
		t.Errorf("provision.ps1 does not parse: %v\n%s", err, out)
	}
}

// agentFake scripts the guest agent with the standard guest-exec responder.
func agentFake(stdout string, exitCode int) *virttest.Fake {
	return &virttest.Fake{State: "running", Agent: virttest.Responder(stdout, "", exitCode)}
}

func TestProvisionStatusDone(t *testing.T) {
	f := agentFake(`{"stage":"done","ok":true,"error":""}`, 0)
	st, err := ProvisionStatus(f, "win11")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done() || st.Stage != "done" {
		t.Errorf("status = %+v, want done", st)
	}
	if !strings.Contains(strings.Join(f.Calls, "\n"), "agent win11") {
		t.Errorf("agent command not sent to win11:\n%v", f.Calls)
	}
}

func TestProvisionStatusFailedStage(t *testing.T) {
	st, err := ProvisionStatus(agentFake(`{"stage":"nvidia-driver","ok":false,"error":"boom"}`, 0), "win11")
	if err != nil {
		t.Fatal(err)
	}
	if st.Done() || st.OK || st.Stage != "nvidia-driver" || st.Error != "boom" {
		t.Errorf("status = %+v, want failed nvidia-driver stage", st)
	}
}

func TestProvisionStatusMissingFile(t *testing.T) {
	if _, err := ProvisionStatus(agentFake("", 1), "win11"); !errors.Is(err, errNoStatus) {
		t.Errorf("want errNoStatus, got %v", err)
	}
}

func TestProvisionStatusAgentDown(t *testing.T) {
	_, err := ProvisionStatus(&virttest.Fake{State: "running"}, "win11")
	if err == nil || errors.Is(err, errNoStatus) {
		t.Errorf("want a hard agent error, got %v", err)
	}
}

func TestGuestExecOutput(t *testing.T) {
	out, errOut, code, err := GuestExec(agentFake("hello from guest", 0), "win11", "cmd.exe", "/c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || string(out) != "hello from guest" || len(errOut) != 0 {
		t.Errorf("out = %q errOut = %q code = %d", out, errOut, code)
	}
}

func TestGuestExecStderr(t *testing.T) {
	f := &virttest.Fake{State: "running", Agent: virttest.Responder("", "no devices were found", 6)}
	out, errOut, code, err := GuestExec(f, "win11", `C:\Windows\System32\nvidia-smi.exe`)
	if err != nil {
		t.Fatal(err)
	}
	if code != 6 || string(errOut) != "no devices were found" || len(out) != 0 {
		t.Errorf("out = %q errOut = %q code = %d, want guest stderr surfaced with exit 6", out, errOut, code)
	}
}

func TestVDDSettingsContent(t *testing.T) {
	p, err := NewProfile("user", "pw", "", 3840, 2160)
	if err != nil {
		t.Fatal(err)
	}
	b := mustRender(t, p)["vdd_settings.xml"]
	wellFormedXML(t, b)
	s := string(b)
	for _, want := range []string{"<width>3840</width>", "<height>2160</height>", "GPU_FRIENDLY_NAME"} {
		if !strings.Contains(s, want) {
			t.Errorf("vdd_settings.xml is missing %q", want)
		}
	}
	for _, want := range []string{"<width>1920</width>", "<width>2560</width>", "<width>3440</width>", "<g_refresh_rate>144</g_refresh_rate>"} {
		if !strings.Contains(s, want) {
			t.Errorf("vdd_settings.xml is missing %q", want)
		}
	}
}

func TestGuestModes(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		want          []Mode
	}{
		{"default", 0, 0, []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}, {3840, 2160}}},
		{"1440p max", 2560, 1440, []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}}},
		{"1080p max", 1920, 1080, []Mode{{1920, 1080}}},
		{"odd max", 1600, 900, []Mode{{1920, 1080}, {1600, 900}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewProfile("user", "pw", "", tc.width, tc.height)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(p.Modes, tc.want) {
				t.Errorf("Modes = %v, want %v", p.Modes, tc.want)
			}
		})
	}
}

func TestNewProfileValidation(t *testing.T) {
	cases := []struct {
		name          string
		user, pass    string
		width, height int
	}{
		{"empty user", "", "pw", 0, 0},
		{"forbidden chars", `us:er`, "pw", 0, 0},
		{"too long", strings.Repeat("a", 21), "pw", 0, 0},
		{"empty password", "user", "", 0, 0},
		{"bad resolution", "user", "pw", -1, 1080},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewProfile(c.user, c.pass, "", c.width, c.height); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFetch(t *testing.T) {
	content := []byte("fake installer payload")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(content)
	}))
	defer srv.Close()
	d := artifacts.Download{Name: "thing", Version: "1.0", URL: srv.URL, SHA256: sum(content), File: "thing.bin"}

	root := t.TempDir()
	var out strings.Builder
	path, err := Fetch(root, d, &out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("fetched content = %q", got)
	}
	if want := filepath.Join(CacheDir(root), "thing.bin"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}

	if _, err := Fetch(root, d, &out); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("server hits = %d, want 1 (cache miss on re-run)", hits)
	}
}

func TestFetchStalledConnectionFails(t *testing.T) {
	old := stallTimeout
	stallTimeout = 20 * time.Millisecond
	defer func() { stallTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("x"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()
	d := artifacts.Download{Name: "thing", Version: "1.0", URL: srv.URL, SHA256: sum([]byte("x")), File: "thing.bin"}

	root := t.TempDir()
	_, err := Fetch(root, d, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "connection stalled") {
		t.Fatalf("err = %v, want a connection-stalled error", err)
	}
	if _, err := os.Stat(filepath.Join(CacheDir(root), "thing.bin.part")); !os.IsNotExist(err) {
		t.Error(".part file survived a stalled download")
	}
}

func TestFetchChecksumMismatchHardFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered payload"))
	}))
	defer srv.Close()
	d := artifacts.Download{Name: "thing", Version: "1.0", URL: srv.URL, SHA256: sum([]byte("expected payload")), File: "thing.bin"}

	root := t.TempDir()
	var out strings.Builder
	if _, err := Fetch(root, d, &out); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("want SHA256 mismatch error, got %v", err)
	}
	entries, _ := os.ReadDir(CacheDir(root))
	for _, e := range entries {
		t.Errorf("cache should be empty after mismatch, found %s", e.Name())
	}
}

func TestFetchCorruptCacheHardFails(t *testing.T) {
	d := artifacts.Download{Name: "thing", Version: "1.0", URL: "http://unused.invalid", SHA256: sum([]byte("good")), File: "thing.bin"}
	root := t.TempDir()
	if err := os.MkdirAll(CacheDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(CacheDir(root), "thing.bin"), []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if _, err := Fetch(root, d, &out); err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("want SHA256 mismatch error for corrupt cache, got %v", err)
	}
}

func TestFetchHTTPErrorFails(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	d := artifacts.Download{Name: "thing", Version: "1.0", URL: srv.URL, SHA256: sum([]byte("x")), File: "thing.bin"}
	var out strings.Builder
	if _, err := Fetch(t.TempDir(), d, &out); err == nil {
		t.Fatal("want error on HTTP 404, got nil")
	}
}

func TestImportInstaller(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(t.TempDir(), "user-driver.exe")
	if err := os.WriteFile(src, []byte("user supplied installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	path, err := ImportInstaller(root, artifacts.NVIDIADriver, src, &out)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != artifacts.NVIDIADriver.File {
		t.Errorf("imported as %s, want %s", filepath.Base(path), artifacts.NVIDIADriver.File)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "user supplied installer" {
		t.Errorf("imported content = %q", b)
	}
	if !strings.Contains(out.String(), sum(b)) {
		t.Error("import should print the file's SHA256 for the record")
	}
}

// TestBuildISO is the provision-ISO contract.
func TestBuildISO(t *testing.T) {
	arts, err := Render(referenceProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	payloadDir := t.TempDir()
	var payloads []string
	for _, d := range artifacts.ProvisionPayloads() {
		p := filepath.Join(payloadDir, d.File)
		if err := os.WriteFile(p, []byte(d.Name), 0o644); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, p)
	}

	out := filepath.Join(t.TempDir(), "win11-provision.iso")
	var buf strings.Builder
	if err := BuildISO(arts, payloads, out, &buf); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("ISO mode = %04o, want 0600", st.Mode().Perm())
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	img, err := iso9660.OpenImage(f)
	if err != nil {
		t.Fatal(err)
	}
	rootDir, err := img.RootDir()
	if err != nil {
		t.Fatal(err)
	}
	children, err := rootDir.GetChildren()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range children {
		got[c.Name()] = true
	}
	want := map[string]bool{
		"autounattend.xml": true, "provision.ps1": true, "vdd_settings.xml": true,
		"nvidia-driver.exe": true, "looking-glass-host.zip": true,
		"vdd-driver.zip": true, "nefcon.zip": true, "win11debloat.zip": true,
	}
	for name := range want {
		if !got[name] {
			t.Errorf("ISO is missing %s (got %v)", name, got)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("ISO has unexpected %s", name)
		}
	}
}

func TestBuildISORefusesLongNames(t *testing.T) {
	long := Artifact{Name: "a-filename-well-past-thirty-characters.xml", Content: []byte("x")}
	err := BuildISO([]Artifact{long}, nil, filepath.Join(t.TempDir(), "out.iso"), &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "Joliet") {
		t.Fatalf("want a loud no-Joliet name refusal, got %v", err)
	}
}

const wimXMLProAndHome = `<WIM><IMAGE INDEX="1"><NAME>Windows 11 Home</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE>` +
	`<IMAGE INDEX="2"><NAME>Windows 11 Pro</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE></WIM>`

const wimXMLHomeOnly = `<WIM><IMAGE INDEX="1"><NAME>Windows 11 Home</NAME></IMAGE></WIM>`

// writeTestWIM hand-builds a minimal install.wim.
func writeTestWIM(t *testing.T, path, xmlBody string) {
	t.Helper()
	u := utf16.Encode([]rune(xmlBody))
	payload := make([]byte, 2+len(u)*2)
	binary.LittleEndian.PutUint16(payload, 0xfeff)
	for i, r := range u {
		binary.LittleEndian.PutUint16(payload[2+i*2:], r)
	}
	hdr := make([]byte, 208)
	copy(hdr, "MSWIM\x00\x00\x00")
	binary.LittleEndian.PutUint32(hdr[8:], 208)
	binary.LittleEndian.PutUint64(hdr[72:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(hdr[80:], 208)
	binary.LittleEndian.PutUint64(hdr[88:], uint64(len(payload)))
	if err := os.WriteFile(path, append(hdr, payload...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeMountISO points the validator's mount seam at a fixture directory.
func fakeMountISO(t *testing.T, populate func(dir string)) {
	t.Helper()
	old := MountISO
	MountISO = func(string) (string, func(), error) {
		dir := t.TempDir()
		if populate != nil {
			populate(dir)
		}
		return dir, func() {}, nil
	}
	t.Cleanup(func() { MountISO = old })
}

func TestValidateWin11ISO(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	withWIM := func(xmlBody string) func(string) {
		return func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeTestWIM(t, filepath.Join(dir, "sources", "install.wim"), xmlBody)
		}
	}

	t.Run("pro present", func(t *testing.T) {
		fakeMountISO(t, withWIM(wimXMLProAndHome))
		var out strings.Builder
		info, err := ValidateWin11ISO(iso, &out)
		if err != nil {
			t.Fatal(err)
		}
		if info.DefaultLanguage != "uk-UA" || len(info.Languages) != 1 || info.Languages[0] != "uk-UA" {
			t.Errorf("WimInfo = %+v, want uk-UA languages", info)
		}
	})

	t.Run("pro absent lists editions", func(t *testing.T) {
		fakeMountISO(t, withWIM(wimXMLHomeOnly))
		var out strings.Builder
		_, err := ValidateWin11ISO(iso, &out)
		if err == nil {
			t.Fatal("want error for Home-only ISO")
		}
		if !strings.Contains(err.Error(), "Windows 11 Home") {
			t.Errorf("error should list the available editions, got: %v", err)
		}
	})

	t.Run("not an installation iso", func(t *testing.T) {
		fakeMountISO(t, nil)
		var out strings.Builder
		if _, err := ValidateWin11ISO(iso, &out); err == nil || !strings.Contains(err.Error(), "install.wim") {
			t.Fatalf("want missing install.wim error, got %v", err)
		}
	})

	t.Run("not a wim", func(t *testing.T) {
		fakeMountISO(t, func(dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "sources", "install.wim"), []byte("garbage"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		var out strings.Builder
		if _, err := ValidateWin11ISO(iso, &out); err == nil {
			t.Fatal("want error for a corrupt install.wim")
		}
	})

	t.Run("missing iso file", func(t *testing.T) {
		var out strings.Builder
		if _, err := ValidateWin11ISO(filepath.Join(t.TempDir(), "nope.iso"), &out); err == nil {
			t.Fatal("want error for missing ISO path")
		}
	})
}
