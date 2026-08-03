// Package hooks implements the libvirt qemu hook: dynamic GPU binding for VFIO passthrough.
package hooks

import (
	"fmt"

	"github.com/stronautt/orthogonals/internal/steps"
)

const LogPath = "/var/log/orthogonals/hooks.log"

const DispatcherStepID = "hook-qemu-dispatcher"

// dispatcherPath is where libvirt looks for the qemu hook.
const dispatcherPath = "/etc/libvirt/hooks/qemu"

// NVIDIA module unload/reload orders, shared with recover.
var (
	NVIDIAUnloadOrder = []string{"nvidia_drm", "nvidia_modeset", "nvidia_uvm", "nvidia"}
	NVIDIAReloadOrder = []string{"nvidia", "nvidia_uvm", "nvidia_drm"}
)

func InstalledPaths() []string { return []string{dispatcherPath} }

func ShimStep(root, user, exe string) (steps.Step, error) {
	if err := steps.CheckUser(user); err != nil {
		return steps.Step{}, err
	}
	if err := steps.CheckExecPath(exe); err != nil {
		return steps.Step{}, err
	}
	// Under --root the tree is synthetic and ownership says nothing.
	if root == "" {
		if err := steps.CheckExecTrusted(exe); err != nil {
			return steps.Step{}, err
		}
	}
	content := fmt.Sprintf("#!/bin/sh\n"+
		"# orthogonals libvirt hook shim — managed by orthogonals apply\n"+
		"exec %s hook --user %s qemu \"$@\"\n", exe, user)
	return steps.Step{
		ID: DispatcherStepID, Kind: steps.KindWriteFile,
		Path: dispatcherPath, Content: []byte(content), Mode: 0o755,
	}, nil
}
