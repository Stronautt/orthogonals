package orchestrate

import (
	"io"
	"os"
	"testing"

	"github.com/stronautt/orthogonals/internal/hooks"
)

// TestMain silences the shared hook progress mirror.
func TestMain(m *testing.M) {
	hooks.LogWriter = io.Discard
	os.Exit(m.Run())
}
