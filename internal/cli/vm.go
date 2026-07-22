package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/virt"
)

// vmOpts carries the flags of vm define, shared with up.
type vmOpts struct {
	vmName        string
	displayName   string
	user          string
	ram           int
	disk          string
	diskSize      int
	resolution    string
	guestUser     string
	guestPassword string
	locale        string
	win11ISO      string
	gpuROM        string
	stage         string
	purge         bool
}

// addVMFlags registers the define flags shared by vm define and up.
func addVMFlags(fs *pflag.FlagSet, o *vmOpts) {
	fs.StringVar(&o.vmName, "vm-name", "", "libvirt domain name (default: win11)")
	fs.StringVar(&o.displayName, "display-name", "", "desktop shortcut name (default: the registered name, \"Windows 11\" for the default VM, else the VM name)")
	fs.StringVar(&o.user, "user", defaultUser(), "desktop user whose ~/Desktop gets the VM shortcut link")
	fs.IntVar(&o.ram, "ram", 0, "guest RAM in GiB (default: 5/8 of host RAM)")
	fs.StringVar(&o.disk, "disk", "", "qcow2 disk image path (default /var/lib/libvirt/images/<vm-name>.qcow2)")
	fs.IntVar(&o.diskSize, "disk-size", 0, "disk image size in GiB (default 100)")
	fs.StringVar(&o.resolution, "resolution", "", "maximum guest resolution WxH, sizes the Looking Glass shared memory; the actual mode is picked in Windows display settings (default 3840x2160)")
	fs.StringVar(&o.guestUser, "guest-user", "", "guest admin account name (default \""+media.DefaultGuestUser+"\")")
	fs.StringVar(&o.guestPassword, "guest-password", "", "guest admin password (default \""+media.DefaultGuestPassword+"\")")
	fs.StringVar(&o.locale, "locale", "", "guest locale and keyboard, e.g. uk-UA (default: the Windows ISO's default language)")
	fs.StringVar(&o.win11ISO, "win11-iso", "", "path to the user-supplied Windows 11 installation ISO, attached as the install CD")
	fs.StringVar(&o.gpuROM, "gpu-rom", "", "path to an extracted GPU vBIOS ROM, installed and rendered as <rom file=>; needed only when a MUXless laptop dGPU gives no guest output")
}

func newVMCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	vm := &cobra.Command{
		Use:   "vm",
		Short: "define, undefine, or launch a managed VM",
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(stderr, "usage: orthogonals vm [flags] define|undefine|launch [flags]")
			return exitCode(2)
		},
	}
	vm.AddCommand(
		newVMDefineCmd(cfg, stdout, stderr),
		newVMUndefineCmd(cfg, stdout, stderr),
		newVMLaunchCmd(cfg, stdout, stderr),
	)
	return vm
}

func newVMDefineCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var o vmOpts
	cmd := &cobra.Command{
		Use:   "define",
		Short: "define a VM or converge an existing one to this binary's settings",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "vm", runVMDefine(cfg, o, stdout, stderr))
		},
	}
	addVMFlags(cmd.Flags(), &o)
	cmd.Flags().StringVar(&o.stage, "stage", "", "pipeline stage to render: install|novideo|final (default: the domain's current stage; the up pipeline advances it)")
	return cmd
}

func newVMUndefineCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var o vmOpts
	cmd := &cobra.Command{
		Use:   "undefine",
		Short: "remove a VM definition, and with --purge its disk image",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return finish(stderr, "vm", runVMUndefine(cfg, o, stdout, stderr))
		},
	}
	cmd.Flags().StringVar(&o.vmName, "vm-name", "", "libvirt domain name (default: the sole managed VM)")
	cmd.Flags().BoolVar(&o.purge, "purge", false, "also delete the disk image and reset the up pipeline, for a from-scratch reinstall")
	return cmd
}

func newVMLaunchCmd(cfg *Config, stdout, stderr io.Writer) *cobra.Command {
	var vmName string
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "start the VM and hand off to looking-glass-client",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			name, err := vmNameOrSole(cfg.Root, vmName)
			if err != nil {
				fmt.Fprintf(stderr, "orthogonals vm: %v\n", err)
				return exitCode(1)
			}
			c := virtClient()
			defer func() { _ = c.Close() }()
			if code := vmLaunch(cfg, c, name, stdout, stderr); code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&vmName, "vm-name", "", "libvirt domain name (default: the sole managed VM)")
	return cmd
}

func runVMDefine(cfg *Config, o vmOpts, stdout, stderr io.Writer) error {
	if o.vmName == "" {
		o.vmName = steps.DefaultVMName
	}
	c := virtClient()
	defer func() { _ = c.Close() }()
	e := newEngine(cfg, stdout, stderr)

	m, err := steps.Load(cfg.Root)
	if err != nil {
		return err
	}
	stage := domain.CurrentStage(cfg.Root, o.vmName)
	if o.stage != "" {
		stage = domain.Stage(o.stage)
		if !slices.Contains(domain.Stages, stage) {
			return fmt.Errorf("unknown --stage %q (install|novideo|final)", o.stage)
		}
	}
	prev := domain.ReadGuestConfig(cfg.Root, o.vmName)
	keep := func(flag, prev string) string {
		if flag != "" {
			return flag
		}
		return prev
	}
	isoPath := prev.Win11ISO
	if o.win11ISO != "" {
		if isoPath, err = filepath.Abs(o.win11ISO); err != nil {
			return err
		}
	}
	if isoPath == "" && stage != domain.StageFinal {
		fmt.Fprintln(stderr, "usage: orthogonals vm --win11-iso <path> [flags] define")
		return exitCode(2)
	}
	w, h, err := parseResolution(keep(o.resolution, prev.Resolution))
	if err != nil {
		return err
	}
	romFile, romContent, err := resolveGPUROM(cfg.Root, o.vmName, o.gpuROM, prev.GPURom)
	if err != nil {
		return err
	}
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return err
	}
	diskPath, diskSizeGiB := o.disk, o.diskSize
	if diskPath == "" {
		if jp, jsize, ok := domain.JournaledDisk(m, o.vmName); ok {
			diskPath = jp
			if diskSizeGiB == 0 {
				diskSizeGiB = jsize
			}
		}
	}
	p, err := domain.NewProfile(res, domain.Options{
		VMName: o.vmName, RAMGiB: o.ram, DiskPath: diskPath, DiskSizeGiB: diskSizeGiB,
		Width: w, Height: h,
		GuestUser:     keep(o.guestUser, prev.User),
		GuestPassword: keep(o.guestPassword, prev.Password),
		Locale:        keep(o.locale, prev.Locale),
		Win11ISO:      isoPath,
		VirtioISO:     filepath.Join(media.CacheDir(""), artifacts.VirtioWin.File),
		ProvisionISO:  media.ISOPath("", o.vmName),
		ROMFile:       romFile,
		ROMContent:    romContent,
	})
	if err != nil {
		return err
	}
	p.ApplyStage(stage)
	if m.Has(domain.DefineStepID(p.Name)) {
		if uuid, err := c.DomainUUID(p.Name); err == nil {
			p.UUID = uuid
		}
	}
	if !m.Has(domain.DiskImageID(p.Name)) {
		if _, err := os.Stat(filepath.Join(cfg.Root, p.DiskPath)); err == nil {
			return fmt.Errorf("disk image %s already exists and is not orthogonals-managed — move it or pass --disk", p.DiskPath)
		}
	}
	list, err := domain.Steps(p)
	if err != nil {
		return err
	}
	exe, err := executablePath()
	if err != nil {
		return err
	}
	vmSteps, err := hostcfg.VMSteps(p.Name, resolveDisplayName(cfg.Root, p.Name, o.displayName), o.user, exe)
	if err != nil {
		return err
	}
	list = append(list, vmSteps...)
	if err := e.Apply(list); err != nil {
		return err
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to apply")
	} else if state, err := c.DomainState(p.Name); err == nil && virt.Live(state) {
		fmt.Fprintf(stdout, "VM %s is %s — the updated definition takes effect on its next boot\n", p.Name, state)
	}
	return nil
}

// resolveGPUROM returns the canonical vBIOS path and bytes: from --gpu-rom, else
// re-read from a registered ROM, else empty.
func resolveGPUROM(root, vm, flag, prev string) (romFile string, content []byte, err error) {
	switch {
	case flag != "":
		src, err := filepath.Abs(flag)
		if err != nil {
			return "", nil, err
		}
		content, err := os.ReadFile(src)
		if err != nil {
			return "", nil, fmt.Errorf("read --gpu-rom %s: %w", flag, err)
		}
		return domain.ROMPath(vm), content, nil
	case prev != "":
		content, err := os.ReadFile(filepath.Join(root, prev))
		if err != nil {
			return "", nil, fmt.Errorf("gpu rom %s is registered but unreadable — re-run with --gpu-rom: %w", prev, err)
		}
		return prev, content, nil
	default:
		return "", nil, nil
	}
}

// resolveDisplayName picks the desktop shortcut name.
func resolveDisplayName(root, name, flag string) string {
	if flag != "" {
		return flag
	}
	if reg := hostcfg.DisplayName(root, name); reg != "" {
		return reg
	}
	if name == steps.DefaultVMName {
		return "Windows 11"
	}
	return name
}

// runVMUndefine removes the domain and its host artifacts, purging the disk with --purge.
func runVMUndefine(cfg *Config, o vmOpts, stdout, stderr io.Writer) error {
	c := virtClient()
	defer func() { _ = c.Close() }()
	e := newEngine(cfg, stdout, stderr)
	name, err := vmNameOrSole(cfg.Root, o.vmName)
	if err != nil {
		return err
	}
	if state, err := c.DomainState(name); err == nil && virt.Live(state) {
		return fmt.Errorf("VM %s is %s — shut it down first: virsh shutdown %s", name, state, name)
	}
	ids := []string{hostcfg.DesktopLinkID(name), hostcfg.DesktopEntryID(name),
		domain.DefineStepID(name),
		domain.ROMRestoreconID(name), domain.ROMFcontextID(name), domain.ROMFileID(name)}
	if o.purge {
		ids = append(ids, domain.DiskRestoreconID(name), domain.DiskFcontextID(name), domain.DiskImageID(name))
	}
	ids = append(ids, domain.DomainXMLID(name))
	any := false
	for _, id := range ids {
		found, err := e.UndoID(id, false)
		if err != nil {
			return err
		}
		any = any || found
	}
	if !any {
		fmt.Fprintf(stdout, "VM %s is not orthogonals-defined, nothing to do\n", name)
		return nil
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to undefine")
		return nil
	}
	if o.purge {
		removeProvisionISO(cfg.Root, name, stdout)
		saved, err := orchestrate.SavedVMName(cfg.Root)
		if err != nil || saved == name || saved == "" {
			_ = os.Remove(steps.StatePath(cfg.Root))
		}
		fmt.Fprintf(stdout, "VM and disk image removed — reinstall with: orthogonals up --yes --vm-name %s --win11-iso <iso>\n", name)
	}
	return nil
}

// vmNameOrSole returns flag when set, else the single managed VM name.
func vmNameOrSole(root, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return soleVMName(root)
}

// soleVMName resolves an omitted --vm-name to the single managed VM.
func soleVMName(root string) (string, error) {
	vms := steps.VMNames(root)
	switch len(vms) {
	case 0:
		return steps.DefaultVMName, nil
	case 1:
		return vms[0], nil
	}
	return "", fmt.Errorf("multiple VMs managed (%s) — pass --vm-name", strings.Join(vms, ", "))
}

// parseResolution parses "1920x1080"; empty picks the default.
func parseResolution(s string) (int, int, error) {
	if s == "" {
		return 0, 0, nil
	}
	ws, hs, ok := strings.Cut(s, "x")
	w, errW := strconv.Atoi(ws)
	h, errH := strconv.Atoi(hs)
	if !ok || errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("bad resolution %q (want WxH, e.g. 1920x1080)", s)
	}
	return w, h, nil
}
