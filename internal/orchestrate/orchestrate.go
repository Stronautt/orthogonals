// Package orchestrate drives the end-to-end `orthogonals up` pipeline as a
// state machine persisted in /var/lib/orthogonals/state.json, and implements
// the health checks behind `orthogonals status` and the end-to-end
// `orthogonals verify` sequence.
package orchestrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/steps"
)

// Banner sets operator instructions apart from step logs — the lines the
// user must act on drown in "already applied" noise otherwise.
func Banner(w io.Writer, lines ...string) {
	rule := strings.Repeat("═", 72)
	fmt.Fprintln(w)
	fmt.Fprintln(w, rule)
	for _, l := range lines {
		fmt.Fprintln(w, "  "+l)
	}
	fmt.Fprintln(w, rule)
}

// State is the persisted position in the up pipeline.
type State string

const (
	StateFresh       State = "fresh"
	StateHostApplied State = "host-applied"
	StateRebooted    State = "rebooted"
	StateVMDefined   State = "vm-defined"
	StateMediaBuilt  State = "media-built"
	StateInstalling  State = "installing"
	StateProvisioned State = "provisioned"
	StateVerified    State = "verified"
)

// stateOrder is the pipeline sequence; Before relies on it.
var stateOrder = []State{
	StateFresh, StateHostApplied, StateRebooted, StateVMDefined,
	StateMediaBuilt, StateInstalling, StateProvisioned, StateVerified,
}

// Before reports whether s comes earlier in the pipeline than t.
func (s State) Before(t State) bool {
	return slices.Index(stateOrder, s) < slices.Index(stateOrder, t)
}

// persisted is the on-disk state.json: the pipeline position plus the domain
// name resolved at first run, so a reboot-resume that omits --vm-name still
// targets the applied hooks (the registry only records the name after `vm
// define`, which runs *after* the reboot boundary).
type persisted struct {
	State State  `json:"state"`
	Name  string `json:"name,omitempty"`
}

func loadPersisted(root string) (persisted, error) {
	b, err := os.ReadFile(steps.StatePath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return persisted{}, nil
	}
	if err != nil {
		return persisted{}, err
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return persisted{}, fmt.Errorf("parse %s: %w", steps.StatePath(root), err)
	}
	return p, nil
}

func writePersisted(root string, p persisted) error {
	if err := os.MkdirAll(steps.StateDir(root), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	// temp+rename so a crash mid-write (the reboot boundary this state
	// machine is built to survive) never leaves a half-written state.json.
	// No fsync before the rename: on power loss some filesystems can still
	// surface an empty file, which LoadState reports loudly — an acceptable
	// failure for a file that just marks pipeline position
	tmp := steps.StatePath(root) + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, steps.StatePath(root))
}

// LoadState reads the persisted pipeline state; a missing file (or a
// name-only record written before the first stage) is a fresh host.
func LoadState(root string) (State, error) {
	p, err := loadPersisted(root)
	if err != nil {
		return "", err
	}
	if p.State == "" {
		return StateFresh, nil
	}
	if !slices.Contains(stateOrder, p.State) {
		return "", fmt.Errorf("%s: unknown state %q (delete the file to start the pipeline over)", steps.StatePath(root), p.State)
	}
	return p.State, nil
}

// SavedVMName returns the domain name up persisted at first run, or "" if
// none — so a reboot-resume that omits --vm-name recovers the applied name
// instead of silently falling back to the default. The error distinguishes a
// corrupt state.json from an absent one.
func SavedVMName(root string) (string, error) {
	p, err := loadPersisted(root)
	return p.Name, err
}

// SaveState persists the pipeline position, preserving the saved VM name.
func SaveState(root string, st State) error {
	p, err := loadPersisted(root)
	if err != nil {
		return err
	}
	p.State = st
	return writePersisted(root, p)
}

// SaveVMName records the domain name for reboot-resume, preserving the
// pipeline position.
func SaveVMName(root, name string) error {
	p, err := loadPersisted(root)
	if err != nil {
		return err
	}
	p.Name = name
	return writePersisted(root, p)
}

// Stages are the pipeline stage implementations, injected so the state
// machine is testable with fake step results.
type Stages struct {
	Apply      func() error // host configuration via the apply engine
	VerifyBoot func() error // post-reboot checks: IOMMU, kargs, vfio module (research §C5)
	DefineVM   func() error
	BuildMedia func() error
	Install    func() error // Windows install + provisioning to completion
	Verify     func() error
}

// Machine runs the up pipeline from wherever the persisted state left off.
type Machine struct {
	Root       string
	Out        io.Writer
	Stages     Stages
	LaunchHint string // completion banner suffix naming the VM's launcher
}

// Run advances the pipeline, persisting state after every transition. It
// stops cleanly (nil) at the reboot boundary — the next run resumes — and
// wraps every failure with a pointer at `orthogonals bundle` so the failure
// message is actionable.
func (m *Machine) Run() error {
	st, err := LoadState(m.Root)
	if err != nil {
		return err
	}
	appliedNow := false
	for {
		switch st {
		case StateFresh:
			fmt.Fprintln(m.Out, "up: applying host configuration")
			if err := m.Stages.Apply(); err != nil {
				return stageErr("host configuration", err)
			}
			appliedNow = true
			st = StateHostApplied
		case StateHostApplied:
			if err := m.Stages.VerifyBoot(); err != nil {
				if appliedNow {
					// half a transaction until the rebooted kernel proves the
					// boot config took (research §C5)
					Banner(m.Out,
						"host configured — reboot now",
						"continue after reboot by re-running the SAME `orthogonals up --yes`",
						"command, with the same flags (--win11-iso, --disk, --locale, …) —",
						"only --vm-name is remembered across the reboot")
					return nil
				}
				// a resumed run before the reboot lands here too — say so
				// instead of presenting the expected state as a failure
				return stageErr("post-reboot verification",
					fmt.Errorf("%w\nif the host has not been rebooted since apply, reboot and re-run `orthogonals up --yes`", err))
			}
			st = StateRebooted
		case StateRebooted:
			fmt.Fprintln(m.Out, "up: defining the VM")
			if err := m.Stages.DefineVM(); err != nil {
				return stageErr("VM definition", err)
			}
			st = StateVMDefined
		case StateVMDefined:
			fmt.Fprintln(m.Out, "up: building guest media")
			if err := m.Stages.BuildMedia(); err != nil {
				return stageErr("guest media build", err)
			}
			st = StateMediaBuilt
		case StateMediaBuilt:
			// persisted before install starts so an interrupted install
			// resumes here, not at the media build
			st = StateInstalling
		case StateInstalling:
			fmt.Fprintln(m.Out, "up: installing and provisioning Windows")
			if err := m.Stages.Install(); err != nil {
				return stageErr("Windows install/provisioning", err)
			}
			st = StateProvisioned
		case StateProvisioned:
			fmt.Fprintln(m.Out, "up: verifying end to end")
			if err := m.Stages.Verify(); err != nil {
				return stageErr("verification", err)
			}
			st = StateVerified
		case StateVerified:
			fmt.Fprintln(m.Out, "up: setup complete — "+m.LaunchHint)
			return nil
		}
		if err := SaveState(m.Root, st); err != nil {
			return err
		}
	}
}

// stageErr keeps every failure path actionable (plan Task 11): name the
// stage and point at the diagnostics bundle.
func stageErr(stage string, err error) error {
	return fmt.Errorf("%s failed: %w\ncollect diagnostics with: orthogonals bundle orthogonals-diagnostics.tar.gz", stage, err)
}

// Remaining lists the stage descriptions Run would still execute from st,
// for dry-run output. StateMediaBuilt is only a resume point inside the
// install stage, so it carries no line of its own.
func Remaining(st State) []string {
	stages := []struct {
		from State
		desc string
	}{
		{StateFresh, "apply host configuration (reboot required when boot config changes)"},
		{StateHostApplied, "verify boot configuration (IOMMU, kernel args, vfio module)"},
		{StateRebooted, "define the VM"},
		{StateVMDefined, "build guest media (autounattend + provision ISO)"},
		{StateInstalling, "install and provision Windows"},
		{StateProvisioned, "verify end to end"},
	}
	var out []string
	for _, s := range stages {
		if !s.from.Before(st) {
			out = append(out, s.desc)
		}
	}
	return out
}
