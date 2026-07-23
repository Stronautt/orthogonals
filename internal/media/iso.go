package media

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kdomanski/iso9660"

	"github.com/stronautt/orthogonals/internal/steps"
)

// VolumeLabel is how the guest locates the provision CD.
const VolumeLabel = "ORTHOGONALS"

// ISOPath is where a VM's provision ISO lands.
func ISOPath(root, vm string) string {
	return filepath.Join(steps.StateDir(root), vm+"-provision.iso")
}

// BuildISO writes the provision ISO natively.
func BuildISO(rendered []Artifact, payloads []string, outPath string, out io.Writer) error {
	names := make([]string, 0, len(rendered)+len(payloads))
	for _, a := range rendered {
		names = append(names, a.Name)
	}
	for _, src := range payloads {
		names = append(names, filepath.Base(src))
	}
	for _, name := range names {
		if err := checkISOName(name); err != nil {
			return err
		}
	}
	w, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer func() { _ = w.Cleanup() }()
	for _, a := range rendered {
		if err := w.AddFile(bytes.NewReader(a.Content), a.Name); err != nil {
			return fmt.Errorf("add %s: %w", a.Name, err)
		}
	}
	for _, src := range payloads {
		if err := w.AddLocalFile(src, filepath.Base(src)); err != nil {
			return fmt.Errorf("add %s: %w", src, err)
		}
	}
	// Build beside the target and rename: the domain XML mounts outPath, and a
	// crash mid-write must not leave a torn ISO there for the next vm launch.
	tmp := outPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := w.WriteTo(f, VolumeLabel); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", outPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, outPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	fmt.Fprintf(out, "wrote %s\n", outPath)
	return nil
}

// checkISOName guards the writer's ceiling: plain ISO9660 without Joliet.
func checkISOName(name string) error {
	if len(name) > 30 {
		return fmt.Errorf("provision ISO filename %q exceeds 30 chars — unsupported without Joliet", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("provision ISO filename %q contains %q — unsupported without Joliet", name, r)
		}
	}
	return nil
}

// WimInfo is what the edition gate learns about the installation image.
type WimInfo struct {
	Languages       []string
	DefaultLanguage string
}

// ValidateWin11ISO loop-mounts the installation ISO and checks install.wim carries the required edition.
func ValidateWin11ISO(path string, out io.Writer) (WimInfo, error) {
	if _, err := os.Stat(path); err != nil {
		return WimInfo{}, fmt.Errorf("win11 ISO: %w", err)
	}
	mnt, cleanup, err := MountISO(path)
	if err != nil {
		return WimInfo{}, err
	}
	defer cleanup()

	wim := ""
	for _, name := range []string{"install.wim", "install.esd"} {
		if _, err := os.Stat(filepath.Join(mnt, "sources", name)); err == nil {
			wim = filepath.Join(mnt, "sources", name)
			break
		}
	}
	if wim == "" {
		return WimInfo{}, fmt.Errorf("%s has no sources/install.wim — not a Windows installation ISO", path)
	}
	images, err := parseWIM(wim)
	if err != nil {
		return WimInfo{}, err
	}
	var editions []string
	for _, img := range images.Images {
		editions = append(editions, img.Name)
	}
	if slices.Contains(editions, Edition) {
		fmt.Fprintf(out, "%s: %q found\n", path, Edition)
		return wimLanguages(images), nil
	}
	return WimInfo{}, fmt.Errorf("%s does not contain %q — it has: %s\nsupply a Pro ISO (download from https://www.microsoft.com/software-download/windows11)",
		path, Edition, strings.Join(editions, ", "))
}

// wimLanguages collects the per-image language lists and the first default.
func wimLanguages(images wimXML) WimInfo {
	var w WimInfo
	seen := map[string]bool{}
	for _, img := range images.Images {
		if w.DefaultLanguage == "" {
			w.DefaultLanguage = img.Windows.Languages.Default
		}
		for _, lang := range img.Windows.Languages.Language {
			if !seen[lang] {
				seen[lang] = true
				w.Languages = append(w.Languages, lang)
			}
		}
	}
	return w
}
