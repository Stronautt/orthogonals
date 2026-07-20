package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stronautt/orthogonals/internal/artifacts"
	"github.com/stronautt/orthogonals/internal/domain"
	"github.com/stronautt/orthogonals/internal/hostcfg"
	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/media"
	"github.com/stronautt/orthogonals/internal/orchestrate"
	"github.com/stronautt/orthogonals/internal/steps"
)

func cmdVM(cfg *Config, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet(cfg, stderr)
	vmName := fs.String("vm-name", "", "libvirt domain name (default: win11 for define, the sole managed VM for undefine)")
	displayName := fs.String("display-name", "", "desktop shortcut name (default: the registered name, \"Windows 11\" for the default VM, else the VM name)")
	user := fs.String("user", defaultUser(), "desktop user whose ~/Desktop gets the VM shortcut link")
	ram := fs.Int("ram", 0, "guest RAM in GiB (default min(half of host RAM, 16))")
	disk := fs.String("disk", "", "qcow2 disk image path (default /var/lib/libvirt/images/<vm-name>.qcow2)")
	diskSize := fs.Int("disk-size", 0, "disk image size in GiB (default 100)")
	resolution := fs.String("resolution", "", "maximum guest resolution WxH, sizes the Looking Glass shared memory; the actual mode is picked in Windows display settings (default 3840x2160)")
	guestUser := fs.String("guest-user", "", "guest admin account name (default \""+media.DefaultGuestUser+"\")")
	guestPassword := fs.String("guest-password", "", "guest admin password (default \""+media.DefaultGuestPassword+"\")")
	locale := fs.String("locale", "", "guest locale and keyboard, e.g. uk-UA (default: the Windows ISO's default language)")
	win11ISO := fs.String("win11-iso", "", "path to the user-supplied Windows 11 installation ISO, attached as the install CD")
	purge := fs.Bool("purge", false, "with undefine: also delete the disk image and reset the up pipeline, for a from-scratch reinstall")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "orthogonals vm: %v\n", err)
		return 1
	}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: orthogonals vm [flags] define|undefine [flags]")
		return 2
	}
	rest := fs.Args()
	if len(rest) == 0 || (rest[0] != "define" && rest[0] != "undefine") {
		return usage()
	}
	verb := rest[0]
	// stdlib flag stops at the first non-flag argument, so flags placed
	// after the verb (vm undefine --purge --yes) need a second parse
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}
	if len(fs.Args()) != 0 {
		return usage()
	}
	e := &steps.Engine{Root: cfg.Root, Yes: cfg.Yes, Out: stdout, Err: stderr}
	if verb == "undefine" {
		name := *vmName
		if name == "" {
			var err error
			if name, err = soleVMName(cfg.Root); err != nil {
				return fail(err)
			}
		}
		return vmUndefine(cfg, e, name, *purge, stdout, stderr)
	}
	if *vmName == "" {
		*vmName = steps.DefaultVMName
	}

	m, err := steps.Load(cfg.Root)
	if err != nil {
		return fail(err)
	}
	// the install ISO is load-bearing only until the pipeline detached the
	// installer media; a converging redefine of an installed VM needs no ISO
	if *win11ISO == "" && !domain.InstallMediaDetached(m, *vmName) {
		fmt.Fprintln(stderr, "usage: orthogonals vm --win11-iso <path> [flags] define")
		return 2
	}
	isoPath := ""
	if *win11ISO != "" {
		if isoPath, err = filepath.Abs(*win11ISO); err != nil {
			return fail(err)
		}
	}
	// re-defines keep the guest settings the VM's <metadata> block already
	// carries unless a flag overrides them — a rebuild must not silently
	// reset credentials or locale on an installed guest
	prev := domain.ReadGuestConfig(cfg.Root, *vmName)
	keep := func(flag, prev string) string {
		if flag != "" {
			return flag
		}
		return prev
	}
	w, h, err := parseResolution(keep(*resolution, prev.Resolution))
	if err != nil {
		return fail(err)
	}
	res, err := hw.Detect(cfg.Root)
	if err != nil {
		return fail(err)
	}
	diskPath, diskSizeGiB := *disk, *diskSize
	if diskPath == "" {
		if jp, jsize, ok := domain.JournaledDisk(m, *vmName); ok {
			diskPath = jp
			if diskSizeGiB == 0 {
				diskSizeGiB = jsize
			}
		}
	}
	p, err := domain.NewProfile(res, domain.Options{
		VMName: *vmName, RAMGiB: *ram, DiskPath: diskPath, DiskSizeGiB: diskSizeGiB,
		Width: w, Height: h,
		GuestUser:     keep(*guestUser, prev.User),
		GuestPassword: keep(*guestPassword, prev.Password),
		Locale:        keep(*locale, prev.Locale),
		// un-rooted host paths: the XML is read by libvirt on the real host.
		// startupPolicy=optional keeps the VM bootable after these are deleted.
		Win11ISO:     isoPath,
		VirtioISO:    filepath.Join(media.CacheDir(""), artifacts.VirtioWin.File),
		ProvisionISO: media.ISOPath("", *vmName),
	})
	if err != nil {
		return fail(err)
	}
	domain.Converge(&p, m)
	// a redefine must carry the live domain's UUID: virsh define refuses a
	// name that already exists under a different one
	if m.Has(domain.DefineStepID(p.Name)) {
		p.UUID = steps.DomainUUID(p.Name)
	}
	// qemu-img create truncates: refuse to run it over a disk image we did
	// not create (a journaled step means the image is ours and is skipped on
	// re-runs anyway)
	if !m.Has(domain.DiskImageID(p.Name)) {
		if _, err := os.Stat(filepath.Join(cfg.Root, p.DiskPath)); err == nil {
			return fail(fmt.Errorf("disk image %s already exists and is not orthogonals-managed — move it or pass --disk", p.DiskPath))
		}
	}
	list, err := domain.Steps(p)
	if err != nil {
		return fail(err)
	}
	vmSteps, err := hostcfg.VMSteps(p.Name, resolveDisplayName(cfg.Root, p.Name, *displayName), *user)
	if err != nil {
		return fail(err)
	}
	list = append(list, vmSteps...)
	if err := e.Apply(list); err != nil {
		return fail(err)
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to apply")
	} else if state := steps.DomainState(p.Name); steps.DomainLive(state) {
		// virsh define only replaces the persistent config of a live domain
		fmt.Fprintf(stdout, "VM %s is %s — the updated definition takes effect on its next boot\n", p.Name, state)
	}
	return 0
}

// resolveDisplayName picks the desktop shortcut name: the flag, the existing
// registration (so re-defines keep it), "Windows 11" for the default VM,
// else the domain name.
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

// vmUndefine removes the domain via the journaled define step's paired undo
// command, so a later full `orthogonals undo` never replays virsh undefine
// against a domain that is already gone — plus the VM's XML, launcher, and
// desktop entry. With purge it also undoes the disk records (the disk image
// is the Windows install) and, for the VM the up pipeline built, resets the
// pipeline state so a following `up --yes` reinstalls from scratch.
func vmUndefine(cfg *Config, e *steps.Engine, name string, purge bool, stdout, stderr io.Writer) int {
	if state := steps.DomainState(name); steps.DomainLive(state) {
		fmt.Fprintf(stderr, "orthogonals vm: VM %s is %s — shut it down first: virsh shutdown %s\n", name, state, name)
		return 1
	}
	// reverse apply order; the media-detach and install-video edits rode on
	// the defined domain
	ids := append(domain.DetachMediaStepIDs(name),
		domain.InstallVideoStepID(name), hostcfg.DesktopLinkID(name), hostcfg.DesktopEntryID(name),
		hostcfg.LauncherName(name), domain.DefineStepID(name))
	if purge {
		// the disk records go only after the domain
		ids = append(ids, domain.DiskRestoreconID(name), domain.DiskFcontextID(name), domain.DiskImageID(name))
	}
	// the domain XML is the registry entry: it goes last, so undo's guards and
	// the hook dispatcher keep covering the domain until it is gone
	ids = append(ids, domain.DomainXMLID(name))
	any := false
	for _, id := range ids {
		found, err := e.UndoID(id, false)
		if err != nil {
			fmt.Fprintf(stderr, "orthogonals vm: %v\n", err)
			return 1
		}
		any = any || found
	}
	if !any {
		fmt.Fprintf(stdout, "VM %s is not orthogonals-defined, nothing to do\n", name)
		return 0
	}
	if !cfg.Yes {
		fmt.Fprintln(stdout, "dry run — re-run with --yes to undefine")
		return 0
	}
	if purge {
		// the VM's provision ISO carries its guest password; a purged VM
		// never needs it again
		removeProvisionISO(cfg.Root, name, stdout)
		// a stale "verified" state would make the next `up --yes` claim setup
		// is already complete instead of rebuilding. Scoped to the VM the
		// pipeline built (a nameless state file predates multi-VM and can only
		// mean the sole pipeline VM) — purging a secondary VM must not reset it.
		saved, err := orchestrate.SavedVMName(cfg.Root)
		if err != nil || saved == name || saved == "" {
			// an unreadable state.json is stale pipeline garbage — purge
			// resets it rather than leaving the next `up` to choke on it
			_ = os.Remove(steps.StatePath(cfg.Root))
		}
		fmt.Fprintf(stdout, "VM and disk image removed — reinstall with: orthogonals up --yes --vm-name %s --win11-iso <iso>\n", name)
	}
	return 0
}

// soleVMName resolves an omitted --vm-name to the single managed VM; with
// more than one registered the flag is required, with none the default
// stands in.
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
