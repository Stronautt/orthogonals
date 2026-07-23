package steps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw"
)

// Record is the journaled outcome of one applied step.
type Record struct {
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	Data   bool   `json:"data,omitempty"`
	Reboot bool   `json:"reboot,omitempty"`

	// write_file
	Path       string   `json:"path,omitempty"`
	Mode       uint32   `json:"mode,omitempty"`
	Restorecon bool     `json:"restorecon,omitempty"`
	NewSHA256  string   `json:"new_sha256,omitempty"`
	Existed    bool     `json:"existed,omitempty"`
	Backup     string   `json:"backup,omitempty"`
	OrigMode   uint32   `json:"orig_mode,omitempty"`
	MadeDirs   []string `json:"made_dirs,omitempty"`

	// run_cmd
	Cmd         []string `json:"cmd,omitempty"`
	UndoCmd     []string `json:"undo_cmd,omitempty"`
	InputSHA256 string   `json:"input_sha256,omitempty"`

	// enable_unit
	Unit       string `json:"unit,omitempty"`
	Enable     bool   `json:"enable,omitempty"`
	PriorState string `json:"prior_state,omitempty"`

	// op
	Op       string            `json:"op,omitempty"`
	OpArgs   map[string]string `json:"op_args,omitempty"`
	UndoOp   string            `json:"undo_op,omitempty"`
	UndoArgs map[string]string `json:"undo_args,omitempty"`
}

// Manifest is the undo journal at /var/lib/orthogonals/manifest.json.
type Manifest struct {
	Kernel        string   `json:"kernel,omitempty"`
	NVIDIAVersion string   `json:"nvidia_version,omitempty"`
	NVIDIAFlavor  string   `json:"nvidia_flavor,omitempty"`
	Records       []Record `json:"records"`
}

func (m *Manifest) stamp(root string) {
	m.Kernel = hw.KernelVersion(root)
	n := hw.DetectNVIDIA(root)
	m.NVIDIAVersion, m.NVIDIAFlavor = n.Version, n.Flavor
}

// StateDirPath is the orthogonals state directory.
const StateDirPath = "/var/lib/orthogonals"

// StateDir is StateDirPath under root.
func StateDir(root string) string { return filepath.Join(root, StateDirPath) }

// ManifestPath is the undo journal path under root.
func ManifestPath(root string) string { return filepath.Join(StateDir(root), "manifest.json") }

// StatePath is the persisted up pipeline position under root.
func StatePath(root string) string { return filepath.Join(StateDir(root), "state.json") }

func backupDir(root string) string { return filepath.Join(StateDir(root), "backup") }

// Load reads the manifest under root.
func Load(root string) (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath(root))
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ManifestPath(root), err)
	}
	return &m, nil
}

func (m *Manifest) save(root string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return WriteAtomic(ManifestPath(root), b)
}

// WriteAtomic writes content to path via a temp file and rename, creating
// parent dirs. File and directory are fsynced: a torn manifest.json after
// power loss would wedge every command and lose undo records for mutations
// already on disk.
func WriteAtomic(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeSync(tmp, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func writeSync(path string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// Has reports whether a step id is already journaled.
func (m *Manifest) Has(id string) bool { return m.find(id) != nil }

// Cmd returns the journaled argv for id, nil if not journaled.
func (m *Manifest) Cmd(id string) []string {
	if r := m.find(id); r != nil {
		return slices.Clone(r.Cmd)
	}
	return nil
}

// OpArgs returns the journaled op args for id, nil if not journaled.
func (m *Manifest) OpArgs(id string) map[string]string {
	if r := m.find(id); r != nil {
		return maps.Clone(r.OpArgs)
	}
	return nil
}

func (m *Manifest) find(id string) *Record {
	for i := range m.Records {
		if m.Records[i].ID == id {
			return &m.Records[i]
		}
	}
	return nil
}

func (m *Manifest) put(r Record) {
	if ex := m.find(r.ID); ex != nil {
		*ex = r
		return
	}
	m.Records = append(m.Records, r)
}

// drop removes a record, undoing a write-ahead journal entry whose mutation
// then failed. Without it a failed step would read as "already applied" on the
// next run and never be retried.
func (m *Manifest) drop(id string) {
	m.Records = slices.DeleteFunc(m.Records, func(r Record) bool { return r.ID == id })
}

// writeBackup makes the backup durable before the journal records it exists —
// otherwise a crash can leave a record whose backup is empty, and undo would
// "restore" the original as a zero-length file.
func writeBackup(root, name string, content []byte) error {
	if err := os.MkdirAll(backupDir(root), 0o700); err != nil {
		return err
	}
	if err := writeSync(filepath.Join(backupDir(root), name), content, 0o600); err != nil {
		return err
	}
	return syncDir(backupDir(root))
}

func backupName(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
