package preflight

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func akmods(enrolled bool) []SigningKey {
	return []SigningKey{{
		Cert:     "/etc/pki/akmods/certs/public_key.der",
		Key:      "/etc/pki/akmods/private/public_key.priv",
		Enrolled: enrolled,
	}}
}

func dkms(enrolled bool) SigningKey {
	return SigningKey{Cert: DKMSCert, Key: DKMSKey, Enrolled: enrolled}
}

func TestPlanSigning(t *testing.T) {
	cases := []struct {
		name       string
		secureBoot bool
		signing    ModuleSigning
		want       SigningPlan
		wantKey    string
	}{
		{
			name:    "secure boot off needs nothing",
			signing: ModuleSigning{DKMS: dkms(false)},
			want:    SigningReady,
		},
		{
			name:       "the dkms key is already enrolled",
			secureBoot: true,
			signing:    ModuleSigning{DKMS: dkms(true)},
			want:       SigningReady,
		},
		{
			name:       "an enrolled akmods key is reused",
			secureBoot: true,
			signing:    ModuleSigning{DKMS: dkms(false), Akmods: akmods(true)},
			want:       SigningReuseAkmods,
			wantKey:    "/etc/pki/akmods/private/public_key.priv",
		},
		{
			name:       "not reused when other dkms modules would be repointed",
			secureBoot: true,
			signing: ModuleSigning{DKMS: dkms(false), Akmods: akmods(true),
				OtherDKMSModules: true},
			want: SigningEnroll,
		},
		{
			name:       "an akmods key that is not enrolled is no help",
			secureBoot: true,
			signing:    ModuleSigning{DKMS: dkms(false), Akmods: akmods(false)},
			want:       SigningEnroll,
		},
		{
			name:       "no dkms key at all",
			secureBoot: true,
			signing:    ModuleSigning{},
			want:       SigningNotBuilt,
		},
		{
			name:       "reuse wins over reporting a missing dkms key",
			secureBoot: true,
			signing:    ModuleSigning{Akmods: akmods(true)},
			want:       SigningReuseAkmods,
			wantKey:    "/etc/pki/akmods/private/public_key.priv",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, key := PlanSigning(tc.secureBoot, tc.signing)
			if got != tc.want {
				t.Errorf("PlanSigning = %v, want %v", got, tc.want)
			}
			if key.Key != tc.wantKey {
				t.Errorf("key = %q, want %q", key.Key, tc.wantKey)
			}
		})
	}
}

func TestKVMFRWillLoad(t *testing.T) {
	cases := []struct {
		name       string
		secureBoot bool
		enrolled   bool
		want       bool
	}{
		{name: "secure boot off imposes nothing", want: true},
		{name: "an enrolled key loads", secureBoot: true, enrolled: true, want: true},
		{name: "secure boot with no enrolled key refuses the module", secureBoot: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := hwtest.ReferenceRoot(t)
			if !tc.enrolled {
				// The same shape, with a certificate that the key of the
				// fixture does not match.
				hwtest.WriteFile(t, root, hwtest.MokListRTPath, hwtest.MOKListRT("a key this host never enrolled"))
			}
			if got := KVMFRWillLoad(root, tc.secureBoot); got != tc.want {
				t.Errorf("KVMFRWillLoad = %v, want %v", got, tc.want)
			}
		})
	}
}

// gatherSigning reads the fixture and not the key state of the developer. That
// is the whole reason the paths take a --root prefix.
func TestGatherSigningReadsTheFixture(t *testing.T) {
	root := hwtest.ReferenceRoot(t)
	s := gatherSigning(root)
	if s.DKMS.Cert != DKMSCert || s.DKMS.Key != DKMSKey {
		t.Errorf("dkms pair = %+v, want the unprefixed host paths", s.DKMS)
	}
	if !s.DKMS.Enrolled {
		t.Error("the fixture enrols its own dkms key; gatherSigning did not see it")
	}
	if s.OtherDKMSModules {
		t.Error("the fixture registers no module besides kvmfr")
	}
}

// efivar puts the attribute mask of efivarfs before the signature lists.
func efivar(parts ...[]byte) []byte {
	return append([]byte{0x06, 0, 0, 0}, bytes.Join(parts, nil)...)
}

// sigList encodes one EFI_SIGNATURE_LIST. Every payload must have the same
// length, because SignatureSize belongs to the list and not to the entry.
func sigList(payloads ...string) []byte { return sigListHdr(0, payloads...) }

// sigListHdr is sigList with a SignatureHeader of hdrLen bytes. That header
// comes between the list header and the first entry, and it moves every entry.
func sigListHdr(hdrLen int, payloads ...string) []byte {
	sigSize := sigOwnerLen + len(payloads[0])
	b := make([]byte, sigListHeaderLen)
	binary.LittleEndian.PutUint32(b[offListSize:], uint32(sigListHeaderLen+hdrLen+len(payloads)*sigSize))
	binary.LittleEndian.PutUint32(b[offHdrSize:], uint32(hdrLen))
	binary.LittleEndian.PutUint32(b[offSigSize:], uint32(sigSize))
	b = append(b, make([]byte, hdrLen)...)
	for _, p := range payloads {
		b = append(b, make([]byte, sigOwnerLen)...) // SignatureOwner
		b = append(b, p...)
	}
	return b
}

// hdr writes one header field of a correct list again, so a malformed case
// reads as "the good list, but this field is a lie". The function clones,
// because the caller keeps the original and append writes through it.
func hdr(list []byte, off int, v uint32) []byte {
	b := bytes.Clone(list)
	binary.LittleEndian.PutUint32(b[off:], v)
	return b
}

func TestMOKListEnrolled(t *testing.T) {
	good := sigList("cert-a")

	cases := []struct {
		name   string
		efivar []byte
		der    string
		want   bool
	}{
		{"the certificate is the only entry", efivar(good), "cert-a", true},
		{"a certificate that is not in the list", efivar(good), "cert-b", false},
		{"found in the second of two lists", efivar(sigList("aa"), good), "cert-a", true},
		{"found as the second entry of one list", efivar(sigList("cert-b", "cert-a")), "cert-a", true},
		{"a signature header is skipped", efivar(sigListHdr(8, "cert-a")), "cert-a", true},
		// The owner GUID comes before the payload and is not part of it. An
		// implementation with bytes.Contains passes every case above and fails
		// this one.
		{"the owner GUID is not part of the payload", efivar(good),
			string(make([]byte, sigOwnerLen)) + "cert-a", false},
		{"empty variable", nil, "cert-a", false},
		{"attribute mask only", []byte{0x06, 0, 0, 0}, "cert-a", false},
		{"a truncated list header", efivar(good)[:24], "cert-a", false},
		{"a list claiming more bytes than there are", efivar(hdr(good, offListSize, 1<<20)), "cert-a", false},
		{"a list smaller than its own header", efivar(hdr(good, offListSize, sigListHeaderLen-1)), "cert-a", false},
		// A loop that moves forward by SignatureListSize never stops on a zero.
		// If this case hangs, the package timeout is the signal and the test is
		// not flaky.
		{"a zero-length list terminates", efivar(hdr(good, offListSize, 0)), "cert-a", false},
		{"a signature no larger than its owner GUID", efivar(hdr(good, offSigSize, sigOwnerLen)), "cert-a", false},
		{"a zero-length signature", efivar(hdr(good, offSigSize, 0)), "cert-a", false},
		{"a signature header larger than the list", efivar(hdr(good, offHdrSize, 1<<30)), "cert-a", false},
		{"trailing bytes after the last list are ignored", efivar(good, []byte{1, 2, 3}), "cert-a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mokListEnrolled(tc.efivar, []byte(tc.der)); got != tc.want {
				t.Errorf("mokListEnrolled = %v, want %v", got, tc.want)
			}
		})
	}
}

// The firmware supplies the MOK list as binary, and the parser walks it by
// length fields that it cannot trust.
func FuzzMOKListEnrolled(f *testing.F) {
	const der = "\x30\x82\x03\x3c\x02\x01\x02" // a DER SEQUENCE header, so seeds look like certificates
	good := sigList(der)
	f.Add(efivar(good))
	f.Add(efivar(good, good))
	f.Add(efivar(good, []byte{1, 2, 3}))
	f.Add(efivar(hdr(good, offListSize, 0)))
	f.Add(efivar(hdr(good, offListSize, sigListHeaderLen-1)))
	f.Add(efivar(hdr(good, offListSize, ^uint32(0))))
	f.Add(efivar(hdr(good, offHdrSize, ^uint32(0))))
	f.Add(efivar(hdr(good, offSigSize, 0)))
	f.Add(efivar(good)[:20])
	f.Add([]byte{0x06, 0, 0, 0})
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, v []byte) {
		// A yes must be a yes about these bytes. An offset error can report a
		// match that it never compared. Such an error passes a fuzz that only
		// looks for a panic, and it fails in the direction that trusts an
		// unsigned module.
		if mokListEnrolled(v, []byte(der)) && !bytes.Contains(v, []byte(der)) {
			t.Fatalf("enrolled reported for %d bytes that do not contain the certificate", len(v))
		}
	})
}

func TestOtherDKMSModules(t *testing.T) {
	cases := []struct {
		name    string
		modules []string
		want    bool
	}{
		{"only kvmfr, beside dkms's own loose files", []string{"kvmfr/0~B7-rc1"}, false},
		{"a second module means the key is shared", []string{"kvmfr/0~B7-rc1", "nvidia/570.153.02"}, true},
		{"nothing built yet", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, m := range tc.modules {
				if err := os.MkdirAll(filepath.Join(root, dkmsDir, m), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The loose files that dkms keeps beside its module trees.
			for _, f := range []string{"mok.pub", "mok.key", "post_transaction.log"} {
				hwtest.WriteFile(t, root, filepath.Join("var/lib/dkms", f), "x")
			}
			if got := otherDKMSModules(root); got != tc.want {
				t.Errorf("otherDKMSModules = %v, want %v", got, tc.want)
			}
		})
	}

	// An unreadable directory is not the same as "nothing else". A change of the
	// dkms key applies to the whole host.
	t.Run("an absent directory is never taken as empty", func(t *testing.T) {
		if !otherDKMSModules(t.TempDir()) {
			t.Error("a directory that cannot be read must not read as kvmfr-only")
		}
	})
}

// A hardcoded path addresses the MOK list. That is safe only because a host
// that cannot answer the question never reaches the path. Secure Boot comes
// from a sibling efivar, so a host without efivarfs has no Secure Boot and gets
// no signing check. If the state comes from anywhere else, a BIOS host is told
// to enroll a key.
func TestSigningIsUnreachableWithoutEFIVars(t *testing.T) {
	t.Run("no efivarfs asks nothing of the user", func(t *testing.T) {
		root := hwtest.ReferenceRoot(t)
		if err := os.RemoveAll(filepath.Join(root, "sys/firmware/efi/efivars")); err != nil {
			t.Fatal(err)
		}
		res, err := hw.Detect(root)
		if err != nil {
			t.Fatal(err)
		}
		if res.Platform.SecureBoot {
			t.Fatal("Secure Boot must read false without efivarfs, or the check below is not the one under test")
		}
		if c := secureBootCheck(t, res, root); c.Status != Pass {
			t.Errorf("secure-boot = %s %q, want a pass: the host cannot be asked", c.Status, c.Message)
		}
	})

	// A Secure Boot host without shim. The list really is absent and the key
	// really is untrusted, so the warning is the true answer and not a failed
	// lookup.
	t.Run("secure boot with no MOK list warns", func(t *testing.T) {
		root := hwtest.ReferenceRoot(t)
		if err := os.Remove(filepath.Join(root, hwtest.MokListRTPath)); err != nil {
			t.Fatal(err)
		}
		res, err := hw.Detect(root)
		if err != nil {
			t.Fatal(err)
		}
		if c := secureBootCheck(t, res, root); c.Status != Warn {
			t.Errorf("secure-boot = %s %q, want a warn", c.Status, c.Message)
		}
	})
}

func secureBootCheck(t *testing.T, res *hw.Result, root string) Check {
	t.Helper()
	for _, c := range Analyze(res, GatherFacts(root)) {
		if c.Name == "secure-boot" {
			return c
		}
	}
	t.Fatal("Analyze produced no secure-boot check")
	return Check{}
}
