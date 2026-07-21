package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/steps"
)

// newUpCmd runs the whole pipeline as a persisted state machine.
func newUpCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var o vmOpts
	var binding, nvidiaInstaller string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "run the whole pipeline (apply → vm → media → install → verify)",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "up", runUp(cfg, o, binding, nvidiaInstaller, stdout, stderr))
		},
	}
	addVMFlags(cmd.Flags(), &o)
	cmd.Flags().StringVar(&binding, "binding", hostcfg.BindingDynamic, "GPU binding mode: dynamic (libvirt hooks) or static (vfio-pci.ids at boot)")
	cmd.Flags().StringVar(&nvidiaInstaller, "nvidia-installer", "", "user-downloaded NVIDIA Windows driver installer")
	return cmd
}

func runUp(cfg *Config, o vmOpts, binding, nvidiaInstaller string, stdout, stderr io.Writer) error {
	st, err := orchestrate.LoadState(cfg.Root)
	if err != nil {
		return err
	}
	name := o.vmName
	if name == "" {
		saved, err := orchestrate.SavedVMName(cfg.Root)
		if err != nil {
			return err
		}
		if saved != "" {
			name = saved
		} else {
			name = steps.DefaultVMName
		}
	}
	man, err := steps.Load(cfg.Root)
	if err != nil {
		return err
	}
	defined := man.Has(domain.DefineStepID(name))
	restart := st == orchestrate.StateVerified && !defined
	if restart {
		fmt.Fprintf(stdout, "up: VM %s is not defined yet — running the pipeline for it on the prepared host\n", name)
		st = orchestrate.StateFresh
	}

	applyO := applyOpts{binding: binding, user: o.user}
	vmO := o
	vmO.vmName = name
	vmO.stage = ""
	vmO.purge = false

	launchHint := fmt.Sprintf("launch with `orthogonals vm launch --vm-name %s` or the %q desktop entry",
		name, resolveDisplayName(cfg.Root, name, o.displayName))

	if defined && domain.CurrentStage(cfg.Root, name) == domain.StageFinal {
		fmt.Fprintf(stdout, "up: setup complete — converging host and VM %s to this binary's settings\n", name)
		if err := runApply(cfg, applyO, stdout, stderr); err != nil {
			return err
		}
		if err := runVMDefine(cfg, vmO, stdout, stderr); err != nil {
			return err
		}
		if cfg.Yes {
			fmt.Fprintf(stdout, "converged — %s\n", launchHint)
		}
		return nil
	}
	if !cfg.Yes {
		fmt.Fprintf(stdout, "pipeline state: %s\n", st)
		for _, s := range orchestrate.Remaining(st) {
			fmt.Fprintf(stdout, "  next: %s\n", s)
		}
		fmt.Fprintln(stdout, "dry run — re-run with --yes to run the pipeline")
		return nil
	}
	if o.win11ISO == "" && st.Before(orchestrate.StateMediaBuilt) {
		fmt.Fprintln(stderr, "usage: orthogonals up --yes --win11-iso <path> [flags]")
		return exitCode(2)
	}
	if err := orchestrate.SaveVMName(cfg.Root, name); err != nil {
		return err
	}
	if restart {
		if err := orchestrate.SaveState(cfg.Root, st); err != nil {
			return err
		}
	}
	mediaO := mediaOpts{win11ISO: o.win11ISO, vmName: name, nvidiaInstaller: nvidiaInstaller}

	c := virtClient()
	defer func() { _ = c.Close() }()
	m := &orchestrate.Machine{Root: cfg.Root, Out: stdout,
		LaunchHint: launchHint,
		Stages: orchestrate.Stages{
			Apply:      func() error { return runApply(cfg, applyO, stdout, stderr) },
			VerifyBoot: func() error { return orchestrate.VerifyBoot(cfg.Root) },
			DefineVM:   func() error { return runVMDefine(cfg, vmO, stdout, stderr) },
			BuildMedia: func() error { return runMedia(cfg, mediaO, stdout, stderr) },
			Install: func() error {
				if err := orchestrate.Install(c, name, stdout); err != nil {
					return err
				}
				noVideo := vmO
				noVideo.stage = string(domain.StageNoVideo)
				return runVMDefine(cfg, noVideo, stdout, stderr)
			},
			Verify: func() error {
				if err := orchestrate.Verify(c, cfg.Root, name, stdout); err != nil {
					return err
				}
				final := vmO
				final.stage = string(domain.StageFinal)
				if err := runVMDefine(cfg, final, stdout, stderr); err != nil {
					return err
				}
				removeProvisionISO(cfg.Root, name, stdout)
				return nil
			},
		}}
	return m.Run()
}

// removeProvisionISO removes a VM's provision ISO once the pipeline verified.
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
