// Package media builds the guest installation media: the unattended Windows
// answer file, the VDD settings, the provision ISO (xorriso, volume label
// ORTHOGONALS), and the checksum-pinned download cache for everything the
// guest install needs (virtio-win, NVIDIA driver, Looking Glass host, VDD,
// nefcon). The Windows 11 ISO itself is user-supplied and only validated —
// never fetched (legal constraint).
package media

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/stronautt/orthogonals/internal/domain"
)

//go:embed templates
var templateFS embed.FS

const (
	// Edition is the only Windows edition v1 installs (Home cannot join the
	// unattended local-account flow reliably); autounattend selects the
	// image by this name and ValidateWin11ISO gates on it.
	Edition = "Windows 11 Pro"

	// DefaultGuestUser matches the Defaults table (--guest-user overrides).
	DefaultGuestUser = "user"

	// DefaultGuestPassword is deliberately trivial: the guest is a local
	// desktop VM behind the host's own login, and autologon means it is
	// rarely typed (--guest-password overrides).
	DefaultGuestPassword = "password"

	defaultLocale = "en-US"
)

// The guest-side display contract provisioning installs and verify asserts:
// the VDD adapter's hardware ID and the Looking Glass host service name.
const (
	VDDHardwareID     = `ROOT\MttVDD`
	LGHostServiceName = "Looking Glass (host)"
)

// forbiddenUserChars are the characters Windows refuses in account names.
const forbiddenUserChars = `"/\[]:;|=,+*?<>@`

// Mode is one display mode the VDD monitor advertises to Windows.
type Mode struct {
	Width, Height int
}

// standardModes is the resolution ladder offered to the guest; NewProfile
// filters it to the modes whose frames fit the Looking Glass buffer, so
// switching resolution in Windows display settings can never outgrow the
// IVSHMEM region (Looking Glass truncates frames that do).
var standardModes = []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}, {3840, 2160}}

// Profile is everything the rendered media varies on.
type Profile struct {
	GuestUser     string
	GuestPassword string
	Locale        string // locale and keyboard layout, e.g. en-US
	Width, Height int    // maximum guest resolution, sizes the IVSHMEM region
	Modes         []Mode // display modes the VDD monitor advertises
	Edition       string
}

// NewProfile validates the guest options; zero/empty values pick the
// Defaults-table values.
func NewProfile(user, password, locale string, width, height int) (Profile, error) {
	if user == "" {
		return Profile{}, errors.New("guest user name is empty")
	}
	if len(user) > 20 || strings.ContainsAny(user, forbiddenUserChars) || strings.Trim(user, ". ") == "" {
		return Profile{}, fmt.Errorf("guest user %q is not a valid Windows account name", user)
	}
	if password == "" {
		return Profile{}, errors.New("guest password is empty")
	}
	if locale == "" {
		locale = defaultLocale
	}
	if width == 0 && height == 0 {
		// the VDD mode ladder is filtered against the IVSHMEM region these
		// dimensions size, so the defaults are the domain package's
		width, height = domain.DefaultWidth, domain.DefaultHeight
	}
	if width <= 0 || height <= 0 {
		return Profile{}, fmt.Errorf("bad resolution %dx%d", width, height)
	}
	return Profile{
		GuestUser: user, GuestPassword: password, Locale: locale,
		Width: width, Height: height, Modes: guestModes(width, height),
		Edition: Edition,
	}, nil
}

// guestModes returns the standard modes that fit the IVSHMEM region sized
// for the maximum, plus the maximum itself when it is off the ladder.
// Region sizes are powers of two, so "same or smaller region" is exactly
// "the double-buffered frame fits".
func guestModes(maxW, maxH int) []Mode {
	budget := domain.IVSHMEMMiB(maxW, maxH)
	var out []Mode
	sawMax := false
	for _, m := range standardModes {
		if domain.IVSHMEMMiB(m.Width, m.Height) > budget {
			continue
		}
		if m == (Mode{maxW, maxH}) {
			sawMax = true
		}
		out = append(out, m)
	}
	if !sawMax {
		out = append(out, Mode{maxW, maxH})
	}
	return out
}

// Artifact is one rendered file that goes on the provision ISO root.
type Artifact struct {
	Name    string
	Content []byte
}

// Render produces the generated provision-ISO files: autounattend.xml,
// vdd_settings.xml, and provision.ps1.
func Render(p Profile) ([]Artifact, error) {
	names := []string{"autounattend.xml", "vdd_settings.xml", "provision.ps1"}
	out := make([]Artifact, 0, len(names))
	funcs := template.FuncMap{"xml": domain.XMLEscape, "ps": psEscape}
	data := struct {
		Profile
		VDDHardwareID string
		LGService     string
	}{p, VDDHardwareID, LGHostServiceName}
	for _, name := range names {
		tpl, err := template.New(name).Funcs(funcs).ParseFS(templateFS, "templates/"+name)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		out = append(out, Artifact{Name: name, Content: buf.Bytes()})
	}
	return out, nil
}

// psEscape makes s safe inside a PowerShell single-quoted string.
func psEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }
