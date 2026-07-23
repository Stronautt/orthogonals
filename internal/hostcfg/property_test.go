package hostcfg

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
	"github.com/stronautt/orthogonals/internal/virt"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

// propTools are every binary the host-configuration steps shell out to.
var propTools = append([]string{"systemctl", "usermod"}, hw.RequiredTools...)

// genProfile draws from the whole space NewProfile can produce: the user
// charset CheckUser accepts, both binding modes, every CPU vendor including
// the unknown one, and one or two passthrough IDs.
func genProfile(t *rapid.T) Profile {
	return Profile{
		User:             rapid.StringMatching(`[a-z_][a-z0-9_-]{0,15}`).Draw(t, "user"),
		Binding:          rapid.SampledFrom([]string{BindingDynamic, BindingStatic}).Draw(t, "binding"),
		IOMMUTable:       rapid.SampledFrom([]string{hw.IOMMUTableDMAR, hw.IOMMUTableIVRS, ""}).Draw(t, "iommu_table"),
		CPUVendor:        rapid.SampledFrom([]string{hw.CPUVendorIntel, hw.CPUVendorAMD, ""}).Draw(t, "cpu_vendor"),
		Laptop:           rapid.Bool().Draw(t, "laptop"),
		VFIOIDs:          rapid.SliceOfN(rapid.StringMatching(`[0-9a-f]{4}:[0-9a-f]{4}`), 1, 2).Draw(t, "vfio_ids"),
		DefaultNetActive: rapid.Bool().Draw(t, "default_net"),
	}
}

// propRoot builds a plausible host tree for one property iteration.
func propRoot(t *rapid.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "orthogonals-prop")
	if err != nil {
		t.Fatalf("temp root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := hwtest.BuildReferenceRoot(root); err != nil {
		t.Fatalf("build fixture: %v", err)
	}
	// Base dirs every real host already has; undo only removes directories
	// apply itself created, so their absence would mask a leak.
	for _, d := range []string{"etc", "var/lib", "usr/local/bin", "usr/share/applications"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return root
}

// propEngine applies under root with both daemon clients faked, so nothing dials.
func propEngine(root string, yes bool) *steps.Engine {
	return &steps.Engine{
		Root: root, Yes: yes, Out: io.Discard, Err: io.Discard,
		Virt: func() virt.Client { return &virttest.Fake{} },
		Sysd: func() sysd.Client { return &sysdtest.Fake{} },
	}
}

func snapshot(t *rapid.T, root string) string {
	t.Helper()
	s, err := stepstest.Snapshot(root)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

// seedPreexisting adds a subset of the profile's kernel args to every boot
// entry and returns it, standing in for a host that already carried them.
// kernelArgsStep undoes only what it actually added, so undo must leave these
// in place — the case a nil preexisting list never reaches.
func seedPreexisting(t *rapid.T, root string, p Profile) []string {
	t.Helper()
	args := strings.Fields(KernelArgs(p))
	keep := make([]string, 0, len(args))
	for _, a := range args {
		if rapid.Bool().Draw(t, "preexisting") {
			keep = append(keep, a)
		}
	}
	if len(keep) == 0 {
		return nil
	}
	entries, err := filepath.Glob(filepath.Join(root, "boot/loader/entries/*.conf"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no boot entries in fixture: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			t.Fatalf("%v", err)
		}
		updated := strings.Replace(string(b), "options ", "options "+strings.Join(keep, " ")+" ", 1)
		if err := os.WriteFile(e, []byte(updated), 0o644); err != nil {
			t.Fatalf("%v", err)
		}
	}
	return keep
}

// TestApplyUndoRestoresTree is the project's central promise: for any profile,
// undo puts the host back exactly as it was — including kernel args the host
// already carried, which undo must not strip.
func TestApplyUndoRestoresTree(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)
		preexisting := seedPreexisting(rt, root, p)
		before := snapshot(rt, root)

		list, err := Steps(p, preexisting)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, true).Apply(list); err != nil {
			rt.Fatalf("apply: %v", err)
		}
		if err := propEngine(root, true).Undo(false, false, nil); err != nil {
			rt.Fatalf("undo: %v", err)
		}

		if d := stepstest.Diff(before, snapshot(rt, root)); d != "" {
			rt.Fatalf("undo did not restore the tree for %+v:\n%s", p, d)
		}
	})
}

// TestApplyIsIdempotent asserts a second apply neither grows the journal nor
// changes the tree.
func TestApplyIsIdempotent(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)

		list, err := Steps(p, nil)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, true).Apply(list); err != nil {
			rt.Fatalf("first apply: %v", err)
		}
		once := snapshot(rt, root)
		first, err := steps.Load(root)
		if err != nil {
			rt.Fatalf("load: %v", err)
		}

		if err := propEngine(root, true).Apply(list); err != nil {
			rt.Fatalf("second apply: %v", err)
		}
		second, err := steps.Load(root)
		if err != nil {
			rt.Fatalf("load: %v", err)
		}
		if len(first.Records) != len(second.Records) {
			rt.Fatalf("re-apply grew the manifest: %d → %d records for %+v",
				len(first.Records), len(second.Records), p)
		}
		if d := stepstest.Diff(once, snapshot(rt, root)); d != "" {
			rt.Fatalf("re-apply changed the tree for %+v:\n%s", p, d)
		}
	})
}

// TestDryRunIsInert asserts apply without --yes never touches the filesystem.
func TestDryRunIsInert(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)
		before := snapshot(rt, root)

		list, err := Steps(p, nil)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, false).Apply(list); err != nil {
			rt.Fatalf("dry-run apply: %v", err)
		}
		if d := stepstest.Diff(before, snapshot(rt, root)); d != "" {
			rt.Fatalf("dry run mutated the tree for %+v:\n%s", p, d)
		}
	})
}
