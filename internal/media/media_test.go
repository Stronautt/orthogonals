package media

import (
	"crypto/sha256"
	"encoding/base64"
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/artifacts"
)

var update = flag.Bool("update", false, "rewrite golden files")

// reference matches the plan's Defaults table: user "user", en-US, 4K max.
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

// The VDD is Authenticode-signed by a third party (SignPath Foundation), not
// WHQL: without pre-trusting its publisher Windows raises a modal "cannot
// verify the publisher" prompt, and a silent installer cannot answer it. The
// NVIDIA package is WHQL-signed throughout and needs no such help.
func TestProvisionTrustsTheVDDPublisher(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	if !strings.Contains(s, "Add-TrustedPublisher $cat.FullName") {
		t.Error("provision.ps1 installs the VDD without trusting its publisher — that hangs on a modal prompt")
	}
	// the trust must reach the machine stores the driver installer consults
	for _, store := range []string{"TrustedPublisher", "Root", "LocalMachine"} {
		if !strings.Contains(s, store) {
			t.Errorf("provision.ps1 does not populate the %s certificate store", store)
		}
	}
}

// Windows Update ships its own NVIDIA driver and installs it mid-provisioning:
// the guest lands on an unpinned version, the concurrent device install throws
// a modal prompt, and the box reboots mid-stage. Pinned artifacts are the whole
// premise of the tool, so provisioning must own the drivers.
func TestProvisionStopsWindowsUpdateFromInstallingDrivers(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	for _, want := range []string{
		"SearchOrderConfig",               // never fetch drivers from Windows Update
		"DontSearchWindowsUpdate",         // ... and the policy that enforces it
		"ExcludeWUDriversInQualityUpdate", // ... including inside quality updates
		"Stop-Service wuauserv",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q — Windows Update will race the pinned driver", want)
		}
	}
	// the update service must come back: only driver search stays disabled
	if !strings.Contains(s, "Set-Service wuauserv -StartupType Manual") {
		t.Error("cleanup must hand the update service back, or the guest never gets security updates")
	}
}

// Win11Debloat runs from the provision ISO, never fetched by the guest, and
// silently — a prompt would hang provisioning like every other modal has.
func TestProvisionDebloatsTheGuest(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	for _, want := range []string{
		"win11debloat.zip",     // shipped on the ISO, not downloaded in-guest
		"Win11Debloat.ps1",     // the entry script inside the archive
		"-Silent -RunDefaults", // unattended, defaults keep the gaming apps
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q", want)
		}
	}
	// it must not run before the drivers are in: a debloat regression must not
	// be able to disturb the GPU, the VDD or the Looking Glass service
	debloat := strings.Index(s, "Name = 'debloat'")
	for _, stage := range []string{"Name = 'nvidia-driver'", "Name = 'vdd'", "Name = 'lg-service'"} {
		if i := strings.Index(s, stage); i == -1 || i > debloat {
			t.Errorf("debloat stage must run after %s", stage)
		}
	}
}

// The host polls provisioning solely through the QEMU guest agent, and the
// clipboard rides the SPICE vdagent — both ship in virtio-win-guest-tools.exe,
// not in the bare virtio-win-gt MSI (drivers only). Installing only the MSI
// leaves the host polling a guest that can never answer, and a guest with no
// clipboard.
func TestProvisionInstallsGuestAgent(t *testing.T) {
	s := string(mustRender(t, referenceProfile(t))["provision.ps1"])
	if !strings.Contains(s, `virtio-win-guest-tools.exe`) {
		t.Error("provision.ps1 never installs the virtio-win guest-tools bundle")
	}
	if !strings.Contains(s, `'spice-agent'`) {
		t.Error("provision.ps1 never checks the SPICE vdagent service — no clipboard without it")
	}
	// a stage whose installer exits 0 without reaching its done state must
	// fail, not report success — that turns a broken install into a silent
	// multi-hour hang on the host side
	if !strings.Contains(s, "did not take effect") {
		t.Error("provision.ps1 does not verify a stage reached its done state after running")
	}
	// the NVIDIA installer restarts the guest mid-install and the re-run then
	// reports failure over a driver that is in fact installed — the adapter,
	// not the exit code, decides whether the stage failed
	if !strings.Contains(s, `$code -ne 0 -and -not (Get-NvidiaGpu)`) {
		t.Error("provision.ps1 fails the nvidia-driver stage on exit code alone, without checking the adapter")
	}
}

func TestAutounattendContent(t *testing.T) {
	b := mustRender(t, referenceProfile(t))["autounattend.xml"]
	wellFormedXML(t, b)
	s := string(b)

	// research §B2: the VM passes Win11 checks legitimately — the bypass
	// machinery is what 24H2/25H2 keeps breaking and must never come back
	for _, banned := range []string{"LabConfig", "BypassTPMCheck", "BypassSecureBootCheck", "BypassRAMCheck", "BypassNRO"} {
		if strings.Contains(s, banned) {
			t.Errorf("autounattend.xml contains forbidden bypass key %s", banned)
		}
	}
	for _, want := range []string{
		"<Value>Windows 11 Pro</Value>", // image selected by name
		// Microsoft's generic installation key for Pro — the only key allowed
		// here (it selects the edition, never activates); a real license key
		// must never land in the repo
		"<Key>VK7JG-NPHTM-C97JM-9MPGT-3V66T</Key>",
		`viostor\w11\amd64`, // VirtIO storage driver for Setup
		"ORTHOGONALS",       // provision volume located by label
		"provision.ps1",
		"<Name>user</Name>",
		"<Value>s3cretPassw0rd16</Value>",
		"<InputLocale>en-US</InputLocale>",
		// a hard stop must not wedge boot in Automatic Repair
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
		"virtio-win-guest-tools.exe", "/install /quiet /norestart", // virtio guest tools bundle, silent
		"-s -noreboot",                 // NVIDIA driver, silent
		"looking-glass-host-setup.exe", // bundled in the host zip
		"'/S'",                         // LG host silent install (brings IVSHMEM driver)
		"nefconc.exe", "ROOT\\MttVDD",  // VDD installed reboot-free via nefcon
		"TrustedPublisher",                             // driver signer trusted before install
		"GPU_FRIENDLY_NAME",                            // vdd_settings pinned to the detected GPU
		"C:\\VirtualDisplayDriver",                     // where VDD expects its settings
		"Looking Glass (host)",                         // service enabled as final install stage
		"C:\\Windows\\Panther\\unattend.xml",           // research §B2: cached answer file
		"C:\\Windows\\System32\\Sysprep\\unattend.xml", // carries the admin password
		"provision-status.json",                        // progress contract with the host poller
		"orthogonals-provision",                        // re-entry scheduled task
		"'user'",                                       // the admin account the task runs as
		"AutoAdminLogon",                               // autologon disabled once provisioning is done
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision.ps1 is missing %q", want)
		}
	}
	// every provision-ISO payload must be consumed by a stage
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

// TestProvisionPwshParse parse-checks the rendered script when pwsh is
// installed (CI has it; plain Fedora dev boxes may not).
func TestProvisionPwshParse(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not installed")
	}
	b := mustRender(t, referenceProfile(t))["provision.ps1"]
	path := filepath.Join(t.TempDir(), "provision.ps1")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// the path is baked into the command string: pwsh 7.6 does not expose
	// post -Command arguments as $args (a t.TempDir path needs no quoting)
	check := fmt.Sprintf(`$errs = $null; [void][System.Management.Automation.Language.Parser]::ParseFile('%s', [ref]$null, [ref]$errs); if ($errs.Count) { $errs | Out-String | Write-Output; exit 1 }`, path)
	out, err := exec.Command("pwsh", "-NoProfile", "-Command", check).CombinedOutput()
	if err != nil {
		t.Errorf("provision.ps1 does not parse: %v\n%s", err, out)
	}
}

// fakeVirshAgent fakes virsh qemu-agent-command: guest-exec returns a pid,
// guest-exec-status returns the given exit code and base64 stdout.
func fakeVirshAgent(t *testing.T, stdout string, exitCode int) string {
	t.Helper()
	return fakeVirshAgentStderr(t, stdout, "", exitCode)
}

func fakeVirshAgentStderr(t *testing.T, stdout, stderr string, exitCode int) string {
	t.Helper()
	dir := fakePath(t)
	b64 := base64.StdEncoding.EncodeToString([]byte(stdout))
	e64 := base64.StdEncoding.EncodeToString([]byte(stderr))
	extra := `case "$*" in
*guest-exec-status*) echo '{"return":{"exited":true,"exitcode":` + strconv.Itoa(exitCode) + `,"out-data":"` + b64 + `","err-data":"` + e64 + `"}}' ;;
*guest-exec*) echo '{"return":{"pid":7}}' ;;
esac`
	return fakeBin(t, dir, "virsh", extra)
}

func TestProvisionStatusDone(t *testing.T) {
	log := fakeVirshAgent(t, `{"stage":"done","ok":true,"error":""}`, 0)
	st, err := ProvisionStatus("win11")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Done() || st.Stage != "done" {
		t.Errorf("status = %+v, want done", st)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "qemu-agent-command win11") {
		t.Errorf("virsh argv missing qemu-agent-command win11:\n%s", b)
	}
}

func TestProvisionStatusFailedStage(t *testing.T) {
	fakeVirshAgent(t, `{"stage":"nvidia-driver","ok":false,"error":"boom"}`, 0)
	st, err := ProvisionStatus("win11")
	if err != nil {
		t.Fatal(err)
	}
	if st.Done() || st.OK || st.Stage != "nvidia-driver" || st.Error != "boom" {
		t.Errorf("status = %+v, want failed nvidia-driver stage", st)
	}
}

func TestProvisionStatusMissingFile(t *testing.T) {
	// type(1) exits nonzero when the status file does not exist yet
	fakeVirshAgent(t, "", 1)
	if _, err := ProvisionStatus("win11"); !errors.Is(err, errNoStatus) {
		t.Errorf("want errNoStatus, got %v", err)
	}
}

func TestProvisionStatusAgentDown(t *testing.T) {
	dir := fakePath(t)
	fakeBin(t, dir, "virsh", "exit 1")
	_, err := ProvisionStatus("win11")
	if err == nil || errors.Is(err, errNoStatus) {
		t.Errorf("want a hard virsh error, got %v", err)
	}
}

func TestGuestExecOutput(t *testing.T) {
	fakeVirshAgent(t, "hello from guest", 0)
	out, errOut, code, err := GuestExec("win11", "cmd.exe", "/c", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || string(out) != "hello from guest" || len(errOut) != 0 {
		t.Errorf("out = %q errOut = %q code = %d", out, errOut, code)
	}
}

func TestGuestExecStderr(t *testing.T) {
	fakeVirshAgentStderr(t, "", "no devices were found", 6)
	out, errOut, code, err := GuestExec("win11", `C:\Windows\System32\nvidia-smi.exe`)
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
	// full ladder at the 4K maximum: switching resolution in Windows must
	// never need a re-provision or a domain re-define
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
		// default (4K buffer): the whole standard ladder fits
		{"default", 0, 0, []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}, {3840, 2160}}},
		// 64M buffer: ultrawide 3440x1440 fits the same region, 4K does not
		{"1440p max", 2560, 1440, []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}}},
		{"1080p max", 1920, 1080, []Mode{{1920, 1080}}},
		// off-ladder maximum is still advertised
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

	// second call is served from the cache
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

	// send one byte, flush, then hang until the client's watchdog cancels —
	// exercises the stall-cancel path and the .part cleanup
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
	// neither the file nor a partial download may remain
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

// TestStageLayout is the provision-ISO layout contract: rendered files plus
// every payload at the ISO root.
func TestStageLayout(t *testing.T) {
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

	stage := t.TempDir()
	if err := Stage(stage, arts, payloads); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"autounattend.xml": true, "provision.ps1": true, "vdd_settings.xml": true,
		"nvidia-driver.exe": true, "looking-glass-host.zip": true,
		"vdd-driver.zip": true, "nefcon.zip": true, "win11debloat.zip": true,
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("staged ISO tree is missing %s", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("staged ISO tree has unexpected %s", name)
		}
	}
}

func fakePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// fakeBin installs an executable stub that appends its argv to a log file,
// then runs extra shell. Returns the log path.
func fakeBin(t *testing.T, dir, name, extra string) string {
	t.Helper()
	log := filepath.Join(dir, name+".log")
	script := "#!/bin/sh\necho \"$*\" >> \"" + log + "\"\n" + extra + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestBuildISO(t *testing.T) {
	dir := fakePath(t)
	// touch the argument following -o so the chmod after xorriso has a target
	log := fakeBin(t, dir, "xorriso", `prev=""; for a in "$@"; do [ "$prev" = "-o" ] && : > "$a"; prev="$a"; done`)

	stage := t.TempDir()
	out := filepath.Join(t.TempDir(), "win11-provision.iso")
	var buf strings.Builder
	if err := BuildISO(stage, out, &buf); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(b)
	for _, want := range []string{"-V " + VolumeLabel, "-o " + out, stage} {
		if !strings.Contains(argv, want) {
			t.Errorf("xorriso argv missing %q: %s", want, argv)
		}
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	// the ISO carries the guest password in autounattend.xml
	if st.Mode().Perm() != 0o600 {
		t.Errorf("ISO mode = %04o, want 0600", st.Mode().Perm())
	}
}

const wiminfoProAndHome = `WIM Information:
----------------
Path:           install.wim
GUID:           0xdeadbeef

Available Images:
-----------------
Index:                  1
Name:                   Windows 11 Home
Description:            Windows 11 Home
Languages:              uk-UA
Default Language:       uk-UA

Index:                  2
Name:                   Windows 11 Pro
Description:            Windows 11 Pro
Languages:              uk-UA
Default Language:       uk-UA
`

const wiminfoHomeOnly = `Available Images:
-----------------
Index:                  1
Name:                   Windows 11 Home
Description:            Windows 11 Home
`

func fakeMountTools(t *testing.T, wiminfoOut string, createWim bool) {
	t.Helper()
	dir := fakePath(t)
	extra := ""
	if createWim {
		// mount -o loop,ro <iso> <mnt>: create sources/install.wim under $4
		extra = `mkdir -p "$4/sources"; : > "$4/sources/install.wim"`
	}
	fakeBin(t, dir, "mount", extra)
	fakeBin(t, dir, "umount", "")
	fakeBin(t, dir, "wiminfo", "cat <<'EOF'\n"+wiminfoOut+"EOF")
}

func TestValidateWin11ISO(t *testing.T) {
	iso := filepath.Join(t.TempDir(), "win11.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("pro present", func(t *testing.T) {
		fakeMountTools(t, wiminfoProAndHome, true)
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
		fakeMountTools(t, wiminfoHomeOnly, true)
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
		fakeMountTools(t, "", false)
		var out strings.Builder
		if _, err := ValidateWin11ISO(iso, &out); err == nil || !strings.Contains(err.Error(), "install.wim") {
			t.Fatalf("want missing install.wim error, got %v", err)
		}
	})

	t.Run("missing iso file", func(t *testing.T) {
		var out strings.Builder
		if _, err := ValidateWin11ISO(filepath.Join(t.TempDir(), "nope.iso"), &out); err == nil {
			t.Fatal("want error for missing ISO path")
		}
	})
}

func TestDownloadPins(t *testing.T) {
	for _, d := range artifacts.Downloads() {
		if d.URL == "" || d.SHA256 == "" || d.File == "" || d.Version == "" {
			t.Errorf("%s: incomplete pin: %+v", d.Name, d)
		}
		if len(d.SHA256) != 64 {
			t.Errorf("%s: SHA256 pin is not a sha256 hex digest", d.Name)
		}
		if !strings.HasPrefix(d.URL, "https://") {
			t.Errorf("%s: URL must be https", d.Name)
		}
	}
}
