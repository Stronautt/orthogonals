package hooks

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
)

// seedIsolation wires the cgroup/cpu sysfs the isolation guard reads.
func seedIsolation(t *testing.T, root, present, controllers string) {
	t.Helper()
	hwtest.WriteFile(t, root, "sys/fs/cgroup/cgroup.controllers", controllers+"\n")
	hwtest.WriteFile(t, root, "sys/devices/system/cpu/present", present+"\n")
}

// writePinnedDomain writes a VM XML whose cputune pins CPUs 2-19 (reference host).
func writePinnedDomain(t *testing.T, root, vm string) {
	t.Helper()
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/"+vm+".xml", `<domain>
  <memory unit='MiB'>24576</memory>
  <cputune>
    <vcpupin vcpu='0' cpuset='2'/>
    <vcpupin vcpu='1' cpuset='3'/>
    <vcpupin vcpu='2' cpuset='4'/>
    <vcpupin vcpu='3' cpuset='5'/>
    <vcpupin vcpu='4' cpuset='6'/>
    <vcpupin vcpu='5' cpuset='7'/>
    <vcpupin vcpu='6' cpuset='8'/>
    <vcpupin vcpu='7' cpuset='9'/>
    <vcpupin vcpu='8' cpuset='10'/>
    <vcpupin vcpu='9' cpuset='11'/>
    <emulatorpin cpuset='12,13,14,15'/>
    <iothreadpin iothread='1' cpuset='16,17,18,19'/>
  </cputune>
</domain>`)
}

func TestIsolateAndUnisolateRoundTrip(t *testing.T) {
	root := t.TempDir()
	seedIsolation(t, root, "0-19", "cpuset cpu io memory")
	writePinnedDomain(t, root, "win11")
	sd := &sysdtest.Fake{}

	isolateCPUs(root, sd, "win11")
	for _, unit := range isolationUnits {
		if !slices.Equal(sd.AllowedCPUs[unit], []int{0, 1}) {
			t.Errorf("%s confined to %v, want the two reserved cores [0 1]", unit, sd.AllowedCPUs[unit])
		}
	}
	if _, err := os.Stat(filepath.Join(root, cpusetSaveFile)); err != nil {
		t.Errorf("isolation marker not written: %v", err)
	}

	sd2 := &sysdtest.Fake{}
	unisolateCPUs(root, sd2)
	full := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	for _, unit := range isolationUnits {
		if !slices.Equal(sd2.AllowedCPUs[unit], full) {
			t.Errorf("%s restore = %v, want the full present set", unit, sd2.AllowedCPUs[unit])
		}
	}
	if _, err := os.Stat(filepath.Join(root, cpusetSaveFile)); !os.IsNotExist(err) {
		t.Error("isolation marker must be removed after unisolate")
	}
}

func TestIsolateCPUsSkipsWithoutCgroupCpuset(t *testing.T) {
	root := t.TempDir()
	// cgroup.controllers lacks cpuset (or cgroup v1) → the feature cannot work.
	seedIsolation(t, root, "0-19", "cpu io memory")
	writePinnedDomain(t, root, "win11")
	sd := &sysdtest.Fake{}

	isolateCPUs(root, sd, "win11")
	if len(sd.Calls) != 0 {
		t.Errorf("isolation must be skipped without the cpuset controller: %v", sd.Calls)
	}
	if _, err := os.Stat(filepath.Join(root, cpusetSaveFile)); !os.IsNotExist(err) {
		t.Error("a skipped isolation must not leave a marker")
	}
}

func TestIsolateCPUsSkipsWhenHostHasNoFreeCore(t *testing.T) {
	root := t.TempDir()
	seedIsolation(t, root, "0-15", "cpuset")
	// Pins cover every present CPU (no-E-core host sharing cores) → nothing to reserve.
	hwtest.WriteFile(t, root, "etc/orthogonals/vms/win11.xml",
		`<domain><cputune><vcpupin vcpu='0' cpuset='0-13'/>`+
			`<emulatorpin cpuset='14,15'/><iothreadpin cpuset='14,15'/></cputune></domain>`)
	sd := &sysdtest.Fake{}

	isolateCPUs(root, sd, "win11")
	if len(sd.Calls) != 0 {
		t.Errorf("isolation must be skipped when the host has no free core: %v", sd.Calls)
	}
}

func TestUnisolateCPUsNoMarker(t *testing.T) {
	root := t.TempDir()
	sd := &sysdtest.Fake{}
	unisolateCPUs(root, sd) // marker absent — silent no-op
	if len(sd.Calls) != 0 {
		t.Errorf("unisolate without a marker must do nothing: %v", sd.Calls)
	}
}

func TestUnisolateCPUsCorruptMarker(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "run/orthogonals-cpuset", "garbage")
	sd := &sysdtest.Fake{}
	unisolateCPUs(root, sd)
	if len(sd.Calls) != 0 {
		t.Errorf("a corrupt marker must not drive systemd: %v", sd.Calls)
	}
	if _, err := os.Stat(filepath.Join(root, cpusetSaveFile)); !os.IsNotExist(err) {
		t.Error("a corrupt marker must still be cleared")
	}
}

func TestIsolateCPUsSkipsWithoutPresentFile(t *testing.T) {
	root := t.TempDir()
	hwtest.WriteFile(t, root, "sys/fs/cgroup/cgroup.controllers", "cpuset cpu\n")
	writePinnedDomain(t, root, "win11") // no present file seeded
	sd := &sysdtest.Fake{}
	isolateCPUs(root, sd, "win11")
	if len(sd.Calls) != 0 {
		t.Errorf("isolation must skip when the present CPU set is unreadable: %v", sd.Calls)
	}
}
