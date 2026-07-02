package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/stronautt/orthogonals/internal/steps"
)

// VolumeLabel is how the guest locates the provision CD — drive letters vary.
const VolumeLabel = "ORTHOGONALS"

// ISOPath is where a VM's provision ISO lands — per VM, so building media
// for one VM can never hand another VM its provisioning. `up` deletes it
// once the pipeline verifies (it carries the guest password).
func ISOPath(root, vm string) string {
	return filepath.Join(steps.StateDir(root), vm+"-provision.iso")
}

// Stage lays out the provision-ISO root in dir: the rendered files plus the
// downloaded installer payloads.
func Stage(dir string, rendered []Artifact, payloads []string) error {
	for _, a := range rendered {
		if err := os.WriteFile(filepath.Join(dir, a.Name), a.Content, 0o600); err != nil {
			return err
		}
	}
	for _, src := range payloads {
		if err := copyFile(src, filepath.Join(dir, filepath.Base(src))); err != nil {
			return err
		}
	}
	return nil
}

// BuildISO wraps the staged tree into the provision ISO with xorriso. The
// ISO carries the guest password (autounattend.xml), so xorriso must create
// it 0600 from birth — a chmod-after-write would leave a world-readable
// window on success and a world-readable residue if xorriso fails partway.
func BuildISO(stageDir, outPath string, out io.Writer) error {
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return steps.RunCmd(out, "xorriso", "-as", "mkisofs", "-quiet",
		"-V", VolumeLabel, "-J", "-joliet-long", "-rock",
		"-o", outPath, stageDir)
}

// WimInfo is what the edition gate learns about the installation image;
// cmdMedia uses it to default and validate the guest locale.
type WimInfo struct {
	Languages       []string
	DefaultLanguage string
}

// ValidateWin11ISO mounts the user-supplied installation ISO and checks that
// install.wim carries the required edition, failing early with the edition
// list otherwise — image-by-name selection would only die ~20 minutes into
// Setup (research §B2).
func ValidateWin11ISO(path string, out io.Writer) (WimInfo, error) {
	if _, err := os.Stat(path); err != nil {
		return WimInfo{}, fmt.Errorf("win11 ISO: %w", err)
	}
	mnt, err := os.MkdirTemp("", "orthogonals-iso-")
	if err != nil {
		return WimInfo{}, err
	}
	defer func() { _ = os.RemoveAll(mnt) }()
	if err := steps.RunCmd(out, "mount", "-o", "loop,ro", path, mnt); err != nil {
		return WimInfo{}, fmt.Errorf("mount %s (root required): %w", path, err)
	}
	defer func() { _ = exec.Command("umount", mnt).Run() }()

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
	info, err := exec.Command("wiminfo", wim).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return WimInfo{}, fmt.Errorf("wiminfo %s: %w\n%s", wim, err, bytes.TrimSpace(ee.Stderr))
		}
		return WimInfo{}, fmt.Errorf("wiminfo %s: %w", wim, err)
	}
	editions := wimEditions(info)
	if slices.Contains(editions, Edition) {
		fmt.Fprintf(out, "%s: %q found\n", path, Edition)
		return wimLanguages(info), nil
	}
	return WimInfo{}, fmt.Errorf("%s does not contain %q — it has: %s\nsupply a Pro ISO (download from https://www.microsoft.com/software-download/windows11)",
		path, Edition, strings.Join(editions, ", "))
}

// wimEditions extracts the image names from wiminfo output.
func wimEditions(info []byte) []string {
	var names []string
	for line := range strings.SplitSeq(string(info), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Name:"); ok {
			names = append(names, strings.TrimSpace(rest))
		}
	}
	return names
}

// wimLanguages extracts the image language lists and the first default.
func wimLanguages(info []byte) WimInfo {
	var w WimInfo
	seen := map[string]bool{}
	for line := range strings.SplitSeq(string(info), "\n") {
		l := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(l, "Default Language:"); ok {
			if w.DefaultLanguage == "" {
				w.DefaultLanguage = strings.TrimSpace(rest)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(l, "Languages:"); ok {
			for lang := range strings.FieldsSeq(rest) {
				if !seen[lang] {
					seen[lang] = true
					w.Languages = append(w.Languages, lang)
				}
			}
		}
	}
	return w
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
