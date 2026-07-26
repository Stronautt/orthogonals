package preflight

import "testing"

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
			signing: ModuleSigning{Checked: true},
			want:    SigningReady,
		},
		{
			name:       "unchecked (--root, or no mokutil) makes no claim",
			secureBoot: true,
			want:       SigningReady,
		},
		{
			// The common case: dkms signs everything with one key, so kvmfr
			// inherits whatever already signs nvidia-open or openrazer.
			name:       "the dkms key is already enrolled",
			secureBoot: true,
			signing:    ModuleSigning{Checked: true, DKMS: dkms(true)},
			want:       SigningReady,
		},
		{
			// akmod-nvidia hosts: their GPU driver works, yet dkms's key is not
			// enrolled, so kvmfr would be rejected without this.
			name:       "an enrolled akmods key is reused",
			secureBoot: true,
			signing:    ModuleSigning{Checked: true, DKMS: dkms(false), Akmods: akmods(true)},
			want:       SigningReuseAkmods,
			wantKey:    "/etc/pki/akmods/private/public_key.priv",
		},
		{
			// Repointing dkms is global, so it must not disturb a module that
			// is signing fine today.
			name:       "not reused when other dkms modules would be repointed",
			secureBoot: true,
			signing: ModuleSigning{Checked: true, DKMS: dkms(false), Akmods: akmods(true),
				OtherDKMSModules: true},
			want: SigningEnroll,
		},
		{
			name:       "an akmods key that is not enrolled is no help",
			secureBoot: true,
			signing:    ModuleSigning{Checked: true, DKMS: dkms(false), Akmods: akmods(false)},
			want:       SigningEnroll,
		},
		{
			// dkms generates its key on the first build, so absence means "not
			// built yet" rather than "not trusted".
			name:       "no dkms key at all",
			secureBoot: true,
			signing:    ModuleSigning{Checked: true},
			want:       SigningNotBuilt,
		},
		{
			name:       "reuse wins over reporting a missing dkms key",
			secureBoot: true,
			signing:    ModuleSigning{Checked: true, Akmods: akmods(true)},
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

// TestKVMFRWillLoad covers the trap the render has to avoid: a module that is
// built but unsigned-for-this-host would make `vm define` emit a domain the
// hook refuses, whose own remedy is `orthogonals up` — which renders it again.
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
			stubSigning(t, tc.enrolled)
			if got := KVMFRWillLoad("", tc.secureBoot); got != tc.want {
				t.Errorf("KVMFRWillLoad = %v, want %v", got, tc.want)
			}
		})
	}

	// --root describes the developer's machine, not the fixture, so the question
	// is unanswerable there and must not downgrade every fixture render.
	t.Run("under --root the host is taken at its word", func(t *testing.T) {
		stubSigning(t, false)
		if !KVMFRWillLoad(t.TempDir(), true) {
			t.Error("a --root run declined kvmfr on the developer's own key state")
		}
	})
}

// stubSigning makes gatherSigning answer without touching the real host: a
// unit test must never read this machine's enrolled keys.
func stubSigning(t *testing.T, enrolled bool) {
	t.Helper()
	oldMok, oldEnrolled, oldOther, oldExists := haveMokutil, keyEnrolled, otherDKMSModules, existsFile
	t.Cleanup(func() {
		haveMokutil, keyEnrolled, otherDKMSModules, existsFile = oldMok, oldEnrolled, oldOther, oldExists
	})
	haveMokutil = func() bool { return true }
	keyEnrolled = func(string) bool { return enrolled }
	otherDKMSModules = func() bool { return false }
	existsFile = func(string) bool { return true }
}

// TestKeyEnrolledReadsOutput pins the trap: mokutil exits 1 when a key IS
// enrolled, so a status-based test would invert every verdict.
func TestKeyEnrolledReadsOutput(t *testing.T) {
	if !haveMokutil() {
		t.Skip("mokutil not installed")
	}
	// A certificate that cannot exist is certainly not enrolled, and mokutil
	// still exits 0 for it — the case that would fool a status check.
	if keyEnrolled("/nonexistent/cert.der") {
		t.Error("a missing certificate must not read as enrolled")
	}
}
