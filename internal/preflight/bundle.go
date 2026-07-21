package preflight

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

// configDirs are installed orthogonals artifacts worth bundling.
var configDirs = []string{steps.EtcDir, "/etc/libvirt/hooks", "/etc/dracut.conf.d", filepath.Dir(hooks.LogPath)}

var (
	macRE  = regexp.MustCompile(`\b[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}\b`)
	uuidRE = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// guestMetaRE matches guest credentials in a domain XML's <metadata> block.
	guestMetaRE = regexp.MustCompile(`<orthogonals:(user|password)>[^<]*</orthogonals:(?:user|password)>`)
)

type bundleEntry struct {
	name string
	data []byte
}

// WriteBundle writes a redacted diagnostics tar.gz.
func WriteBundle(w io.Writer, root string, detect *hw.Result) error {
	detectJSON, err := json.MarshalIndent(detect, "", "  ")
	if err != nil {
		return fmt.Errorf("bundle: encode detect result: %w", err)
	}
	entries := []bundleEntry{
		{"detect.json", detectJSON},
		{"lspci.txt", cmdOutput("lspci", "-nnk")},
		{"journal.txt", cmdOutput("journalctl", "-b", "--no-pager", "-g", "vfio|nvidia")},
	}
	for _, dir := range configDirs {
		base := filepath.Join(root, dir)
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				data = []byte(fmt.Sprintf("[unreadable: %v]\n", err))
			}
			if strings.HasSuffix(d.Name(), ".xml") {
				data = guestMetaRE.ReplaceAll(data, []byte("<orthogonals:$1>[redacted]</orthogonals:$1>"))
			}
			rel, _ := filepath.Rel(base, path)
			entries = append(entries, bundleEntry{filepath.Join("configs", dir, rel), data})
			return nil
		})
	}

	red := newRedactor(root)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		data := red.redact(e.data)
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
		if _, err := tw.Write(data); err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	return nil
}

// cmdOutput never fails: a missing tool becomes a note in its output.
func cmdOutput(name string, args ...string) []byte {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return append(out, fmt.Appendf(nil, "\n[%s unavailable: %v]\n", name, err)...)
	}
	return out
}

type redactor struct{ rep *strings.Replacer }

// newRedactor collects host-identifying literals to redact.
func newRedactor(root string) *redactor {
	var pairs []string
	if hn, err := os.Hostname(); err == nil && len(hn) >= 2 {
		pairs = append(pairs, hn, "REDACTED-HOSTNAME")
	}
	for _, rel := range []string{
		"/sys/class/dmi/id/product_serial",
		"/sys/class/dmi/id/board_serial",
		"/sys/class/dmi/id/chassis_serial",
		"/sys/class/dmi/id/product_uuid",
		"/etc/machine-id",
	} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if len(v) >= 4 && strings.ContainsAny(v, "0123456789") {
			pairs = append(pairs, v, "REDACTED")
		}
	}
	return &redactor{rep: strings.NewReplacer(pairs...)}
}

func (r *redactor) redact(b []byte) []byte {
	s := r.rep.Replace(string(b))
	s = macRE.ReplaceAllString(s, "REDACTED-MAC")
	s = uuidRE.ReplaceAllString(s, "REDACTED-UUID")
	return []byte(s)
}
