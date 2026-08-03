package preflight

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/utils"
)

// Module-signing key pairs a host can already have. Only locally generated keys
// have their private half on the machine, which is why these two are the whole
// list: the Fedora CA cert is also enrolled everywhere, but Red Hat holds that
// private key.
const (
	DKMSCert   = "/var/lib/dkms/mok.pub"
	DKMSKey    = "/var/lib/dkms/mok.key"
	akmodsCert = "/etc/pki/akmods/certs/*.der"
	akmodsKey  = "/etc/pki/akmods/private"
)

type SigningKey struct {
	Cert     string `json:"cert"`
	Key      string `json:"key"`
	Enrolled bool   `json:"enrolled"`
}

// ModuleSigning decides whether the kvmfr module will load under Secure Boot.
// dkms signs with one host-wide key, so kvmfr inherits whatever already signs
// the host's other out-of-tree modules — usually nothing to do.
type ModuleSigning struct {
	// Checked is false under --root, where these paths describe the developer's
	// machine rather than the fixture, and the question cannot be answered.
	Checked bool         `json:"checked"`
	DKMS    SigningKey   `json:"dkms"`
	Akmods  []SigningKey `json:"akmods,omitempty"`
	// OtherDKMSModules marks dkms modules besides kvmfr. Repointing dkms at
	// another key is global, so it is only safe when nothing else uses it.
	OtherDKMSModules bool `json:"other_dkms_modules"`
}

// SigningPlan is what has to happen before kvmfr can load.
type SigningPlan int

const (
	// SigningReady covers Secure Boot off and the common case where the key
	// dkms signs with is already enrolled.
	SigningReady SigningPlan = iota
	// SigningReuseAkmods points dkms at an akmods key the host already enrolled.
	SigningReuseAkmods
	// SigningNotBuilt means dkms has not built anything yet, so there is no key
	// to judge — dkms generates one on its first build.
	SigningNotBuilt
	// SigningEnroll needs the user at the MOK screen after a reboot.
	SigningEnroll
)

// PlanSigning classifies the host. The returned key is the akmods pair to hand
// dkms, set only for SigningReuseAkmods.
func PlanSigning(secureBoot bool, s ModuleSigning) (SigningPlan, SigningKey) {
	switch {
	case !secureBoot || !s.Checked:
		return SigningReady, SigningKey{}
	case s.DKMS.Enrolled:
		return SigningReady, SigningKey{}
	}
	// Reuse before reporting a missing dkms key: repointing dkms at an enrolled
	// pair fixes the first build too.
	for _, k := range s.Akmods {
		if k.Enrolled && k.Key != "" && !s.OtherDKMSModules {
			return SigningReuseAkmods, k
		}
	}
	if s.DKMS.Cert == "" {
		return SigningNotBuilt, SigningKey{}
	}
	return SigningEnroll, SigningKey{}
}

// KVMFRWillLoad reports whether Secure Boot would accept the module dkms built.
// Built-but-rejected is the one state the hook's "run `orthogonals up`" remedy
// cannot escape: the file exists, so the render picks kvmfr again. The render
// has to decline it here.
func KVMFRWillLoad(root string, secureBoot bool) bool {
	plan, _ := PlanSigning(secureBoot, gatherSigning(root))
	return plan == SigningReady || plan == SigningReuseAkmods
}

func gatherSigning(root string) ModuleSigning {
	if root != "" || !haveMokutil() {
		return ModuleSigning{}
	}
	s := ModuleSigning{Checked: true, OtherDKMSModules: otherDKMSModules()}
	// Both halves, as the akmods branch below requires: a cert without its key
	// reads as ready to sign, and dkms then builds a module Secure Boot rejects.
	if existsFile(DKMSCert) && existsFile(DKMSKey) {
		s.DKMS = SigningKey{Cert: DKMSCert, Key: DKMSKey, Enrolled: keyEnrolled(DKMSCert)}
	}
	certs, _ := filepath.Glob(akmodsCert)
	for _, cert := range certs {
		name := strings.TrimSuffix(filepath.Base(cert), ".der")
		key := filepath.Join(akmodsKey, name+".priv")
		if !existsFile(key) {
			continue
		}
		s.Akmods = append(s.Akmods, SigningKey{Cert: cert, Key: key, Enrolled: keyEnrolled(cert)})
	}
	return s
}

// keyEnrolled reads the verdict out of mokutil's output rather than its status:
// mokutil exits 1 when the key IS enrolled and 0 when it is not.
var keyEnrolled = func(cert string) bool {
	out, _ := exec.Command("mokutil", "--test-key", cert).Output()
	return strings.Contains(string(out), "is already enrolled")
}

var otherDKMSModules = func() bool {
	out, err := exec.Command("dkms", "status").Output()
	if err != nil {
		return true // unknown: never repoint a key on a guess
	}
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "kvmfr/") {
			return true
		}
	}
	return false
}

var haveMokutil = func() bool {
	_, err := exec.LookPath("mokutil")
	return err == nil
}

// existsFile is a seam: these key paths are absolute rather than --root
// prefixed, so a unit test cannot create them. An unreadable key reads as
// absent — /var/lib/dkms is root-only and preflight runs unprivileged, so the
// alternative is refusing to classify on every ordinary run.
var existsFile = func(path string) bool {
	ok, _ := utils.Exists(path)
	return ok
}
