package preflight

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/utils"
)

// Module-signing key pairs a host can already have. Only locally generated keys
// have their private half on the machine, which is why these two are the whole
// list: the Fedora CA cert is also enrolled everywhere, but Red Hat holds that
// private key.
const (
	dkmsDir    = "/var/lib/dkms"
	DKMSCert   = dkmsDir + "/mok.pub"
	DKMSKey    = dkmsDir + "/mok.key"
	akmodsCert = "/etc/pki/akmods/certs/*.der"
	akmodsKey  = "/etc/pki/akmods/private"

	// mokListRT is the runtime copy of the MOK list that shim keeps. It is the
	// variable that `mokutil --test-key` reads. Every user can read it, unlike
	// the keys that it vouches for.
	//
	// The GUID is the vendor namespace of shim, and libefivar exports the same
	// 16 bytes as efi_guid_shim. Nothing in the GUID comes from this machine, so
	// it is as fixed as the EFI_GLOBAL_VARIABLE GUID that hw.secureBootEnabled
	// carries. The GUID is also what makes the list the list of shim and not the
	// list of another vendor. A literal is therefore correct here, and a
	// MokListRT-* glob is not.
	mokListRT = "/sys/firmware/efi/efivars/MokListRT-605dab50-e046-4300-abb6-3dd810dd8b23"
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
	DKMS   SigningKey   `json:"dkms"`
	Akmods []SigningKey `json:"akmods,omitempty"`
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
	case !secureBoot:
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
	s := ModuleSigning{OtherDKMSModules: otherDKMSModules(root)}
	// Both halves, as the akmods branch below requires: a cert without its key
	// reads as ready to sign, and dkms then builds a module Secure Boot rejects.
	if existsFile(root, DKMSCert) && existsFile(root, DKMSKey) {
		s.DKMS = SigningKey{Cert: DKMSCert, Key: DKMSKey, Enrolled: keyEnrolled(root, DKMSCert)}
	}
	found, _ := filepath.Glob(filepath.Join(root, akmodsCert))
	for _, match := range found {
		// The record keeps the host path and never the prefixed one. PlanSigning
		// hands this pair to DKMSSigningSteps, which writes both paths into the
		// dkms drop-in that the real dkms reads.
		cert := unroot(root, match)
		name := strings.TrimSuffix(filepath.Base(cert), ".der")
		key := filepath.Join(akmodsKey, name+".priv")
		if !existsFile(root, key) {
			continue
		}
		s.Akmods = append(s.Akmods, SigningKey{Cert: cert, Key: key, Enrolled: keyEnrolled(root, cert)})
	}
	return s
}

// unroot removes a --root prefix from a path that a glob returned.
func unroot(root, path string) string {
	if root == "" {
		return path
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return "/" + rel
}

// keyEnrolled reports whether the DER certificate at cert is in the MOK list of
// the firmware.
//
// Enrollment is necessary and not sufficient. Fedora links MOK keys into the
// module keyring only when MokListTrustedRT is set, and this function does not
// read that variable.
func keyEnrolled(root, cert string) bool {
	// The file is present and unreadable, because existsFile already found it.
	// The answer "not enrolled" sends the user to mokutil --import, which is the
	// safe direction.
	der, err := os.ReadFile(filepath.Join(root, cert))
	if err != nil {
		return false
	}
	// A host without shim has no MOK list, so shim enrolled nothing on it. The
	// answer and the remedy are the same as for a key that the list does not
	// hold. A host without efivarfs never reaches this point. Secure Boot comes
	// from a sibling path, so it reads false and the caller stops earlier.
	mok, err := os.ReadFile(filepath.Join(root, mokListRT))
	if err != nil {
		return false
	}
	return mokListEnrolled(mok, der)
}

const (
	sigListHeaderLen = 28 // EFI_GUID SignatureType, then three UINT32
	sigOwnerLen      = 16 // every EFI_SIGNATURE_DATA opens with a SignatureOwner GUID

	// Offsets of the three UINT32 within the list header.
	offListSize = 16
	offHdrSize  = 20
	offSigSize  = 24
)

// mokListEnrolled reports whether der appears without change as one
// SignatureData payload in efivar. efivar is the raw content of a MOK-list
// variable from efivarfs.
//
// The function reads the bytes alone, because a unit run on an ordinary machine
// can reach neither input. It returns no error. Every malformed list gets the
// same answer, "not enrolled", and that answer is the safe one. It costs one MOK
// enrollment, where the opposite answer loads a module that the firmware
// rejects.
//
// UEFI 2.10 §32.4.1, little-endian, after the attribute mask of efivarfs:
//
//	EFI_GUID SignatureType; UINT32 SignatureListSize, SignatureHeaderSize,
//	SignatureSize; UINT8 SignatureHeader[SignatureHeaderSize];
//	EFI_SIGNATURE_DATA Signatures[]   // 16-byte owner GUID, then the payload
//
// The function does not read SignatureType. The offsets do not depend on it,
// and no entry that is not a certificate can equal a DER certificate. Such an
// entry is a SHA-256 digest of 32 bytes.
func mokListEnrolled(efivar, der []byte) bool {
	if len(efivar) < utils.EFIVarAttrLen {
		return false
	}
	b := efivar[utils.EFIVarAttrLen:]
	for len(b) >= sigListHeaderLen {
		// Every field widens to int before any arithmetic. On a 64-bit int each
		// sum below is exact, so no length field can wrap into an offset that
		// looks valid.
		listSize := int(binary.LittleEndian.Uint32(b[offListSize:]))
		hdrSize := int(binary.LittleEndian.Uint32(b[offHdrSize:]))
		sigSize := int(binary.LittleEndian.Uint32(b[offSigSize:]))
		// The lower bound is what stops this loop. The tail below becomes
		// shorter by at least the length of the header that this pass read. The
		// tests for a negative value matter only where int is 32 bits. There a
		// field above 2^31 arrives as an offset that indexes backwards.
		if listSize < sigListHeaderLen || listSize > len(b) || hdrSize < 0 || sigSize < 0 {
			return false
		}
		list := b[:listSize] // a reslice, so no inner offset can read past it
		if sigSize > sigOwnerLen && hdrSize <= listSize-sigListHeaderLen {
			for e := sigListHeaderLen + hdrSize; e+sigSize <= listSize; e += sigSize {
				if bytes.Equal(list[e+sigOwnerLen:e+sigSize], der) {
					return true
				}
			}
		}
		b = b[listSize:]
	}
	return false
}

// otherDKMSModules reports whether dkms has a module registered other than
// kvmfr. dkms keeps every module as <module>/<version>/. The loose files beside
// them, mok.pub and mok.key and post_transaction.log, are not modules. An
// unreadable directory gets the answer "yes". A change of the dkms key applies
// to the whole host, so this function never guesses.
func otherDKMSModules(root string) bool {
	ents, err := os.ReadDir(filepath.Join(root, dkmsDir))
	if err != nil {
		return true
	}
	for _, e := range ents {
		if e.IsDir() && e.Name() != "kvmfr" {
			return true
		}
	}
	return false
}

// existsFile treats an unreadable path as absent. The alternative is a refusal
// to classify on every ordinary unprivileged run.
func existsFile(root, path string) bool {
	ok, _ := utils.Exists(filepath.Join(root, path))
	return ok
}
