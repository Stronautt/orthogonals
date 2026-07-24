package virt

import (
	"errors"
	"testing"
)

// TestXMLEscape: CreateVolumeQCow2 interpolates the --disk path into pool and
// volume XML by hand, so this is the only thing between a path and a malformed
// (or attacker-shaped) document reaching libvirt.
func TestXMLEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary path", "/var/lib/libvirt/images/win11.qcow2", "/var/lib/libvirt/images/win11.qcow2"},
		{"ampersand", "a&b", "a&amp;b"},
		{"angle brackets", "<pool>", "&lt;pool&gt;"},
		{"apostrophe", "it's", "it&apos;s"},
		{"double quote", `say "hi"`, "say &quot;hi&quot;"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := xmlEscape(tt.in); got != tt.want {
				t.Errorf("xmlEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSpiceDisplay(t *testing.T) {
	cases := []struct {
		name          string
		xml           string
		host, port    string
		wantNoDisplay bool
	}{
		{
			name: "child listen element",
			xml:  `<domain><devices><graphics type='spice' port='5901' autoport='yes'><listen type='address' address='127.0.0.1'/></graphics></devices></domain>`,
			host: "127.0.0.1", port: "5901",
		},
		{
			name: "listen attr only",
			xml:  `<domain><devices><graphics type='spice' port='5902' listen='0.0.0.0'/></devices></domain>`,
			host: "0.0.0.0", port: "5902",
		},
		{
			name: "no listen defaults to loopback",
			xml:  `<domain><devices><graphics type='spice' port='5903'/></devices></domain>`,
			host: "127.0.0.1", port: "5903",
		},
		{
			name:          "unallocated port",
			xml:           `<domain><devices><graphics type='spice' port='-1' autoport='yes'/></devices></domain>`,
			wantNoDisplay: true,
		},
		{
			name:          "no spice graphics",
			xml:           `<domain><devices><graphics type='vnc' port='5904'/></devices></domain>`,
			wantNoDisplay: true,
		},
		{
			// Port 0 is how the client is told the host argument is a path.
			name: "socket listen",
			xml:  `<domain><devices><graphics type='spice'><listen type='socket' socket='/run/orthogonals/win11/spice.sock'/></graphics></devices></domain>`,
			host: "/run/orthogonals/win11/spice.sock", port: "0",
		},
		{
			// libvirt fills the path in when the domain starts; before that
			// there is nothing to connect to, and launch must keep polling.
			name:          "socket listen with no path yet",
			xml:           `<domain><devices><graphics type='spice'><listen type='socket'/></graphics></devices></domain>`,
			wantNoDisplay: true,
		},
		{
			// A port left over from a definition made before the socket switch
			// must not win over the socket the running domain actually has.
			name: "socket listen alongside a stale port",
			xml:  `<domain><devices><graphics type='spice' port='5901'><listen type='socket' socket='/run/orthogonals/win11/spice.sock'/></graphics></devices></domain>`,
			host: "/run/orthogonals/win11/spice.sock", port: "0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := parseSpiceDisplay(tc.xml)
			if tc.wantNoDisplay {
				if !errors.Is(err, ErrNoDisplay) {
					t.Fatalf("err = %v, want ErrNoDisplay", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if host != tc.host || port != tc.port {
				t.Errorf("got %s:%s, want %s:%s", host, port, tc.host, tc.port)
			}
		})
	}
}
