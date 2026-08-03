package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

// Every failure must carry the prefix: `vm launch` matches on it to print the
// reason verbatim instead of burying it in "starting win11: ...".
func requireKVMFRPrefix(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.HasPrefix(err.Error(), KVMFRErrPrefix) {
		t.Errorf("error %q lacks the %q prefix vm launch matches on", err, KVMFRErrPrefix)
	}
}

func TestEnsureKVMFRRefusesUnusableSizes(t *testing.T) {
	for _, size := range []uint64{0, 1 << 40} {
		err := EnsureKVMFR(t.TempDir(), "nobody", size)
		requireKVMFRPrefix(t, err)
	}
}

func TestWaitForDevice(t *testing.T) {
	old := KVMFRTimeout
	KVMFRTimeout = 100 * time.Millisecond
	t.Cleanup(func() { KVMFRTimeout = old })

	t.Run("a regular file is refused, not mapped", func(t *testing.T) {
		dev := filepath.Join(t.TempDir(), "kvmfr0")
		if err := os.WriteFile(dev, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		err := waitForDevice(dev)
		requireKVMFRPrefix(t, err)
		if !strings.Contains(err.Error(), "not a character device") {
			t.Errorf("error does not name the cause: %v", err)
		}
	})

	t.Run("absent times out", func(t *testing.T) {
		requireKVMFRPrefix(t, waitForDevice(filepath.Join(t.TempDir(), "kvmfr0")))
	})
}

func TestLoadedSizeMiB(t *testing.T) {
	cases := []struct {
		name    string
		content string
		write   bool
		want    uint64
	}{
		{name: "no note forces a reload", want: 0},
		{name: "recorded size", content: "128\n", write: true, want: 128},
		{name: "garbage forces a reload", content: "not a number", write: true, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.write {
				path := filepath.Join(root, kvmfrSizeFile)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := loadedSizeMiB(root); got != tc.want {
				t.Errorf("loadedSizeMiB = %d, want %d", got, tc.want)
			}
		})
	}
}

// A client still holding the device makes the reload impossible — which must be
// an error naming the fix, not a silently undersized buffer.
func TestLoadKVMFRBusyModuleFails(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/module/kvmfr"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := DeleteModule
	DeleteModule = func(string) error { return os.ErrPermission }
	t.Cleanup(func() { DeleteModule = old })

	err := loadKVMFR(root, hookLog(root, "kvmfr"), 128)
	requireKVMFRPrefix(t, err)
	if !strings.Contains(err.Error(), "Looking Glass client") {
		t.Errorf("error does not tell the user what to close: %v", err)
	}
}

// A resident module already big enough must not be reloaded: rmmod would fail
// anyway once a client is attached.
func TestLoadKVMFRKeepsABigEnoughModule(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/module/kvmfr"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, kvmfrSizeFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("256"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := DeleteModule
	DeleteModule = func(string) error {
		t.Error("unloaded a module that was already large enough")
		return nil
	}
	t.Cleanup(func() { DeleteModule = old })
	if err := loadKVMFR(root, hookLog(root, "kvmfr"), 128); err != nil {
		t.Fatalf("loadKVMFR: %v", err)
	}
}

func TestKVMFRDeviceIsShared(t *testing.T) {
	// The hook, the rendered domain and qemu.conf must all name one path.
	if steps.KVMFRDevice != "/dev/kvmfr0" {
		t.Errorf("KVMFRDevice = %q", steps.KVMFRDevice)
	}
}

// Runs the module setup against a real kernel: modprobe, the udev race, the
// chown, and the SELinux label that decides whether qemu can open the node at
// all. A --root fixture answers none of those — /dev/kvmfr0 there is a file the
// test wrote itself. The VM tier installs and depmods the module first, so this
// also proves hw.KVMFRAvailable reads a modules.dep depmod actually generated.
func TestEnsureKVMFRAgainstTheRealKernel(t *testing.T) {
	if os.Getenv("ORTHOGONALS_TIER_KVMFR") != "1" {
		t.Skip("loads a kernel module — covered by the VM tier (make test-vm)")
	}
	if os.Geteuid() != 0 {
		t.Skip("loading a module needs root")
	}
	owner := os.Getenv("ORTHOGONALS_TEST_USER")
	if owner == "" {
		t.Fatal("ORTHOGONALS_TEST_USER is unset: the node has to be handed to a real user")
	}
	if !hw.KVMFRAvailable("/") {
		t.Fatal("hw.KVMFRAvailable says no module for this kernel, but the tier just installed one")
	}
	// No teardown here: the tier's trap unloads the module. The steps after this
	// one — the DMABUF round-trip and the probe domain — need the device still
	// present, and a t.Cleanup would pull it out from under them.
	const sizeMiB = 32
	if err := EnsureKVMFR("/", owner, sizeMiB); err != nil {
		t.Fatalf("EnsureKVMFR: %v", err)
	}

	fi, err := os.Stat(steps.KVMFRDevice)
	if err != nil {
		t.Fatalf("stat %s: %v", steps.KVMFRDevice, err)
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		t.Errorf("%s is %s, want a character device", steps.KVMFRDevice, fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o660 {
		t.Errorf("mode = %o, want 660", perm)
	}
	if got := currentLabel(steps.KVMFRDevice); got != KVMFRLabel {
		t.Errorf("label = %q, want %q", got, KVMFRLabel)
	}

	// A second call must be a no-op rather than a reload: the client holds the
	// device open across VM restarts.
	if err := EnsureKVMFR("/", owner, sizeMiB); err != nil {
		t.Errorf("second EnsureKVMFR: %v", err)
	}
}
