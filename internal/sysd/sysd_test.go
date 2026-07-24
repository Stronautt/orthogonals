package sysd

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"testing"

	godbus "github.com/godbus/dbus/v5"
)

// TestIsNoSuchUnit pins what gets swallowed. StopUnit and ResetFailedUnit
// return nil when this says true, so a false positive makes the qemu hook
// report that it stopped the sleep inhibitor when it did not.
func TestIsNoSuchUnit(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"typed dbus error", godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}, true},
		{"wrapped typed error", fmt.Errorf("stop unit: %w", godbus.Error{Name: "org.freedesktop.systemd1.NoSuchUnit"}), true},
		{"a different dbus error", godbus.Error{Name: "org.freedesktop.systemd1.JobTypeNotApplicable"}, false},
		{"not-loaded text", errors.New("Unit ghost.service not loaded."), true},
		{"NoSuchUnit text", errors.New("NoSuchUnit"), true},
		{"unrelated failure", errors.New("connection refused"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNoSuchUnit(tt.err); got != tt.want {
				t.Errorf("isNoSuchUnit(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsClosedConn decides whether a dropped private connection is redialled.
// A false negative surfaces a transport hiccup as a failed mutation.
func TestIsClosedConn(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"godbus closed", godbus.ErrClosed, true},
		{"net closed", net.ErrClosed, true},
		{"wrapped net closed", fmt.Errorf("enable unit: %w", net.ErrClosed), true},
		{"closed-connection text", errors.New("use of closed network connection"), true},
		{"closed-by-user text", errors.New("connection closed by user"), true},
		{"unrelated failure", errors.New("permission denied"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClosedConn(tt.err); got != tt.want {
				t.Errorf("isClosedConn(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAllowedCPUsMask(t *testing.T) {
	cases := []struct {
		name string
		cpus []int
		want []byte
	}{
		{"two low cores", []int{0, 1}, []byte{0x03}},
		{"reserved host plus e-cores", []int{0, 1, 12, 13, 14, 15, 16, 17, 18, 19}, []byte{0x03, 0xf0, 0x0f}},
		{"single high core needs a second byte", []int{8}, []byte{0x00, 0x01}},
		{"order does not matter", []int{1, 0}, []byte{0x03}},
		{"empty", nil, []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allowedCPUsMask(tc.cpus); !bytes.Equal(got, tc.want) {
				t.Errorf("allowedCPUsMask(%v) = %#v, want %#v", tc.cpus, got, tc.want)
			}
		})
	}
}
