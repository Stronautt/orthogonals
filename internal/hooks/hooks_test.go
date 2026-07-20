package hooks

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
)

var update = flag.Bool("update", false, "rewrite golden files")

func referenceProfile(t *testing.T) Profile {
	t.Helper()
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProfile(res, "stronautt")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func referenceSteps(t *testing.T) []steps.Step {
	t.Helper()
	list, err := Steps(referenceProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestStepsGolden(t *testing.T) {
	list := referenceSteps(t)
	wantPaths := []string{
		"/etc/libvirt/hooks/qemu",
		"/etc/libvirt/hooks/orthogonals-gpu-detach.sh",
		"/etc/libvirt/hooks/orthogonals-gpu-reattach.sh",
	}
	if len(list) != len(wantPaths) {
		t.Fatalf("got %d steps, want %d", len(list), len(wantPaths))
	}
	for i, s := range list {
		if s.Path != wantPaths[i] {
			t.Errorf("step %d path = %s, want %s", i, s.Path, wantPaths[i])
		}
		if s.Kind != steps.KindWriteFile {
			t.Errorf("%s: kind = %s, want write_file", s.ID, s.Kind)
		}
		if s.Mode != 0o755 {
			t.Errorf("%s: mode = %o, want 0755 (hooks must be executable)", s.ID, s.Mode)
		}
		golden := filepath.Join("testdata", "golden", filepath.Base(s.Path))
		if *update {
			if err := os.WriteFile(golden, s.Content, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("%s: %v (run go test -update)", golden, err)
		}
		if string(s.Content) != string(want) {
			t.Errorf("%s: rendered content differs from %s:\n%s", s.Path, golden, s.Content)
		}
	}
}

func stepContent(t *testing.T, list []steps.Step, id string) string {
	t.Helper()
	for _, s := range list {
		if s.ID == id {
			return string(s.Content)
		}
	}
	t.Fatalf("no step %q", id)
	return ""
}

// TestDispatcher pins the registry match, the single-VM mutex, both hook
// directions, and the sleep-inhibitor unit — the fail-safe wiring the golden
// file alone would silently accept changes to under -update.
func TestDispatcher(t *testing.T) {
	got := stepContent(t, referenceSteps(t), "hook-qemu-dispatcher")
	for _, want := range []string{
		`VMS_DIR="` + steps.VMsDirPath + `"`,
		`RUN_DIR="` + steps.LibvirtRunDir + `"`,
		`[ -f "$VMS_DIR/${1:-}.xml" ] || exit 0`, // unmanaged domains pass through
		`[ -e "$RUN_DIR/$other.xml" ]`,           // one VM at a time
		"one VM at a time",
		"prepare/begin)",
		"release/end)",
		"orthogonals-gpu-detach.sh",
		"orthogonals-gpu-reattach.sh",
		"systemd-run --unit=\"libvirt-nosleep-$1\"",
		"systemd-inhibit --what=sleep",
		"systemctl stop \"libvirt-nosleep-$1\"",
		"exit 1", // failed detach/reattach must propagate into libvirt's error
		LogPath,  // failure message points at the stage log
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dispatcher missing %q:\n%s", want, got)
		}
	}
}

func TestDetach(t *testing.T) {
	got := stepContent(t, referenceSteps(t), "hook-gpu-detach")
	for _, want := range []string{
		`GPU="0000:01:00.0"`,
		`AUD="0000:01:00.1"`,
		"fuser -v /dev/nvidia*", // research §A: nvidia-smi misses GL clients
		"notify-send",
		`id -u "$NOTIFY_USER"`, // uid resolved at hook runtime
		"/run/user/${uid}/bus",
		"refusing handover", // refuse-and-list, never kill
		"systemctl stop nvidia-persistenced.service",
		"modprobe -r",
		"driver_override",
		"not vfio-pci — aborting VM start", // verify-or-abort
		"systemctl try-restart switcheroo-control.service",
		LogPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("detach missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "kill ") || strings.Contains(got, "kill -") {
		t.Error("detach must never kill holder processes")
	}
	// persistenced holds /dev/nvidia* open; stopping it after the holder gate
	// would refuse every VM start with our own daemon listed as the culprit
	stop := strings.Index(got, "systemctl stop nvidia-persistenced.service")
	gate := strings.Index(got, "fuser -v /dev/nvidia*")
	if stop == -1 || gate == -1 || stop > gate {
		t.Errorf("persistenced stop must precede the holder gate:\n%s", got)
	}
	// the governor flip runs only after the start can no longer fail: libvirt
	// skips release/end when prepare fails, and nothing would restore it
	boost := strings.LastIndex(got, "boost_governor")
	verify := strings.Index(got, "not vfio-pci — aborting VM start")
	if boost == -1 || verify == -1 || boost < verify {
		t.Errorf("governor flip must follow the verify-or-abort stage:\n%s", got)
	}
}

func TestReattach(t *testing.T) {
	got := stepContent(t, referenceSteps(t), "hook-gpu-reattach")
	// the guard must come before any unbind (PoC incident 9: release/end
	// fires even for a failed start)
	guard := strings.Index(got, "exit 0")
	unbind := strings.Index(got, "/driver/unbind")
	if guard == -1 || unbind == -1 || guard > unbind {
		t.Errorf("reattach guard (exit 0) must precede unbind:\n%s", got)
	}
	for _, want := range []string{
		`GPU="0000:01:00.0"`,
		"vfio-pci", // guard condition
		"modprobe nvidia",
		"nvidia-smi",
		"orthogonals recover",
		"systemctl try-restart switcheroo-control.service",
		"/remove",
		"/sys/bus/pci/rescan",
		LogPath,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reattach missing %q:\n%s", want, got)
		}
	}
	// the remove+rescan fallback runs only after a failed health check
	smi := strings.Index(got, "nvidia-smi")
	rescan := strings.Index(got, "/sys/bus/pci/rescan")
	if smi == -1 || rescan == -1 || smi > rescan {
		t.Errorf("health check must precede the PCI rescan fallback:\n%s", got)
	}
	// the governor restore must precede the vfio guard's exit — a refused or
	// failed start would otherwise leave the host stuck on performance
	restore := strings.Index(got, "GOV_SAVE")
	if restore == -1 || restore > guard {
		t.Errorf("governor restore must precede the vfio guard:\n%s", got)
	}
}

// Both scripts name the same governor state file; drift would leave the host
// stuck on the performance governor after VM shutdown.
func TestGovernorStateFileShared(t *testing.T) {
	const save = "GOV_SAVE=/run/orthogonals-governor"
	list := referenceSteps(t)
	for _, id := range []string{"hook-gpu-detach", "hook-gpu-reattach"} {
		if !strings.Contains(stepContent(t, list, id), save) {
			t.Errorf("%s missing %q", id, save)
		}
	}
}

func TestBashSyntax(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, list := range [][]steps.Step{referenceSteps(t), noAudioSteps(t)} {
		for _, s := range list {
			path := filepath.Join(t.TempDir(), filepath.Base(s.Path))
			if err := os.WriteFile(path, s.Content, 0o755); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
				t.Errorf("bash -n %s: %v\n%s", s.ID, err, out)
			}
		}
	}
}

// noAudioSteps renders for a dGPU without an audio function.
func noAudioSteps(t *testing.T) []steps.Step {
	t.Helper()
	res := &hw.Result{GPUs: hw.GPUs{DGPUs: []hw.DGPU{
		{PCIDevice: hw.PCIDevice{Address: "0000:03:00.0", Vendor: "0x10de", Device: "0x2206"}},
	}}}
	p, err := NewProfile(res, "user")
	if err != nil {
		t.Fatal(err)
	}
	list, err := Steps(p)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// The rendered scripts keep the NVIDIA module lists as shell literals; this
// pins them to the exported orders `orthogonals recover` shares, so the two
// paths cannot drift apart.
func TestHookScriptsUseModuleLists(t *testing.T) {
	scripts := map[string]string{}
	var paths []string
	for _, s := range referenceSteps(t) {
		scripts[s.Path] = string(s.Content)
		paths = append(paths, s.Path)
	}
	if !slices.Equal(paths, InstalledPaths()) {
		t.Errorf("InstalledPaths() = %v, want the rendered step paths %v", InstalledPaths(), paths)
	}
	detach := scripts["/etc/libvirt/hooks/orthogonals-gpu-detach.sh"]
	if want := "for m in " + strings.Join(NVIDIAUnloadOrder, " ") + ";"; !strings.Contains(detach, want) {
		t.Errorf("gpu-detach.sh does not unload NVIDIAUnloadOrder %q", want)
	}
	reattach := scripts["/etc/libvirt/hooks/orthogonals-gpu-reattach.sh"]
	last := -1
	for _, m := range NVIDIAReloadOrder {
		i := strings.Index(reattach, "modprobe "+m)
		if i < 0 {
			t.Errorf("gpu-reattach.sh does not reload %s", m)
			continue
		}
		if i < last {
			t.Errorf("gpu-reattach.sh reloads %s out of NVIDIAReloadOrder order", m)
		}
		last = i
	}
}

func TestNoAudioFunction(t *testing.T) {
	list := noAudioSteps(t)
	detach := stepContent(t, list, "hook-gpu-detach")
	if !strings.Contains(detach, `GPU="0000:03:00.0"`) || !strings.Contains(detach, `AUD=""`) {
		t.Errorf("no-audio detach rendered wrong BDFs:\n%s", detach)
	}
}

func TestNewProfileErrors(t *testing.T) {
	res, err := hw.Detect(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProfile(res, ""); err == nil {
		t.Error("empty user: want error")
	}
	if _, err := NewProfile(&hw.Result{}, "u"); err == nil {
		t.Error("no dGPU: want error")
	}
}
