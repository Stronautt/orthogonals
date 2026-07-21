package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/media"
)

// test seam: the real pinned URLs are not reachable from tests.
var downloads = artifacts.Downloads

type mediaOpts struct {
	win11ISO        string
	vmName          string
	nvidiaInstaller string
}

func newMediaCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var o mediaOpts
	cmd := &cobra.Command{
		Use:   "media",
		Short: "build the provision ISO and fetch the pinned downloads",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "media", runMedia(cfg, o, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&o.win11ISO, "win11-iso", "", "path to the user-supplied Windows 11 installation ISO (required; get it from https://www.microsoft.com/software-download/windows11)")
	cmd.Flags().StringVar(&o.vmName, "vm-name", "", "libvirt domain name whose guest settings and provision ISO to build (default: the sole managed VM)")
	cmd.Flags().StringVar(&o.nvidiaInstaller, "nvidia-installer", "", "user-downloaded NVIDIA Windows driver installer, used instead of the pinned download")
	return cmd
}

func runMedia(cfg *Config, o mediaOpts, stdout, stderr io.Writer) error {
	if o.win11ISO == "" {
		fmt.Fprintln(stderr, "usage: orthogonals media --win11-iso <path> [flags]")
		return exitCode(2)
	}
	name, err := vmNameOrSole(cfg.Root, o.vmName)
	if err != nil {
		return err
	}

	meta := domain.ReadGuestConfig(cfg.Root, name)
	user := meta.User
	if user == "" {
		user = media.DefaultGuestUser
	}
	pass := meta.Password
	if pass == "" {
		pass = media.DefaultGuestPassword
	}
	loc := meta.Locale
	w, h, err := parseResolution(meta.Resolution)
	if err != nil {
		return err
	}

	if !cfg.Yes {
		fmt.Fprintf(stdout, "would validate %s (requires edition %q)\n", o.win11ISO, media.Edition)
		fmt.Fprintf(stdout, "would fetch into %s:\n", media.CacheDir(cfg.Root))
		for _, d := range downloads() {
			if d.Name == artifacts.NVIDIADriver.Name && o.nvidiaInstaller != "" {
				fmt.Fprintf(stdout, "  %s: user-supplied %s\n", d.Name, o.nvidiaInstaller)
				continue
			}
			fmt.Fprintf(stdout, "  %s %s  %s\n", d.Name, d.Version, d.URL)
		}
		fmt.Fprintf(stdout, "would build provision ISO at %s (volume %s, mode 0600)\n", media.ISOPath(cfg.Root, name), media.VolumeLabel)
		fmt.Fprintln(stdout, "dry run — re-run with --yes to apply")
		return nil
	}

	info, err := media.ValidateWin11ISO(o.win11ISO, stdout)
	if err != nil {
		return err
	}
	if loc == "" {
		loc = info.DefaultLanguage
	}
	p, err := media.NewProfile(user, pass, loc, w, h)
	if err != nil {
		return err
	}
	if len(info.Languages) > 0 && !slices.ContainsFunc(info.Languages, func(l string) bool {
		return strings.EqualFold(l, p.Locale)
	}) {
		return fmt.Errorf("the Windows ISO cannot display %q — its languages: %s (re-run vm define with --locale)",
			p.Locale, strings.Join(info.Languages, ", "))
	}

	var payloads []string
	for _, d := range downloads() {
		var path string
		if d.Name == artifacts.NVIDIADriver.Name && o.nvidiaInstaller != "" {
			path, err = media.ImportInstaller(cfg.Root, d, o.nvidiaInstaller, stdout)
		} else {
			path, err = media.Fetch(cfg.Root, d, stdout)
			if err != nil && d.Name == artifacts.NVIDIADriver.Name {
				err = fmt.Errorf("%w\nif the pinned driver no longer covers your GPU, download one from https://www.nvidia.com/drivers and re-run with --nvidia-installer <path>", err)
			}
		}
		if err != nil {
			return err
		}
		if artifacts.OnProvisionISO(d) {
			payloads = append(payloads, path)
		}
	}

	rendered, err := media.Render(p)
	if err != nil {
		return err
	}
	if err := media.BuildISO(rendered, payloads, media.ISOPath(cfg.Root, name), stdout); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "provision ISO ready: %s\n", media.ISOPath(cfg.Root, name))
	return nil
}
