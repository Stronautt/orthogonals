package artifacts

import (
	"regexp"
	"strings"
	"testing"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// the pin table only ever fails at real download time; sanity-check it here
// so a malformed bump is caught before a multi-GB fetch.
func TestDownloadPinsWellFormed(t *testing.T) {
	seen := map[string]string{}
	all := append(Downloads(), LookingGlassSource)
	for _, d := range all {
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
	// LG requires client and guest host to be the same release
	if LookingGlassHost.Version != LookingGlassSource.Version {
		t.Errorf("Looking Glass host %s and source %s versions must match",
			LookingGlassHost.Version, LookingGlassSource.Version)
	}
	if len(Packages) == 0 {
		t.Error("host package list is empty")
	}
}
