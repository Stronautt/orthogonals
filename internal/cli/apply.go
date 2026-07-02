package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/preflight"
	"github.com/stronautt/orthogonals/internal/steps"
)

func cmdApply(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	binding := fs.String("binding", hostcfg.BindingDynamic, "GPU binding mode: dynamic (libvirt hooks) or static (vfio-pci.ids at boot)")
	user := fs.String("user", defaultUser(), "desktop user that owns the Looking Glass shm file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orthogonals apply: %v\n", err)
		return 1
	}

	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return fail(err)
	}
	facts := preflight.GatherFacts(cfg.Root)
	// the Overview contract: unsupported hosts are refused before anything is
	// mutated, with the failing gate and its remedy spelled out
	checks := preflight.Analyze(res, facts)
	if preflight.Overall(checks) == preflight.Fail {
		for _, c := range checks {
			if c.Status != preflight.Fail {
				continue
			}
			fmt.Fprintf(stderr, "preflight %s: %s\n", c.Name, c.Message)
			if c.Remedy != "" {
				fmt.Fprintf(stderr, "  remedy: %s\n", c.Remedy)
			}
		}
		return fail(fmt.Errorf("host refused by preflight (run `orthogonals preflight` for the full report)"))
	}
	p, err := hostcfg.NewProfile(res, *user, *binding, facts.DefaultNetActive)
	if err != nil {
		return fail(err)
	}
	// captured before the kargs step runs, so undo removes only the tokens
	// apply added; dry-run passes nil (it executes nothing, grubby included)
	var preexisting []string
	if cfg.Yes {
		if preexisting, err = hostcfg.CurrentKargTokens(); err != nil {
			return fail(err)
		}
	}
	list, err := hostcfg.Steps(p, preexisting)
	if err != nil {
		return fail(err)
	}
	overrides, err := hostcfg.IGPUOverrides(cfg.Root)
	if err != nil {
		return fail(err)
	}
	for _, a := range overrides {
		list = append(list, steps.Step{ID: a.ID, Kind: steps.KindWriteFile, Path: a.Path, Content: a.Content, Mode: a.Mode})
	}
	// static binding keeps the GPU on vfio-pci from boot — a reattach hook
	// would fight it, so the hook scripts are dynamic-only
	if *binding == hostcfg.BindingDynamic {
		hp, err := hooks.NewProfile(res, *user)
		if err != nil {
			return fail(err)
		}
		hookSteps, err := hooks.Steps(hp)
		if err != nil {
			return fail(err)
		}
		list = append(list, hookSteps...)
	}
	prior, err := steps.Load(cfg.Root)
	if err != nil {
		return fail(err)
	}

	e := &steps.Engine{Root: cfg.Root, Yes: cfg.Yes, Out: stdout, Err: stderr}
	// pre-v1 applies installed /usr/local/bin/gpu-recover.sh; the escape hatch
	// is `orthogonals recover` now, so drop the stale script and its record
	if _, err := e.UndoID("gpu-recover", false); err != nil {
		return fail(err)
	}
	if err := e.Apply(list); err != nil {
		return fail(err)
	}

	needReboot := false
	for _, s := range list {
		if s.Reboot && !prior.Has(s.ID) {
			needReboot = true
		}
	}
	// the journal alone under-reports: args applied by an earlier run still
	// need the reboot until they are live on the running kernel
	argsLive := false
	if b, err := os.ReadFile(filepath.Join(cfg.Root, "/proc/cmdline")); err == nil {
		argsLive = strings.Contains(string(b), hostcfg.KernelArgs(p))
	}
	switch {
	case cfg.Yes && (needReboot || !argsLive):
		// boot-menu escape hatch (research §D3): the recovery story for a
		// host that no longer reaches the desktop after apply.
		orchestrate.Banner(stdout,
			"REBOOT REQUIRED — kernel arguments and initramfs changed",
			"if the desktop does not come back after the reboot: press 'e' at the",
			"GRUB menu and delete these kernel arguments for a one-boot disable:",
			"  "+hostcfg.KernelArgs(p))
	case cfg.Yes:
		fmt.Fprintf(stdout, "recovery: if the host fails to boot to the desktop, press 'e' at the GRUB menu and delete these kernel arguments for a one-boot disable: %s\n", hostcfg.KernelArgs(p))
	default:
		if needReboot {
			fmt.Fprintln(stdout, "apply will change kernel arguments and the initramfs — a reboot will be required")
		}
		fmt.Fprintf(stdout, "recovery: if the host fails to boot to the desktop, press 'e' at the GRUB menu and delete these kernel arguments for a one-boot disable: %s\n", hostcfg.KernelArgs(p))
		fmt.Fprintln(stdout, "dry run — re-run with --yes to apply")
	}
	return 0
}

// defaultUser is the desktop user for artifacts like the Looking Glass shm
// tmpfiles entry: the user behind sudo, not root.
func defaultUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	return os.Getenv("USER")
}
