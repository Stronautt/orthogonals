package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/stronautt/orthogonals/internal/hw"
)

func newDetectCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "detect",
		Short: "detect the host GPUs and platform",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "detect", runDetect(cfg, stdout))
		},
	}
}

func runDetect(cfg *Config, stdout io.Writer) error {
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return err
	}
	if cfg.JSON {
		if err := writeJSON(stdout, res); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		return nil
	}
	fmt.Fprint(stdout, res.Summary())
	return nil
}
