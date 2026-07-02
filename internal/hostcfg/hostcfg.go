// Package hostcfg renders the host-side configuration artifacts (dracut,
// udev, environment.d, tmpfiles, libvirt-guests) from detect results and
// assembles the ordered step list `orthogonals apply` feeds the journaled
// apply engine.
package hostcfg

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"text/template"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/steps"
)

//go:embed templates
var templateFS embed.FS

// baseKernelArgs enables the IOMMU in passthrough mode; static binding
// appends vfio-pci.ids on top.
const baseKernelArgs = "intel_iommu=on iommu=pt"

// Profile is everything host configuration varies on. Per-VM artifacts
// (launcher, desktop entry) render via VMSteps instead — `vm define` owns
// those.
type Profile struct {
	User             string   // desktop user that owns the Looking Glass shm file
	Binding          string   // "dynamic" (libvirt hooks) or "static" (vfio-pci.ids at boot)
	VFIOIDs          []string // dGPU + audio function vendor:device pairs, e.g. 10de:2206
	DefaultNetActive bool     // libvirt default network already active → skip net steps
}

// NewProfile derives the profile from a detect result. Exactly one NVIDIA
// dGPU is required — the same topology preflight gates on.
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

// GPU binding modes (--binding): dynamic hands the GPU over per VM start via
// the libvirt hooks; static pins it to vfio-pci from boot.
const (
	BindingDynamic = "dynamic"
	BindingStatic  = "static"
)

// The journaled boot-config contract: orchestrate reads the kernel-args
// record back by ID (VerifyBoot) and detects static binding by the
// vfio-pci.ids= prefix, so the producers and readers share these.
const (
	KernelArgsStepID = "kernel-args"
	VFIOIDsPrefix    = "vfio-pci.ids="
)

// Units apply enables/disables; preflight facts and orchestrate status probe
// the same names.
const (
	UnitPersistenced  = "nvidia-persistenced.service"
	UnitLibvirtGuests = "libvirt-guests.service"
	UnitSwitcheroo    = "switcheroo-control.service"

	SwitcherooStepID = "enable-switcheroo-control"
)

// KernelArgs is the exact karg string apply adds and the boot-menu escape
// hatch tells the user to delete for a one-boot disable.
func KernelArgs(p Profile) string {
	if p.Binding == BindingStatic {
		return baseKernelArgs + " " + VFIOIDsPrefix + strings.Join(p.VFIOIDs, ",")
	}
	return baseKernelArgs
}

// undoKargsCmd builds the kernel-args undo command, removing only the tokens
// apply is actually adding — nil when every token pre-existed (they were the
// user's; undo must leave them).
func undoKargsCmd(args string, preexisting []string) []string {
	var added []string
	for _, tok := range strings.Fields(args) {
		if !slices.Contains(preexisting, tok) {
			added = append(added, tok)
		}
	}
	if len(added) == 0 {
		return nil
	}
	return []string{"grubby", "--update-kernel=ALL", "--remove-args=" + strings.Join(added, " ")}
}

// GrubbyArgs extracts the --args= payload from a journaled grubby argv —
// the exact kargs apply added, so verification never re-derives them.
func GrubbyArgs(cmd []string) (string, bool) {
	for _, a := range cmd {
		if s, ok := strings.CutPrefix(a, "--args="); ok {
			return s, true
		}
	}
	return "", false
}

// CurrentKargTokens is the union of kernel-arg tokens configured across all
// boot entries (grubby --info=ALL). Apply captures it before adding args so
// undo removes only the tokens orthogonals added — a token the user already
// had (e.g. intel_iommu=on) must survive undo.
func CurrentKargTokens() ([]string, error) {
	b, err := exec.Command("grubby", "--info=ALL").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("grubby --info=ALL: %w\n%s", err, bytes.TrimSpace(b))
	}
	var tokens []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		args, ok := strings.CutPrefix(strings.TrimSpace(line), "args=")
		if !ok {
			continue
		}
		for _, tok := range strings.Fields(strings.Trim(args, `"`)) {
			if !seen[tok] {
				seen[tok] = true
				tokens = append(tokens, tok)
			}
		}
	}
	return tokens, nil
}

// Per-VM artifact names and journal step IDs. cli's undefine ordering and
// up's launch hint consume the same constructors the producers use. The
// launcher's leading underscore keeps it out of shell autocompletion — the
// desktop entry is the user-facing way in.
func LauncherName(vm string) string   { return "_ort-run-" + vm + "-lg" }
func DesktopEntryID(vm string) string { return "desktop-entry-" + vm }

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
	// TODO(refactor): Fedora's modular libvirt (virtqemud). A host still running the
	// monolithic libvirtd would need the same pair against libvirtd.conf and
	// libvirtd.socket; the reload step below fails loudly there rather than
	// silently leaving polkit in the path.
	{"virtqemud.conf", "/etc/libvirt/virtqemud.conf", "libvirt-socket-auth", 0o644},
	{"virtqemud-socket.conf", "/etc/systemd/system/virtqemud.socket.d/orthogonals.conf", "libvirt-socket-perms", 0o644},
	{"61-mutter-ignore-nvidia.rules", "/etc/udev/rules.d/61-mutter-ignore-nvidia.rules", "udev-mutter-ignore", 0o644},
	{"50-orthogonals-igpu.conf", "/etc/environment.d/50-orthogonals-igpu.conf", "environment-igpu-pins", 0o644},
	{"looking-glass.conf", "/etc/tmpfiles.d/looking-glass.conf", "tmpfiles-looking-glass", 0o644},
	{"libvirt-guests", "/etc/sysconfig/libvirt-guests", "sysconfig-libvirt-guests", 0o644},
	{"lg-build.sh", "/var/lib/orthogonals/lg-build.sh", "lg-build-script", 0o755},
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

// VMSteps renders the per-VM artifacts `vm define` journals alongside the
// domain: the launcher and the desktop entry named after the display name.
func VMSteps(vmName, displayName string) ([]steps.Step, error) {
	if err := steps.CheckVMName(vmName); err != nil {
		return nil, err
	}
	// the display name lands verbatim in the desktop entry and the launcher
	if strings.ContainsAny(displayName, "\n\r") {
		return nil, fmt.Errorf("bad display name %q: newlines not allowed", displayName)
	}
	data := struct{ VMName, DisplayName, Launcher string }{vmName, displayName, LauncherName(vmName)}
	specs := []tplSpec{
		{"vm-lg", "/usr/local/bin/" + LauncherName(vmName), LauncherName(vmName), 0o755},
		{"vm-looking-glass.desktop", desktopEntryPath(vmName), DesktopEntryID(vmName), 0o644},
	}
	var list []steps.Step
	for _, spec := range specs {
		content, err := renderTemplate(spec.tpl, data)
		if err != nil {
			return nil, err
		}
		list = append(list, steps.Step{
			ID: spec.id, Kind: steps.KindWriteFile,
			Path: spec.path, Content: content, Mode: spec.mode,
		})
	}
	return list, nil
}

// desktopEntryPath carries the .orthogonals marker so a VM name can never
// collide with a distro-shipped desktop entry.
func desktopEntryPath(vm string) string {
	return "/usr/share/applications/" + vm + ".orthogonals.desktop"
}

// DisplayName returns the display name a defined VM's desktop entry carries,
// "" when the entry is absent — re-defines without --display-name keep the
// name the VM was defined with.
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

// igpuApps are desktop entries opted out of the NVIDIA Vulkan driver:
// Chromium/Electron GPU processes (and Zed's Vulkan-native UI) create a
// Vulkan instance at startup, and with the NVIDIA ICD visible that holds
// /dev/nvidia* for the process lifetime — an idle browser would block VM
// start. Scope is exactly that mechanism: GTK4 apps are handled as a class
// by GSK_RENDERER in 50-orthogonals-igpu.conf, and GL-only apps (Firefox,
// kitty, alacritty, GNOME Terminal) are already covered by the EGL pin.
// Absent entries are skipped, so listing all packaging variants is free.
// Everything else keeps the stock default (Vulkan sees both GPUs, games
// pick the discrete one on their own).
var igpuApps = []string{
	// Chromium-family browsers (each has an official RPM repo)
	"google-chrome.desktop",
	"com.google.Chrome.desktop",
	"chromium-browser.desktop",
	"org.chromium.Chromium.desktop",
	"brave-browser.desktop",
	"microsoft-edge.desktop",
	"vivaldi-stable.desktop",
	"opera.desktop",
	// Electron IDEs
	"code.desktop",
	"code-url-handler.desktop",
	"code-insiders.desktop",
	"code-insiders-url-handler.desktop",
	"codium.desktop",
	"codium-url-handler.desktop",
	"cursor.desktop",
	// Vulkan-native editor
	"dev.zed.Zed.desktop",
	// Electron chat apps with system-wide installs
	"slack.desktop",
	"discord.desktop",
}

// IGPUOverrides renders higher-priority copies (/usr/local/share wins over
// /usr/share in XDG_DATA_DIRS order) of the installed igpuApps entries with
// every Exec line prefixed to select the Intel Vulkan driver only. Entries
// not installed on this host are skipped.
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
	data := struct {
		Profile
		LG       artifacts.Download // Looking Glass source pin for lg-build.sh
		CacheDir string             // media's download cache, shared by lg-build.sh
		StateDir string
	}{p, artifacts.LookingGlassSource, media.CachePath, steps.StateDirPath}
	out := make([]Artifact, 0, len(artifactSpecs))
	for _, spec := range artifactSpecs {
		content, err := renderTemplate(spec.tpl, data)
		if err != nil {
			return nil, err
		}
		out = append(out, Artifact{ID: spec.id, Path: spec.path, Mode: spec.mode, Content: content})
	}
	return out, nil
}

// Steps assembles the ordered host-configuration step list: packages first
// (later steps need semanage/virsh), config files, then boot config, SELinux,
// units, and the libvirt default network when it is inactive. preexisting is
// the CurrentKargTokens capture (nil on dry-run): kargs the host already had
// are excluded from the journaled undo command.
func Steps(p Profile, preexisting []string) ([]steps.Step, error) {
	arts, err := renderArtifacts(p)
	if err != nil {
		return nil, err
	}
	list := []steps.Step{{
		ID: "packages", Kind: steps.KindRunCmd,
		Cmd: append([]string{"dnf", "install", "-y"}, artifacts.Packages...),
		// no UndoCmd: undo leaves packages installed (documented no-op)
	}}
	for _, a := range arts {
		list = append(list, steps.Step{
			ID: a.ID, Kind: steps.KindWriteFile,
			Path: a.Path, Content: a.Content, Mode: a.Mode,
		})
	}
	args := KernelArgs(p)
	// the socket's mode/group and the auth setting only take effect once
	// systemd re-creates the socket and virtqemud re-reads its config. The same
	// command undoes it: undo restores the two files first (engine order), so
	// re-running this reload lands the defaults back — polkit auth on an 0666
	// socket. try-restart leaves a socket-activated daemon that is not running
	// alone.
	reloadLibvirt := []string{"sh", "-c",
		"systemctl daemon-reload && systemctl restart virtqemud.socket && systemctl try-restart virtqemud.service"}
	list = append(list,
		steps.Step{
			ID: "libvirt-socket-reload", Kind: steps.KindRunCmd,
			Cmd: reloadLibvirt, UndoCmd: reloadLibvirt,
		},
		steps.Step{
			// runs after packages (cmake + build deps) and after the
			// lg-build-script write above; the script pin-verifies the
			// source tarball and ldd-checks the installed binary
			ID: "lg-client-build", Kind: steps.KindRunCmd,
			Cmd:     []string{"bash", "/var/lib/orthogonals/lg-build.sh"},
			UndoCmd: []string{"rm", "-f", "/usr/local/bin/looking-glass-client"},
		},
		steps.Step{
			ID: KernelArgsStepID, Kind: steps.KindRunCmd, Reboot: true,
			Cmd:     []string{"grubby", "--update-kernel=ALL", "--args=" + args},
			UndoCmd: undoKargsCmd(args, preexisting),
		},
		steps.Step{
			ID: "dracut-regenerate", Kind: steps.KindRunCmd, Reboot: true,
			Cmd: []string{"dracut", "-f", "--regenerate-all"},
			// undo regenerates again so the initramfs matches the restored confs
			UndoCmd: []string{"dracut", "-f", "--regenerate-all"},
		},
		steps.Step{
			ID: "selinux-lg-fcontext", Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "svirt_tmpfs_t", "/dev/shm/looking-glass"},
			UndoCmd: []string{"semanage", "fcontext", "-d", "/dev/shm/looking-glass"},
		},
		steps.Step{
			ID: "lg-shm-restorecon", Kind: steps.KindRunCmd,
			// -i ignores the not-yet-existing shm file: tmpfiles creates it at
			// boot with the fcontext above; this only relabels a pre-existing
			// file left over from a manual setup (PoC incident: user_tmp_t).
			Cmd: []string{"restorecon", "-i", "/dev/shm/looking-glass"},
		},
		steps.Step{
			// the per-VM launchers run `virsh --connect qemu:///system` as
			// the desktop user; Fedora's polkit rule grants that to the libvirt
			// group, so without this the one-click launch prompts for a password
			// every time. usermod -aG is idempotent. No UndoCmd: gpasswd -d would
			// evict a user who was already a member, so undo leaves the harmless
			// group membership in place (documented no-op, like packages).
			ID: "user-libvirt-group", Kind: steps.KindRunCmd,
			Cmd: []string{"usermod", "-aG", "libvirt", p.User},
		},
		steps.Step{ID: "disable-nvidia-persistenced", Kind: steps.KindEnableUnit, Unit: UnitPersistenced, Enable: false},
		steps.Step{ID: "enable-libvirt-guests", Kind: steps.KindEnableUnit, Unit: UnitLibvirtGuests, Enable: true},
		steps.Step{ID: SwitcherooStepID, Kind: steps.KindEnableUnit, Unit: UnitSwitcheroo, Enable: true},
	)
	if !p.DefaultNetActive {
		list = append(list,
			// no UndoCmds: an autostarted idle network is harmless to keep
			steps.Step{ID: "net-default-autostart", Kind: steps.KindRunCmd, Cmd: []string{"virsh", "net-autostart", "default"}},
			// libvirt may autostart the network between plan and execution;
			// already-active must count as success (net-list --name is
			// locale-independent, the error text is not)
			steps.Step{ID: "net-default-start", Kind: steps.KindRunCmd,
				Cmd: []string{"sh", "-c", "virsh net-start default || virsh net-list --name | grep -qx default"}},
		)
	}
	return list, nil
}
