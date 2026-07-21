// Package mediatest lets other packages' tests run the media validation without root.
package mediatest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/stronautt/orthogonals/internal/media"
)

// WimXMLProUkrainian lists Windows 11 Pro (and Home) with uk-UA as the only display language.
const WimXMLProUkrainian = `<WIM><IMAGE INDEX="1"><NAME>Windows 11 Home</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE>` +
	`<IMAGE INDEX="2"><NAME>Windows 11 Pro</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE></WIM>`

// InstallFixture points media.MountISO at a fixture directory whose sources/install.wim carries the given XML info document.
func InstallFixture(t testing.TB, wimXML string) {
	t.Helper()
	old := media.MountISO
	media.MountISO = func(string) (string, func(), error) {
		dir := t.TempDir()
		if wimXML != "" {
			if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
				t.Fatal(err)
			}
			WriteWIM(t, filepath.Join(dir, "sources", "install.wim"), wimXML)
		}
		return dir, func() {}, nil
	}
	t.Cleanup(func() { media.MountISO = old })
}

// WriteWIM hand-builds a minimal WIM with the given XML info document.
func WriteWIM(t testing.TB, path, xmlBody string) {
	t.Helper()
	u := utf16.Encode([]rune(xmlBody))
	payload := make([]byte, 2+len(u)*2)
	binary.LittleEndian.PutUint16(payload, 0xfeff)
	for i, r := range u {
		binary.LittleEndian.PutUint16(payload[2+i*2:], r)
	}
	hdr := make([]byte, 208)
	copy(hdr, "MSWIM\x00\x00\x00")
	binary.LittleEndian.PutUint32(hdr[8:], 208)
	binary.LittleEndian.PutUint64(hdr[72:], uint64(len(payload)))
	binary.LittleEndian.PutUint64(hdr[80:], 208)
	binary.LittleEndian.PutUint64(hdr[88:], uint64(len(payload)))
	if err := os.WriteFile(path, append(hdr, payload...), 0o644); err != nil {
		t.Fatal(err)
	}
}
