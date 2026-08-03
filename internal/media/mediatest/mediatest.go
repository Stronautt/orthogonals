// Package mediatest builds the install media fixtures the media validation
// reads. It imports nothing from this module: media's own tests are in package
// media, so any dependency on them would be an import cycle.
package mediatest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"
)

// WimXMLProUkrainian lists Pro and Home with uk-UA as the only display language.
const WimXMLProUkrainian = `<WIM><IMAGE INDEX="1"><NAME>Windows 11 Home</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE>` +
	`<IMAGE INDEX="2"><NAME>Windows 11 Pro</NAME>` +
	`<WINDOWS><LANGUAGES><LANGUAGE>uk-UA</LANGUAGE><DEFAULT>uk-UA</DEFAULT></LANGUAGES></WINDOWS></IMAGE></WIM>`

// ISORoot returns a directory shaped like a mounted Windows ISO, with
// sources/install.wim carrying the given XML info document. An empty document
// leaves the directory bare, which is how a non-Windows ISO is fixtured.
func ISORoot(t testing.TB, wimXML string) string {
	t.Helper()
	dir := t.TempDir()
	if wimXML == "" {
		return dir
	}
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	WriteWIM(t, filepath.Join(dir, "sources", "install.wim"), wimXML)
	return dir
}

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
