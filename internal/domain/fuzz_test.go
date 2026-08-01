package domain

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stronautt/orthogonals/internal/steps"
)

// fuzzDomainXML makes the registry directory once, for the whole run: a
// t.TempDir() per execution costs more I/O than the xml.Unmarshal under test,
// enough on a loaded runner to stall every worker and time the run out.
func fuzzDomainXML(f *testing.F) (root, path string) {
	f.Helper()
	root = f.TempDir()
	path = filepath.Join(root, xmlPath("win11"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	return root, path
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

	root, path := fuzzDomainXML(f)
	f.Fuzz(func(t *testing.T, content string) {
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

// FuzzKVMFRSizeMiB asserts arbitrary domain XML never yields a buffer size the
// hook would hand modprobe. qemu maps the declared bytes regardless, so a
// module sized below them leaves the guest writing past the end of the buffer.
func FuzzKVMFRSizeMiB(f *testing.F) {
	arg := func(size string) string {
		return `<domain><qemu:commandline><qemu:arg value='{"qom-type":"memory-backend-file",` +
			`"mem-path":"` + steps.KVMFRDevice + `","size":` + size + `}'/></qemu:commandline></domain>`
	}
	f.Add(arg("134217728"))
	f.Add(arg("0"))
	f.Add(arg("1"))
	// MaxUint64 rounded up by mib-1 wraps back to near zero.
	f.Add(arg("18446744073709551615"))
	f.Add(arg("99999999999999999999999"))
	f.Add(`<domain><shmem name='looking-glass'><size unit='M'>128</size></shmem></domain>`)
	f.Add(`not xml at all`)
	f.Add(``)

	root, path := fuzzDomainXML(f)
	f.Fuzz(func(t *testing.T, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		size, ok := KVMFRSizeMiB(root, "win11")
		switch {
		case !ok && size != 0:
			t.Fatalf("no kvmfr backend reported alongside %d MiB", size)
		case ok && size == 0:
			t.Fatalf("kvmfr backend reported a zero-MiB buffer from %q", content)
		case ok && size > math.MaxInt32:
			// The number becomes a static_size_mb argv token and the module takes
			// an int; EnsureKVMFR refuses it, but it must not get that far.
			t.Fatalf("kvmfr buffer of %d MiB does not fit static_size_mb", size)
		}
	})
}

// FuzzReadGuestConfig asserts the metadata reader tolerates any XML; it runs on
// files a user may have hand-edited.
func FuzzReadGuestConfig(f *testing.F) {
	f.Add(`<domain><metadata><guest><user>u</user></guest></metadata></domain>`)
	f.Add(`<domain>`)
	f.Add(``)

	root, path := fuzzDomainXML(f)
	f.Fuzz(func(t *testing.T, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		ReadGuestConfig(root, "win11")
	})
}
