// Package artifacts is the single place to bump pinned external inputs: the
// host package set and the download pins (URL, version, SHA256) for
// virtio-win, the NVIDIA Windows driver, the Looking Glass host, VDD, and
// nefcon. To bump a pin: download the new URL, sha256sum it, update here.
package artifacts

// Download is one pinned external artifact `orthogonals media` fetches into
// /var/lib/orthogonals/cache on the user's machine (legal constraint: nothing
// proprietary is ever bundled or redistributed). A SHA256 mismatch is a hard
// fail everywhere.
type Download struct {
	Name    string // human-readable label
	Version string
	URL     string
	SHA256  string // hex digest of the file at URL
	File    string // stable filename in the cache and on the provision ISO
}

var (
	// VirtioWin is attached to the VM as its own CD: viostor for Windows
	// Setup, the guest-tools bundle (drivers + QEMU-GA + SPICE vdagent) for
	// provisioning. Immutable archive URL, not the stable-virtio symlink
	// (which moves and would break the pin).
	VirtioWin = Download{
		Name:    "virtio-win ISO",
		Version: "0.1.285-1",
		URL:     "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.285-1/virtio-win-0.1.285.iso",
		SHA256:  "e14cf2b94492c3e925f0070ba7fdfedeb2048c91eea9c5a5afb30232a3976331",
		File:    "virtio-win.iso",
	}
	// NVIDIADriver is the pinned known-good Windows driver (research §B3):
	// the URL is constructible from the version, fetched from NVIDIA on the
	// user's machine at media-build time. When the pin does not cover the
	// GPU (or was pulled), `media --nvidia-installer` takes a user-supplied
	// installer instead.
	NVIDIADriver = Download{
		Name:    "NVIDIA Windows driver",
		Version: "580.88",
		URL:     "https://us.download.nvidia.com/Windows/580.88/580.88-desktop-win10-win11-64bit-international-dch-whql.exe",
		SHA256:  "90c49f925b41ee062e02cc8094a16b6d057abfde4e85235a76d63ffe267f63a9",
		File:    "nvidia-driver.exe",
	}
	// LookingGlassHost is the official B7 host zip, which wraps
	// looking-glass-host-setup.exe (silent /S install bundles the IVSHMEM
	// driver since B6). Version must match the client built in apply.
	LookingGlassHost = Download{
		Name:    "Looking Glass host",
		Version: "B7",
		URL:     "https://looking-glass.io/artifact/B7/host",
		SHA256:  "c2415a5a0c405f1d6aa936986bdd4b806c50574b4521747e113c3be2be047b1b",
		File:    "looking-glass-host.zip",
	}
	// LookingGlassSource is the client source tarball `orthogonals apply`
	// builds on the host (lg-build.sh). Version must match LookingGlassHost —
	// LG requires client and guest host to be the same release. Fetched by
	// the build script (curl + sha256sum), not by media.Downloads.
	LookingGlassSource = Download{
		Name:    "Looking Glass source",
		Version: "B7",
		URL:     "https://looking-glass.io/artifact/B7/source",
		SHA256:  "09e506660ccc1b9691d06caa70179b52ffb4393299895cff3c2f0e74fcd69985",
		File:    "looking-glass-B7.tar.gz",
	}
	// VDD is the signed Virtual Display Driver (driver-only zip) — the
	// "monitor" the passed-through GPU renders to; installed via nefcon.
	VDD = Download{
		Name:    "Virtual Display Driver",
		Version: "24.12.24",
		URL:     "https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/24.12.24/Signed-Driver-v24.12.24-x64.zip",
		SHA256:  "f93e7ce3d640c83c419b1b72d2c83b2d8e34d83e8b7ba11f3328a40d2da82fa4",
		File:    "vdd-driver.zip",
	}
	// Nefcon installs the VDD inf without a reboot (nefconc.exe).
	Nefcon = Download{
		Name:    "nefcon",
		Version: "v1.17.40",
		URL:     "https://github.com/nefarius/nefcon/releases/download/v1.17.40/nefcon_v1.17.40.zip",
		SHA256:  "812bae7ed7dfb7d6d2284bc7de2f8ccebc92ed2a0b1ae893c53b337096e50c1a",
		File:    "nefcon.zip",
	}
	// Win11Debloat strips the consumer bloat, telemetry and ads from the guest.
	// The release's only asset is a Get.ps1 that downloads the script at run
	// time, so the tag archive is pinned instead and provisioning runs it from
	// the ISO — the guest never fetches anything itself.
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
// virtio-win is the sole exception — it rides along as its own CD instead.
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

// Packages is what `orthogonals apply` installs via dnf. dnf install -y is
// idempotent, so re-runs double as verification; undo leaves packages
// installed (documented no-op).
var Packages = []string{
	// virtualization stack
	"qemu-kvm", "libvirt", "virt-install", "edk2-ovmf", "swtpm-tools",
	// guest media build + host configuration tools (wimlib-utils: wiminfo
	// validates the Win11 ISO's edition list before install)
	"xorriso", "wimlib-utils", "policycoreutils-python-utils", "psmisc", "lsof",
	// dGPU launch UX: GNOME's "Launch using Discrete Graphics Card"
	"switcheroo-control",
	// Looking Glass client build dependencies (upstream's Fedora list)
	"cmake", "make", "gcc", "gcc-c++", "pkgconf-pkg-config", "binutils-devel",
	"libglvnd-devel", "fontconfig-devel", "spice-protocol", "nettle-devel",
	"libXi-devel", "libXinerama-devel", "libXcursor-devel", "libXpresent-devel",
	"libxkbcommon-x11-devel", "wayland-devel", "wayland-protocols-devel",
	"libXScrnSaver-devel", "libXrandr-devel", "dejavu-sans-mono-fonts",
	"libdecor-devel", "pipewire-devel", "libsamplerate-devel",
	"pulseaudio-libs-devel", // B7 cmake hard-requires libpulse even on PipeWire hosts
}
