package artifacts

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestDownloadPinsWellFormed(t *testing.T) {
	seen := map[string]string{}
	for _, d := range Downloads() {
		if d.Name == "" || d.Version == "" {
			t.Errorf("download missing name/version: %+v", d)
		}
		if !strings.HasPrefix(d.URL, "https://") {
			t.Errorf("%s: URL %q is not https", d.Name, d.URL)
		}
		if !sha256Hex.MatchString(d.SHA256) {
			t.Errorf("%s: malformed SHA256 pin %q", d.Name, d.SHA256)
		}
		if d.File == "" {
			t.Errorf("%s: no stable cache filename", d.Name)
		}
		if prev, dup := seen[d.File]; dup {
			t.Errorf("%s and %s share cache filename %q", prev, d.Name, d.File)
		}
		seen[d.File] = d.Name
	}
	if LookingGlassVersion == "" || !strings.Contains(LookingGlassHost.URL, LookingGlassVersion) {
		t.Errorf("LookingGlassHost.URL %q must embed LookingGlassVersion %q",
			LookingGlassHost.URL, LookingGlassVersion)
	}
}

// TestRPMVersionMatchesTheMakefile holds the one string Go and rpm must agree
// on: the RPMs are built with --define "lgver $(LG_RPMVER)", so that expression
// names the dkms tree a Go caller addresses.
func TestRPMVersionMatchesTheMakefile(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^LG_RPMVER\s*:?=\s*(\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("no LG_RPMVER assignment in the Makefile — this test no longer guards anything")
	}
	want := strings.Replace(string(m[1]), "$(LG_VERSION)", LookingGlassVersion, 1)
	if LookingGlassRPMVersion != want {
		t.Errorf("LookingGlassRPMVersion = %q, but the Makefile builds %q", LookingGlassRPMVersion, want)
	}
}
