// Package media builds the guest installation media.
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
	// Edition is the only Windows edition v1 installs.
	Edition = "Windows 11 Pro"

	// DefaultGuestUser is the default guest account name.
	DefaultGuestUser = "user"

	// DefaultGuestPassword is the default guest account password.
	DefaultGuestPassword = "password"

	defaultLocale = "en-US"
)

// Guest-side display contract: VDD hardware ID and Looking Glass host service name.
const (
	VDDHardwareID     = `ROOT\MttVDD`
	LGHostServiceName = "Looking Glass (host)"
)

// forbiddenUserChars are the characters Windows refuses in account names.
const forbiddenUserChars = `"/\[]:;|=,+*?<>@`

// Mode is one display mode the VDD monitor advertises.
type Mode struct {
	Width, Height int
}

// standardModes is the resolution ladder offered to the guest.
var standardModes = []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}, {3840, 2160}}

// Profile is everything the rendered media varies on.
type Profile struct {
	GuestUser     string
	GuestPassword string
	Locale        string
	Width, Height int
	Modes         []Mode
	Edition       string
}

// NewProfile validates the guest options.
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

// guestModes returns the standard modes that fit the max's IVSHMEM region, plus the max itself.
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

// templates holds the embedded provision templates, parsed once.
var templates = template.Must(template.New("media").
	Funcs(template.FuncMap{"xml": domain.XMLEscape, "ps": psEscape}).
	ParseFS(templateFS, "templates/*"))

// Render produces the generated provision-ISO files.
func Render(p Profile) ([]Artifact, error) {
	names := []string{"autounattend.xml", "vdd_settings.xml", "provision.ps1"}
	out := make([]Artifact, 0, len(names))
	data := struct {
		Profile
		VDDHardwareID string
		LGService     string
	}{p, VDDHardwareID, LGHostServiceName}
	for _, name := range names {
		var buf bytes.Buffer
		if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", name, err)
		}
		out = append(out, Artifact{Name: name, Content: buf.Bytes()})
	}
	return out, nil
}

// psEscape makes s safe inside a PowerShell single-quoted string.
func psEscape(s string) string { return strings.ReplaceAll(s, "'", "''") }
