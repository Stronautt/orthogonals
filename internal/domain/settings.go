package domain

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// settingsNS is the metadata namespace libvirt requires on its children.
const settingsNS = "https://github.com/stronautt/orthogonals"

// Settings are the per-VM knobs a `vm define` registers. The whole record
// round-trips through the domain metadata and merges field-blind in Over, so a
// knob is sticky by adding a field here, and nothing reads or writes these one
// at a time.
type Settings struct {
	// DisplayName and User are the desktop shortcut's name and owner: hostcfg
	// renders them, nothing in the domain XML uses them.
	DisplayName string `xml:"display-name,omitempty"`
	User        string `xml:"user,omitempty"`
	RAMGiB      int    `xml:"ram-gib,omitempty"`
	Disk        string `xml:"disk,omitempty"`
	DiskSizeGiB int    `xml:"disk-size-gib,omitempty"`
	Resolution  string `xml:"resolution,omitempty"`
	// secret:"true" is what the diagnostics bundle strips; a credential added
	// without it ships in the clear.
	GuestUser     string `xml:"guest-user,omitempty" secret:"true"`
	GuestPassword string `xml:"guest-password,omitempty" secret:"true"`
	Locale        string `xml:"locale,omitempty"`
	Win11ISO      string `xml:"win11-iso,omitempty"`
	// GPUROM is the installed vBIOS copy, never the source --gpu-rom named:
	// re-reading the source on every converge would break the moment it moves.
	GPUROM string `xml:"gpu-rom,omitempty"`
	// Shares are the registered virtiofs directories in drive-letter order;
	// NewShares derives tags and letters from that order, they are not stored.
	Shares []string `xml:"share,omitempty"`
}

// SecretElements are the settings elements whose text is a credential, named
// from the struct tags — the diagnostics bundle redacts by this list, so a
// rename here reaches it.
func SecretElements() []string {
	var out []string
	for _, f := range reflect.VisibleFields(reflect.TypeOf(Settings{})) {
		if f.Tag.Get("secret") == "true" {
			name, _, _ := strings.Cut(f.Tag.Get("xml"), ",")
			out = append(out, name)
		}
	}
	return out
}

// Over returns s with every zero-valued field taken from prev. A zero value can
// only mean "keep", so a knob that has to be switchable off needs a sentinel the
// way --share "" does.
func (s Settings) Over(prev Settings) Settings {
	out := s
	v, p := reflect.ValueOf(&out).Elem(), reflect.ValueOf(prev)
	for i := range v.NumField() {
		// Setting an unexported field panics; encoding/xml drops them anyway.
		if !v.Field(i).CanSet() || !v.Field(i).IsZero() {
			continue
		}
		f := p.Field(i)
		// Set copies a slice header, so out and prev would share the array and
		// an in-place edit would rewrite what was read off disk.
		if f.Kind() == reflect.Slice && !f.IsNil() {
			c := reflect.MakeSlice(f.Type(), f.Len(), f.Len())
			reflect.Copy(c, f)
			f = c
		}
		v.Field(i).Set(f)
	}
	return out
}

// ReadSettings loads the registered settings from the VM's registry XML under
// root. An undefined VM reads empty with no error; a registry that exists but
// will not parse is an error, since Unmarshal fills up to the break and a
// half-read record converges the domain to defaults for everything past it.
func ReadSettings(root, name string) (Settings, error) {
	var doc struct {
		Settings Settings `xml:"metadata>settings"`
	}
	path := filepath.Join(root, xmlPath(name))
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if err := xml.Unmarshal(b, &doc); err != nil {
		return Settings{}, fmt.Errorf("read registered settings from %s: %w", path, err)
	}
	return doc.Settings, nil
}

// SettingsXML renders the metadata block, marshalled from the struct rather
// than listed in the template, so the two cannot drift.
func (p Profile) SettingsXML() (string, error) {
	var doc struct {
		XMLName xml.Name `xml:"settings"`
		NS      string   `xml:"xmlns,attr"`
		Settings
	}
	doc.NS, doc.Settings = settingsNS, p.Settings
	b, err := xml.MarshalIndent(doc, "    ", "  ")
	if err != nil {
		return "", fmt.Errorf("render domain settings: %w", err)
	}
	return string(b), nil
}

// ParseResolution parses the "1920x1080" form Settings stores, defaulting an
// empty one.
func ParseResolution(s string) (int, int, error) {
	if s == "" {
		return DefaultWidth, DefaultHeight, nil
	}
	ws, hs, ok := strings.Cut(s, "x")
	w, errW := strconv.Atoi(ws)
	h, errH := strconv.Atoi(hs)
	if !ok || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad resolution %q (want WxH, e.g. 1920x1080)", s)
	}
	return CheckResolution(w, h)
}

// CheckResolution bounds an already-parsed resolution: past the ceiling
// IVSHMEMMiB's doubling loop overflows uint64 and never terminates.
func CheckResolution(w, h int) (int, int, error) {
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad resolution %dx%d", w, h)
	}
	if w > MaxDimension || h > MaxDimension {
		return 0, 0, fmt.Errorf("resolution %dx%d exceeds the %d-pixel per-axis maximum", w, h, MaxDimension)
	}
	return w, h, nil
}
