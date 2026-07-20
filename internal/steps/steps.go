// Package steps is the apply engine: every host mutation is a journaled Step,
// recorded in a manifest with the original bytes backed up, so undo can
// restore the host byte-identically. All root-privilege changes anywhere in
// orthogonals route through this package.
package steps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Kind selects the step behavior.
type Kind string

const (
	KindWriteFile  Kind = "write_file"
	KindRunCmd     Kind = "run_cmd"
	KindEnableUnit Kind = "enable_unit"
)

// Step is one journaled mutation. Only the fields for its Kind are set.
type Step struct {
	ID     string
	Kind   Kind
	Data   bool // undoing destroys user data (disk image, cache); plain undo keeps these
	Reboot bool // touches boot config (kargs, initramfs); apply and undo report "reboot required"

	// KindWriteFile
	Path       string // absolute path, un-rooted (--root is prepended)
	Content    []byte
	Mode       fs.FileMode
	Restorecon bool

	// KindRunCmd
	Cmd     []string
	UndoCmd []string // paired inverse or regeneration command; empty = nothing to undo
	// CreatesPath is the command's product (un-rooted, like Cmd). When set, a
	// journaled step whose product was externally deleted re-runs instead of
	// skipping — the command must be idempotent. Empty keeps skip-on-record.
	CreatesPath string
	// Input is the content the command consumes (e.g. the XML virsh define
	// reads). When set, a journaled input-hash mismatch re-runs the command
	// instead of skipping — the command must be idempotent.
	Input []byte

	// KindEnableUnit
	Unit   string
	Enable bool // false disables the unit
}

// Engine applies and undoes steps under Root. Dry-run unless Yes.
type Engine struct {
	Root string
	Yes  bool
	Out  io.Writer
	Err  io.Writer
}

// Apply runs steps in order. Each step is journaled into the manifest before
// its mutation lands, so a partial failure is always undoable.
func (e *Engine) Apply(list []Step) error {
	seen := make(map[string]bool, len(list))
	backups := make(map[string]string, len(list))
	for _, s := range list {
		if seen[s.ID] {
			return fmt.Errorf("duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if prev, ok := backups[backupName(s.ID)]; ok {
			return fmt.Errorf("step ids %q and %q map to the same backup file", prev, s.ID)
		}
		backups[backupName(s.ID)] = s.ID
	}
	m, err := Load(e.Root)
	if err != nil {
		return err
	}
	if e.Yes {
		// persisted by the first step that saves; a no-op re-run keeps the
		// stamps from when the mutations actually landed
		m.stamp(e.Root)
	}
	for _, s := range list {
		if err := e.applyOne(m, s); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
	}
	return nil
}

func (e *Engine) applyOne(m *Manifest, s Step) error {
	if s.ID == "" {
		return errors.New("step has no id")
	}
	switch s.Kind {
	case KindWriteFile:
		if !filepath.IsAbs(s.Path) {
			return fmt.Errorf("write_file needs an absolute path, got %q", s.Path)
		}
		if s.Mode == 0 {
			return errors.New("write_file needs a file mode")
		}
		return e.applyWriteFile(m, s)
	case KindRunCmd:
		if len(s.Cmd) == 0 {
			return errors.New("run_cmd needs a command")
		}
		return e.applyRunCmd(m, s)
	case KindEnableUnit:
		if s.Unit == "" {
			return errors.New("enable_unit needs a unit name")
		}
		return e.applyEnableUnit(m, s)
	}
	return fmt.Errorf("unknown step kind %q", s.Kind)
}

func (e *Engine) applyWriteFile(m *Manifest, s Step) error {
	full := filepath.Join(e.Root, s.Path)
	cur, err := os.ReadFile(full)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var curMode fs.FileMode
	if exists {
		st, err := os.Stat(full)
		if err != nil {
			return err
		}
		curMode = st.Mode().Perm()
	}
	rec := m.find(s.ID)
	if rec != nil && rec.Path != s.Path {
		// the journaled backup belongs to the old path; silently rebinding the
		// record would corrupt undo (restore old bytes over the new path)
		return fmt.Errorf("journaled at %s but now targets %s — settings changed since apply; undo first (orthogonals undo, or vm undefine for VM steps)",
			rec.Path, s.Path)
	}
	same := exists && bytes.Equal(cur, s.Content) && curMode == s.Mode.Perm()
	// "unchanged" only when the journal matches too: a file hand-edited to the
	// new content leaves NewSHA256 stale, and undo would refuse to restore it
	if same && rec != nil && rec.NewSHA256 == sha256hex(s.Content) {
		fmt.Fprintf(e.Out, "%s: unchanged\n", s.Path)
		return nil
	}
	if !e.Yes {
		if same {
			fmt.Fprintf(e.Out, "%s: already as desired (apply journals it)\n", s.Path)
		} else {
			fmt.Fprint(e.Out, renderDiff(s.Path, exists, cur, curMode, s.Content, s.Mode.Perm()))
		}
		if s.Restorecon {
			fmt.Fprintf(e.Out, "would run: restorecon %s\n", s.Path)
		}
		return nil
	}
	if rec == nil {
		r := Record{
			ID: s.ID, Kind: KindWriteFile, Data: s.Data, Reboot: s.Reboot,
			Path: s.Path, Restorecon: s.Restorecon, Existed: exists,
			MadeDirs: missingDirs(e.Root, filepath.Dir(full)),
		}
		if exists {
			r.Backup = backupName(s.ID)
			// two step IDs can sanitize to the same backup filename;
			// overwriting would destroy the journaled step's original bytes
			// and corrupt its undo. A crashed pre-save attempt of this same
			// step left no record, so its orphaned file is safe to overwrite.
			for i := range m.Records {
				if m.Records[i].Backup == r.Backup {
					return fmt.Errorf("step %s would overwrite the backup of journaled step %s (both map to backup/%s) — undo %s first",
						s.ID, m.Records[i].ID, r.Backup, m.Records[i].ID)
				}
			}
			r.OrigMode = uint32(curMode)
			if err := writeBackup(e.Root, r.Backup, cur); err != nil {
				return err
			}
		}
		m.put(r)
		rec = m.find(s.ID)
	}
	rec.Mode = uint32(s.Mode.Perm())
	rec.NewSHA256 = sha256hex(s.Content)
	// journal before the write lands so the original is never lost
	if err := m.save(e.Root); err != nil {
		return err
	}
	if err := e.writeFile(full, s.Content, s.Mode.Perm(), s.Restorecon); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "wrote %s\n", s.Path)
	return nil
}

// writeFile lands content at full, creating parent dirs. os.WriteFile only
// sets the mode on create, so Chmod re-asserts it on existing files.
func (e *Engine) writeFile(full string, content []byte, mode fs.FileMode, restorecon bool) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(full, mode); err != nil {
		return err
	}
	if restorecon {
		return RunCmd(e.Out, "restorecon", full)
	}
	return nil
}

func (e *Engine) applyRunCmd(m *Manifest, s Step) error {
	inputDrift := false
	if rec := m.find(s.ID); rec != nil {
		if !slices.Equal(rec.Cmd, s.Cmd) {
			// e.g. --binding or --disk changed since apply: skipping would
			// silently keep the old setup, re-running would stack onto it
			return fmt.Errorf("journaled command differs from the current settings — undo first (orthogonals undo, or vm undefine for VM steps)\nwas: %s\nnow: %s",
				strings.Join(rec.Cmd, " "), strings.Join(s.Cmd, " "))
		}
		switch {
		case len(s.Input) > 0 && rec.InputSHA256 != sha256hex(s.Input):
			// a pre-Input record (empty hash) also lands here: one extra
			// idempotent re-run converges it
			inputDrift = true
		case s.CreatesPath != "":
			// Stat, not Lstat: a dangling symlink product counts as gone too
			if _, err := os.Stat(s.CreatesPath); err == nil {
				fmt.Fprintf(e.Out, "%s: already applied\n", s.ID)
				return nil
			}
			fmt.Fprintf(e.Out, "%s: %s is gone — reapplying\n", s.ID, s.CreatesPath)
		default:
			fmt.Fprintf(e.Out, "%s: already applied\n", s.ID)
			return nil
		}
	}
	if !e.Yes {
		if inputDrift {
			fmt.Fprintf(e.Out, "would run: %s (journaled input changed)\n", strings.Join(s.Cmd, " "))
		} else {
			fmt.Fprintf(e.Out, "would run: %s\n", strings.Join(s.Cmd, " "))
		}
		return nil
	}
	if err := RunCmd(e.Out, s.Cmd...); err != nil {
		return err
	}
	r := Record{ID: s.ID, Kind: KindRunCmd, Data: s.Data, Reboot: s.Reboot, Cmd: s.Cmd, UndoCmd: s.UndoCmd}
	if len(s.Input) > 0 {
		r.InputSHA256 = sha256hex(s.Input)
	}
	m.put(r)
	return m.save(e.Root)
}

func (e *Engine) applyEnableUnit(m *Manifest, s Step) error {
	verb := "enable"
	if !s.Enable {
		verb = "disable"
	}
	if rec := m.find(s.ID); rec != nil {
		if rec.Unit != s.Unit || rec.Enable != s.Enable {
			// same invariant as write_file/run_cmd: a journaled step diverging
			// from current settings is refused (undo first), never silently
			// skipped — otherwise undo would restore the wrong unit's prior state
			return fmt.Errorf("journaled unit/state differs from the current settings — undo first (orthogonals undo)\nwas: %s enable=%v\nnow: %s enable=%v",
				rec.Unit, rec.Enable, s.Unit, s.Enable)
		}
		// a journaled unit can drift: NVIDIA driver updates preset-enable
		// nvidia-persistenced behind our back. Re-assert the desired state;
		// the journal keeps the original PriorState so undo still restores
		// what the host had before orthogonals touched it.
		cur := unitState(s.Unit)
		drifted := (s.Enable && cur == "disabled") || (!s.Enable && cur == "enabled")
		if !drifted {
			fmt.Fprintf(e.Out, "%s: already applied\n", s.ID)
			return nil
		}
		if !e.Yes {
			fmt.Fprintf(e.Out, "would run: systemctl %s %s (unit drifted to %s)\n", verb, s.Unit, cur)
			return nil
		}
		return RunCmd(e.Out, "systemctl", verb, s.Unit)
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would run: systemctl %s %s\n", verb, s.Unit)
		return nil
	}
	prior := unitState(s.Unit)
	// disabling a unit the host never installed (e.g. nvidia-persistenced
	// without xorg-x11-drv-nvidia-power) is a no-op, not a failure
	if !s.Enable && prior == "unknown" {
		fmt.Fprintf(e.Out, "%s: unit not installed, nothing to disable\n", s.Unit)
		return nil
	}
	if err := RunCmd(e.Out, "systemctl", verb, s.Unit); err != nil {
		return err
	}
	m.put(Record{ID: s.ID, Kind: KindEnableUnit, Data: s.Data, Reboot: s.Reboot, Unit: s.Unit, Enable: s.Enable, PriorState: prior})
	return m.save(e.Root)
}

// RunCmd echoes argv to out and runs it, folding combined output into the
// error — the one command-running idiom every package shares.
func RunCmd(out io.Writer, argv ...string) error {
	fmt.Fprintf(out, "run: %s\n", strings.Join(argv, " "))
	b, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, bytes.TrimSpace(b))
	}
	return nil
}

// UnitEnabled is the read-only unit-enablement fact: wants-symlinks under
// root, falling back to systemctl on the live host (vendor-preset symlinks
// live under /usr/lib, not /etc). Distinct from unitState, which needs the
// tri-state is-enabled answer at mutation time.
func UnitEnabled(root, unit string) bool {
	wants, _ := filepath.Glob(filepath.Join(root, "/etc/systemd/system/*.wants/", unit))
	if len(wants) > 0 {
		return true
	}
	if root == "" {
		return exec.Command("systemctl", "is-enabled", "--quiet", unit).Run() == nil
	}
	return false
}

// unitState asks systemctl for the unit's enablement. The exit status is
// ignored on purpose: is-enabled reports "disabled" with a nonzero exit.
func unitState(unit string) string {
	out, _ := exec.Command("systemctl", "is-enabled", unit).Output()
	state, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if state == "" {
		return "unknown"
	}
	return state
}

// missingDirs lists the not-yet-existing ancestors of dir, deepest first and
// un-rooted, so undo can remove directories that apply created.
func missingDirs(root, dir string) []string {
	var made []string
	for d := dir; d != root && d != "/" && d != "."; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		}
		made = append(made, strings.TrimPrefix(d, root))
	}
	return made
}
