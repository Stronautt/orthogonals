// Package testsupport holds the test helpers that belong to no single package.
//
// Everything else in this module keeps its helpers beside their subject
// (hwtest, virttest, sysdtest, mediatest, stepstest). These have no subject:
// the golden harness is shared by every package that renders an artifact, and
// Swap by every package with a package-level func var seam. It imports nothing
// from this module, so it can never close a cycle.
package testsupport

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/diff"
)

// One flag for the whole module. Four packages used to declare their own and
// they drifted: one wrote its golden and then compared against the bytes it
// had just written, and one reported a missing golden without naming -update.
var update = flag.Bool("update", false, "rewrite golden files")

func goldenPath(name string) string { return filepath.Join("testdata", "golden", name) }

// Golden compares got against testdata/golden/<name>, or rewrites it under
// -update.
func Golden(t testing.TB, name string, got []byte) { GoldenAs(t, name, name, got) }

// GoldenAs is Golden where the golden's file name is not what a reader needs
// to be told. hostcfg stores /etc/dracut.conf.d/vfio.conf under its basename,
// but a failure has to name the path that would be written to the host.
func GoldenAs(t testing.TB, name, label string, got []byte) {
	t.Helper()
	path := goldenPath(name)
	if *update {
		writeGolden(t, path, got)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run make goldens)", err)
	}
	// diff.Diff opens with bytes.Equal and returns nil when they match, so this
	// is the byte-exact comparison as well as the message.
	if d := diff.Diff(path, want, label, got); d != nil {
		t.Errorf("%s differs:\n%s", label, d)
	}
}

// GoldenDelta goldens got as its departure from base, for a case that is a
// variation on a baseline the caller goldens in full. It reports whether got
// was indistinguishable from base, in which case no file is stored.
//
// A change to what the baseline and the case share then moves the baseline
// golden alone, instead of rewriting the same hunk into every sibling — which
// is the only reason to store a departure rather than the whole artifact.
func GoldenDelta(t testing.TB, name string, base, got []byte) bool {
	t.Helper()
	path := goldenPath(name)
	// Against the freshly rendered baseline, never the stored one: a targeted
	// -run must not record a departure from a baseline it did not recompute.
	d := delta(base, got)
	if *update {
		if d == nil {
			removeGolden(t, path)
		} else {
			writeGolden(t, path, d)
		}
		return d == nil
	}
	want, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("%v (run make goldens)", err)
	}
	if !bytes.Equal(want, d) {
		t.Errorf("%s departs from the baseline differently than %s records:\n%s",
			name, path, diff.Diff(path, want, "current", d))
	}
	return d == nil
}

// delta reduces a unified diff to the lines that differ, dropping the banner,
// the @@ headers and the context. What is left is a function of content alone.
//
// The line numbers are the whole point of dropping them: with them, a field
// added to every report renumbers every hunk and churns 9 of 10 stored
// departures with noise carrying no information — which would defeat the
// reason to store departures at all. Failure messages are rendered with full
// context separately, so nothing is lost at the moment a reader needs it.
func delta(base, got []byte) []byte {
	d := diff.Diff("baseline", base, "case", got)
	if d == nil {
		return nil
	}
	// Everything up to the first hunk header is the three-line banner. Past it
	// every line opens with ' ', '+', '-' or '@', so the marker is unambiguous
	// even when the content itself begins with a dash.
	if i := bytes.Index(d, []byte("\n@@")); i >= 0 {
		d = d[i+1:]
	}
	var out bytes.Buffer
	for line := range bytes.SplitSeq(d, []byte("\n")) {
		if len(line) > 0 && (line[0] == '+' || line[0] == '-') {
			out.Write(line)
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func writeGolden(t testing.TB, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeGolden(t testing.TB, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}
