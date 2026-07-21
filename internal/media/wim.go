package media

import (
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"os"
	"unicode/utf16"
)

// The WIM header layout: the XML data resource descriptor sits at a fixed offset, stored uncompressed UTF-16LE.
const (
	wimMagic        = "MSWIM\x00\x00\x00"
	wimHeaderSize   = 96
	wimXMLSizeOff   = 72
	wimXMLOffsetOff = 80
	wimXMLSizeLimit = 64 << 20
)

// wimXML mirrors the <WIM> info document.
type wimXML struct {
	Images []struct {
		Name    string `xml:"NAME"`
		Windows struct {
			Languages struct {
				Language []string `xml:"LANGUAGE"`
				Default  string   `xml:"DEFAULT"`
			} `xml:"LANGUAGES"`
		} `xml:"WINDOWS"`
	} `xml:"IMAGE"`
}

// parseWIM reads the image names and languages out of install.wim/.esd.
func parseWIM(path string) (wimXML, error) {
	var w wimXML
	f, err := os.Open(path)
	if err != nil {
		return w, err
	}
	defer func() { _ = f.Close() }()
	hdr := make([]byte, wimHeaderSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return w, fmt.Errorf("%s: read WIM header: %w", path, err)
	}
	if string(hdr[:len(wimMagic)]) != wimMagic {
		return w, fmt.Errorf("%s is not a WIM image", path)
	}
	size := binary.LittleEndian.Uint64(hdr[wimXMLSizeOff:]) & 0x00ff_ffff_ffff_ffff
	offset := binary.LittleEndian.Uint64(hdr[wimXMLOffsetOff:])
	if size == 0 || size > wimXMLSizeLimit {
		return w, fmt.Errorf("%s: implausible WIM XML data size %d", path, size)
	}
	raw := make([]byte, size)
	if _, err := f.ReadAt(raw, int64(offset)); err != nil {
		return w, fmt.Errorf("%s: read WIM XML data: %w", path, err)
	}
	if err := xml.Unmarshal(decodeUTF16LE(raw), &w); err != nil {
		return w, fmt.Errorf("%s: parse WIM XML data: %w", path, err)
	}
	return w, nil
}

func decodeUTF16LE(b []byte) []byte {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:]))
	}
	if len(u) > 0 && u[0] == 0xfeff {
		u = u[1:]
	}
	return []byte(string(utf16.Decode(u)))
}
