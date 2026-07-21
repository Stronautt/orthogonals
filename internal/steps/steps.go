// Package steps is the apply engine: every host mutation is a journaled Step.
package steps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/virt"
)

// Kind selects the step behavior.
type Kind string

const (
	KindWriteFile  Kind = "write_file"
	KindRunCmd     Kind = "run_cmd"
	KindEnableUnit Kind = "enable_unit"
	KindOp         Kind = "op"
)

// Step is one journaled mutation.
type Step struct {
	ID     string
	Kind   Kind
	Data   bool
	Reboot bool

	// KindWriteFile
	Path       string
	Content    []byte
	Mode       fs.FileMode
	Restorecon bool

	// KindRunCmd
	Cmd     []string
	UndoCmd []string
	// CreatesPath is the command's product; when gone, a journaled step re-runs.
	CreatesPath string
	// Input is the content the command or op consumes; a hash mismatch re-runs the step.
	Input []byte

	// KindEnableUnit
	Unit   string
	Enable bool

	// KindOp
	Op       string
	Args     map[string]string
	UndoOp   string
	UndoArgs map[string]string
}

// Engine applies and undoes steps under Root. Dry-run unless Yes.
type Engine struct {
	Root string
	Yes  bool
	Out  io.Writer
	Err  io.Writer
	// Virt supplies the libvirt client; nil dials the local hypervisor.
	Virt func() virt.Client
	// Sysd supplies the systemd client; nil dials the local manager.
	Sysd func() sysd.Client
}

func (e *Engine) newOpClients() *OpClients {
	oc := &OpClients{virt: e.Virt, sysd: e.Sysd, injected: e.Virt != nil || e.Sysd != nil}
	if oc.virt == nil {
		oc.virt = virt.New
	}
	if oc.sysd == nil {
		oc.sysd = sysd.New
	}
	return oc
}

// skipUnderRoot reports a dialing step under --root with no injected clients.
func (e *Engine) skipUnderRoot(oc *OpClients) bool { return e.Root != "" && !oc.injected }

// Apply runs steps in order, journaling each before its mutation lands.
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
		m.stamp(e.Root)
	}
	oc := e.newOpClients()
	defer oc.close()
	for _, s := range list {
		if err := e.applyOne(m, s, oc); err != nil {
			return fmt.Errorf("step %s: %w", s.ID, err)
		}
	}
	return nil
}

func (e *Engine) applyOne(m *Manifest, s Step, oc *OpClients) error {
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
		return e.applyEnableUnit(m, s, oc)
	case KindOp:
		if s.Op == "" {
			return errors.New("op step needs an op name")
		}
		return e.applyOp(m, s, oc)
	}
	return fmt.Errorf("unknown step kind %q", s.Kind)
}

func (e *Engine) applyOp(m *Manifest, s Step, oc *OpClients) error {
	entry, ok := ops[s.Op]
	if !ok {
		return fmt.Errorf("unknown op %q", s.Op)
	}
	if s.UndoOp != "" {
		if _, ok := ops[s.UndoOp]; !ok {
			return fmt.Errorf("unknown undo op %q", s.UndoOp)
		}
	}
	inputDrift := false
	if rec := m.find(s.ID); rec != nil {
		if rec.Op != s.Op || !maps.Equal(rec.OpArgs, s.Args) {
			return fmt.Errorf("journaled op differs from the current settings — undo first (orthogonals undo, or vm undefine for VM steps)\nwas: %s\nnow: %s",
				opLine(rec.Op, rec.OpArgs), opLine(s.Op, s.Args))
		}
		if len(s.Input) > 0 && rec.InputSHA256 != sha256hex(s.Input) {
			inputDrift = true
		} else {
			fmt.Fprintf(e.Out, "%s: already applied\n", s.ID)
			return nil
		}
	}
	if !e.Yes {
		if inputDrift {
			fmt.Fprintf(e.Out, "would: %s (journaled input changed)\n", opLine(s.Op, s.Args))
		} else {
			fmt.Fprintf(e.Out, "would: %s\n", opLine(s.Op, s.Args))
		}
		return nil
	}
	if entry.dials && e.skipUnderRoot(oc) {
		fmt.Fprintf(e.Out, "%s: skipped under --root (needs live libvirt/systemd — covered by test-vm)\n", s.ID)
	} else if err := entry.fn(oc, e.Root, e.Out, s.Args); err != nil {
		return err
	}
	r := Record{ID: s.ID, Kind: KindOp, Data: s.Data, Reboot: s.Reboot,
		Op: s.Op, OpArgs: s.Args, UndoOp: s.UndoOp, UndoArgs: s.UndoArgs}
	if len(s.Input) > 0 {
		r.InputSHA256 = sha256hex(s.Input)
	}
	m.put(r)
	return m.save(e.Root)
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
		return fmt.Errorf("journaled at %s but now targets %s — settings changed since apply; undo first (orthogonals undo, or vm undefine for VM steps)",
			rec.Path, s.Path)
	}
	same := exists && bytes.Equal(cur, s.Content) && curMode == s.Mode.Perm()
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
	if err := m.save(e.Root); err != nil {
		return err
	}
	if err := e.writeFile(full, s.Content, s.Mode.Perm(), s.Restorecon); err != nil {
		return err
	}
	fmt.Fprintf(e.Out, "wrote %s\n", s.Path)
	return nil
}

// writeFile lands content at full, creating parent dirs.
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
		return runCmd(e.Out, "restorecon", full)
	}
	return nil
}

func (e *Engine) applyRunCmd(m *Manifest, s Step) error {
	inputDrift := false
	if rec := m.find(s.ID); rec != nil {
		if !slices.Equal(rec.Cmd, s.Cmd) {
			return fmt.Errorf("journaled command differs from the current settings — undo first (orthogonals undo, or vm undefine for VM steps)\nwas: %s\nnow: %s",
				strings.Join(rec.Cmd, " "), strings.Join(s.Cmd, " "))
		}
		switch {
		case len(s.Input) > 0 && rec.InputSHA256 != sha256hex(s.Input):
			inputDrift = true
		case s.CreatesPath != "":
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
	if err := runCmd(e.Out, s.Cmd...); err != nil {
		return err
	}
	r := Record{ID: s.ID, Kind: KindRunCmd, Data: s.Data, Reboot: s.Reboot,
		Cmd: s.Cmd, UndoCmd: s.UndoCmd, UndoOp: s.UndoOp, UndoArgs: s.UndoArgs}
	if len(s.Input) > 0 {
		r.InputSHA256 = sha256hex(s.Input)
	}
	m.put(r)
	return m.save(e.Root)
}

func (e *Engine) applyEnableUnit(m *Manifest, s Step, oc *OpClients) error {
	verb := "enable"
	if !s.Enable {
		verb = "disable"
	}
	setUnit := func() error {
		if e.skipUnderRoot(oc) {
			fmt.Fprintf(e.Out, "%s: skipped under --root (needs live systemd — covered by test-vm)\n", s.ID)
			return nil
		}
		if s.Enable {
			return oc.Sysd().EnableUnit(s.Unit)
		}
		return oc.Sysd().DisableUnit(s.Unit)
	}
	if rec := m.find(s.ID); rec != nil {
		if rec.Unit != s.Unit || rec.Enable != s.Enable {
			return fmt.Errorf("journaled unit/state differs from the current settings — undo first (orthogonals undo)\nwas: %s enable=%v\nnow: %s enable=%v",
				rec.Unit, rec.Enable, s.Unit, s.Enable)
		}
		cur := e.unitState(oc, s.Unit)
		drifted := (s.Enable && cur == "disabled") || (!s.Enable && cur == "enabled")
		if !drifted {
			fmt.Fprintf(e.Out, "%s: already applied\n", s.ID)
			return nil
		}
		if !e.Yes {
			fmt.Fprintf(e.Out, "would: %s unit %s (unit drifted to %s)\n", verb, s.Unit, cur)
			return nil
		}
		return setUnit()
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would: %s unit %s\n", verb, s.Unit)
		return nil
	}
	prior := e.unitState(oc, s.Unit)
	if !s.Enable && prior == "unknown" {
		fmt.Fprintf(e.Out, "%s: unit not installed, nothing to disable\n", s.Unit)
		return nil
	}
	if err := setUnit(); err != nil {
		return err
	}
	m.put(Record{ID: s.ID, Kind: KindEnableUnit, Data: s.Data, Reboot: s.Reboot, Unit: s.Unit, Enable: s.Enable, PriorState: prior})
	return m.save(e.Root)
}

// runCmd echoes argv to out and runs it.
func runCmd(out io.Writer, argv ...string) error {
	fmt.Fprintf(out, "run: %s\n", strings.Join(argv, " "))
	b, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(argv, " "), err, bytes.TrimSpace(b))
	}
	return nil
}

// UnitEnabled reports whether unit is enabled under root.
func UnitEnabled(root, unit string) bool {
	wants, _ := filepath.Glob(filepath.Join(root, "/etc/systemd/system/*.wants/", unit))
	if len(wants) > 0 {
		return true
	}
	if root == "" {
		c := sysd.New()
		defer func() { _ = c.Close() }()
		return c.UnitFileState(unit) == "enabled"
	}
	return false
}

// unitState reports the unit's enablement tri-state.
func (e *Engine) unitState(oc *OpClients, unit string) string {
	if e.skipUnderRoot(oc) {
		if UnitEnabled(e.Root, unit) {
			return "enabled"
		}
		return "disabled"
	}
	return oc.Sysd().UnitFileState(unit)
}

// missingDirs lists the not-yet-existing ancestors of dir, deepest first.
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
