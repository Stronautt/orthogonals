package hw

import (
	"os"
	"path/filepath"
	"testing"
)

// writeUnder writes content at root/rel, creating parents.
func writeUnder(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// FuzzMeminfoKiB asserts arbitrary /proc/meminfo content never panics; preflight
// sizes the guest from this value.
func FuzzMeminfoKiB(f *testing.F) {
	f.Add("MemTotal:       33554432 kB\nMemFree:  20000000 kB\n")
	f.Add("MemTotal:\n")
	f.Add("MemTotal: notanumber kB\n")
	f.Add("MemTotal: -1 kB\n")
	f.Add("MemTotal: 99999999999999999999999 kB\n")
	f.Add("")
	f.Add("MemTotal 33554432 kB\n")

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		writeUnder(t, root, "proc/meminfo", content)
		MeminfoKiB(root, "MemTotal")
	})
}

// FuzzDetectNVIDIA asserts arbitrary driver-version content never panics; the
// manifest stamps this string and undo compares against it.
func FuzzDetectNVIDIA(f *testing.F) {
	f.Add("NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n")
	f.Add("NVRM version:\n")
	f.Add("garbage")
	f.Add("")

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		writeUnder(t, root, "proc/driver/nvidia/version", content)
		DetectNVIDIA(root)
	})
}

// FuzzChassisType asserts arbitrary DMI content never panics and never yields a
// laptop verdict from a value that is not a laptop class.
func FuzzChassisType(f *testing.F) {
	f.Add("3\n")
	f.Add("10\n")
	f.Add("")
	f.Add("-1\n")
	f.Add("99999999999999999999\n")
	f.Add("not a number\n")

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		writeUnder(t, root, "sys/class/dmi/id/chassis_type", content)
		got := ChassisType(root)
		if IsLaptopChassis(got) && ChassisName(got) == "" {
			t.Fatalf("chassis %d reads as a laptop but has no name", got)
		}
	})
}
