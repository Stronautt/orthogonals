package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hooks"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/preflight"
	"github.com/stronautt/orthogonals/internal/steps"
)

type applyOpts struct {
	binding string
	user    string
}

func newApplyCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var o applyOpts
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "apply the host configuration",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "apply", runApply(cfg, o, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&o.binding, "binding", hostcfg.BindingDynamic, "GPU binding mode: dynamic (libvirt hooks) or static (vfio-pci.ids at boot)")
	cmd.Flags().StringVar(&o.user, "user", defaultUser(), "desktop user that owns the Looking Glass shm file")
	return cmd
}

func runApply(cfg *Config, o applyOpts, stdout, stderr io.Writer) error {
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return err
	}
	facts := preflight.GatherFacts(cfg.Root)
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
		return fmt.Errorf("host refused by preflight (run `orthogonals preflight` for the full report)")
	}
	p, err := hostcfg.NewProfile(res, o.user, o.binding, facts.DefaultNetActive)
	if err != nil {
		return err
	}
	preexisting, err := bls.Tokens(cfg.Root)
	if err != nil {
		return err
	}
	list, err := hostcfg.Steps(p, preexisting)
	if err != nil {
		return err
	}
	overrides, err := hostcfg.IGPUOverrides(cfg.Root)
	if err != nil {
		return err
	}
	for _, a := range overrides {
		list = append(list, steps.Step{ID: a.ID, Kind: steps.KindWriteFile, Path: a.Path, Content: a.Content, Mode: a.Mode})
	}
	if o.binding == hostcfg.BindingDynamic {
		exe, err := executablePath()
		if err != nil {
			return err
		}
		shim, err := hooks.ShimStep(o.user, exe)
		if err != nil {
			return err
		}
		list = append(list, shim)
	}
	prior, err := steps.Load(cfg.Root)
	if err != nil {
		return err
	}

	e := newEngine(cfg, stdout, stderr)
	if _, err := e.UndoID("gpu-recover", false); err != nil {
		return err
	}
	if err := e.Apply(list); err != nil {
		return err
	}

	needReboot := false
	for _, s := range list {
		if s.Reboot && !prior.Has(s.ID) {
			needReboot = true
		}
	}
	argsLive := false
	if b, err := os.ReadFile(filepath.Join(cfg.Root, "/proc/cmdline")); err == nil {
		argsLive = strings.Contains(string(b), hostcfg.KernelArgs(p))
	}
	switch {
	case cfg.Yes && (needReboot || !argsLive):
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
	return nil
}

// defaultUser is the desktop user behind sudo, not root.
func defaultUser() string {
	if u := os.Getenv("SUDO_USER"); u != "" {
		return u
	}
	return os.Getenv("USER")
}
