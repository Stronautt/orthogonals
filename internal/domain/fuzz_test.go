package domain

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stronautt/orthogonals/internal/steps"
)

// fuzzDomainXML makes the registry directory once, for the whole run: a
// t.TempDir() per execution costs more I/O than the xml.Unmarshal under test,
// enough on a loaded runner to time the run out.
func fuzzDomainXML(f *testing.F) (root, path string) {
	f.Helper()
	root = f.TempDir()
	path = filepath.Join(root, xmlPath("win11"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.Fatal(err)
	}
	return root, path
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
			// The number becomes a static_size_mb argv token; the module takes an int.
			t.Fatalf("kvmfr buffer of %d MiB does not fit static_size_mb", size)
		}
	})
}

// FuzzReadSettings asserts the metadata reader tolerates any XML; it runs on
// files a user may have hand-edited.
func FuzzReadSettings(f *testing.F) {
	f.Add(`<domain><metadata><settings><ram-gib>12</ram-gib></settings></metadata></domain>`)
	f.Add(`<domain><metadata><settings><ram-gib>nonsense</ram-gib></settings></metadata></domain>`)
	f.Add(`<domain>`)
	f.Add(``)

	root, path := fuzzDomainXML(f)
	f.Fuzz(func(t *testing.T, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		// Never a half-filled record beside an error: converging on one resets
		// every knob past the break.
		if s, err := ReadSettings(root, "win11"); err != nil && !reflect.DeepEqual(s, Settings{}) {
			t.Fatalf("error %v returned with a partial record %+v", err, s)
		}
	})
}
