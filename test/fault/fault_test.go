// Package fault proves the apply journal is crash-consistent. Every host
// mutation is journaled before it lands so an interrupted apply can still be
// undone; nothing else in the suite ever interrupts one. These tests kill a
// real apply process and then run a real undo against the wreckage.
package fault

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
)

// stateDir is the one subtree a killed apply is allowed to leave behind: undo
// keeps the manifest and backups whenever it skipped a record.
const stateDir = "var/lib" + string(os.PathSeparator) + "orthogonals"

// applyTools are the binaries apply shells out to under --root. A missing stub
// makes the clean run fail loudly, so drift here surfaces rather than silently
// running the developer's real dracut.
var applyTools = append([]string{"systemctl", "usermod", "bash"}, hw.RequiredTools...)

// orthogonals is the binary under test: the RPM-installed one when
// ORTHOGONALS_BIN is set (under tmt), otherwise one built from this tree.
var orthogonals string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	orthogonals = os.Getenv("ORTHOGONALS_BIN")
	if orthogonals == "" {
		// Built beside this test, not in a temp dir: the binary refuses to run
		// from anywhere under os.TempDir() (cli.executablePath).
		abs, err := filepath.Abs("orthogonals")
		if err != nil {
			panic(err)
		}
		defer func() { _ = os.Remove(abs) }()
		build := exec.Command("go", "build", "-o", abs, ".")
		build.Dir = filepath.Join("..", "..")
		if out, err := build.CombinedOutput(); err != nil {
			panic("build orthogonals: " + err.Error() + "\n" + string(out))
		}
		orthogonals = abs
	}
	return m.Run()
}

// host builds a fresh reference fixture and returns its root plus the PATH the
// binary must run with.
func host(t *testing.T) (root, path string) {
	t.Helper()
	root = t.TempDir()
	if err := hwtest.BuildReferenceRoot(root); err != nil {
		t.Fatal(err)
	}
	// Base dirs every real host already has: undo only removes directories
	// apply itself created, so their absence would look like a leak.
	for _, d := range []string{"etc", "var/lib", "usr/local/bin", "usr/share/applications"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, hwtest.FakeTools(t, applyTools...) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// snap renders the tree without the orthogonals state directory, which a
// killed apply legitimately leaves populated.
func snap(t *testing.T, root string) string {
	t.Helper()
	full, err := stepstest.Snapshot(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	var keep []string
	for line := range strings.SplitSeq(full, "\n") {
		if line != "" && !strings.HasPrefix(line, stateDir) {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// orth runs a subcommand to completion and returns its combined output.
func orth(t *testing.T, path string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(orthogonals, args...)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// applyKilledAfter starts a real apply and SIGKILLs it once it has emitted n
// lines, returning how many it actually got out. Where the kill lands within
// the step is deliberately uncontrolled: undo must cope wherever it died.
func applyKilledAfter(t *testing.T, root, path string, n int) int {
	t.Helper()
	cmd := exec.Command(orthogonals, "apply", "--yes", "--root", root, "--user", "testuser")
	cmd.Env = append(os.Environ(), "PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	seen := 0
	reader := bufio.NewReader(stdout)
	for seen < n {
		if _, err := reader.ReadString('\n'); err != nil {
			break // apply finished, or the pipe closed, before the kill point
		}
		seen++
	}
	_ = cmd.Process.Kill()
	// Drain before Wait: StdoutPipe forbids waiting with reads outstanding.
	_, _ = io.Copy(io.Discard, stdout)
	_ = cmd.Wait()
	return seen
}

// cleanRun counts the lines a complete apply emits, so the kill loop knows how
// many interruption points exist.
func cleanRun(t *testing.T) int {
	t.Helper()
	root, path := host(t)
	out, err := orth(t, path, "apply", "--yes", "--root", root, "--user", "testuser")
	if err != nil {
		t.Fatalf("clean apply failed: %v\n%s", err, out)
	}
	return len(strings.Split(strings.TrimRight(out, "\n"), "\n"))
}

// undoFully runs undo, escalating to --force when the first pass reports it
// skipped records. Both passes are part of the contract: plain undo refuses to
// clobber a file whose content does not match what it journaled, and --force is
// the documented way through that.
func undoFully(t *testing.T, root, path string) (forced bool) {
	t.Helper()
	out, err := orth(t, path, "undo", "--yes", "--root", root)
	if err != nil {
		t.Fatalf("undo failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "kept in the manifest") {
		return false
	}
	if out, err := orth(t, path, "undo", "--force", "--yes", "--root", root); err != nil {
		t.Fatalf("undo --force failed: %v\n%s", err, out)
	}
	return true
}

// TestUndoAfterKillAtEveryLine is the core crash-consistency property: kill a
// real apply at each of its progress points in turn and require undo to put the
// tree back byte for byte every time.
func TestUndoAfterKillAtEveryLine(t *testing.T) {
	lines := cleanRun(t)
	t.Logf("a complete apply emits %d lines", lines)
	var forcedAt []int
	for k := 1; k <= lines; k++ {
		root, path := host(t)
		before := snap(t, root)

		got := applyKilledAfter(t, root, path, k)
		if got < k {
			// apply finished before the kill landed; still a valid state to undo.
			t.Logf("kill point %d: process ended after %d lines", k, got)
		}
		if forced := undoFully(t, root, path); forced {
			forcedAt = append(forcedAt, k)
		}
		if d := stepstest.Diff(before, snap(t, root)); d != "" {
			t.Fatalf("kill after line %d: undo did not restore the tree:\n%s", k, d)
		}
	}
	if len(forcedAt) > 0 {
		// Reachable only when a killed step had already journaled a file it had
		// not yet written, and that file pre-existed.
		t.Logf("undo --force was required at kill points %v", forcedAt)
	}
}

// TestUndoFromConstructedResidue covers the intra-step crash windows a kill
// cannot hit reliably, by building the on-disk wreckage directly.
func TestUndoFromConstructedResidue(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, root string)
	}{
		{
			// Crash between writeBackup and the manifest save: a backup exists
			// that no record claims.
			name: "orphan backup",
			corrupt: func(t *testing.T, root string) {
				dir := filepath.Join(steps.StateDir(root), "backup")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "ghost-step"), []byte("orphan\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Crash between the manifest save and the file write: the journal
			// claims content that never landed.
			name: "journal ahead of file",
			corrupt: func(t *testing.T, root string) {
				path := recordedPath(t, root)
				if err := os.Remove(filepath.Join(root, path)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// os.WriteFile is not atomic, so a crash mid-write truncates.
			name: "truncated file",
			corrupt: func(t *testing.T, root string) {
				path := recordedPath(t, root)
				if err := os.WriteFile(filepath.Join(root, path), []byte("trunc"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, path := host(t)
			before := snap(t, root)
			if out, err := orth(t, path, "apply", "--yes", "--root", root, "--user", "testuser"); err != nil {
				t.Fatalf("apply failed: %v\n%s", err, out)
			}
			tt.corrupt(t, root)
			undoFully(t, root, path)
			if d := stepstest.Diff(before, snap(t, root)); d != "" {
				t.Fatalf("undo did not restore the tree:\n%s", d)
			}
		})
	}
}

// recordedPath returns the path of the first write_file record in the manifest.
func recordedPath(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(steps.ManifestPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Records []struct {
			Kind string `json:"kind"`
			Path string `json:"path"`
		} `json:"records"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, r := range m.Records {
		if r.Kind == "write_file" {
			return r.Path
		}
	}
	t.Fatal("manifest has no write_file record")
	return ""
}
