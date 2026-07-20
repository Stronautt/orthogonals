package hostcfg

import (
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

var update = flag.Bool("update", false, "rewrite golden files")

// referenceProfile builds the Profile from the PoC reference machine fixture,
// exercising the detect → profile value flow.
func referenceProfile(t *testing.T, binding string) Profile {
	t.Helper()
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProfile(res, "stronautt", binding, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestArtifactsGolden(t *testing.T) {
	arts, err := renderArtifacts(referenceProfile(t, "dynamic"))
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{
		"/etc/dracut.conf.d/vfio.conf",
		"/etc/libvirt/virtqemud.conf",
		"/etc/systemd/system/virtqemud.socket.d/orthogonals.conf",
		"/etc/udev/rules.d/61-mutter-ignore-nvidia.rules",
		"/etc/environment.d/50-orthogonals-igpu.conf",
		"/etc/tmpfiles.d/looking-glass.conf",
		"/etc/sysconfig/libvirt-guests",
		"/var/lib/orthogonals/lg-build.sh",
	}
	if len(arts) != len(wantPaths) {
		t.Fatalf("got %d artifacts, want %d", len(arts), len(wantPaths))
	}
	for i, a := range arts {
		if a.Path != wantPaths[i] {
			t.Errorf("artifact %d path = %s, want %s", i, a.Path, wantPaths[i])
		}
		golden := filepath.Join("testdata", "golden", filepath.Base(a.Path))
		if *update {
			if err := os.WriteFile(golden, a.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", golden, err)
		}
		if string(a.Content) != string(want) {
			t.Errorf("%s: rendered content differs from %s:\n%s", a.Path, golden, a.Content)
		}
	}
	// by path, not by index: an artifact inserted anywhere in the list must not
	// silently re-point these assertions at the wrong file
	byPath := map[string]Artifact{}
	for _, a := range arts {
		byPath[a.Path] = a
	}
	if p := "/var/lib/orthogonals/lg-build.sh"; byPath[p].Mode != 0o755 {
		t.Errorf("%s mode = %o, want 0755", p, byPath[p].Mode)
	}
	if byPath["/etc/dracut.conf.d/vfio.conf"].Mode != 0o644 {
		t.Errorf("vfio.conf mode = %o, want 0644", byPath["/etc/dracut.conf.d/vfio.conf"].Mode)
	}
	if tmpfiles := string(byPath["/etc/tmpfiles.d/looking-glass.conf"].Content); !strings.Contains(tmpfiles, "0660 stronautt qemu") {
		t.Errorf("tmpfiles entry not rendered for user: %s", tmpfiles)
	}

	build := string(byPath["/var/lib/orthogonals/lg-build.sh"].Content)
	for _, want := range []string{
		"-DENABLE_BACKTRACE=OFF", // PoC incident 4: libbfd.a wants ZSTD symbols on Fedora
		artifacts.LookingGlassSource.URL,
		artifacts.LookingGlassSource.SHA256,
		"/usr/local/bin/looking-glass-client",
		"not found", // PoC incident 7: ldd check for orphaned runtime libs
	} {
		if !strings.Contains(build, want) {
			t.Errorf("lg-build.sh missing %q:\n%s", want, build)
		}
	}

}

// TestVMStepsGolden pins the default win11 VM's launcher, desktop entry, and
// ~/Desktop link — IDs, paths, modes, commands, and rendered bytes.
func TestVMStepsGolden(t *testing.T) {
	list, err := VMSteps("win11", "Windows 11", "testuser")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id, path string
		mode     fs.FileMode
	}{
		{"_ort-run-win11-lg", "/usr/local/bin/_ort-run-win11-lg", 0o755},
		{"desktop-entry-win11", "/usr/share/applications/win11.orthogonals.desktop", 0o755},
	}
	if len(list) != len(want)+1 {
		t.Fatalf("got %d steps, want %d", len(list), len(want)+1)
	}
	for i, w := range want {
		s := list[i]
		if s.ID != w.id || s.Path != w.path || s.Mode != w.mode {
			t.Errorf("step %d = %s %s %o, want %s %s %o", i, s.ID, s.Path, s.Mode, w.id, w.path, w.mode)
		}
	}
	link := list[2]
	if link.ID != "desktop-link-win11" {
		t.Errorf("link step ID = %s", link.ID)
	}
	const entry = "/usr/share/applications/win11.orthogonals.desktop"
	const linkPath = "/home/testuser/Desktop/win11.orthogonals.desktop"
	wantCmd := "runuser -u testuser -- sh -c " +
		"mkdir -p /home/testuser/Desktop && ln -sfn " + entry + " " + linkPath +
		" && DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus gio set " + linkPath + " metadata::trusted true"
	if got := strings.Join(link.Cmd, " "); got != wantCmd {
		t.Errorf("link cmd = %q\nwant       %q", got, wantCmd)
	}
	if got := strings.Join(link.UndoCmd, " "); got != "rm -f "+linkPath {
		t.Errorf("link undo = %q", got)
	}
	// a hand-deleted link must be recreated on re-define, not skipped as
	// already applied
	if link.CreatesPath != linkPath {
		t.Errorf("link CreatesPath = %q, want %q", link.CreatesPath, linkPath)
	}
	for _, s := range list {
		if s.Kind != steps.KindWriteFile {
			continue
		}
		golden := filepath.Join("testdata", "golden", filepath.Base(s.Path))
		if *update {
			if err := os.WriteFile(golden, s.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		wantB, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", golden, err)
		}
		if string(s.Content) != string(wantB) {
			t.Errorf("%s: rendered content differs from %s:\n%s", s.Path, golden, s.Content)
		}
	}
	launcher := string(list[0].Content)
	for _, want := range []string{
		"win11", "qemu:///system",
		// the SPICE target must come from domdisplay for THIS domain: the
		// client default (127.0.0.1:5900) is whatever VM autoport gave first,
		// so a second running VM would receive the input/clipboard channel
		`looking-glass-client -F -c "${hostport%:*}" -p "${hostport##*:}"`,
	} {
		if !strings.Contains(launcher, want) {
			t.Errorf("launcher missing %q:\n%s", want, launcher)
		}
	}
}

func TestVMStepsSecondVM(t *testing.T) {
	list, err := VMSteps("gaming", "Gaming Rig", "testuser")
	if err != nil {
		t.Fatal(err)
	}
	launcher, desktop := list[0], list[1]
	if launcher.Path != "/usr/local/bin/_ort-run-gaming-lg" || launcher.ID != "_ort-run-gaming-lg" {
		t.Errorf("launcher = %s at %s", launcher.ID, launcher.Path)
	}
	if !strings.Contains(string(launcher.Content), `VM="gaming"`) {
		t.Errorf("launcher must target its own domain:\n%s", launcher.Content)
	}
	if desktop.Path != "/usr/share/applications/gaming.orthogonals.desktop" || desktop.ID != "desktop-entry-gaming" {
		t.Errorf("desktop = %s at %s", desktop.ID, desktop.Path)
	}
	for _, want := range []string{"Name=Gaming Rig", "Exec=/usr/local/bin/_ort-run-gaming-lg"} {
		if !strings.Contains(string(desktop.Content), want) {
			t.Errorf("desktop entry missing %q:\n%s", want, desktop.Content)
		}
	}
	if _, err := VMSteps("bad name", "x", "testuser"); err == nil {
		t.Error("invalid VM name must be rejected")
	}
	if _, err := VMSteps("gaming", "x", ""); err == nil {
		t.Error("empty user must be rejected — the ~/Desktop link path would be garbage")
	}
}

// DisplayName reads the name back from the journaled desktop entry, so a
// re-define without --display-name keeps the name the VM was defined with.
func TestDisplayNameFromDesktopEntry(t *testing.T) {
	root := t.TempDir()
	list, err := VMSteps("gaming", "Gaming Rig", "testuser")
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(root, list[1].Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, list[1].Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DisplayName(root, "gaming"); got != "Gaming Rig" {
		t.Errorf("DisplayName = %q, want %q", got, "Gaming Rig")
	}
	if got := DisplayName(root, "missing"); got != "" {
		t.Errorf("DisplayName for an undefined VM = %q, want empty", got)
	}
}

// The one-click launcher runs `virsh --connect qemu:///system` unprivileged.
// libvirt's default is to ask polkit, and a polkitd that has leaked its file
// descriptors (polkit 127 does, steadily) stops evaluating rules and denies
// everything — the launch then fails with "access denied by policy" and the
// user has no way to know why. The libvirt group must be the credential on the
// socket itself, so no daemon sits between the user and their VM.
func TestLibvirtAuthenticatesOnTheSocketNotPolkit(t *testing.T) {
	arts, err := renderArtifacts(referenceProfile(t, "dynamic"))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]string{}
	for _, a := range arts {
		byPath[a.Path] = string(a.Content)
	}
	if conf := byPath["/etc/libvirt/virtqemud.conf"]; !strings.Contains(conf, `auth_unix_rw = "none"`) {
		t.Errorf("virtqemud.conf still leaves polkit in the launcher's path:\n%s", conf)
	}
	// with auth off, the socket mode IS the access control — it ships 0666
	sock := byPath["/etc/systemd/system/virtqemud.socket.d/orthogonals.conf"]
	for _, want := range []string{"SocketMode=0660", "SocketGroup=libvirt"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket drop-in missing %q — an unauthenticated 0666 socket is world-writable:\n%s", want, sock)
		}
	}
	// both only take effect once systemd re-creates the socket and the daemon
	// re-reads its config
	list, err := Steps(referenceProfile(t, "dynamic"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var reload steps.Step
	for _, s := range list {
		if s.ID == "libvirt-socket-reload" {
			reload = s
		}
	}
	cmd := strings.Join(reload.Cmd, " ")
	if !strings.Contains(cmd, "daemon-reload") || !strings.Contains(cmd, "restart virtqemud.socket") {
		t.Errorf("socket config is never applied: %q", cmd)
	}
	// undo restores the two files, then this same command puts the defaults
	// back — otherwise the running daemon keeps unauthenticated access
	if !slices.Equal(reload.Cmd, reload.UndoCmd) {
		t.Errorf("undo does not restore the default socket auth: %v", reload.UndoCmd)
	}
}

func TestShellArtifactsBashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	arts, err := renderArtifacts(referenceProfile(t, "dynamic"))
	if err != nil {
		t.Fatal(err)
	}
	vmList, err := VMSteps("win11", "Windows 11", "testuser")
	if err != nil {
		t.Fatal(err)
	}
	shellPaths := map[string]bool{
		"/var/lib/orthogonals/lg-build.sh": true,
		"/usr/local/bin/_ort-run-win11-lg":          true,
	}
	checked := 0
	for _, a := range arts {
		if shellPaths[a.Path] {
			checked++
			bashSyntaxCheck(t, a.Path, a.Content)
		}
	}
	for _, s := range vmList {
		if shellPaths[s.Path] {
			checked++
			bashSyntaxCheck(t, s.Path, s.Content)
		}
	}
	if checked != len(shellPaths) {
		t.Errorf("checked %d shell artifacts, want %d", checked, len(shellPaths))
	}
}

func bashSyntaxCheck(t *testing.T, name string, content []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), filepath.Base(name))
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("bash -n %s: %v\n%s", name, err, out)
	}
}

func TestKernelArgs(t *testing.T) {
	if got := KernelArgs(referenceProfile(t, "dynamic")); got != "intel_iommu=on iommu=pt" {
		t.Errorf("dynamic kargs = %q", got)
	}
	want := "intel_iommu=on iommu=pt vfio-pci.ids=10de:2206,10de:1aef"
	if got := KernelArgs(referenceProfile(t, "static")); got != want {
		t.Errorf("static kargs = %q, want %q", got, want)
	}
}

func TestUndoKargsCmdKeepsPreexistingTokens(t *testing.T) {
	args := "intel_iommu=on iommu=pt"
	cases := []struct {
		name        string
		preexisting []string
		want        string
	}{
		{"none preexisting", nil, "grubby --update-kernel=ALL --remove-args=intel_iommu=on iommu=pt"},
		{"one preexisting", []string{"ro", "intel_iommu=on"}, "grubby --update-kernel=ALL --remove-args=iommu=pt"},
		{"all preexisting", []string{"intel_iommu=on", "iommu=pt"}, ""},
	}
	for _, tc := range cases {
		if got := strings.Join(undoKargsCmd(args, tc.preexisting), " "); got != tc.want {
			t.Errorf("%s: undo = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCurrentKargTokensParsesGrubbyInfo(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
[ "$1" = "--info=ALL" ] || exit 1
echo 'index=0'
echo 'kernel="/boot/vmlinuz-6.14.9-300.fc42.x86_64"'
echo 'args="ro rootflags=subvol=root rhgb quiet intel_iommu=on"'
echo 'root="UUID=abc"'
echo 'index=1'
echo 'kernel="/boot/vmlinuz-6.14.5-300.fc42.x86_64"'
echo 'args="ro rootflags=subvol=root rhgb quiet"'
echo 'root="UUID=abc"'
`
	if err := os.WriteFile(filepath.Join(dir, "grubby"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got, err := CurrentKargTokens()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ro", "rootflags=subvol=root", "rhgb", "quiet", "intel_iommu=on"}
	if !slices.Equal(got, want) {
		t.Errorf("tokens = %v, want %v", got, want)
	}
}

func TestCurrentKargTokensGrubbyFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grubby"), []byte("#!/bin/sh\necho boom >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if _, err := CurrentKargTokens(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("want a loud error carrying grubby's output, got %v", err)
	}
}

func TestNewProfileErrors(t *testing.T) {
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProfile(res, "", "dynamic", false); err == nil {
		t.Error("empty user: want error")
	}
	if _, err := NewProfile(res, "u", "sideways", false); err == nil {
		t.Error("bad binding: want error")
	}
	if _, err := NewProfile(&hw.Result{}, "u", "dynamic", false); err == nil {
		t.Error("no dGPU: want error")
	}
}

func stepByID(t *testing.T, list []steps.Step, id string) steps.Step {
	t.Helper()
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %q", id)
	return steps.Step{}
}

func TestSteps(t *testing.T) {
	list, err := Steps(referenceProfile(t, "dynamic"), nil)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	for _, s := range list {
		if seen[s.ID] {
			t.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
	}

	pkg := list[0]
	if pkg.ID != "packages" || pkg.Cmd[0] != "dnf" || pkg.Cmd[1] != "install" || pkg.Cmd[2] != "-y" {
		t.Errorf("first step must be dnf install -y, got %v", pkg.Cmd[:3])
	}
	if !strings.Contains(strings.Join(pkg.Cmd, " "), "switcheroo-control") {
		t.Error("package list must include switcheroo-control")
	}
	if len(pkg.UndoCmd) != 0 {
		t.Error("packages undo is a documented no-op, must have no undo command")
	}

	kargs := stepByID(t, list, "kernel-args")
	wantCmd := "grubby --update-kernel=ALL --args=intel_iommu=on iommu=pt"
	if got := strings.Join(kargs.Cmd, " "); got != wantCmd {
		t.Errorf("kargs cmd = %q, want %q", got, wantCmd)
	}
	wantUndo := "grubby --update-kernel=ALL --remove-args=intel_iommu=on iommu=pt"
	if got := strings.Join(kargs.UndoCmd, " "); got != wantUndo {
		t.Errorf("kargs undo = %q, want %q", got, wantUndo)
	}
	if !kargs.Reboot {
		t.Error("kernel-args must be flagged Reboot")
	}

	dracut := stepByID(t, list, "dracut-regenerate")
	if got := strings.Join(dracut.Cmd, " "); got != "dracut -f --regenerate-all" {
		t.Errorf("dracut cmd = %q", got)
	}
	if strings.Join(dracut.UndoCmd, " ") != "dracut -f --regenerate-all" {
		t.Error("dracut undo must regenerate again after restore")
	}
	if !dracut.Reboot {
		t.Error("dracut must be flagged Reboot")
	}

	se := stepByID(t, list, "selinux-lg-fcontext")
	if got := strings.Join(se.Cmd, " "); got != "semanage fcontext -a -t svirt_tmpfs_t /dev/shm/looking-glass" {
		t.Errorf("semanage cmd = %q", got)
	}
	if got := strings.Join(se.UndoCmd, " "); got != "semanage fcontext -d /dev/shm/looking-glass" {
		t.Errorf("semanage undo = %q", got)
	}

	for id, enable := range map[string]bool{
		"disable-nvidia-persistenced": false,
		"enable-libvirt-guests":       true,
		"enable-switcheroo-control":   true,
	} {
		s := stepByID(t, list, id)
		if s.Kind != steps.KindEnableUnit || s.Enable != enable {
			t.Errorf("%s: kind=%s enable=%v", id, s.Kind, s.Enable)
		}
	}

	// default network inactive in the reference profile → both net steps
	stepByID(t, list, "net-default-autostart")
	net := stepByID(t, list, "net-default-start")
	// libvirt may autostart the network between plan and execution;
	// already-active must count as success
	if got := strings.Join(net.Cmd, " "); got != "sh -c virsh net-start default || virsh net-list --name | grep -qx default" {
		t.Errorf("net-start cmd = %q", got)
	}

	grp := stepByID(t, list, "user-libvirt-group")
	if got := strings.Join(grp.Cmd, " "); got != "usermod -aG libvirt stronautt" {
		t.Errorf("libvirt-group cmd = %q", got)
	}
	if len(grp.UndoCmd) != 0 {
		t.Error("libvirt-group undo is a documented no-op, must have no undo command")
	}

	build := stepByID(t, list, "lg-client-build")
	if got := strings.Join(build.Cmd, " "); got != "bash /var/lib/orthogonals/lg-build.sh" {
		t.Errorf("lg-client-build cmd = %q", got)
	}
	if got := strings.Join(build.UndoCmd, " "); got != "rm -f /usr/local/bin/looking-glass-client" {
		t.Errorf("lg-client-build undo = %q", got)
	}

	// dracut regeneration must come after the vfio dracut conf is written
	// and after the kernel-args step.
	order := map[string]int{}
	for i, s := range list {
		order[s.ID] = i
	}
	if order["dracut-regenerate"] < order["dracut-vfio-conf"] {
		t.Error("dracut must regenerate after vfio.conf is written")
	}
	if order["lg-shm-restorecon"] < order["selinux-lg-fcontext"] {
		t.Error("restorecon must follow the semanage rule")
	}
	// the build needs the deps from the packages step and the script on disk
	if order["lg-client-build"] < order["packages"] || order["lg-client-build"] < order["lg-build-script"] {
		t.Error("lg-client-build must run after packages and the build script write")
	}
}

func TestStepsNetActiveAndStatic(t *testing.T) {
	p := referenceProfile(t, "static")
	p.DefaultNetActive = true
	list, err := Steps(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if strings.HasPrefix(s.ID, "net-default") {
			t.Errorf("active default network must skip %s", s.ID)
		}
	}
	kargs := stepByID(t, list, "kernel-args")
	if !strings.Contains(strings.Join(kargs.Cmd, " "), "vfio-pci.ids=10de:2206,10de:1aef") {
		t.Errorf("static binding must add vfio-pci.ids karg, got %v", kargs.Cmd)
	}
	if !strings.Contains(strings.Join(kargs.UndoCmd, " "), "vfio-pci.ids=") {
		t.Error("static kargs undo must remove vfio-pci.ids too")
	}
}

func TestIGPUOverridesPrefixEveryExec(t *testing.T) {
	root := t.TempDir()
	apps := filepath.Join(root, "usr/share/applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	chrome := `[Desktop Entry]
Name=Google Chrome
Exec=/usr/bin/google-chrome-stable %U
Actions=new-window;

[Desktop Action new-window]
Name=New Window
Exec=/usr/bin/google-chrome-stable
`
	// an entry not on the list must be left alone
	other := "[Desktop Entry]\nName=Editor\nExec=/usr/bin/editor %F\n"
	for name, content := range map[string]string{"google-chrome.desktop": chrome, "editor.desktop": other} {
		if err := os.WriteFile(filepath.Join(apps, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := IGPUOverrides(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d overrides, want 1 (only the installed listed entry): %+v", len(out), out)
	}
	a := out[0]
	if a.ID != "igpu-override-google-chrome.desktop" || a.Path != "/usr/local/share/applications/google-chrome.desktop" {
		t.Errorf("ID/Path = %s / %s", a.ID, a.Path)
	}
	got := string(a.Content)
	for _, want := range []string{
		"Exec=env VK_LOADER_DRIVERS_SELECT=*intel* /usr/bin/google-chrome-stable %U",
		"Exec=env VK_LOADER_DRIVERS_SELECT=*intel* /usr/bin/google-chrome-stable\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("override missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Exec=/usr/bin") {
		t.Errorf("an Exec line escaped the prefix:\n%s", got)
	}
}

func TestIGPUOverridesEmptyWhenNoneInstalled(t *testing.T) {
	out, err := IGPUOverrides(t.TempDir())
	if err != nil || len(out) != 0 {
		t.Fatalf("got %v, %v; want empty, nil", out, err)
	}
}
