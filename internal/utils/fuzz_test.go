package utils

import (
	"encoding/xml"
	"strings"
	"testing"
)

// FuzzXMLEscape asserts escaped text always parses back as XML character data:
// the domain and media templates interpolate user strings through it.
func FuzzXMLEscape(f *testing.F) {
	f.Add("plain")
	f.Add("<script>alert(1)</script>")
	f.Add("a & b")
	f.Add("]]>")
	f.Add("\x00\x01")
	f.Add("emoji 🙂 and ünïcode")

	f.Fuzz(func(t *testing.T, s string) {
		escaped := XMLEscape(s)
		doc := "<e>" + escaped + "</e>"
		var out struct {
			Value string `xml:",chardata"`
		}
		if err := xml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("XMLEscape(%q) produced unparsable XML %q: %v", s, doc, err)
		}
		if strings.ContainsAny(escaped, "<>") {
			t.Fatalf("XMLEscape(%q) left a markup character: %q", s, escaped)
		}
	})
}
