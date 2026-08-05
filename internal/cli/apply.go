package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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
	args := hostcfg.KernelArgs(p)
	boot, err := bls.Wanted(cfg.Root, args)
	if err != nil {
		return err
	}
	// Computed from the file libvirt shipped, never from a copy of its default
	// list: that list changes between libvirt releases.
	qemuConf, err := os.ReadFile(filepath.Join(cfg.Root, steps.QemuConfPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", steps.QemuConfPath, err)
	}
	list, err := hostcfg.Steps(p, boot, string(qemuConf))
	if err != nil {
		return err
	}
	igpuVendor := ""
	if res.GPUs.IGPU != nil {
		igpuVendor = res.GPUs.IGPU.Vendor
	}
	overrides, err := hostcfg.IGPUOverrides(cfg.Root, igpuVendor)
	if err != nil {
		return err
	}
	for _, a := range overrides {
		list = append(list, steps.Step{ID: a.ID, Kind: steps.KindWriteFile, Path: a.Path, Content: a.Content, Mode: a.Mode})
	}
	// An akmod-nvidia host trusts the akmods key and not dkms's, so kvmfr is
	// rejected at load even though the GPU driver works. Reuse the enrolled key
	// instead of sending the user to the MOK screen.
	if plan, key := preflight.PlanSigning(res.Platform.SecureBoot, facts.Signing); plan == preflight.SigningReuseAkmods {
		list = append(list, hostcfg.DKMSSigningSteps(key.Cert, key.Key)...)
	}
	if o.binding == hostcfg.BindingDynamic {
		exe, err := executablePath()
		if err != nil {
			return err
		}
		shim, err := hooks.ShimStep(cfg.Root, o.user, exe)
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
	// Per-token: a substring test on the joined args answers about adjacency in
	// /proc/cmdline, so one extra arg between them re-banners every idempotent
	// re-run as a pending reboot.
	argsLive := false
	if missing, err := bls.MissingLive(cfg.Root, args); err == nil {
		argsLive = len(missing) == 0
	}
	const recovery = "recovery: if the host fails to boot to the desktop, press 'e' at the GRUB menu and delete these kernel arguments for a one-boot disable: %s\n"
	switch {
	case cfg.Yes && (needReboot || !argsLive):
		orchestrate.Banner(stdout,
			"REBOOT REQUIRED — kernel arguments and initramfs changed",
			"if the desktop does not come back after the reboot: press 'e' at the",
			"GRUB menu and delete these kernel arguments for a one-boot disable:",
			"  "+args)
	case cfg.Yes:
		fmt.Fprintf(stdout, recovery, args)
	default:
		if needReboot {
			fmt.Fprintln(stdout, "apply will change kernel arguments and the initramfs — a reboot will be required")
		}
		fmt.Fprintf(stdout, recovery, args)
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
