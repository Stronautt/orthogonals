package hw

import (
	"reflect"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func seq(from, to int) []int {
	var out []int
	for n := from; n <= to; n++ {
		out = append(out, n)
	}
	return out
}

func TestDetectCPUHybridReference(t *testing.T) {
	c, err := detectCPU(hwtest.ReferenceRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	if c.Threads != 20 {
		t.Errorf("Threads = %d, want 20", c.Threads)
	}
	if c.Cores != 14 {
		t.Errorf("Cores = %d, want 14", c.Cores)
	}
	if !c.Hybrid {
		t.Error("Hybrid = false, want true")
	}
	if !reflect.DeepEqual(c.PCores, seq(0, 11)) {
		t.Errorf("PCores = %v, want 0-11", c.PCores)
	}
	if !reflect.DeepEqual(c.ECores, seq(12, 19)) {
		t.Errorf("ECores = %v, want 12-19", c.ECores)
	}
	if c.Vendor != CPUVendorIntel {
		t.Errorf("Vendor = %q, want %q", c.Vendor, CPUVendorIntel)
	}
}

func TestCPUVendor(t *testing.T) {
	tests := []struct {
		name    string
		cpuinfo string
		want    string
	}{
		{name: "intel", cpuinfo: "vendor_id\t: GenuineIntel\n", want: CPUVendorIntel},
		{name: "amd", cpuinfo: "vendor_id\t: AuthenticAMD\n", want: CPUVendorAMD},
		{name: "unknown vendor", cpuinfo: "vendor_id\t: SomethingElse\n", want: ""},
		{name: "no vendor_id line", cpuinfo: "processor\t: 0\n", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			hwtest.WriteFile(t, root, "proc/cpuinfo", tt.cpuinfo)
			if got := cpuVendor(root); got != tt.want {
				t.Errorf("cpuVendor = %q, want %q", got, tt.want)
			}
		})
	}
	if got := cpuVendor(t.TempDir()); got != "" {
		t.Errorf("cpuVendor with no cpuinfo = %q, want empty", got)
	}
}

func TestDetectCPUUniformFallback(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "sys/devices/system/cpu/present", "0-7\n")
	for cpu, core := range []int{0, 0, 1, 1, 2, 2, 3, 3} {
		hwtest.WriteFile(t, root, "sys/devices/system/cpu/cpu"+string(rune('0'+cpu))+"/topology/core_id", string(rune('0'+core))+"\n")
	}

	c, err := detectCPU(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Threads != 8 {
		t.Errorf("Threads = %d, want 8", c.Threads)
	}
	if c.Cores != 4 {
		t.Errorf("Cores = %d, want 4", c.Cores)
	}
	if c.Hybrid {
		t.Error("Hybrid = true, want false without cpu_core/cpu_atom")
	}
	if !reflect.DeepEqual(c.PCores, seq(0, 7)) {
		t.Errorf("PCores = %v, want all CPUs 0-7", c.PCores)
	}
	if c.ECores != nil {
		t.Errorf("ECores = %v, want nil", c.ECores)
	}
}

func TestDetectCPUMissingTopologyFallsBackToThreads(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "sys/devices/system/cpu/present", "0-3\n")

	c, err := detectCPU(root)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cores != 4 {
		t.Errorf("Cores = %d, want Threads fallback 4", c.Cores)
	}
}

func TestDetectCPUMissingPresent(t *testing.T) {
	if _, err := detectCPU(t.TempDir()); err == nil {
		t.Fatal("want error when cpu present file is missing")
	}
}

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		in      string
		want    []int
		wantErr bool
	}{
		{in: "0", want: []int{0}},
		{in: "0-3", want: []int{0, 1, 2, 3}},
		{in: "0-2,8-9", want: []int{0, 1, 2, 8, 9}},
		{in: "12-19", want: seq(12, 19)},
		{in: "", want: nil},
		{in: "abc", wantErr: true},
		{in: "3-1", wantErr: true},
		{in: "1-x", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseCPUList(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseCPUList(%q): want error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPUList(%q): %v", tt.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseCPUList(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
