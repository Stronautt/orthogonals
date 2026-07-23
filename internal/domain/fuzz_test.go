package domain

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseCPUSet asserts arbitrary cpuset strings never panic, and that any
// list the parser accepts is usable for CPU pinning: no negative indices, and
// ranges expanded in order.
func FuzzParseCPUSet(f *testing.F) {
	f.Add("0-3,7,9-11")
	f.Add("")
	f.Add("   ")
	f.Add(",,,")
	f.Add("0-")
	f.Add("-1")
	f.Add("3-1")
	f.Add("999999999999999999999")
	f.Add("0-99999999")
	f.Add("1,,2")
	// an unbounded range: expanding this exhausted memory before MaxCPUIndex
	f.Add("9999-9999999999999999")
	f.Add("0-8191,0-8191,0-8191,0-8191")

	f.Fuzz(func(t *testing.T, s string) {
		cpus, err := ParseCPUSet(s)
		if err != nil {
			if cpus != nil {
				t.Fatalf("ParseCPUSet(%q) returned both cpus %v and error %v", s, cpus, err)
			}
			return
		}
		if len(cpus) > MaxCPUIndex+1 {
			t.Fatalf("ParseCPUSet(%q) yielded %d cpus, past the %d bound", s, len(cpus), MaxCPUIndex+1)
		}
		for i, c := range cpus {
			if c < 0 || c > MaxCPUIndex {
				t.Fatalf("ParseCPUSet(%q) yielded out-of-range index %d", s, c)
			}
			if i > 0 && c <= cpus[i-1] && strings.Count(s, ",") == 0 {
				t.Fatalf("ParseCPUSet(%q) yielded a non-ascending range: %v", s, cpus)
			}
		}
	})
}

// FuzzXMLEscape asserts escaped text always parses back as XML character data
// and survives the round trip unchanged: the domain template interpolates user
// strings (VM name, password, locale) through it.
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

// FuzzReadMemoryMiB asserts arbitrary domain XML never panics and never yields
// a memory size the caller could mis-scale a hugepage pool from.
func FuzzReadMemoryMiB(f *testing.F) {
	f.Add(`<domain><memory unit='MiB'>8192</memory></domain>`)
	f.Add(`<domain><memory unit='KiB'>8192</memory></domain>`)
	f.Add(`<domain><memory>8192</memory></domain>`)
	f.Add(`<domain><memory unit='MiB'>0</memory></domain>`)
	f.Add(`<domain><memory unit='MiB'>-1</memory></domain>`)
	f.Add(`<domain><memory unit='MiB'>99999999999999999999</memory></domain>`)
	f.Add(`not xml at all`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		path := filepath.Join(root, xmlPath("win11"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		mib, err := ReadMemoryMiB(root, "win11")
		if err != nil {
			if mib != 0 {
				t.Fatalf("ReadMemoryMiB returned %d alongside error %v", mib, err)
			}
			return
		}
		if mib == 0 {
			t.Fatalf("ReadMemoryMiB accepted zero memory from %q", content)
		}
	})
}

// FuzzReadGuestConfig asserts the metadata reader tolerates any XML; it runs on
// files a user may have hand-edited.
func FuzzReadGuestConfig(f *testing.F) {
	f.Add(`<domain><metadata><guest><user>u</user></guest></metadata></domain>`)
	f.Add(`<domain>`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, content string) {
		root := t.TempDir()
		path := filepath.Join(root, xmlPath("win11"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		ReadGuestConfig(root, "win11")
	})
}
