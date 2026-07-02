package orchestrate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMachine wires a Machine whose stages record their call order; failAt
// makes the named stage fail.
func fakeMachine(t *testing.T, root string, failAt string) (*Machine, *[]string, *bytes.Buffer) {
	t.Helper()
	var calls []string
	stage := func(name string) func() error {
		return func() error {
			calls = append(calls, name)
			if name == failAt {
				return errors.New("boom")
			}
			return nil
		}
	}
	var out bytes.Buffer
	m := &Machine{Root: root, Out: &out, Stages: Stages{
		Apply:      stage("apply"),
		VerifyBoot: stage("verify-boot"),
		DefineVM:   stage("define-vm"),
		BuildMedia: stage("build-media"),
		Install:    stage("install"),
		Verify:     stage("verify"),
	}}
	return m, &calls, &out
}

func TestRunFreshToVerified(t *testing.T) {
	root := t.TempDir()
	m, calls, out := fakeMachine(t, root, "")
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	want := []string{"apply", "verify-boot", "define-vm", "build-media", "install", "verify"}
	if got := strings.Join(*calls, ","); got != strings.Join(want, ",") {
		t.Errorf("stage order = %s, want %s", got, strings.Join(want, ","))
	}
	if st, _ := LoadState(root); st != StateVerified {
		t.Errorf("final state = %s, want verified", st)
	}
	if !strings.Contains(out.String(), "setup complete") {
		t.Errorf("missing completion message:\n%s", out.String())
	}
}

func TestRunStopsAtRebootBoundary(t *testing.T) {
	root := t.TempDir()
	m, calls, out := fakeMachine(t, root, "verify-boot")
	if err := m.Run(); err != nil {
		t.Fatalf("reboot boundary must stop cleanly, got %v", err)
	}
	if got := strings.Join(*calls, ","); got != "apply,verify-boot" {
		t.Errorf("calls = %s, want apply,verify-boot", got)
	}
	if st, _ := LoadState(root); st != StateHostApplied {
		t.Errorf("state = %s, want host-applied", st)
	}
	for _, want := range []string{"reboot now", "orthogonals up"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("boundary message missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunResumesAfterReboot(t *testing.T) {
	root := t.TempDir()
	if err := SaveState(root, StateHostApplied); err != nil {
		t.Fatal(err)
	}
	m, calls, _ := fakeMachine(t, root, "")
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	if (*calls)[0] != "verify-boot" {
		t.Errorf("resume must not re-run apply, first call = %s", (*calls)[0])
	}
	if st, _ := LoadState(root); st != StateVerified {
		t.Errorf("final state = %s, want verified", st)
	}
}

func TestRunResumeFailedBootVerificationIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := SaveState(root, StateHostApplied); err != nil {
		t.Fatal(err)
	}
	m, _, _ := fakeMachine(t, root, "verify-boot")
	err := m.Run()
	if err == nil {
		t.Fatal("post-reboot verification failure on resume must be an error")
	}
	for _, want := range []string{"post-reboot verification", "orthogonals bundle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// Every stage failure names the stage and points at the diagnostics bundle,
// and the persisted state resumes at the failed stage.
func TestRunStageFailuresAreActionable(t *testing.T) {
	cases := []struct {
		failAt, wantStage string
		wantState         State
	}{
		{"apply", "host configuration", StateFresh},
		{"define-vm", "VM definition", StateRebooted},
		{"build-media", "guest media build", StateVMDefined},
		{"install", "Windows install/provisioning", StateInstalling},
		{"verify", "verification", StateProvisioned},
	}
	for _, tc := range cases {
		t.Run(tc.failAt, func(t *testing.T) {
			root := t.TempDir()
			m, _, _ := fakeMachine(t, root, tc.failAt)
			err := m.Run()
			if err == nil {
				t.Fatal("want an error")
			}
			for _, want := range []string{tc.wantStage, "orthogonals bundle"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing %q: %v", want, err)
				}
			}
			if st, _ := LoadState(root); st != tc.wantState {
				t.Errorf("state = %s, want %s", st, tc.wantState)
			}
			// resume retries the failed stage, not the whole pipeline
			m2, calls, _ := fakeMachine(t, root, "")
			if err := m2.Run(); err != nil {
				t.Fatal(err)
			}
			first := map[string]string{
				"apply": "apply", "define-vm": "define-vm", "build-media": "build-media",
				"install": "install", "verify": "verify",
			}[tc.failAt]
			if (*calls)[0] != first {
				t.Errorf("resume first call = %s, want %s", (*calls)[0], first)
			}
		})
	}
}

func TestRunAlreadyVerified(t *testing.T) {
	root := t.TempDir()
	if err := SaveState(root, StateVerified); err != nil {
		t.Fatal(err)
	}
	m, calls, out := fakeMachine(t, root, "")
	if err := m.Run(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Errorf("verified pipeline ran stages: %v", *calls)
	}
	if !strings.Contains(out.String(), "setup complete") {
		t.Errorf("missing completion message:\n%s", out.String())
	}
}

func TestLoadStateUnknown(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "var/lib/orthogonals")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"state":"warp-drive"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(root); err == nil || !strings.Contains(err.Error(), "warp-drive") {
		t.Errorf("want unknown-state error, got %v", err)
	}
}

// TestSavedVMNameSurvivesStateWrites is the reboot-resume guarantee: the name
// up records at first run must survive the SaveState calls the machine makes
// as it advances, so a resume that omits --vm-name recovers it.
func TestSavedVMNameSurvivesStateWrites(t *testing.T) {
	root := t.TempDir()
	if err := SaveVMName(root, "myvm"); err != nil {
		t.Fatal(err)
	}
	// a name-only record (written before the first stage) reads as fresh
	if st, _ := LoadState(root); st != StateFresh {
		t.Errorf("state = %q, want fresh before any stage", st)
	}
	if err := SaveState(root, StateHostApplied); err != nil {
		t.Fatal(err)
	}
	if got, _ := SavedVMName(root); got != "myvm" {
		t.Errorf("SavedVMName = %q, want myvm (SaveState clobbered the name)", got)
	}
	if st, _ := LoadState(root); st != StateHostApplied {
		t.Errorf("state = %q, want host-applied", st)
	}
}

func TestSavedVMNameAbsent(t *testing.T) {
	got, err := SavedVMName(t.TempDir())
	if err != nil || got != "" {
		t.Errorf("SavedVMName on fresh host = %q, %v, want empty, nil", got, err)
	}
}

func TestRemaining(t *testing.T) {
	fresh := Remaining(StateFresh)
	if len(fresh) != 6 {
		t.Errorf("fresh remaining = %d stages, want 6: %v", len(fresh), fresh)
	}
	if got := Remaining(StateInstalling); len(got) != 2 || !strings.Contains(got[0], "install") {
		t.Errorf("installing remaining = %v", got)
	}
	if got := Remaining(StateVerified); len(got) != 0 {
		t.Errorf("verified remaining = %v, want none", got)
	}
}
