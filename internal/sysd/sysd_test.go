package sysd

import (
	"bytes"
	"testing"
)

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
