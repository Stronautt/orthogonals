package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestDetectJSON(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))
	root := hwtest.ReferenceRoot(t)

	code, stdout, stderr := run(t, "detect", "--root", root, "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	var res hw.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout)
	}
	if res.GPUs.IGPU == nil || res.GPUs.IGPU.Address != "0000:00:02.0" {
		t.Errorf("IGPU = %+v, want 0000:00:02.0", res.GPUs.IGPU)
	}
	if len(res.GPUs.DGPUs) != 1 || res.GPUs.DGPUs[0].Address != "0000:01:00.0" {
		t.Errorf("DGPUs = %+v, want one at 0000:01:00.0", res.GPUs.DGPUs)
	}
	if res.Platform.IOMMUAddressWidth != 39 {
		t.Errorf("IOMMUAddressWidth = %d, want 39", res.Platform.IOMMUAddressWidth)
	}
	for _, key := range []string{`"devices"`, `"gpus"`, `"cpu"`, `"platform"`, `"iommu_address_width"`, `"iommu_group"`, `"nvidia"`} {
		if !strings.Contains(stdout, key) {
			t.Errorf("JSON contract key %s missing from output", key)
		}
	}
}

func TestDetectHuman(t *testing.T) {
	t.Setenv("PATH", hwtest.FakeTools(t, "dracut"))
	root := hwtest.ReferenceRoot(t)

	code, stdout, stderr := run(t, "detect", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stdout == "" || !strings.Contains(stdout, "0000:01:00.0") {
		t.Errorf("non-JSON detect output missing the dGPU address:\n%s", stdout)
	}
}

func TestDetectErrorExit(t *testing.T) {
	code, _, stderr := run(t, "detect", "--root", t.TempDir())
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 for missing sysfs tree", code)
	}
	if !strings.Contains(stderr, "detect") {
		t.Errorf("stderr should mention detect, got: %q", stderr)
	}
}
