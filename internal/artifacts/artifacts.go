// Package artifacts is the single place to bump pinned external download inputs.
package artifacts

import (
	_ "embed"
	"strings"
)

// Download is one pinned external artifact `orthogonals media` fetches into the cache.
type Download struct {
	Name    string
	Version string
	URL     string
	SHA256  string
	File    string
}

//go:embed looking-glass.version
var lgVersionRaw string

//go:embed looking-glass.sha256
var lgHostSHA256Raw string

// LookingGlassVersion is the committed Looking Glass release lockfile.
var LookingGlassVersion = strings.TrimSpace(lgVersionRaw)

var (
	// VirtioWin is the virtio-win ISO attached to the VM as its own CD.
	VirtioWin = Download{
		Name:    "virtio-win ISO",
		Version: "0.1.285-1",
		URL:     "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.285-1/virtio-win-0.1.285.iso",
		SHA256:  "e14cf2b94492c3e925f0070ba7fdfedeb2048c91eea9c5a5afb30232a3976331",
		File:    "virtio-win.iso",
	}
	// NVIDIADriver is the pinned known-good Windows driver.
	NVIDIADriver = Download{
		Name:    "NVIDIA Windows driver",
		Version: "580.88",
		URL:     "https://us.download.nvidia.com/Windows/580.88/580.88-desktop-win10-win11-64bit-international-dch-whql.exe",
		SHA256:  "90c49f925b41ee062e02cc8094a16b6d057abfde4e85235a76d63ffe267f63a9",
		File:    "nvidia-driver.exe",
	}
	// LookingGlassHost is the Looking Glass host installer; it must match the client release.
	LookingGlassHost = Download{
		Name:    "Looking Glass host",
		Version: LookingGlassVersion,
		URL:     "https://looking-glass.io/artifact/" + LookingGlassVersion + "/host",
		SHA256:  strings.TrimSpace(lgHostSHA256Raw),
		File:    "looking-glass-host.zip",
	}
	// VDD is the signed Virtual Display Driver the passed-through GPU renders to.
	VDD = Download{
		Name:    "Virtual Display Driver",
		Version: "24.12.24",
		URL:     "https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/24.12.24/Signed-Driver-v24.12.24-x64.zip",
		SHA256:  "f93e7ce3d640c83c419b1b72d2c83b2d8e34d83e8b7ba11f3328a40d2da82fa4",
		File:    "vdd-driver.zip",
	}
	// Nefcon installs the VDD inf without a reboot.
	Nefcon = Download{
		Name:    "nefcon",
		Version: "v1.17.40",
		URL:     "https://github.com/nefarius/nefcon/releases/download/v1.17.40/nefcon_v1.17.40.zip",
		SHA256:  "812bae7ed7dfb7d6d2284bc7de2f8ccebc92ed2a0b1ae893c53b337096e50c1a",
		File:    "nefcon.zip",
	}
	// Win11Debloat strips consumer bloat, telemetry, and ads from the guest.
	Win11Debloat = Download{
		Name:    "Win11Debloat",
		Version: "2026.07.11",
		URL:     "https://github.com/Raphire/Win11Debloat/archive/refs/tags/2026.07.11.zip",
		SHA256:  "e97c8e36698c7b543da0b77cc34439c1a0b4917525b45a9d1ae7a02e23d4711d",
		File:    "win11debloat.zip",
	}
)

// Downloads is everything media fetches into the cache.
func Downloads() []Download {
	return []Download{VirtioWin, NVIDIADriver, LookingGlassHost, VDD, Nefcon, Win11Debloat}
}

// OnProvisionISO reports whether a download belongs on the provision ISO.
func OnProvisionISO(d Download) bool { return d.Name != VirtioWin.Name }

// ProvisionPayloads is the subset of Downloads that lands on the provision ISO.
func ProvisionPayloads() []Download {
	var out []Download
	for _, d := range Downloads() {
		if OnProvisionISO(d) {
			out = append(out, d)
		}
	}
	return out
}
