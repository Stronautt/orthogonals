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
	"github.com/stronautt/orthogonals/internal/utils"
)

// VolumeLabel is how the guest locates the provision CD.
const VolumeLabel = "ORTHOGONALS"

const isoSuffix = "-provision.iso"

func ISOPath(root, vm string) string {
	return filepath.Join(steps.StateDir(root), vm+isoSuffix)
}

// ProvisionISOs lists every provision ISO under root. They hold the guest
// password in cleartext and are not journaled steps, so undo has to find them
// by name — the VM registry that would name them is gone by then.
func ProvisionISOs(root string) []string {
	paths, _ := filepath.Glob(filepath.Join(steps.StateDir(root), "*"+isoSuffix))
	return paths
}

// newWriterStagingIn keeps the staging area of iso9660 on the same filesystem
// as the payloads and the image. NewWriter stages under $TMPDIR. On Fedora that
// is tmpfs, and the os.Link fast path of AddLocalFile cannot cross a
// filesystem. Every payload is then copied into the RAM that the guest needs.
//
// The staging area carries TempPrefix in its name because the staged copies
// hold the guest password. A build that is killed before Cleanup must leave
// something that SweepTemps collects.
//
// ponytail: TMPDIR is process-global; a per-writer staging directory if BuildISO
// ever runs concurrently.
func newWriterStagingIn(dir string) (*iso9660.ImageWriter, error) {
	staging := filepath.Join(dir, utils.TempPrefix+"staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, err
	}
	prior, had := os.LookupEnv("TMPDIR")
	if err := os.Setenv("TMPDIR", staging); err != nil {
		return nil, err
	}
	defer func() {
		if had {
			_ = os.Setenv("TMPDIR", prior)
			return
		}
		_ = os.Unsetenv("TMPDIR")
	}()
	return iso9660.NewWriter()
}

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
	dir := filepath.Dir(outPath)
	// The sweep runs before the writer, so a staging area that an earlier kill
	// stranded goes first. It runs again at the end, because Cleanup removes
	// only what iso9660 made inside that area and not the area itself.
	utils.SweepTemps(dir)
	w, err := newWriterStagingIn(dir)
	if err != nil {
		return err
	}
	defer func() {
		_ = w.Cleanup()
		utils.SweepTemps(dir)
	}()
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
	// Build beside the target and rename. The domain XML mounts outPath, and a
	// crash during the write must not leave a torn ISO there for the next
	// `vm launch`. The name carries TempPrefix because this file holds the guest
	// password as cleartext. A kill between the create and the rename must leave
	// something that SweepTemps collects. A ".tmp" suffix matches neither that
	// sweep nor the *-provision.iso glob of undo.
	tmp := filepath.Join(dir, utils.TempPrefix+filepath.Base(outPath))
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

type WimInfo struct {
	Languages       []string
	DefaultLanguage string
}

// ValidateWin11ISO loop-mounts the ISO, so it needs root — the name does not
// say so and callers reach it from unprivileged paths.
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
