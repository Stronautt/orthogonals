// Package hooks renders the libvirt qemu hook scripts that implement dynamic
// GPU binding: detach to vfio-pci on VM start (fail-safe — any failure aborts
// the start), reattach to the NVIDIA driver on shutdown. Ported from the
// working PoC scripts; the manual escape hatch is `orthogonals recover`.
package hooks

import (
	"bytes"
	"embed"
	"fmt"
	"text/template"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

//go:embed templates
var templateFS embed.FS

// LogPath is where every hook stage appends; the diagnostics bundle picks it
// up (research §D1: swallowed hook stderr = black-screen mysteries).
const LogPath = "/var/log/orthogonals/hooks.log"

// Profile is everything the hook scripts vary on. The dispatcher matches
// domains against the registry (steps.VMsDir) at runtime, so no VM name is
// baked in.
type Profile struct {
	User  string // desktop user that receives failure notifications
	GPU   string // dGPU PCI address, e.g. 0000:01:00.0
	Audio string // dGPU audio function address; empty when absent
}

// NewProfile derives the hook profile from a detect result. Exactly one
// NVIDIA dGPU is required — the same topology preflight gates on.
func NewProfile(r *hw.Result, user string) (Profile, error) {
	if err := steps.CheckUser(user); err != nil {
		return Profile{}, err
	}
	gpu, err := r.GPUs.SoleNVIDIA()
	if err != nil {
		return Profile{}, err
	}
	p := Profile{User: user, GPU: gpu.Address}
	if gpu.Audio != nil {
		p.Audio = gpu.Audio.Address
	}
	return p, nil
}

// DispatcherStepID is the qemu dispatcher's journal ID; orchestrate status
// gates its hook health check on it.
const DispatcherStepID = "hook-qemu-dispatcher"

// The NVIDIA module lists the hook scripts and `orthogonals recover` share:
// unload leaves first (nvidia_drm holds nvidia), reload lets modprobe pull
// dependencies in (nvidia_drm brings nvidia_modeset). The rendered scripts
// keep these as literals; TestHookScriptsUseModuleLists pins them to these
// exact lists so the two can never drift apart again.
var (
	NVIDIAUnloadOrder = []string{"nvidia_drm", "nvidia_modeset", "nvidia_uvm", "nvidia"}
	NVIDIAReloadOrder = []string{"nvidia", "nvidia_uvm", "nvidia_drm"}
)

// scriptSpecs maps embedded templates to install paths, in apply order.
var scriptSpecs = []struct{ tpl, path, id string }{
	{"qemu", "/etc/libvirt/hooks/qemu", DispatcherStepID},
	{"gpu-detach.sh", "/etc/libvirt/hooks/orthogonals-gpu-detach.sh", "hook-gpu-detach"},
	{"gpu-reattach.sh", "/etc/libvirt/hooks/orthogonals-gpu-reattach.sh", "hook-gpu-reattach"},
}

// InstalledPaths lists where the hook scripts land, in apply order —
// orchestrate status probes the same paths instead of hardcoding them.
func InstalledPaths() []string {
	paths := make([]string, len(scriptSpecs))
	for i, s := range scriptSpecs {
		paths[i] = s.path
	}
	return paths
}

// Steps renders every hook script as an executable WriteFile step for the
// apply engine, so install and undo are journaled like everything else.
func Steps(p Profile) ([]steps.Step, error) {
	data := struct {
		Profile
		LogPath string
		VMsDir  string
		RunDir  string
	}{p, LogPath, steps.VMsDirPath, steps.LibvirtRunDir}
	list := make([]steps.Step, 0, len(scriptSpecs))
	for _, spec := range scriptSpecs {
		tpl, err := template.ParseFS(templateFS, "templates/"+spec.tpl)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("render %s: %w", spec.tpl, err)
		}
		list = append(list, steps.Step{
			ID: spec.id, Kind: steps.KindWriteFile,
			Path: spec.path, Content: buf.Bytes(), Mode: 0o755,
		})
	}
	return list, nil
}
