// Package media builds the guest installation media.
package media

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"unicode"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/utils"
)

//go:embed templates
var templateFS embed.FS

const (
	Edition = "Windows 11 Pro"

	DefaultGuestUser = "user"

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

var standardModes = []Mode{{1920, 1080}, {2560, 1440}, {3440, 1440}, {3840, 2160}}

// Profile is everything the rendered media varies on.
type Profile struct {
	GuestUser     string
	GuestPassword string
	Locale        string
	Width, Height int
	Modes         []Mode
	Edition       string
	// Shares get a guest mount service each. Baked into the media, so a share
	// added after the install does not reach the guest.
	Shares []domain.Share
}

func NewProfile(user, password, locale string, width, height int, shares []domain.Share) (Profile, error) {
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
	width, height, err := domain.CheckResolution(width, height)
	if err != nil {
		return Profile{}, err
	}
	return Profile{
		GuestUser: user, GuestPassword: password, Locale: locale,
		Width: width, Height: height, Modes: guestModes(width, height),
		Edition: Edition, Shares: shares,
	}, nil
}

// guestModes drops the standard modes that would not fit the max's IVSHMEM region.
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

var templates = template.Must(template.New("media").
	Funcs(template.FuncMap{"xml": utils.XMLEscape, "ps": utils.PowerShellEscape}).
	ParseFS(templateFS, "templates/*"))

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
		if err := checkASCII(name, buf.Bytes()); err != nil {
			return nil, err
		}
		out = append(out, Artifact{Name: name, Content: buf.Bytes()})
	}
	return out, nil
}

// errNotASCII is the refusal of a provisioning script that is not ASCII. The
// script ships without a byte order mark, and Windows PowerShell 5.1 reads a
// .ps1 without one as the ANSI codepage. One byte above ASCII mis-parses the
// whole provisioning run.
var errNotASCII = errors.New("the guest reads the script as the ANSI codepage and cannot parse it")

// checkASCII holds the rendered script to ASCII, and the rendered text is what
// it reads. A golden test holds the template to ASCII. But --guest-user and the
// tag of a share reach the same file. This function is the one point that every
// field passes through.
func checkASCII(name string, content []byte) error {
	if !strings.HasSuffix(name, ".ps1") {
		return nil
	}
	i := bytes.IndexFunc(content, func(r rune) bool { return r > unicode.MaxASCII })
	if i < 0 {
		return nil
	}
	return fmt.Errorf("%s is not ASCII at byte %d (%q): %w",
		name, i, content[i:min(i+20, len(content))], errNotASCII)
}
