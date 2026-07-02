package steps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw"
)

// Record is the journaled outcome of one applied step — self-contained, so
// undo works from the manifest alone.
type Record struct {
	ID     string `json:"id"`
	Kind   Kind   `json:"kind"`
	Data   bool   `json:"data,omitempty"`
	Reboot bool   `json:"reboot,omitempty"` // boot config; undo reports "reboot required"

	// write_file
	Path       string   `json:"path,omitempty"`
	Mode       uint32   `json:"mode,omitempty"`
	Restorecon bool     `json:"restorecon,omitempty"`
	NewSHA256  string   `json:"new_sha256,omitempty"` // verified before restore
	Existed    bool     `json:"existed,omitempty"`
	Backup     string   `json:"backup,omitempty"` // filename under backup/, "" when the file was new
	OrigMode   uint32   `json:"orig_mode,omitempty"`
	MadeDirs   []string `json:"made_dirs,omitempty"` // dirs apply created, deepest first

	// run_cmd
	Cmd     []string `json:"cmd,omitempty"`
	UndoCmd []string `json:"undo_cmd,omitempty"`

	// enable_unit
	Unit       string `json:"unit,omitempty"`
	Enable     bool   `json:"enable,omitempty"`
	PriorState string `json:"prior_state,omitempty"`
}

// Manifest is the undo journal at /var/lib/orthogonals/manifest.json. The
// version fields are stamped at apply time so undo can report host drift
// ("host has since updated X → Y") — bug-report context, not a gate
// (research §C2).
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

// StateDirPath is where orthogonals keeps its manifest, backups, cache, and
// state; templates that need the path baked in take it as data from here.
const StateDirPath = "/var/lib/orthogonals"

// StateDir is StateDirPath under root (the test seam).
func StateDir(root string) string { return filepath.Join(root, StateDirPath) }

// ManifestPath is the undo journal under root; preflight facts probe it to
// report whether orthogonals already manages the host.
func ManifestPath(root string) string { return filepath.Join(StateDir(root), "manifest.json") }

// StatePath is the persisted `up` pipeline position; undo removes it and
// `vm undefine --purge` clears a stale one.
func StatePath(root string) string { return filepath.Join(StateDir(root), "state.json") }

func backupDir(root string) string { return filepath.Join(StateDir(root), "backup") }

// Load reads the manifest under root; a missing manifest is an empty one.
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
	if err := os.MkdirAll(StateDir(root), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := ManifestPath(root) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ManifestPath(root))
}

// Has reports whether a step id is already journaled — callers use it to
// tell a fresh mutation from a no-op re-run (e.g. "reboot required" only
// when the kargs step actually landed this time).
func (m *Manifest) Has(id string) bool { return m.find(id) != nil }

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

func writeBackup(root, name string, content []byte) error {
	if err := os.MkdirAll(backupDir(root), 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(backupDir(root), name), content, 0o600)
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
