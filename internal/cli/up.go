package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/steps"
)

// cmdUp runs the whole pipeline (apply → reboot → vm → media → install →
// verify) as a persisted state machine: it stops cleanly at the reboot
// boundary and resumes from /var/lib/orthogonals/state.json.
func cmdUp(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	binding := fs.String("binding", hostcfg.BindingDynamic, "GPU binding mode: dynamic (libvirt hooks) or static (vfio-pci.ids at boot)")
	user := fs.String("user", defaultUser(), "desktop user that owns the Looking Glass shm file")
	vmName := fs.String("vm-name", "", "libvirt domain name (default: win11; a reboot-resume recovers the name the first run applied)")
	displayName := fs.String("display-name", "", "desktop shortcut name (default: \"Windows 11\" for the default VM, else the VM name)")
	ram := fs.Int("ram", 0, "guest RAM in GiB (default min(half of host RAM, 16))")
	disk := fs.String("disk", "", "qcow2 disk image path (default /var/lib/libvirt/images/<vm-name>.qcow2)")
	diskSize := fs.Int("disk-size", 0, "disk image size in GiB (default 100)")
	resolution := fs.String("resolution", "", "maximum guest resolution WxH (default 3840x2160)")
	win11ISO := fs.String("win11-iso", "", "path to the user-supplied Windows 11 installation ISO")
	guestUser := fs.String("guest-user", "", "guest admin account name")
	guestPassword := fs.String("guest-password", "", "guest admin password (default: \""+media.DefaultGuestPassword+"\")")
	locale := fs.String("locale", "", "guest locale and keyboard, e.g. uk-UA (default: the ISO's default language)")
	nvidiaInstaller := fs.String("nvidia-installer", "", "user-downloaded NVIDIA Windows driver installer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orthogonals up: %v\n", err)
		return 1
	}
	st, err := orchestrate.LoadState(cfg.Root)
	if err != nil {
		return fail(err)
	}
	// resolve the domain name once and persist it: a reboot-resume that omits
	// --vm-name must target the name the first run applied into the hooks, not
	// the default. state.json carries it across the reboot; it is removed by
	// plain undo along with the rest of the pipeline state.
	name := *vmName
	if name == "" {
		saved, err := orchestrate.SavedVMName(cfg.Root)
		if err != nil {
			return fail(err)
		}
		if saved != "" {
			name = saved
		} else {
			name = steps.DefaultVMName
		}
	}
	// a completed pipeline plus a name the journal has no define step for is
	// a NEW VM on a prepared host: restart the pipeline from scratch — apply
	// no-ops, the boot checks pass without a reboot, and the remaining stages
	// build the new domain. Mid-pipeline states stay untouched: that pipeline
	// is still building its own VM.
	man, err := steps.Load(cfg.Root)
	if err != nil {
		return fail(err)
	}
	defined := man.Has(domain.DefineStepID(name))
	restart := st == orchestrate.StateVerified && !defined
	if restart {
		fmt.Fprintf(stdout, "up: VM %s is not defined yet — running the pipeline for it on the prepared host\n", name)
		st = orchestrate.StateFresh
	}
	if !cfg.Yes {
		fmt.Fprintf(stdout, "pipeline state: %s\n", st)
		for _, s := range orchestrate.Remaining(st) {
			fmt.Fprintf(stdout, "  next: %s\n", s)
		}
		fmt.Fprintln(stdout, "dry run — re-run with --yes to run the pipeline")
		return 0
	}
	// the media stage needs the ISO; only demand it while that stage is ahead
	if *win11ISO == "" && st.Before(orchestrate.StateMediaBuilt) {
		fmt.Fprintln(stderr, "usage: orthogonals up --yes --win11-iso <path> [flags]")
		return 2
	}
	if err := orchestrate.SaveVMName(cfg.Root, name); err != nil {
		return fail(err)
	}
	if restart {
		// Machine.Run reloads the state from disk, so the restart must land
		// there before the run starts
		if err := orchestrate.SaveState(cfg.Root, st); err != nil {
			return fail(err)
		}
	}

	run := func(cmd command, cmdArgs []string) error {
		if code := cmd(cfg, cmdArgs, stdout, stderr); code != 0 {
			return fmt.Errorf("exit code %d (see output above)", code)
		}
		return nil
	}
	applyArgs := []string{"--binding", *binding, "--user", *user}
	vmArgs := []string{"--vm-name", name, "--win11-iso", *win11ISO}
	if *displayName != "" {
		vmArgs = append(vmArgs, "--display-name", *displayName)
	}
	if *ram > 0 {
		vmArgs = append(vmArgs, "--ram", strconv.Itoa(*ram))
	}
	if *disk != "" {
		vmArgs = append(vmArgs, "--disk", *disk)
	}
	if *diskSize > 0 {
		vmArgs = append(vmArgs, "--disk-size", strconv.Itoa(*diskSize))
	}
	if *resolution != "" {
		vmArgs = append(vmArgs, "--resolution", *resolution)
	}
	// guest settings land in the domain's <metadata> block at define; the
	// media stage reads them back from there
	for _, opt := range []struct{ flag, val string }{
		{"--guest-user", *guestUser}, {"--guest-password", *guestPassword},
		{"--locale", *locale},
	} {
		if opt.val != "" {
			vmArgs = append(vmArgs, opt.flag, opt.val)
		}
	}
	vmArgs = append(vmArgs, "define")
	mediaArgs := []string{"--win11-iso", *win11ISO, "--vm-name", name}
	if *nvidiaInstaller != "" {
		mediaArgs = append(mediaArgs, "--nvidia-installer", *nvidiaInstaller)
	}

	m := &orchestrate.Machine{Root: cfg.Root, Out: stdout,
		LaunchHint: fmt.Sprintf("launch with %s or the %q desktop entry",
			hostcfg.LauncherName(name), resolveDisplayName(cfg.Root, name, *displayName)),
		Stages: orchestrate.Stages{
			Apply:      func() error { return run(cmdApply, applyArgs) },
			VerifyBoot: func() error { return orchestrate.VerifyBoot(cfg.Root) },
			DefineVM:   func() error { return run(cmdVM, vmArgs) },
			BuildMedia: func() error { return run(cmdMedia, mediaArgs) },
			Install: func() error {
				if err := orchestrate.Install(name, stdout); err != nil {
					return err
				}
				// journaled like every other host mutation, so re-runs skip it
				// and vm undefine drops it with the domain
				e := &steps.Engine{Root: cfg.Root, Yes: cfg.Yes, Out: stdout, Err: stderr}
				return e.Apply([]steps.Step{domain.InstallVideoStep(name)})
			},
			Verify: func() error {
				if err := orchestrate.Verify(cfg.Root, name, stdout); err != nil {
					return err
				}
				// only now: a failed verify may still want the install media around
				removeProvisionISO(cfg.Root, name, stdout)
				return nil
			},
		}}
	if err := m.Run(); err != nil {
		return fail(err)
	}
	return 0
}

// removeProvisionISO removes a VM's provision ISO once the pipeline verified —
// it carries the guest credentials and is never needed again (the VM's cdrom
// uses startupPolicy=optional, so the file can go at any time).
func removeProvisionISO(root, vm string, stdout io.Writer) {
	path := media.ISOPath(root, vm)
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(stdout, "could not remove the provision ISO %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(stdout, "removed the provision ISO %s\n", path)
}
