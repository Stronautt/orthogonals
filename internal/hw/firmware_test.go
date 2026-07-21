package hw

import (
	"reflect"
	"testing"

	"github.com/stronautt/orthogonals/internal/hw/hwtest"
)

func TestGPUMux(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"hybrid", "1\n", GPUMuxHybrid},
		{"discrete", "0\n", GPUMuxDiscrete},
		{"unexpected value", "2\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			hwtest.WriteFile(t, root, GPUMuxPath, tt.value)
			if got := gpuMux(root); got != tt.want {
				t.Errorf("gpuMux = %q, want %q", got, tt.want)
			}
		})
	}
	if got := gpuMux(t.TempDir()); got != "" {
		t.Errorf("gpuMux with no knob = %q, want empty", got)
	}
}

func TestIsIOMMUAttr(t *testing.T) {
	for _, name := range []string{"Vtd", "VtForDirectIo", "VTdFeature", "IommuSupport", "AMD-Vi"} {
		if !isIOMMUAttr(name) {
			t.Errorf("isIOMMUAttr(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"SecureBoot", "VtxSupport", "BootOrder"} {
		if isIOMMUAttr(name) {
			t.Errorf("isIOMMUAttr(%q) = true, want false", name)
		}
	}
}

func TestFirmwareIOMMUAttrs(t *testing.T) {
	root := t.TempDir()
	base := "sys/class/firmware-attributes"
	hwtest.WriteFile(t, root, base+"/dell-wmi-sysman/attributes/Vtd/current_value", "Disabled\n")
	hwtest.WriteFile(t, root, base+"/dell-wmi-sysman/attributes/Vtd/possible_values", "Disabled;Enabled\n")
	hwtest.WriteFile(t, root, base+"/dell-wmi-sysman/attributes/SecureBoot/current_value", "Enabled\n")

	attrs := firmwareIOMMUAttrs(root)
	if len(attrs) != 1 {
		t.Fatalf("got %d IOMMU attrs, want 1: %+v", len(attrs), attrs)
	}
	want := FirmwareAttr{
		Driver: "dell-wmi-sysman", Name: "Vtd", Current: "Disabled",
		PossibleValues: []string{"Disabled", "Enabled"},
	}
	if !reflect.DeepEqual(attrs[0], want) {
		t.Errorf("attr = %+v, want %+v", attrs[0], want)
	}

	if got := firmwareIOMMUAttrs(t.TempDir()); got != nil {
		t.Errorf("firmwareIOMMUAttrs with no driver = %+v, want nil", got)
	}
}
