package hostcfg

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/steps"
)

// fedoraQemuConf is the shape Fedora 44 ships with libvirt 12: the block
// commented out and without /dev/kvm, the trap this editor exists for.
const fedoraQemuConf = `# Master configuration file for the QEMU driver.

#user = "qemu"

# cgroup_device_acl is a list of devices to be allowed in the VM's cgroup.
# Setting this replaces the built-in default list.
#cgroup_device_acl = [
#    "/dev/null", "/dev/full", "/dev/zero",
#    "/dev/random", "/dev/urandom",
#    "/dev/ptmx", "/dev/userfaultfd"
#]
#
# RDMA migration requires the following extra files to be added to the list:
#   "/dev/infiniband/rdma_cm",

#seccomp_sandbox = 1
`

func TestEnsureDeviceACL(t *testing.T) {
	t.Run("activates the shipped block and adds both devices", func(t *testing.T) {
		got, err := EnsureDeviceACL(fedoraQemuConf, aclDevices)
		if err != nil {
			t.Fatal(err)
		}
		block := activeACL.FindString(got)
		if block == "" {
			t.Fatalf("no active block after the edit:\n%s", got)
		}
		for _, want := range []string{
			`"/dev/kvm"`, `"/dev/kvmfr0"`,
			// libvirt's own entries must survive: the key replaces the
			// compiled-in default rather than extending it.
			`"/dev/null"`, `"/dev/ptmx"`, `"/dev/userfaultfd"`,
		} {
			if !strings.Contains(block, want) {
				t.Errorf("active list lacks %s:\n%s", want, block)
			}
		}
		// Untouched context must survive verbatim.
		for _, want := range []string{`#user = "qemu"`, "#seccomp_sandbox = 1", "#   \"/dev/infiniband/rdma_cm\","} {
			if !strings.Contains(got, want) {
				t.Errorf("edit lost %q", want)
			}
		}
		if strings.Contains(got, "#cgroup_device_acl") {
			t.Error("the commented block was left behind alongside the active one")
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		once, err := EnsureDeviceACL(fedoraQemuConf, aclDevices)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := EnsureDeviceACL(once, aclDevices)
		if err != nil {
			t.Fatal(err)
		}
		if once != twice {
			t.Errorf("second pass changed the file:\n%s", twice)
		}
	})

	t.Run("an already active list only gains what it lacks", func(t *testing.T) {
		active := "cgroup_device_acl = [\n    \"/dev/null\", \"/dev/kvm\"\n]\n"
		got, err := EnsureDeviceACL(active, aclDevices)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(got, `"/dev/kvm"`); n != 1 {
			t.Errorf("/dev/kvm appears %d times, want 1:\n%s", n, got)
		}
		if !strings.Contains(got, `"/dev/kvmfr0"`) {
			t.Errorf("missing device not added:\n%s", got)
		}
	})

	// The reason the presence test is quoted on both sides.
	t.Run("kvmfr0 does not satisfy kvm", func(t *testing.T) {
		active := "cgroup_device_acl = [\n    \"/dev/kvmfr0\"\n]\n"
		got, err := EnsureDeviceACL(active, aclDevices)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, `"/dev/kvm"`) {
			t.Errorf("substring match skipped /dev/kvm:\n%s", got)
		}
	})

	t.Run("no block at all is refused, not guessed", func(t *testing.T) {
		if _, err := EnsureDeviceACL("#user = \"qemu\"\n", aclDevices); err == nil {
			t.Error("a qemu.conf without the key must be an error")
		}
	})
}

func TestDeviceACLStep(t *testing.T) {
	step, err := DeviceACLStep(fedoraQemuConf)
	if err != nil {
		t.Fatal(err)
	}
	if step.Path != steps.QemuConfPath || step.Kind != steps.KindWriteFile {
		t.Errorf("step = %s %s, want a write_file for %s", step.Kind, step.Path, steps.QemuConfPath)
	}
	if step.Mode != 0o644 {
		t.Errorf("mode = %o, want 0644 (what libvirt ships)", step.Mode)
	}
	if !strings.Contains(string(step.Content), `"`+steps.KVMFRDevice+`"`) {
		t.Error("step content does not grant the kvmfr device")
	}
}

// TestDKMSSigningSteps covers the host that trusts an akmods key but not dkms's
// own: apply repoints dkms and forces a rebuild, re-signing with the enrolled key.
func TestDKMSSigningSteps(t *testing.T) {
	const cert, key = "/etc/pki/akmods/certs/public_key.der", "/etc/pki/akmods/private/public_key.priv"
	list := DKMSSigningSteps(cert, key)

	dropIn := list[0]
	if dropIn.Path != DKMSDropInPath || dropIn.Kind != steps.KindWriteFile {
		t.Errorf("first step = %s %s, want a write_file for %s", dropIn.Kind, dropIn.Path, DKMSDropInPath)
	}
	// dkms reads only these two names, and only out of framework.conf.d.
	for _, want := range []string{"mok_signing_key=" + key, "mok_certificate=" + cert} {
		if !strings.Contains(string(dropIn.Content), want) {
			t.Errorf("drop-in lacks %q:\n%s", want, dropIn.Content)
		}
	}

	// The spec names the dkms tree for the RPM version, so the bare upstream
	// release addresses a version dkms has never heard of.
	module := "kvmfr/" + artifacts.LookingGlassRPMVersion
	for _, step := range list[1:] {
		if step.Kind != steps.KindRunCmd {
			t.Errorf("%s is a %s, want run_cmd", step.ID, step.Kind)
			continue
		}
		if !slices.Contains(step.Cmd, module) {
			t.Errorf("%s addresses %v, want the module as %q", step.ID, step.Cmd, module)
		}
		// Without Input the rebuild is journaled once and never re-runs, leaving
		// a later key change unapplied.
		if !bytes.Equal(step.Input, dropIn.Content) {
			t.Errorf("%s does not re-run when the key changes", step.ID)
		}
	}
}

// TestDeviceACLAgainstTheRealLibvirt converts the qemu.conf the installed
// libvirt actually shipped, which is the only way to catch that vendor file
// changing shape — Fedora's sample already omits /dev/kvm, and a future
// release may move more. It writes the file, so it runs only when the VM tier
// asks for it by name; the tier restarts virtqemud and starts a probe domain
// afterwards to prove the resulting list is complete.
func TestDeviceACLAgainstTheRealLibvirt(t *testing.T) {
	if os.Getenv("ORTHOGONALS_TIER_KVMFR") != "1" {
		t.Skip("rewrites /etc/libvirt/qemu.conf — covered by the VM tier (make test-vm)")
	}
	if os.Geteuid() != 0 {
		t.Skip("writing libvirt configuration needs root")
	}
	prior, err := os.ReadFile(steps.QemuConfPath)
	if err != nil {
		t.Fatalf("read the shipped %s: %v", steps.QemuConfPath, err)
	}
	// Deliberately no restore here: the tier restarts virtqemud onto this file
	// and then starts a probe domain through it, so putting the original back
	// when the test ends would test the pristine config instead of ours. The
	// caller that set ORTHOGONALS_TIER_KVMFR owns putting it back — for the VM
	// tier that is the cleanup trap in test/tmt/kvmfr.sh.

	step, err := DeviceACLStep(string(prior))
	if err != nil {
		t.Fatalf("convert the shipped %s: %v", steps.QemuConfPath, err)
	}
	block := activeACL.FindString(string(step.Content))
	if block == "" {
		t.Fatalf("no active list after conversion:\n%s", step.Content)
	}
	for _, want := range aclDevices {
		if !strings.Contains(block, `"`+want+`"`) {
			t.Errorf("converted list lacks %s:\n%s", want, block)
		}
	}
	// Whatever this libvirt listed for itself has to survive, since setting the
	// key replaces its compiled-in default wholesale.
	if shipped := commentedACL.FindString(string(prior)); shipped != "" {
		for _, line := range strings.Split(uncomment(shipped), "\n") {
			for _, dev := range strings.Split(line, ",") {
				dev = strings.TrimSpace(dev)
				if !strings.HasPrefix(dev, `"/dev/`) {
					continue
				}
				if !strings.Contains(block, strings.TrimSuffix(dev, ",")) {
					t.Errorf("conversion dropped libvirt's own entry %s", dev)
				}
			}
		}
	}
	if err := os.WriteFile(steps.QemuConfPath, step.Content, step.Mode); err != nil {
		t.Fatalf("write %s: %v", steps.QemuConfPath, err)
	}
	t.Logf("wrote the converted %s:\n%s", steps.QemuConfPath, block)
}
