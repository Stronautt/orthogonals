package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/stronautt/orthogonals/internal/hw"
)

func cmdDetect(cfg *Config, args []string, stdout, stderr io.Writer) int {
	if _, ok := parseFlags(cfg, args, stderr); !ok {
		return 2
	}
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		fmt.Fprintf(stderr, "orthogonals detect: %v\n", err)
		return 1
	}
	if cfg.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(stderr, "orthogonals detect: encode: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprint(stdout, res.Summary())
	return 0
}
