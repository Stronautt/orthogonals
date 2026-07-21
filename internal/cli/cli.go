// Package cli assembles the orthogonals command tree.
package cli

import (
	"io"

	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/virt"
)

// Version is the binary version.
var Version = "dev"

// newVirt/newSysd are the client injection points cli tests fill with fakes.
var (
	newVirt func() virt.Client
	newSysd func() sysd.Client
)

func virtClient() virt.Client {
	if newVirt != nil {
		return newVirt()
	}
	return virt.New()
}

func sysdClient() sysd.Client {
	if newSysd != nil {
		return newSysd()
	}
	return sysd.New()
}

// Config carries the global flags shared by every subcommand.
type Config struct {
	JSON bool
	Yes  bool
	Root string
}

// newEngine builds the apply engine from the global config and injected clients.
func newEngine(cfg *Config, stdout, stderr io.Writer) *steps.Engine {
	return &steps.Engine{Root: cfg.Root, Yes: cfg.Yes, Out: stdout, Err: stderr, Virt: newVirt, Sysd: newSysd}
}
