package hostcfg

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/steps/stepstest"
	"github.com/stronautt/orthogonals/internal/sysd"
	"github.com/stronautt/orthogonals/internal/sysd/sysdtest"
	"github.com/stronautt/orthogonals/internal/virt"
	"github.com/stronautt/orthogonals/internal/virt/virttest"
)

var propTools = append([]string{"systemctl"}, hw.RequiredTools...)

// genProfile draws the whole space NewProfile can produce: the charset
// CheckUser accepts, both bindings, every CPU vendor including the unknown one.
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

func propRoot(t *rapid.T) string {
	t.Helper()
	root := stepstest.RapidRoot(t, "etc", "var/lib", "usr/local/bin", "usr/share/applications")
	if err := hwtest.BuildReferenceRoot(root); err != nil {
		t.Fatalf("build fixture: %v", err)
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

// seedPreexisting adds a subset of the profile's kernel args to each boot-config
// target, drawn per target so the targets may disagree. That disagreement is the
// case a single union of removals gets wrong whichever way it leans: undo is
// journaled per target, so every one of them owes back exactly what it started
// with.
func seedPreexisting(t *rapid.T, root string, p Profile) {
	t.Helper()
	args := strings.Fields(KernelArgs(p))
	subset := func(label string) []string {
		var keep []string
		for _, a := range args {
			if rapid.Bool().Draw(t, label+" carries "+a) {
				keep = append(keep, a)
			}
		}
		return keep
	}
	splice := func(path, after string, keep []string) {
		if len(keep) == 0 {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		updated := strings.Replace(string(b), after, after+strings.Join(keep, " ")+" ", 1)
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			t.Fatalf("%v", err)
		}
	}
	entries, err := filepath.Glob(filepath.Join(root, "boot/loader/entries/*.conf"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("no boot entries in fixture: %v", err)
	}
	for i, e := range entries {
		splice(e, "options ", subset("entry "+strconv.Itoa(i)))
	}
	splice(filepath.Join(root, bls.KernelCmdlinePath), "", subset("cmdline"))
	splice(filepath.Join(root, bls.GrubDefaultsPath), `GRUB_CMDLINE_LINUX="`, subset("grub"))
}

// For any profile, undo puts the host back exactly as it was — including kernel
// args the host already carried, which undo must not strip.
func TestApplyUndoRestoresTree(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)
		seedPreexisting(rt, root, p)
		before := stepstest.MustSnapshot(rt, root)

		boot, err := bls.Wanted(root, KernelArgs(p))
		if err != nil {
			rt.Fatalf("read boot config: %v", err)
		}
		list, err := Steps(p, boot, fedoraQemuConf)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, true).Apply(list); err != nil {
			rt.Fatalf("apply: %v", err)
		}
		if err := propEngine(root, true).Undo(false, false, nil); err != nil {
			rt.Fatalf("undo: %v", err)
		}

		if d := stepstest.Diff(before, stepstest.MustSnapshot(rt, root)); d != "" {
			rt.Fatalf("undo did not restore the tree for %+v:\n%s", p, d)
		}
	})
}

func TestApplyIsIdempotent(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)

		list, err := Steps(p, bls.Args{}, fedoraQemuConf)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, true).Apply(list); err != nil {
			rt.Fatalf("first apply: %v", err)
		}
		once := stepstest.MustSnapshot(rt, root)
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
		if d := stepstest.Diff(once, stepstest.MustSnapshot(rt, root)); d != "" {
			rt.Fatalf("re-apply changed the tree for %+v:\n%s", p, d)
		}
	})
}

func TestDryRunIsInert(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, propTools...))
	rapid.Check(t, func(rt *rapid.T) {
		p := genProfile(rt)
		root := propRoot(rt)
		before := stepstest.MustSnapshot(rt, root)

		list, err := Steps(p, bls.Args{}, fedoraQemuConf)
		if err != nil {
			rt.Fatalf("Steps(%+v): %v", p, err)
		}
		if err := propEngine(root, false).Apply(list); err != nil {
			rt.Fatalf("dry-run apply: %v", err)
		}
		if d := stepstest.Diff(before, stepstest.MustSnapshot(rt, root)); d != "" {
			rt.Fatalf("dry run mutated the tree for %+v:\n%s", p, d)
		}
	})
}
