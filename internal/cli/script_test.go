package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
)

// fixtureBuilders are the synthetic host topologies a script can materialize.
// Same registry test/fixture wraps: the trees live in internal/hw/hwtest only.
var fixtureBuilders = hwtest.Roots

// TestScript runs the CLI contract scripts in testdata/script.
func TestScript(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: filepath.Join("testdata", "script"),
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"fixture":   cmdFixture,
			"faketools": cmdFakeTools,
			"wantexit":  cmdWantExit,
			"snapshot":  cmdSnapshot,
		},
	})
}

// cmdWantExit asserts an exact exit status, which the builtin exec cannot
// express: wantexit <code> <cmd> [args...]. Captured output stays available
// to the stdout/stderr assertions that follow.
func cmdWantExit(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("wantexit does not support negation")
	}
	if len(args) < 2 {
		ts.Fatalf("usage: wantexit <code> <cmd> [args...]")
	}
	want, err := strconv.Atoi(args[0])
	if err != nil {
		ts.Fatalf("wantexit: bad exit code %q", args[0])
	}
	got := 0
	if err := ts.Exec(args[1], args[2:]...); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			ts.Fatalf("wantexit: %v", err)
		}
		got = ee.ExitCode()
	}
	if got != want {
		ts.Fatalf("%s exited %d, want %d", strings.Join(args[1:], " "), got, want)
	}
}

// cmdFixture builds a synthetic host tree: fixture <dir> [kind].
func cmdFixture(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("fixture does not support negation")
	}
	if len(args) < 1 || len(args) > 2 {
		ts.Fatalf("usage: fixture <dir> [reference|laptop|laptop-amd]")
	}
	kind := "reference"
	if len(args) == 2 {
		kind = args[1]
	}
	build := fixtureBuilders[kind]
	if build == nil {
		ts.Fatalf("unknown fixture kind %q", kind)
	}
	root := ts.MkAbs(args[0])
	if err := build(root); err != nil {
		ts.Fatalf("build %s fixture: %v", kind, err)
	}
	// Base dirs every real host already has: undo only removes directories
	// apply itself created, so their absence would mask a leak.
	for _, d := range []string{"etc", "var/lib", "usr/local/bin", "usr/share/applications"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			ts.Fatalf("%v", err)
		}
	}
}

// cmdFakeTools puts argv-logging stubs for every exec'd vendor tool on PATH,
// plus any extra names given: faketools [name...].
func cmdFakeTools(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("faketools does not support negation")
	}
	dir := ts.MkAbs("fakebin")
	if err := writeFakeTools(dir, append(append([]string{}, applyFakeBins...), args...)); err != nil {
		ts.Fatalf("%v", err)
	}
	ts.Setenv("PATH", dir+string(os.PathListSeparator)+ts.Getenv("PATH"))
}

// cmdSnapshot records a tree's shape, modes, and content hashes so two points
// in a script can be compared with the builtin cmp: snapshot <dir> <outfile>.
func cmdSnapshot(ts *testscript.TestScript, neg bool, args []string) {
	if neg {
		ts.Fatalf("snapshot does not support negation")
	}
	if len(args) != 2 {
		ts.Fatalf("usage: snapshot <dir> <outfile>")
	}
	state, err := stepstest.Snapshot(ts.MkAbs(args[0]))
	if err != nil {
		ts.Fatalf("snapshot %s: %v", args[0], err)
	}
	if err := os.WriteFile(ts.MkAbs(args[1]), []byte(state), 0o644); err != nil {
		ts.Fatalf("%v", err)
	}
}

// writeFakeTools installs an argv-logging stub per name in dir; each stub
// appends its arguments to <name>.log beside it.
func writeFakeTools(dir string, names []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		script := "#!/bin/sh\necho \"$*\" >> \"" + filepath.Join(dir, name+".log") + "\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}
