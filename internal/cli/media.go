package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

// test seam: the real pinned URLs are not reachable from tests.
var downloads = artifacts.Downloads

func cmdMedia(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	win11ISO := fs.String("win11-iso", "", "path to the user-supplied Windows 11 installation ISO (required; get it from https://www.microsoft.com/software-download/windows11)")
	vmName := fs.String("vm-name", "", "libvirt domain name whose guest settings and provision ISO to build (default: the sole managed VM)")
	nvidiaInstaller := fs.String("nvidia-installer", "", "user-downloaded NVIDIA Windows driver installer, used instead of the pinned download")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orthogonals media: %v\n", err)
		return 1
	}
	if *win11ISO == "" {
		fmt.Fprintln(stderr, "usage: orthogonals media --win11-iso <path> [flags]")
		return 2
	}
	name := *vmName
	if name == "" {
		var err error
		if name, err = soleVMName(cfg.Root); err != nil {
			return fail(err)
		}
	}

	// credentials, locale, resolution come from the VM's <metadata> block —
	// `vm define --guest-user/--guest-password/--locale/--resolution` owns
	// them, so a media rebuild always renders what the VM was defined with
	// (a drifting locale renders an answer file the media cannot display,
	// see the gate below)
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
		return fail(err)
	}

	if !cfg.Yes {
		fmt.Fprintf(stdout, "would validate %s (requires edition %q)\n", *win11ISO, media.Edition)
		fmt.Fprintf(stdout, "would fetch into %s:\n", media.CacheDir(cfg.Root))
		for _, d := range downloads() {
			if d.Name == artifacts.NVIDIADriver.Name && *nvidiaInstaller != "" {
				fmt.Fprintf(stdout, "  %s: user-supplied %s\n", d.Name, *nvidiaInstaller)
				continue
			}
			fmt.Fprintf(stdout, "  %s %s  %s\n", d.Name, d.Version, d.URL)
		}
		fmt.Fprintf(stdout, "would build provision ISO at %s (volume %s, mode 0600)\n", media.ISOPath(cfg.Root, name), media.VolumeLabel)
		fmt.Fprintln(stdout, "dry run — re-run with --yes to apply")
		return 0
	}

	info, err := media.ValidateWin11ISO(*win11ISO, stdout)
	if err != nil {
		return fail(err)
	}
	if loc == "" {
		loc = info.DefaultLanguage
	}
	p, err := media.NewProfile(user, pass, loc, w, h)
	if err != nil {
		return fail(err)
	}
	// Setup falls back to an interactive language page when the answer file
	// requests a display language the media does not carry (it logs
	// 0x8007000D) — refuse here, where the fix is one flag away
	if len(info.Languages) > 0 && !slices.ContainsFunc(info.Languages, func(l string) bool {
		return strings.EqualFold(l, p.Locale)
	}) {
		return fail(fmt.Errorf("the Windows ISO cannot display %q — its languages: %s (re-run vm define with --locale)",
			p.Locale, strings.Join(info.Languages, ", ")))
	}

	// everything is cached; virtio-win rides as its own CD, the rest lands
	// on the provision ISO
	var payloads []string
	for _, d := range downloads() {
		var path string
		if d.Name == artifacts.NVIDIADriver.Name && *nvidiaInstaller != "" {
			path, err = media.ImportInstaller(cfg.Root, d, *nvidiaInstaller, stdout)
		} else {
			path, err = media.Fetch(cfg.Root, d, stdout)
			if err != nil && d.Name == artifacts.NVIDIADriver.Name {
				err = fmt.Errorf("%w\nif the pinned driver no longer covers your GPU, download one from https://www.nvidia.com/drivers and re-run with --nvidia-installer <path>", err)
			}
		}
		if err != nil {
			return fail(err)
		}
		if artifacts.OnProvisionISO(d) {
			payloads = append(payloads, path)
		}
	}

	rendered, err := media.Render(p)
	if err != nil {
		return fail(err)
	}
	stage, err := os.MkdirTemp(steps.StateDir(cfg.Root), "provision-stage-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := media.Stage(stage, rendered, payloads); err != nil {
		return fail(err)
	}
	if err := media.BuildISO(stage, media.ISOPath(cfg.Root, name), stdout); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "provision ISO ready: %s\n", media.ISOPath(cfg.Root, name))
	return 0
}
