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

	"github.com/stronautt/orthogonals/internal/bls"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/steps"
)

//go:embed templates
var templateFS embed.FS

// Profile is everything host configuration varies on.
type Profile struct {
	User    string
	Binding string
	// IOMMUTable ("DMAR"/"IVRS"/"") selects the IOMMU kernel args; CPUVendor
	// ("intel"/"amd"/"") is the fallback for firmware that exposes no table.
	IOMMUTable string
	CPUVendor  string
	// Laptop gates the RTD3 power-management artifacts and units.
	Laptop           bool
	VFIOIDs          []string
	DefaultNetActive bool
}

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
	return Profile{User: user, Binding: binding,
		IOMMUTable: r.Platform.IOMMUTable, CPUVendor: r.CPU.Vendor,
		Laptop:  hw.IsLaptopChassis(r.Platform.ChassisType),
		VFIOIDs: gpu.VendorDeviceIDs(), DefaultNetActive: defaultNetActive}, nil
}

// BindingDynamic and BindingStatic are the --binding modes.
const (
	BindingDynamic = "dynamic"
	BindingStatic  = "static"
)

const (
	KernelArgsStepID = "kernel-args"
	VFIOIDsPrefix    = "vfio-pci.ids="
)

const (
	UnitPersistenced  = "nvidia-persistenced.service"
	UnitLibvirtGuests = "libvirt-guests.service"
	UnitSwitcheroo    = "switcheroo-control.service"
	// UnitPowerd holds the GPU open, blocking dynamic unbinding; disabled on laptops.
	UnitPowerd = "nvidia-powerd.service"

	SwitcherooStepID = "enable-switcheroo-control"
)

// IOMMUIsAMD reports whether the platform's IOMMU is AMD-Vi. The firmware's
// ACPI table names the technology directly; the CPU vendor answers only for
// firmware that exposes no table yet, before the BIOS switch is flipped.
func IOMMUIsAMD(iommuTable, cpuVendor string) bool {
	if iommuTable != "" {
		return iommuTable == hw.IOMMUTableIVRS
	}
	return cpuVendor == hw.CPUVendorAMD
}

// IOMMUKernelArgs is the single platform→karg mapping: preflight quotes it as
// the remedy, so a private copy there could drift and lie.
func IOMMUKernelArgs(iommuTable, cpuVendor string) string {
	if IOMMUIsAMD(iommuTable, cpuVendor) {
		return "iommu=pt"
	}
	return "intel_iommu=on iommu=pt"
}

// ErrNoKernelArgsStep is a manifest that loaded and holds no kernel-args step.
// Callers must keep it apart from a manifest they could not read at all: it is
// 0600, so an unprivileged caller gets a permission error, and answering that
// with "apply has never run" tells a configured host to configure itself.
var ErrNoKernelArgsStep = errors.New("no journaled kernel-args step — run `orthogonals apply --yes` first")

// JournaledKernelArgs is the kernel args a previous apply recorded, so a check
// answers against what this host was configured with rather than what a fresh
// profile would ask for now.
func JournaledKernelArgs(root string) (string, error) {
	m, err := steps.Load(root)
	if err != nil {
		return "", err
	}
	if args := m.OpArgs(KernelArgsStepID); args["args"] != "" {
		return args["args"], nil
	}
	return "", ErrNoKernelArgsStep
}

func KernelArgs(p Profile) string {
	args := IOMMUKernelArgs(p.IOMMUTable, p.CPUVendor)
	if p.Binding == BindingStatic {
		return args + " " + VFIOIDsPrefix + strings.Join(p.VFIOIDs, ",")
	}
	return args
}

// kernelArgsStep adds args to every boot-config target, undoing only what it
// added. A token any target lacks rechecks the journaled step: kernel-install
// regenerates the entries from /etc/kernel/cmdline on every kernel update, so
// "already applied" is not "still there".
//
// The undo is what each target was missing, per target: a host whose targets
// disagreed before apply gets a different set written to each, and a single
// union would either strip a token a file carried beforehand or, erring the
// other way, leave everything apply wrote.
func kernelArgsStep(args string, boot bls.Args) steps.Step {
	s := steps.Step{
		ID: KernelArgsStepID, Kind: steps.KindOp, Reboot: true,
		Op: steps.OpKernelArgsAdd, Args: map[string]string{"args": args},
		Recheck: len(boot.Missing) > 0,
	}
	// Gate on Missing, not on MissingIn: MissingIn names every target whether or
	// not it needs anything, so its length says nothing about whether this step
	// will write.
	if len(boot.Missing) > 0 {
		s.UndoOp = steps.OpKernelArgsRem
		s.UndoArgs = boot.MissingIn
	}
	return s
}

func DesktopEntryID(vm string) string { return "desktop-entry-" + vm }
func DesktopLinkID(vm string) string  { return "desktop-link-" + vm }

func RunDirConfID(vm string) string   { return "vm-rundir-conf-" + vm }
func RunDirCreateID(vm string) string { return "vm-rundir-create-" + vm }

func runDirConfPath(vm string) string { return "/etc/tmpfiles.d/orthogonals-" + vm + ".conf" }

// Artifact is one rendered configuration file ready for a WriteFile step.
type Artifact struct {
	ID      string
	Path    string
	Mode    fs.FileMode
	Content []byte
}

type tplSpec struct {
	tpl, path, id string
	mode          fs.FileMode
}

// tmpfilesLookingGlass is both where the fragment installs and what the
// install-run systemd-tmpfiles --create names; two literals drift.
const tmpfilesLookingGlass = "/etc/tmpfiles.d/looking-glass.conf"

// artifactSpecs maps templates to install paths, in apply order.
var artifactSpecs = []tplSpec{
	{"vfio.conf", "/etc/dracut.conf.d/vfio.conf", "dracut-vfio-conf", 0o644},
	// Assumes Fedora's modular libvirt (virtqemud), not monolithic libvirtd.
	{"virtqemud.conf", "/etc/libvirt/virtqemud.conf", "libvirt-socket-auth", 0o644},
	{"virtqemud-socket.conf", "/etc/systemd/system/virtqemud.socket.d/orthogonals.conf", "libvirt-socket-perms", 0o644},
	{"61-mutter-ignore-nvidia.rules", "/etc/udev/rules.d/61-mutter-ignore-nvidia.rules", "udev-mutter-ignore", 0o644},
	{"50-orthogonals-igpu.conf", "/etc/environment.d/50-orthogonals-igpu.conf", "environment-igpu-pins", 0o644},
	{"looking-glass.conf", tmpfilesLookingGlass, "tmpfiles-looking-glass", 0o644},
	{"libvirt-guests", "/etc/sysconfig/libvirt-guests", "sysconfig-libvirt-guests", 0o644},
}

// laptopArtifactSpecs are the RTD3 artifacts added on laptop hosts; the modprobe.d
// conf must precede the dracut-regenerate step to reach the initramfs.
var laptopArtifactSpecs = []tplSpec{
	{"nvidia-rtd3.conf", "/etc/modprobe.d/nvidia-rtd3.conf", "nvidia-rtd3", 0o644},
	{"80-orthogonals-nvidia-pm.rules", "/etc/udev/rules.d/80-orthogonals-nvidia-pm.rules", "udev-nvidia-pm", 0o644},
}

var templates = template.Must(template.ParseFS(templateFS, "templates/*"))

func renderTemplate(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := templates.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// VMSteps renders the per-VM artifacts `vm define` journals.
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
	data := struct{ VMName, DisplayName, Exe, User, AppID string }{vmName, displayName, exe, user, DesktopAppID(vmName)}
	content, err := renderTemplate("vm-looking-glass.desktop", data)
	if err != nil {
		return nil, err
	}
	runDir, err := renderTemplate("vm-rundir.conf", data)
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
		ID: DesktopLinkID(vmName), Kind: steps.KindOp,
		Op: steps.OpDesktopLink,
		Args: map[string]string{
			"user":  user,
			"entry": desktopEntryPath(vmName),
			"link":  link,
		},
		UndoOp:      steps.OpRemoveFile,
		UndoArgs:    map[string]string{"path": link},
		CreatesPath: link,
	})
	return append(list,
		steps.Step{
			ID: RunDirConfID(vmName), Kind: steps.KindWriteFile,
			Path: runDirConfPath(vmName), Content: runDir, Mode: 0o644,
		},
		// CreatesPath is in /run: re-runs once per boot, never otherwise.
		steps.Step{
			ID: RunDirCreateID(vmName), Kind: steps.KindRunCmd,
			Cmd:         []string{"systemd-tmpfiles", "--create", runDirConfPath(vmName)},
			Input:       runDir,
			CreatesPath: steps.VMRunDir(vmName),
		},
	), nil
}

// DesktopAppID is the entry's basename and the window app-id `vm launch` hands
// looking-glass-client — the shell matches a window to its launcher by that
// pair, and without the match the dock shows "looking-glass-client" and the
// stock binary icon instead of the VM's name and icon. The .orthogonals marker
// also keeps the entry from colliding with a distro one.
func DesktopAppID(vm string) string { return vm + ".orthogonals" }

func desktopEntryPath(vm string) string {
	return "/usr/share/applications/" + DesktopAppID(vm) + ".desktop"
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

func vulkanDriverSelect(igpuVendor string) string {
	if igpuVendor == hw.VendorAMD {
		return "*radeon*"
	}
	return "*intel*"
}

// IGPUOverrides renders iGPU-Vulkan-only copies of the installed igpuApps entries.
func IGPUOverrides(root, igpuVendor string) ([]Artifact, error) {
	driver := vulkanDriverSelect(igpuVendor)
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
				lines[i] = "Exec=env VK_LOADER_DRIVERS_SELECT=" + driver + " " + cmd
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

func renderArtifacts(p Profile) ([]Artifact, error) {
	specs := artifactSpecs
	if p.Laptop {
		specs = append(slices.Clone(artifactSpecs), laptopArtifactSpecs...)
	}
	out := make([]Artifact, 0, len(specs))
	for _, spec := range specs {
		content, err := renderTemplate(spec.tpl, p)
		if err != nil {
			return nil, err
		}
		out = append(out, Artifact{ID: spec.id, Path: spec.path, Mode: spec.mode, Content: content})
	}
	return out, nil
}

// Steps assembles the ordered host-configuration step list. boot is the live
// state of the kernel args; its zero value reads as a host carrying none.
func Steps(p Profile, boot bls.Args, qemuConf string) ([]steps.Step, error) {
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
	// Before libvirt-socket-reload below, which restarts virtqemud and so makes
	// the new ACL live.
	acl, err := DeviceACLStep(qemuConf)
	if err != nil {
		return nil, err
	}
	list = append(list, acl)
	args := KernelArgs(p)
	list = append(list,
		steps.Step{
			ID: "libvirt-socket-reload", Kind: steps.KindOp,
			Op: steps.OpSocketReload, UndoOp: steps.OpSocketReload,
		},
		kernelArgsStep(args, boot),
		steps.Step{
			ID: "dracut-regenerate", Kind: steps.KindRunCmd, Reboot: true,
			Cmd:     []string{"dracut", "-f", "--regenerate-all"},
			UndoCmd: []string{"dracut", "-f", "--regenerate-all"},
		},
		steps.Step{
			ID: "selinux-lg-fcontext", Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "svirt_tmpfs_t", steps.LookingGlassSHM},
			UndoCmd: []string{"semanage", "fcontext", "-d", steps.LookingGlassSHM},
		},
		steps.Step{
			ID: "lg-shm-restorecon", Kind: steps.KindRunCmd,
			Cmd: []string{"restorecon", "-i", steps.LookingGlassSHM},
		},
		// qemu_var_run_t is the policy's type for /var/lib/libvirt/qemu, where
		// libvirt drops its own SPICE sockets; svirt_var_run_t does not exist.
		// The rule, not a restorecon, is what survives /run being volatile.
		steps.Step{
			ID: "selinux-spice-fcontext", Kind: steps.KindRunCmd,
			Cmd:     []string{"semanage", "fcontext", "-a", "-t", "qemu_var_run_t", steps.RunDirPath + "(/.*)?"},
			UndoCmd: []string{"semanage", "fcontext", "-d", steps.RunDirPath + "(/.*)?"},
		},
		// tmpfiles.d owns these from the next boot; this covers the install run.
		steps.Step{
			ID: "tmpfiles-create", Kind: steps.KindRunCmd,
			Cmd: []string{"systemd-tmpfiles", "--create", tmpfilesLookingGlass},
		},
		steps.Step{
			ID: "spice-rundir-restorecon", Kind: steps.KindRunCmd,
			Cmd: []string{"restorecon", "-R", "-i", steps.RunDirPath},
		},
		// No UndoCmd on purpose: the user may have been in libvirt before this
		// ever ran, and removing them on undo would take away access orthogonals
		// never granted.
		steps.Step{
			ID: "user-libvirt-group", Kind: steps.KindRunCmd,
			Cmd: []string{"usermod", "-aG", "libvirt", p.User},
		},
		steps.Step{ID: "disable-nvidia-persistenced", Kind: steps.KindEnableUnit, Unit: UnitPersistenced, Enable: false},
		steps.Step{ID: "enable-libvirt-guests", Kind: steps.KindEnableUnit, Unit: UnitLibvirtGuests, Enable: true},
		steps.Step{ID: SwitcherooStepID, Kind: steps.KindEnableUnit, Unit: UnitSwitcheroo, Enable: true},
	)
	if p.Laptop {
		list = append(list, steps.Step{ID: "disable-nvidia-powerd", Kind: steps.KindEnableUnit, Unit: UnitPowerd, Enable: false})
	}
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
