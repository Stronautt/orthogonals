package hw

import (
	"os"
	"path/filepath"
	"strings"
)

// GPUMuxPath is the ASUS ROG display-MUX knob (asus-wmi).
const GPUMuxPath = "/sys/devices/platform/asus-nb-wmi/gpu_mux_mode"

// ASUS display MUX modes. ponytail: asusctl maps 0=Discrete, 1=Optimus; confirm on hardware.
const (
	GPUMuxHybrid   = "hybrid"
	GPUMuxDiscrete = "discrete"
)

// gpuMux reads the ASUS display MUX, "" when the knob is absent.
func gpuMux(root string) string {
	switch readTrim(filepath.Join(root, GPUMuxPath)) {
	case "1":
		return GPUMuxHybrid
	case "0":
		return GPUMuxDiscrete
	default:
		return ""
	}
}

// FirmwareAttr is one BIOS setting exposed via /sys/class/firmware-attributes.
type FirmwareAttr struct {
	Driver         string   `json:"driver"`
	Name           string   `json:"name"`
	Current        string   `json:"current"`
	PossibleValues []string `json:"possible_values,omitempty"`
}

// firmwareIOMMUAttrs finds firmware-attributes controlling the IOMMU across the
// dell-wmi-sysman, thinklmi, and hp-bioscfg drivers.
func firmwareIOMMUAttrs(root string) []FirmwareAttr {
	drivers, err := os.ReadDir(filepath.Join(root, "/sys/class/firmware-attributes"))
	if err != nil {
		return nil
	}
	var out []FirmwareAttr
	for _, drv := range drivers {
		attrsDir := filepath.Join(root, "/sys/class/firmware-attributes", drv.Name(), "attributes")
		attrs, err := os.ReadDir(attrsDir)
		if err != nil {
			continue
		}
		for _, a := range attrs {
			if !isIOMMUAttr(a.Name()) {
				continue
			}
			ad := filepath.Join(attrsDir, a.Name())
			out = append(out, FirmwareAttr{
				Driver:         drv.Name(),
				Name:           a.Name(),
				Current:        readTrim(filepath.Join(ad, "current_value")),
				PossibleValues: splitTrim(readTrim(filepath.Join(ad, "possible_values")), ";"),
			})
		}
	}
	return out
}

// iommuAttrKeywords match a firmware-attribute name controlling the IOMMU.
var iommuAttrKeywords = []string{"iommu", "vtd", "directio", "amdvi"}

// isIOMMUAttr reports whether an attribute name controls the IOMMU.
func isIOMMUAttr(name string) bool {
	n := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(name))
	for _, kw := range iommuAttrKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// splitTrim splits on sep and trims each part, nil for empty input.
func splitTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, sep)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
