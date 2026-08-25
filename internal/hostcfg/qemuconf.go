package hostcfg

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/steps"
)

const DeviceACLStepID = "libvirt-device-acl"

// aclDevices are the entries orthogonals guarantees in cgroup_device_acl.
// Setting that key REPLACES libvirt's compiled-in default list rather than
// extending it, and Fedora's commented sample omits /dev/kvm — so uncommenting
// the shipped block, which is what the Looking Glass documentation instructs,
// leaves the guest unable to use KVM.
var aclDevices = []string{"/dev/kvm", steps.KVMFRDevice}

var (
	activeACL    = regexp.MustCompile(`(?ms)^cgroup_device_acl\s*=\s*\[.*?^\]`)
	commentedACL = regexp.MustCompile(`(?ms)^#cgroup_device_acl\s*=\s*\[.*?^#\]`)
)

// DeviceACLStep is the journaled edit that lets qemu open the kvmfr device.
// libvirt populates the private /dev it gives qemu from the same list, so one
// edit covers both the cgroup filter and the node's visibility, and it ignores
// non-existent paths, so the entry is safe on a host that never loads kvmfr.
func DeviceACLStep(qemuConf string) (steps.Step, error) {
	out, err := EnsureDeviceACL(qemuConf, aclDevices)
	if err != nil {
		return steps.Step{}, err
	}
	return steps.Step{
		ID: DeviceACLStepID, Kind: steps.KindWriteFile,
		Path: steps.QemuConfPath, Content: []byte(out), Mode: 0o644,
	}, nil
}

// EnsureDeviceACL returns conf with every device present in an active
// cgroup_device_acl list, activating libvirt's commented sample if that is all
// the file has. Idempotent: a conf that already lists everything is unchanged.
func EnsureDeviceACL(conf string, devices []string) (string, error) {
	block := activeACL.FindString(conf)
	if block == "" {
		commented := commentedACL.FindString(conf)
		if commented == "" {
			return "", fmt.Errorf("no cgroup_device_acl block in %s: cannot grant qemu access to %s",
				steps.QemuConfPath, steps.KVMFRDevice)
		}
		block = uncomment(commented)
		conf = strings.Replace(conf, commented, block, 1)
	}
	for _, dev := range devices {
		// Quoted on both sides: a bare substring test would find "/dev/kvm"
		// inside "/dev/kvmfr0" and skip an entry the domain needs.
		if strings.Contains(block, `"`+dev+`"`) {
			continue
		}
		// Split on the '[' the regexps already guaranteed, never on a literal
		// "[\n": what follows the bracket is whatever the file carries (trailing
		// space, CRLF), and matching on it would refuse a conf this can edit.
		open := strings.Index(block, "[") + 1
		grown := block[:open] + "\n    \"" + dev + "\"," + block[open:]
		conf = strings.Replace(conf, block, grown, 1)
		block = grown
	}
	return conf, nil
}

// DKMSDropInPath repoints dkms at a signing key. dkms reads its signing
// variables from framework.conf.d and nowhere else — dkms.conf's own variable
// whitelist excludes them — so this file is global to every dkms module, which
// is why the caller only reaches here when no other dkms module exists.
const DKMSDropInPath = "/etc/dkms/framework.conf.d/orthogonals-kvmfr.conf"

// DKMSSigningSteps make kvmfr sign with a key Secure Boot already trusts. The
// rebuild is not optional: the RPM's module was signed with dkms's own
// unenrolled key, and only a forced rebuild re-signs it for the running kernel.
// The version is not a step parameter. dkms addresses the tree that kvmfr-dkms
// registered, and the spec names that tree for the RPM version. A version in
// the identity of the step makes every `make lg-bump` refuse instead of
// rebuild.
func DKMSSigningSteps(cert, key string) []steps.Step {
	content := []byte("# Written by orthogonals: sign kvmfr with the key this host has already\n" +
		"# enrolled, so Secure Boot needs no new MOK.\n" +
		"mok_signing_key=" + key + "\n" +
		"mok_certificate=" + cert + "\n")
	// Input re-runs these steps after a key change, so a rotation converges and
	// no stale signature stays. Input also re-runs them after a version bump,
	// which the identity of the op does not carry.
	input := append(slices.Clone(content), "\n"+artifacts.LookingGlassRPMVersion...)
	module := map[string]string{"module": "kvmfr"}
	return []steps.Step{
		{
			ID: "dkms-signing-key", Kind: steps.KindWriteFile,
			Path: DKMSDropInPath, Content: content, Mode: 0o644,
		},
		{
			ID: "kvmfr-rebuild", Kind: steps.KindOp, Input: input,
			Op: steps.OpDKMSBuild, Args: module,
		},
		{
			ID: "kvmfr-reinstall", Kind: steps.KindOp, Input: input,
			Op: steps.OpDKMSInstall, Args: module,
		},
	}
}

func uncomment(block string) string {
	var b strings.Builder
	for line := range strings.Lines(block) {
		b.WriteString(strings.TrimPrefix(line, "#"))
	}
	return b.String()
}
