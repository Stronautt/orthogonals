// Package hostcfg renders the host-side configuration artifacts and step list.
package hostcfg

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

//go:embed templates
var templateFS embed.FS

// baseKernelArgs enables the IOMMU in passthrough mode.
const baseKernelArgs = "intel_iommu=on iommu=pt"

// Profile is everything host configuration varies on.
type Profile struct {
	User             string
	Binding          string
	VFIOIDs          []string
	DefaultNetActive bool
}

// NewProfile derives the profile from a detect result.
func NewProfile(r *hw.Result, user, binding string, defaultNetActive bool) (Profile, error) {
	if err := steps.CheckUser(user); err != nil {
		return Profile{}, err
	}
	if binding != BindingDynamic && binding != BindingStatic {
		return Profile{}, fmt.Errorf("unknown binding mode %q (dynamic or static)", binding)
	}
	gpu, err := r.GPUs.SoleNVIDIA()
	if err != nil {
		return Profile{}, err
	}
	ids := []string{gpu.VendorDeviceID()}
	if gpu.Audio != nil {
		ids = append(ids, gpu.Audio.VendorDeviceID())
	}
	return Profile{User: user, Binding: binding, VFIOIDs: ids, DefaultNetActive: defaultNetActive}, nil
}

// BindingDynamic and BindingStatic are the --binding modes.
const (
	BindingDynamic = "dynamic"
	BindingStatic  = "static"
)

// KernelArgsStepID and VFIOIDsPrefix are the journaled boot-config contract.
const (
	KernelArgsStepID = "kernel-args"
	VFIOIDsPrefix    = "vfio-pci.ids="
)

// Unit names apply enables or disables.
const (
	UnitPersistenced  = "nvidia-persistenced.service"
	UnitLibvirtGuests = "libvirt-guests.service"
	UnitSwitcheroo    = "switcheroo-control.service"

	SwitcherooStepID = "enable-switcheroo-control"
)

// KernelArgs is the exact karg string apply adds.
func KernelArgs(p Profile) string {
	if p.Binding == BindingStatic {
		return baseKernelArgs + " " + VFIOIDsPrefix + strings.Join(p.VFIOIDs, ",")
	}
	return baseKernelArgs
}

// addedKargs is args minus the tokens the host already had.
func addedKargs(args string, preexisting []string) string {
	var added []string
	for _, tok := range strings.Fields(args) {
		if !slices.Contains(preexisting, tok) {
			added = append(added, tok)
		}
	}
	return strings.Join(added, " ")
}

// kernelArgsStep adds args to every BLS entry, undoing only what it added.
func kernelArgsStep(args string, preexisting []string) steps.Step {
	s := steps.Step{
		ID: KernelArgsStepID, Kind: steps.KindOp, Reboot: true,
		Op: steps.OpKernelArgsAdd, Args: map[string]string{"args": args},
	}
	if added := addedKargs(args, preexisting); added != "" {
		s.UndoOp = steps.OpKernelArgsRem
		s.UndoArgs = map[string]string{"args": added}
	}
	return s
}

// DesktopEntryID and DesktopLinkID are per-VM journal step IDs.
func DesktopEntryID(vm string) string { return "desktop-entry-" + vm }
func DesktopLinkID(vm string) string  { return "desktop-link-" + vm }

// Artifact is one rendered configuration file ready for a WriteFile step.
type Artifact struct {
	ID      string
	Path    string
	Mode    fs.FileMode
	Content []byte
}

// tplSpec maps one embedded template to its install path.
type tplSpec struct {
	tpl, path, id string
	mode          fs.FileMode
}

// artifactSpecs maps embedded templates to install paths, in apply order.
var artifactSpecs = []tplSpec{
	{"vfio.conf", "/etc/dracut.conf.d/vfio.conf", "dracut-vfio-conf", 0o644},
	// TODO(refactor): assumes Fedora's modular libvirt (virtqemud), not monolithic libvirtd.
	{"virtqemud.conf", "/etc/libvirt/virtqemud.conf", "libvirt-socket-auth", 0o644},
	{"virtqemud-socket.conf", "/etc/systemd/system/virtqemud.socket.d/orthogonals.conf", "libvirt-socket-perms", 0o644},
	{"61-mutter-ignore-nvidia.rules", "/etc/udev/rules.d/61-mutter-ignore-nvidia.rules", "udev-mutter-ignore", 0o644},
	{"50-orthogonals-igpu.conf", "/etc/environment.d/50-orthogonals-igpu.conf", "environment-igpu-pins", 0o644},
	{"looking-glass.conf", "/etc/tmpfiles.d/looking-glass.conf", "tmpfiles-looking-glass", 0o644},
	{"libvirt-guests", "/etc/sysconfig/libvirt-guests", "sysconfig-libvirt-guests", 0o644},
}

// renderTemplate executes one embedded template against data.
func renderTemplate(name string, data any) ([]byte, error) {
	tpl, err := template.ParseFS(templateFS, "templates/"+name)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// VMSteps renders the per-VM artifacts `vm define` journals: desktop entry and ~/Desktop link.
func VMSteps(vmName, displayName, user, exe string) ([]steps.Step, error) {
	if err := steps.CheckVMName(vmName); err != nil {
		return nil, err
	}
	if strings.ContainsAny(displayName, "\n\r") {
		return nil, fmt.Errorf("bad display name %q: newlines not allowed", displayName)
	}
	if user == "" || strings.ContainsAny(user, " \t\n'\"`$\\") {
		return nil, fmt.Errorf("bad desktop user %q — pass --user", user)
	}
	if err := steps.CheckExecPath(exe); err != nil {
		return nil, err
	}
	data := struct{ VMName, DisplayName, Exe string }{vmName, displayName, exe}
	content, err := renderTemplate("vm-looking-glass.desktop", data)
	if err != nil {
		return nil, err
	}
	list := []steps.Step{{
		ID: DesktopEntryID(vmName), Kind: steps.KindWriteFile,
		Path: desktopEntryPath(vmName), Content: content, Mode: 0o755,
	}}
	// ponytail: hardcodes /home/<user>/Desktop; xdg-user-dir DESKTOP if localized dirs matter.
	link := "/home/" + user + "/Desktop/" + vmName + ".orthogonals.desktop"
	list = append(list, steps.Step{
		ID: DesktopLinkID(vmName), Kind: steps.KindRunCmd,
		Cmd: []string{"runuser", "-u", user, "--", "sh", "-c",
			"mkdir -p /home/" + user + "/Desktop && ln -sfn " + desktopEntryPath(vmName) + " " + link +
				" && DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus gio set " + link + " metadata::trusted true"},
		UndoOp:      steps.OpRemoveFile,
		UndoArgs:    map[string]string{"path": link},
		CreatesPath: link,
	})
	return list, nil
}

// desktopEntryPath carries the .orthogonals marker to avoid distro-entry collisions.
func desktopEntryPath(vm string) string {
	return "/usr/share/applications/" + vm + ".orthogonals.desktop"
}

// DisplayName returns the display name a defined VM's desktop entry carries.
func DisplayName(root, vm string) string {
	b, err := os.ReadFile(filepath.Join(root, desktopEntryPath(vm)))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if name, ok := strings.CutPrefix(line, "Name="); ok {
			return name
		}
	}
	return ""
}

// igpuApps are desktop entries opted out of the NVIDIA Vulkan driver.
var igpuApps = []string{
	"google-chrome.desktop",
	"com.google.Chrome.desktop",
	"chromium-browser.desktop",
	"org.chromium.Chromium.desktop",
	"brave-browser.desktop",
	"microsoft-edge.desktop",
	"vivaldi-stable.desktop",
	"opera.desktop",
	"code.desktop",
	"code-url-handler.desktop",
	"code-insiders.desktop",
	"code-insiders-url-handler.desktop",
	"codium.desktop",
	"codium-url-handler.desktop",
	"cursor.desktop",
	"dev.zed.Zed.desktop",
	"slack.desktop",
	"discord.desktop",
}

// IGPUOverrides renders Intel-Vulkan-only copies of the installed igpuApps entries.
func IGPUOverrides(root string) ([]Artifact, error) {
	var out []Artifact
	for _, name := range igpuApps {
		b, err := os.ReadFile(filepath.Join(root, "/usr/share/applications", name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("igpu override scan: %w", err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if cmd, ok := strings.CutPrefix(l, "Exec="); ok {
				lines[i] = "Exec=env VK_LOADER_DRIVERS_SELECT=*intel* " + cmd
			}
		}
		out = append(out, Artifact{
			ID:      "igpu-override-" + name,
			Path:    "/usr/local/share/applications/" + name,
			Mode:    0o644,
			Content: []byte(strings.Join(lines, "\n")),
		})
	}
	return out, nil
}

// renderArtifacts renders every host configuration file for the profile.
func renderArtifacts(p Profile) ([]Artifact, error) {
	out := make([]Artifact, 0, len(artifactSpecs))
	for _, spec := range artifactSpecs {
		content, err := renderTemplate(spec.tpl, p)
		if err != nil {
			return nil, err
		}
		out = append(out, Artifact{ID: spec.id, Path: spec.path, Mode: spec.mode, Content: content})
	}
	return out, nil
}

// Steps assembles the ordered host-configuration step list.
func Steps(p Profile, preexisting []string) ([]steps.Step, error) {
	arts, err := renderArtifacts(p)
	if err != nil {
		return nil, err
	}
	var list []steps.Step
	for _, a := range arts {
		list = append(list, steps.Step{
			ID: a.ID, Kind: steps.KindWriteFile,
			Path: a.Path, Content: a.Content, Mode: a.Mode,
		})
	}
	args := KernelArgs(p)
	list = append(list,
		steps.Step{
			ID: "libvirt-socket-reload", Kind: steps.KindOp,
			Op: steps.OpSocketReload, UndoOp: steps.OpSocketReload,
		},
		kernelArgsStep(args, preexisting),
		steps.Step{
			ID: "dracut-regenerate", Kind: steps.KindRunCmd, Reboot: true,
			Cmd:     []string{"dracut", "-f", "--regenerate-all"},
			UndoCmd: []string{"dracut", "-f", "--regenerate-all"},
		},
		steps.Step{
			ID: "selinux-lg-fcontext", Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "svirt_tmpfs_t", "/dev/shm/looking-glass"},
			UndoCmd: []string{"semanage", "fcontext", "-d", "/dev/shm/looking-glass"},
		},
		steps.Step{
			ID: "lg-shm-restorecon", Kind: steps.KindRunCmd,
			Cmd: []string{"restorecon", "-i", "/dev/shm/looking-glass"},
		},
		steps.Step{
			ID: "user-libvirt-group", Kind: steps.KindRunCmd,
			Cmd: []string{"usermod", "-aG", "libvirt", p.User},
		},
		steps.Step{ID: "disable-nvidia-persistenced", Kind: steps.KindEnableUnit, Unit: UnitPersistenced, Enable: false},
		steps.Step{ID: "enable-libvirt-guests", Kind: steps.KindEnableUnit, Unit: UnitLibvirtGuests, Enable: true},
		steps.Step{ID: SwitcherooStepID, Kind: steps.KindEnableUnit, Unit: UnitSwitcheroo, Enable: true},
	)
	if !p.DefaultNetActive {
		list = append(list,
			steps.Step{ID: "net-default-autostart", Kind: steps.KindOp,
				Op: steps.OpNetAutostart, Args: map[string]string{"network": "default"}},
			steps.Step{ID: "net-default-start", Kind: steps.KindOp,
				Op: steps.OpNetActive, Args: map[string]string{"network": "default"}},
		)
	}
	return list, nil
}
