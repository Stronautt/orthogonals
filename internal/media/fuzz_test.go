package media

import (
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/utils"
)

// FuzzRender guards the two escapers between user-supplied guest settings and
// root-authored artifacts: utils.XMLEscape for the answer file,
// utils.PowerShellEscape for provision.ps1. --guest-user, --guest-password and --locale reach both
// unmodified, and the script runs elevated in the guest.
func FuzzRender(f *testing.F) {
	f.Add("user", "password", "en-US", 1920, 1080)
	f.Add("", "", "", 0, 0)
	f.Add("a'b", `c"d`, "uk-UA", 3840, 2160)
	f.Add("x&y", "<hack>", "en-GB", 2560, 1440)
	// The shapes an escaper is supposed to neutralize.
	f.Add("user", "p'; Remove-Item C:\\ -Recurse #", "en-US", 1920, 1080)
	f.Add("user", "pw", "en-US'; exit #", 1920, 1080)
	f.Add("ünïcode", "паролль", "uk-UA", 1280, 720)
	// A frame size whose doubling loop used to overflow uint64 and never return.
	f.Add("user", "pw", "en-US", 1<<30, 1<<30)

	f.Fuzz(func(t *testing.T, user, password, locale string, w, h int) {
		p, err := NewProfile(user, password, locale, w, h, nil)
		if err != nil {
			return // refusal is the validator's job; nothing to render
		}
		arts, err := Render(p)
		if err != nil {
			t.Fatalf("Render(%+v): %v", p, err)
		}
		byName := make(map[string][]byte, len(arts))
		for _, a := range arts {
			byName[a.Name] = a.Content
		}

		for _, name := range []string{"autounattend.xml", "vdd_settings.xml"} {
			b, ok := byName[name]
			if !ok {
				t.Fatalf("Render produced no %s", name)
			}
			wellFormedXML(t, b)
		}

		// Every interpolation sits in a single-quoted PowerShell literal, so
		// the escaped form being present means it never terminated the string.
		ps := string(byName["provision.ps1"])
		if !strings.Contains(ps, "'"+utils.PowerShellEscape(p.GuestUser)+"'") {
			t.Errorf("provision.ps1 does not carry the guest user %q as an escaped literal:\n%s",
				p.GuestUser, ps)
		}
	})
}
