package hostcfg

import (
	"flag"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

var update = flag.Bool("update", false, "rewrite golden files")

// referenceProfile builds the Profile from the PoC reference machine fixture.
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

// laptopReferenceProfile is the reference machine reported as a laptop chassis.
func laptopReferenceProfile(t *testing.T) Profile {
	t.Helper()
	root := hwtest.ReferenceRoot(t)
	hwtest.WriteFile(t, root, "sys/class/dmi/id/chassis_type", "10\n")
	res, err := hw.Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProfile(res, "stronautt", "dynamic", false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Laptop {
		t.Fatal("NewProfile did not mark the notebook chassis as a laptop")
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
	byPath := map[string]Artifact{}
	for _, a := range arts {
		byPath[a.Path] = a
	}
	if byPath["/etc/dracut.conf.d/vfio.conf"].Mode != 0o644 {
		t.Errorf("vfio.conf mode = %o, want 0644", byPath["/etc/dracut.conf.d/vfio.conf"].Mode)
	}
	if tmpfiles := string(byPath["/etc/tmpfiles.d/looking-glass.conf"].Content); !strings.Contains(tmpfiles, "0660 stronautt qemu") {
		t.Errorf("tmpfiles entry not rendered for user: %s", tmpfiles)
	}

}

func TestLaptopArtifactsGolden(t *testing.T) {
	arts, err := renderArtifacts(laptopReferenceProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Artifact{}
	for _, a := range arts {
		byPath[a.Path] = a
	}
	for _, path := range []string{
		"/etc/modprobe.d/nvidia-rtd3.conf",
		"/etc/udev/rules.d/80-orthogonals-nvidia-pm.rules",
	} {
		a, ok := byPath[path]
		if !ok {
			t.Fatalf("laptop profile missing artifact %s", path)
		}
		golden := filepath.Join("testdata", "golden", filepath.Base(path))
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
			t.Errorf("%s: rendered content differs from %s:\n%s", path, golden, a.Content)
		}
	}
}

func TestLaptopStepsAddPowerManagement(t *testing.T) {
	ids := func(p Profile) map[string]bool {
		list, err := Steps(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, s := range list {
			out[s.ID] = true
		}
		return out
	}

	laptop := ids(laptopReferenceProfile(t))
	for _, id := range []string{"nvidia-rtd3", "udev-nvidia-pm", "disable-nvidia-powerd"} {
		if !laptop[id] {
			t.Errorf("laptop Steps missing %q", id)
		}
	}

	desktop := ids(referenceProfile(t, "dynamic"))
	for _, id := range []string{"nvidia-rtd3", "udev-nvidia-pm", "disable-nvidia-powerd"} {
		if desktop[id] {
			t.Errorf("desktop Steps must not contain %q", id)
		}
	}
}

// TestVMStepsGolden pins the default win11 VM's desktop entry and ~/Desktop link.
func TestVMStepsGolden(t *testing.T) {
	list, err := VMSteps("win11", "Windows 11", "testuser", "/usr/bin/orthogonals")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d steps, want 2 (desktop entry + link)", len(list))
	}
	entry := list[0]
	if entry.ID != "desktop-entry-win11" || entry.Path != "/usr/share/applications/win11.orthogonals.desktop" || entry.Mode != 0o755 {
		t.Errorf("desktop entry = %s %s %o", entry.ID, entry.Path, entry.Mode)
	}
	link := list[1]
	if link.ID != "desktop-link-win11" {
		t.Errorf("link step ID = %s", link.ID)
	}
	const entryPath = "/usr/share/applications/win11.orthogonals.desktop"
	const linkPath = "/home/testuser/Desktop/win11.orthogonals.desktop"
	if link.Kind != steps.KindOp || link.Op != steps.OpDesktopLink {
		t.Errorf("link step = %s/%s, want an op step (%s)", link.Kind, link.Op, steps.OpDesktopLink)
	}
	// The args are the whole contract: undo replays them from a fresh process.
	wantArgs := map[string]string{"user": "testuser", "entry": entryPath, "link": linkPath}
	if !maps.Equal(link.Args, wantArgs) {
		t.Errorf("link args = %v, want %v", link.Args, wantArgs)
	}
	if link.UndoOp != steps.OpRemoveFile || link.UndoArgs["path"] != linkPath {
		t.Errorf("link undo = %s %v", link.UndoOp, link.UndoArgs)
	}
	if link.CreatesPath != linkPath {
		t.Errorf("link CreatesPath = %q, want %q", link.CreatesPath, linkPath)
	}
	golden := filepath.Join("testdata", "golden", filepath.Base(entry.Path))
	if *update {
		if err := os.WriteFile(golden, entry.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		wantB, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", golden, err)
		}
		if string(entry.Content) != string(wantB) {
			t.Errorf("%s: rendered content differs from %s:\n%s", entry.Path, golden, entry.Content)
		}
	}
	if !strings.Contains(string(entry.Content), "Exec=/usr/bin/orthogonals vm launch --vm-name win11") {
		t.Errorf("desktop entry Exec wrong:\n%s", entry.Content)
	}
}

func TestVMStepsSecondVM(t *testing.T) {
	list, err := VMSteps("gaming", "Gaming Rig", "testuser", "/usr/bin/orthogonals")
	if err != nil {
		t.Fatal(err)
	}
	desktop := list[0]
	if desktop.Path != "/usr/share/applications/gaming.orthogonals.desktop" || desktop.ID != "desktop-entry-gaming" {
		t.Errorf("desktop = %s at %s", desktop.ID, desktop.Path)
	}
	for _, want := range []string{"Name=Gaming Rig", "Exec=/usr/bin/orthogonals vm launch --vm-name gaming"} {
		if !strings.Contains(string(desktop.Content), want) {
			t.Errorf("desktop entry missing %q:\n%s", want, desktop.Content)
		}
	}
	if _, err := VMSteps("bad name", "x", "testuser", "/usr/bin/orthogonals"); err == nil {
		t.Error("invalid VM name must be rejected")
	}
	if _, err := VMSteps("gaming", "x", "", "/usr/bin/orthogonals"); err == nil {
		t.Error("empty user must be rejected — the ~/Desktop link path would be garbage")
	}
	if _, err := VMSteps("gaming", "x", "testuser", "/opt/my app/orthogonals"); err == nil {
		t.Error("exe path with a space must be rejected, not shell-quoted into Exec")
	}
}

func TestDisplayNameFromDesktopEntry(t *testing.T) {
	root := t.TempDir()
	list, err := VMSteps("gaming", "Gaming Rig", "testuser", "/usr/bin/orthogonals")
	if err != nil {
		t.Fatal(err)
	}
	entry := list[0]
	full := filepath.Join(root, entry.Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, entry.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DisplayName(root, "gaming"); got != "Gaming Rig" {
		t.Errorf("DisplayName = %q, want %q", got, "Gaming Rig")
	}
	if got := DisplayName(root, "missing"); got != "" {
		t.Errorf("DisplayName for an undefined VM = %q, want empty", got)
	}
}

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
	sock := byPath["/etc/systemd/system/virtqemud.socket.d/orthogonals.conf"]
	for _, want := range []string{"SocketMode=0660", "SocketGroup=libvirt"} {
		if !strings.Contains(sock, want) {
			t.Errorf("socket drop-in missing %q — an unauthenticated 0666 socket is world-writable:\n%s", want, sock)
		}
	}
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
	if reload.Kind != steps.KindOp || reload.Op != steps.OpSocketReload {
		t.Errorf("socket config is never applied: %+v", reload)
	}
	if reload.UndoOp != steps.OpSocketReload {
		t.Errorf("undo does not restore the default socket auth: %v", reload.UndoOp)
	}
}

func TestKernelArgs(t *testing.T) {
	tests := []struct {
		name    string
		profile Profile
		want    string
	}{
		{"dmar table", Profile{IOMMUTable: hw.IOMMUTableDMAR, Binding: BindingDynamic}, "intel_iommu=on iommu=pt"},
		{"ivrs table drops intel_iommu", Profile{IOMMUTable: hw.IOMMUTableIVRS, Binding: BindingDynamic}, "iommu=pt"},
		{
			"table outranks cpu vendor",
			Profile{IOMMUTable: hw.IOMMUTableDMAR, CPUVendor: hw.CPUVendorAMD, Binding: BindingDynamic},
			"intel_iommu=on iommu=pt",
		},
		{"no table falls back to intel vendor", Profile{CPUVendor: hw.CPUVendorIntel, Binding: BindingDynamic}, "intel_iommu=on iommu=pt"},
		{"no table falls back to amd vendor", Profile{CPUVendor: hw.CPUVendorAMD, Binding: BindingDynamic}, "iommu=pt"},
		{"nothing known keeps intel default", Profile{Binding: BindingDynamic}, "intel_iommu=on iommu=pt"},
		{
			"intel static appends vfio ids",
			Profile{CPUVendor: hw.CPUVendorIntel, Binding: BindingStatic, VFIOIDs: []string{"10de:2206", "10de:1aef"}},
			"intel_iommu=on iommu=pt vfio-pci.ids=10de:2206,10de:1aef",
		},
		{
			"amd static appends vfio ids",
			Profile{CPUVendor: hw.CPUVendorAMD, Binding: BindingStatic, VFIOIDs: []string{"10de:2206", "10de:1aef"}},
			"iommu=pt vfio-pci.ids=10de:2206,10de:1aef",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KernelArgs(tt.profile); got != tt.want {
				t.Errorf("KernelArgs = %q, want %q", got, tt.want)
			}
		})
	}
	// The reference (Intel) fixture flows through NewProfile to the same result.
	if got := KernelArgs(referenceProfile(t, "static")); got != "intel_iommu=on iommu=pt vfio-pci.ids=10de:2206,10de:1aef" {
		t.Errorf("reference static kargs = %q", got)
	}
}

func TestAddedKargsKeepsPreexistingTokens(t *testing.T) {
	args := "intel_iommu=on iommu=pt"
	cases := []struct {
		name        string
		preexisting []string
		want        string
	}{
		{"none preexisting", nil, "intel_iommu=on iommu=pt"},
		{"one preexisting", []string{"ro", "intel_iommu=on"}, "iommu=pt"},
		{"all preexisting", []string{"intel_iommu=on", "iommu=pt"}, ""},
	}
	for _, tc := range cases {
		if got := addedKargs(args, tc.preexisting); got != tc.want {
			t.Errorf("%s: added = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKernelArgsStepOmitsUndoWhenAllPreexisting(t *testing.T) {
	args := "intel_iommu=on iommu=pt"
	added := kernelArgsStep(args, nil)
	if added.Op != steps.OpKernelArgsAdd || added.Args["args"] != args {
		t.Errorf("add step = %+v", added)
	}
	if added.UndoOp != steps.OpKernelArgsRem || added.UndoArgs["args"] != args {
		t.Errorf("undo = %s %v, want remove-all", added.UndoOp, added.UndoArgs)
	}
	if s := kernelArgsStep(args, strings.Fields(args)); s.UndoOp != "" {
		t.Errorf("undo should be empty when all preexisting, got %s", s.UndoOp)
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

	kargs := stepByID(t, list, "kernel-args")
	if kargs.Op != steps.OpKernelArgsAdd || kargs.Args["args"] != "intel_iommu=on iommu=pt" {
		t.Errorf("kargs op = %s %v, want kernel-args-add", kargs.Op, kargs.Args)
	}
	if kargs.UndoOp != steps.OpKernelArgsRem || kargs.UndoArgs["args"] != "intel_iommu=on iommu=pt" {
		t.Errorf("kargs undo = %s %v", kargs.UndoOp, kargs.UndoArgs)
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

	auto := stepByID(t, list, "net-default-autostart")
	if auto.Kind != steps.KindOp || auto.Op != steps.OpNetAutostart || auto.Args["network"] != "default" {
		t.Errorf("net-autostart step = %+v", auto)
	}
	net := stepByID(t, list, "net-default-start")
	if net.Kind != steps.KindOp || net.Op != steps.OpNetActive || net.Args["network"] != "default" {
		t.Errorf("net-start step = %+v", net)
	}

	grp := stepByID(t, list, "user-libvirt-group")
	if got := strings.Join(grp.Cmd, " "); got != "usermod -aG libvirt stronautt" {
		t.Errorf("libvirt-group cmd = %q", got)
	}
	if len(grp.UndoCmd) != 0 {
		t.Error("libvirt-group undo is a documented no-op, must have no undo command")
	}

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
	if !strings.Contains(kargs.Args["args"], "vfio-pci.ids=10de:2206,10de:1aef") {
		t.Errorf("static binding must add vfio-pci.ids karg, got %v", kargs.Args)
	}
	if !strings.Contains(kargs.UndoArgs["args"], "vfio-pci.ids=") {
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
	other := "[Desktop Entry]\nName=Editor\nExec=/usr/bin/editor %F\n"
	for name, content := range map[string]string{"google-chrome.desktop": chrome, "editor.desktop": other} {
		if err := os.WriteFile(filepath.Join(apps, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out, err := IGPUOverrides(root, hw.VendorIntel)
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
	out, err := IGPUOverrides(t.TempDir(), hw.VendorIntel)
	if err != nil || len(out) != 0 {
		t.Fatalf("got %v, %v; want empty, nil", out, err)
	}
}

func TestIGPUOverridesAMDSelectsRadeon(t *testing.T) {
	root := t.TempDir()
	apps := filepath.Join(root, "usr/share/applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apps, "google-chrome.desktop"),
		[]byte("[Desktop Entry]\nExec=/usr/bin/google-chrome-stable %U\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := IGPUOverrides(root, hw.VendorAMD)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d overrides, want 1", len(out))
	}
	if got := string(out[0].Content); !strings.Contains(got, "VK_LOADER_DRIVERS_SELECT=*radeon* ") {
		t.Errorf("AMD iGPU override should select *radeon*:\n%s", got)
	}
}
