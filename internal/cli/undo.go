package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

func newUndoCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var force, purge bool
	var step string
	cmd := &cobra.Command{
		Use:   "undo",
		Short: "restore the host to its pre-apply state",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			e := newEngine(cfg, stdout, stderr)
			if step != "" {
				return finish(stderr, "undo", undoStep(e, cfg, step, force, purge, stdout))
			}
			if err := e.Undo(force, purge, os.Stdin); err != nil {
				return finish(stderr, "undo", err)
			}
			// After the undo, so a refused one changes nothing. The ISO is not
			// a journaled step, so nothing else clears the cleartext password;
			// --purge has already taken the whole state directory.
			for _, iso := range media.ProvisionISOs(cfg.Root) {
				removeProvisionISO(iso, cfg.Yes, stdout)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restore files even if they changed after apply")
	cmd.Flags().BoolVar(&purge, "purge", false, "also remove the VM disk image, ISO cache, state, and config")
	cmd.Flags().StringVar(&step, "step", "", "undo one journaled step by id, leaving the rest of the manifest")
	return cmd
}

// undoStep reverses a single journaled step — what a refused apply asks for
// when one step diverged and the other thirty did not.
func undoStep(e *steps.Engine, cfg *Config, id string, force, purge bool, stdout io.Writer) error {
	if purge {
		return errors.New("--step undoes one step; --purge takes the whole state directory")
	}
	found, err := e.UndoID(id, force)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no journaled step %q (orthogonals status lists them)", id)
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to undo this step")
	}
	return nil
}
