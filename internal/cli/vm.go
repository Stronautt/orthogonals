package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/preflight"
	"github.com/stronautt/orthogonals/internal/steps"
	"github.com/stronautt/orthogonals/internal/virt"
)

// vmOpts carries the flags of vm define, shared with up. A flag left at its
// zero value means "keep what is registered", never "use the default", so no
// flag may declare a default — defaults belong in domain.NewProfile.
type vmOpts struct {
	vmName string
	domain.Settings
	stage string
	purge bool
}

func addVMFlags(fs *pflag.FlagSet, o *vmOpts) {
	// Every "(default: …)" is interpolated from the constant that decides it, or
	// the help goes stale the day a default moves.
	fs.StringVar(&o.vmName, "vm-name", "", "libvirt domain name (default: "+steps.DefaultVMName+")")
	fs.StringVar(&o.DisplayName, "display-name", "", fmt.Sprintf("desktop shortcut name (default: the registered name, %q for the default VM, else the VM name)", defaultDisplayName(steps.DefaultVMName)))
	fs.StringVar(&o.User, "user", "", "desktop user whose ~/Desktop gets the VM shortcut link (default: the registered user, else the user behind sudo)")
	fs.IntVar(&o.RAMGiB, "ram", 0, fmt.Sprintf("guest RAM in GiB (default: %d/%d of host RAM)", domain.DefaultRAMNum, domain.DefaultRAMDen))
	fs.StringVar(&o.Disk, "disk", "", "qcow2 disk image path (default "+filepath.Join(domain.ImagesDir, "<vm-name>.qcow2")+")")
	fs.IntVar(&o.DiskSizeGiB, "disk-size", 0, fmt.Sprintf("disk image size in GiB (default %d)", domain.DefaultDiskSizeGiB))
	fs.StringVar(&o.Resolution, "resolution", "", fmt.Sprintf("maximum guest resolution WxH, sizes the Looking Glass shared memory; the actual mode is picked in Windows display settings (default %dx%d)", domain.DefaultWidth, domain.DefaultHeight))
	fs.StringVar(&o.GuestUser, "guest-user", "", "guest admin account name (default \""+media.DefaultGuestUser+"\")")
	fs.StringVar(&o.GuestPassword, "guest-password", "", "guest admin password (default \""+media.DefaultGuestPassword+"\")")
	fs.StringVar(&o.Locale, "locale", "", "guest locale and keyboard, e.g. uk-UA (default: the Windows ISO's default language)")
	fs.StringVar(&o.Win11ISO, "win11-iso", "", "path to the user-supplied Windows 11 installation ISO, attached as the install CD")
	fs.StringVar(&o.GPUROM, "gpu-rom", "", "path to an extracted GPU vBIOS ROM, installed and rendered as <rom file=>; needed only when a MUXless laptop dGPU gives no guest output")
	fs.StringArrayVar(&o.Shares, "share", nil, "host directory to export to the guest over virtiofs; repeat for more, each taking the next drive letter down from Z:. Passing --share replaces the registered set, --share \"\" clears it")
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
				return finish(stderr, "vm", err)
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
	prev, err := domain.ReadSettings(cfg.Root, o.vmName)
	if err != nil {
		return err
	}
	s, romContent, err := resolveSettings(cfg.Root, m, o, prev)
	if err != nil {
		return err
	}
	if s.Win11ISO == "" && stage != domain.StageFinal {
		fmt.Fprintln(stderr, "usage: orthogonals vm --win11-iso <path> [flags] define")
		return exitCode(2)
	}
	sharesChanged := !slices.Equal(s.Shares, prev.Shares)
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return err
	}
	wantKVMFR := hw.KVMFRAvailable(cfg.Root)
	// Built is not loadable. A domain the hook refuses traps the user: the
	// refusal's remedy is `orthogonals up`, which lands back here.
	if wantKVMFR && !preflight.KVMFRWillLoad(cfg.Root, res.Platform.SecureBoot) {
		fmt.Fprintf(stdout, "kvmfr declined: Secure Boot trusts no key dkms can sign with, so the module cannot load — using %s\n",
			steps.LookingGlassSHM)
		fmt.Fprintf(stdout, "  for the faster path: sudo mokutil --import %s, then choose Enroll MOK at the next reboot\n",
			preflight.DKMSCert)
		wantKVMFR = false
	}
	p, err := domain.NewProfile(res, domain.Options{
		VMName: o.vmName, Settings: s, KVMFR: wantKVMFR,
		VirtioISO:    filepath.Join(media.CacheDir(""), artifacts.VirtioWin.File),
		ProvisionISO: media.ISOPath("", o.vmName),
		ROMContent:   romContent,
	})
	if err != nil {
		return err
	}
	if wantKVMFR && !p.KVMFR {
		fmt.Fprintf(stdout, "kvmfr declined: a %d MiB buffer for %dx%d is more than 1/%d of host RAM — using /dev/shm\n",
			p.IVSHMEMMiB, p.Width, p.Height, domain.KVMFRRAMDivisor)
	}
	if stage != domain.StageInstall && sharesChanged {
		warnLateShares(stdout, p.Name, p.Shares)
	}
	p.ApplyStage(stage)
	if m.Has(domain.DefineStepID(p.Name)) {
		uuid, err := c.DomainUUID(p.Name)
		switch {
		case err == nil:
			p.UUID = uuid
		case cfg.Root == "" && !virt.IsNotFound(err):
			// Swallowed, this re-renders the XML UUID-less and the redefine
			// fails far away with "domain already exists". Under --root there
			// is no daemon to ask, so only a live host treats it as fatal.
			return fmt.Errorf("read UUID of defined domain %s: %w", p.Name, err)
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
	vmSteps, err := hostcfg.VMSteps(p.Name, p.Settings.DisplayName, p.Settings.User, exe)
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

// resolveSettings merges the flags over what the last define registered, then
// normalises the knobs that cannot be a plain copy. Each resolves from the
// merged value, never from the flag and the registered value separately.
func resolveSettings(root string, m *steps.Manifest, o vmOpts, prev domain.Settings) (domain.Settings, []byte, error) {
	s := o.Over(prev)

	// Absolute, so a converge run from another directory registers the same file.
	for _, p := range []*string{&s.Win11ISO, &s.Disk} {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return domain.Settings{}, nil, err
		}
		*p = abs
	}
	// The journal outlives the domain XML an undefine removes, so it is what
	// re-adopts an existing disk when nothing is registered any more.
	if s.Disk == "" {
		if path, size, ok := domain.JournaledDisk(m, o.vmName); ok {
			s.Disk = path
			if s.DiskSizeGiB == 0 {
				s.DiskSizeGiB = size
			}
		}
	}
	romContent, err := resolveGPUROM(root, o.vmName, &s)
	if err != nil {
		return domain.Settings{}, nil, err
	}
	if s.Shares, err = resolveShares(root, s.Shares); err != nil {
		return domain.Settings{}, nil, err
	}
	return hostDefaults(s, o.vmName), romContent, nil
}

// hostDefaults fills the two knobs whose default comes from the host rather
// than from domain.NewProfile. Shared with up: resolved twice, the desktop link
// and the shortcut it names could end up on different users.
func hostDefaults(s domain.Settings, vm string) domain.Settings {
	s.User = cmp.Or(s.User, defaultUser())
	s.DisplayName = cmp.Or(s.DisplayName, defaultDisplayName(vm))
	return s
}

// resolveShares refuses any share that is not an existing directory: libvirt
// starts virtiofsd before the guest, so a bad path costs the whole VM its
// start. A lone empty --share is how the registered set is cleared, and it
// survives the merge as a one-element slice to say so.
func resolveShares(root string, dirs []string) ([]string, error) {
	// nil, not an empty slice: the resolved record has to compare equal to the
	// one read back, and a share-less domain reads back nil.
	if len(dirs) == 0 || (len(dirs) == 1 && dirs[0] == "") {
		return nil, nil
	}
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			return nil, errors.New("--share needs a directory path (pass it alone to clear the registered shares)")
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("--share %s: %w", dir, err)
		}
		fi, err := os.Stat(filepath.Join(root, abs))
		if err != nil {
			return nil, fmt.Errorf("--share %s: %w", dir, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("--share %s is not a directory", dir)
		}
		out = append(out, abs)
	}
	return out, nil
}

// warnLateShares reports that the guest cannot mount a share the domain just
// gained: the mount services are made during provisioning only.
func warnLateShares(w io.Writer, vm string, shares []domain.Share) {
	if len(shares) == 0 {
		fmt.Fprintf(w, "%s: the virtiofs devices are gone from the domain, but its mount services remain in the guest — delete them there with sc.exe delete\n", vm)
		return
	}
	fmt.Fprintf(w, "%s is already installed, so provisioning will not run again to create the guest mount services. In Windows, as Administrator:\n", vm)
	for i, s := range shares {
		verb := "create"
		if i == 0 {
			verb = "config" // virtio-win already installs this one
		}
		fmt.Fprintf(w, "  sc.exe %s %s binPath= \"<virtiofs.exe> -t %s -m %s\" start= auto depend= WinFsp.Launcher/VirtioFsDrv   (%s)\n",
			verb, s.Service, s.Tag, s.Drive, s.Dir)
	}
	fmt.Fprintln(w, "  <virtiofs.exe> is the path already in VirtioFsSvc's ImagePath")
}

// resolveGPUROM rewrites s.GPUROM to the installed copy and returns its bytes.
func resolveGPUROM(root, vm string, s *domain.Settings) ([]byte, error) {
	installed := domain.ROMPath(vm)
	switch s.GPUROM {
	case "":
		return nil, nil
	case installed:
		content, err := os.ReadFile(filepath.Join(root, installed))
		if err != nil {
			return nil, fmt.Errorf("gpu rom %s is registered but unreadable — re-run with --gpu-rom: %w", installed, err)
		}
		return content, nil
	default:
		src, err := filepath.Abs(s.GPUROM)
		if err != nil {
			return nil, err
		}
		content, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("read --gpu-rom %s: %w", s.GPUROM, err)
		}
		s.GPUROM = installed
		return content, nil
	}
}

func defaultDisplayName(vm string) string {
	if vm == steps.DefaultVMName {
		return "Windows 11"
	}
	return vm
}

// displayName is the registered shortcut name; an unreadable registry falls
// back to the default rather than failing a command whose work is already done.
func displayName(root, vm string) string {
	s, _ := domain.ReadSettings(root, vm)
	return cmp.Or(s.DisplayName, defaultDisplayName(vm))
}

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
		hostcfg.RunDirCreateID(name), hostcfg.RunDirConfID(name),
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
	// Not gated on --purge: the ISO is regenerable, unlike the disk image.
	removeProvisionISO(media.ISOPath(cfg.Root, name), cfg.Yes, stdout)
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to undefine")
		return nil
	}
	if o.purge {
		saved, err := orchestrate.SavedVMName(cfg.Root)
		if err != nil || saved == name || saved == "" {
			_ = os.Remove(steps.StatePath(cfg.Root))
		}
		fmt.Fprintf(stdout, "VM and disk image removed — reinstall with: orthogonals up --yes --vm-name %s --win11-iso <iso>\n", name)
	}
	return nil
}

// vmNameOrSole returns flag when set, else the single managed VM name.
// Validated here, not per caller: the name reaches ISOPath, the registry and
// the journal step ids.
func vmNameOrSole(root, flag string) (string, error) {
	if flag != "" {
		if err := steps.CheckVMName(flag); err != nil {
			return "", err
		}
		return flag, nil
	}
	return soleVMName(root)
}

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
