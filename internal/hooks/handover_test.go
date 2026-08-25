package hooks

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
	"github.com/stronautt/orthogonals/internal/testsupport"
)

// Writing to these is a command, not a setting: a real kernel keeps no record
// of what went in, only the fixture's regular file does. Everything else under
// sys is state a rollback owes the host back.
var sysfsActuators = []string{"unbind", "bind", "drivers_probe", "remove", "rescan"}

func hostState(t *testing.T, root string) string {
	t.Helper()
	snap, err := stepstest.Snapshot(filepath.Join(root, "sys"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for line := range strings.SplitSeq(strings.TrimRight(snap, "\n"), "\n") {
		path, _, _ := strings.Cut(line, " ")
		if !slices.Contains(sysfsActuators, filepath.Base(path)) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

func chmodReadOnly(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
}

// A handover that fails owes the host every mutation back. The assertion is an
// empty diff rather than a list of expected changes, so a mutation added to
// Detach without a rollback shows up here as an unexplained line — nobody has
// to remember to extend a table. The last row runs every mutation before
// failing, which covers anything inserted between the others.
func TestDetachRollsBackEveryMutation(t *testing.T) {
	tests := []struct {
		name           string
		modprobeScript string
		inject         func(t *testing.T, root string)
	}{
		{
			name: "wake times out",
			inject: func(t *testing.T, root string) {
				for _, d := range []string{gpuAddr, audAddr} {
					hwtest.WriteFile(t, root, "sys/bus/pci/devices/"+d+"/power/control", "auto\n")
				}
				stubRuntimeStatus(t, func(_, _ string) string { return "suspended" })
				testsupport.Swap(t, &WakeTimeout, time.Millisecond)
			},
		},
		{
			name: "module unload fails partway",
			inject: func(t *testing.T, _ string) {
				stubDeleteModuleFunc(t, func(name string) error {
					if name == "nvidia_uvm" {
						return unix.EWOULDBLOCK
					}
					return nil
				})
			},
		},
		{
			name:           "modprobe vfio-pci fails",
			modprobeScript: "exit 1",
			inject:         func(t *testing.T, _ string) {},
		},
		{
			name: "driver_override write fails",
			inject: func(t *testing.T, root string) {
				chmodReadOnly(t, filepath.Join(root, "sys/bus/pci/devices", gpuAddr, "driver_override"))
			},
		},
		{
			name: "probe write fails",
			inject: func(t *testing.T, root string) {
				hwtest.WriteFile(t, root, "sys/bus/pci/drivers_probe", "")
				chmodReadOnly(t, filepath.Join(root, "sys/bus/pci/drivers_probe"))
			},
		},
		{
			name: "final verify fails",
			inject: func(t *testing.T, _ string) {
				stubDeviceDriver(t, func(_, _ string) string { return "nvidia" })
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := hookRoot(t)
			stubDeviceDriver(t, driverFromOverride)
			stubDeleteModule(t, nil)
			stubNotify(t)
			fakeBin(t, "modprobe", tc.modprobeScript)
			fakeBin(t, "nvidia-smi", "")
			tc.inject(t, root)
			sd := &sysdtest.Fake{}

			// After the injection, so a failure arranged by chmod is part of the
			// baseline rather than a diff of its own.
			before := hostState(t, root)

			if err := Detach(root, "tester", sd); err == nil {
				t.Fatal("Detach succeeded, expected the injected failure")
			}
			if diff := stepstest.Diff(before, hostState(t, root)); diff != "" {
				t.Errorf("failed handover left the host changed:\n%s", diff)
			}
			if handoverStarted(root) {
				t.Error("handover marker survived a rollback that reached a healthy GPU")
			}
			// apply disables these permanently — restarting one re-opens the GPU
			// the rollback just handed back, so they are the mutations a rollback
			// must leave alone.
			for _, c := range sd.Calls {
				for _, unit := range []string{hostcfg.UnitPersistenced, hostcfg.UnitPowerd} {
					if strings.Contains(c, unit) && !strings.HasPrefix(c, "stop ") {
						t.Errorf("rollback restarted a permanently disabled unit: %q", c)
					}
				}
			}
		})
	}
}
