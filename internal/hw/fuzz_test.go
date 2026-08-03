package hw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fuzzFile makes the parent directory once for the whole run: a t.TempDir() per
// execution costs more I/O than the parse under test, enough on a loaded runner
// to stall every worker and time the run out.
func fuzzFile(f *testing.F, rel string) (root, path string) {
	f.Helper()
	root = f.TempDir()
	path = filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	return root, path
}

// writeFuzzFile replaces the whole file, so no input carries over to the next.
func writeFuzzFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// FuzzMeminfoKiB guards the value preflight sizes the guest from.
func FuzzMeminfoKiB(f *testing.F) {
	f.Add("MemTotal:       33554432 kB\nMemFree:  20000000 kB\n")
	f.Add("MemTotal:\n")
	f.Add("MemTotal: notanumber kB\n")
	f.Add("MemTotal: -1 kB\n")
	f.Add("MemTotal: 99999999999999999999999 kB\n")
	f.Add("")
	f.Add("MemTotal 33554432 kB\n")

	root, path := fuzzFile(f, "proc/meminfo")
	f.Fuzz(func(t *testing.T, content string) {
		writeFuzzFile(t, path, content)
		MeminfoKiB(root, "MemTotal")
	})
}

// FuzzDetectNVIDIA guards the string the manifest stamps and undo compares
// against.
func FuzzDetectNVIDIA(f *testing.F) {
	f.Add("NVRM version: NVIDIA UNIX x86_64 Kernel Module  570.153.02  Wed Apr 30 01:53:00 UTC 2025\n")
	f.Add("NVRM version:\n")
	f.Add("garbage")
	f.Add("")

	root, path := fuzzFile(f, "proc/driver/nvidia/version")
	f.Fuzz(func(t *testing.T, content string) {
		writeFuzzFile(t, path, content)
		DetectNVIDIA(root)
	})
}

func FuzzChassisType(f *testing.F) {
	f.Add("3\n")
	f.Add("10\n")
	f.Add("")
	f.Add("-1\n")
	f.Add("99999999999999999999\n")
	f.Add("not a number\n")

	root, path := fuzzFile(f, "sys/class/dmi/id/chassis_type")
	f.Fuzz(func(t *testing.T, content string) {
		writeFuzzFile(t, path, content)
		got := ChassisType(root)
		if IsLaptopChassis(got) && ChassisName(got) == "" {
			t.Fatalf("chassis %d reads as a laptop but has no name", got)
		}
	})
}

func FuzzParseCPUList(f *testing.F) {
	f.Add("0-3,7,9-11")
	f.Add("")
	f.Add("   ")
	f.Add(",,,")
	f.Add("0-")
	f.Add("-1")
	f.Add("3-1")
	f.Add("999999999999999999999")
	f.Add("0-99999999")
	f.Add("1,,2")
	// an unbounded range: expanding this exhausted memory before MaxCPUIndex
	f.Add("9999-9999999999999999")
	f.Add("0-8191,0-8191,0-8191,0-8191")

	f.Fuzz(func(t *testing.T, s string) {
		cpus, err := ParseCPUList(s)
		if err != nil {
			if cpus != nil {
				t.Fatalf("ParseCPUList(%q) returned both cpus %v and error %v", s, cpus, err)
			}
			return
		}
		if len(cpus) > MaxCPUIndex+1 {
			t.Fatalf("ParseCPUList(%q) yielded %d cpus, past the %d bound", s, len(cpus), MaxCPUIndex+1)
		}
		for i, c := range cpus {
			if c < 0 || c > MaxCPUIndex {
				t.Fatalf("ParseCPUList(%q) yielded out-of-range index %d", s, c)
			}
			if i > 0 && c <= cpus[i-1] && strings.Count(s, ",") == 0 {
				t.Fatalf("ParseCPUList(%q) yielded a non-ascending range: %v", s, cpus)
			}
		}
	})
}
