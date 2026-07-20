package steps

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw"
)

// DefaultVMName matches the Defaults table; `vm define --vm-name` overrides
// it, and the registry records the name actually applied.
const DefaultVMName = "win11"

// CheckVMName rejects names that would break the root-run scripts and XML the
// name is interpolated into (hook scripts, domain XML, virsh argv).
func CheckVMName(name string) error {
	if name == "" {
		return errors.New("a VM name is required")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return fmt.Errorf("bad VM name %q: use letters, digits, '_', '-', '.'", name)
		}
	}
	return nil
}

// CheckUser rejects usernames that would break the root-run scripts the name
// is interpolated into (the tmpfiles.d owner column, shell-quoted hook
// variables) or be mistaken for a flag by usermod/virsh. Matches the portable
// Linux username set (useradd NAME_REGEX): a letter or '_', then letters,
// digits, '_' or '-'.
func CheckUser(user string) error {
	if user == "" {
		return errors.New("a desktop user is required (--user, or run via sudo)")
	}
	for i, r := range user {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case (r >= '0' && r <= '9' || r == '-') && i > 0:
		default:
			return fmt.Errorf("bad user %q: use a letter or '_', then letters, digits, '_' or '-'", user)
		}
	}
	return nil
}

// EtcDir is the orthogonals config directory; every host-side config path
// derives from it.
const EtcDir = "/etc/orthogonals"

// VMsDirPath is the managed-VM registry: one <name>.xml domain definition per
// managed domain — the file `virsh define` reads doubles as the membership
// marker. `vm define` journals it; the qemu hook dispatcher gates on
// membership at runtime, so adding or removing a VM never rewrites the hook.
const VMsDirPath = EtcDir + "/vms"

// VMsDir is VMsDirPath under root (the test seam).
func VMsDir(root string) string { return filepath.Join(root, VMsDirPath) }

// LibvirtRunDir holds libvirt's live-domain state XML, one file per running
// domain — the qemu hook dispatcher's one-VM-at-a-time check keys on it (the
// hook runs inside libvirt, where the default URI is a given; Go code asks
// virsh via DomainState instead).
const LibvirtRunDir = "/run/libvirt/qemu"

// DomainState reports a domain's virsh state ("running", "shut off", ...),
// "" when the domain does not exist or libvirt is unreachable. LC_ALL=C:
// callers match English tokens and virsh localizes.
func DomainState(name string) string {
	cmd := exec.Command("virsh", "domstate", name)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// DomainUUID reports a defined domain's UUID, "" when the domain does not
// exist or libvirt is unreachable. A redefine must render it into the XML:
// virsh define refuses a name that already exists under a different UUID.
func DomainUUID(name string) string {
	cmd := exec.Command("virsh", "domuuid", name)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line
}

// DomainLive reports whether a DomainState means the guest still holds its
// resources (the GPU included) — anything between start and a completed
// shutdown or destroy.
func DomainLive(state string) bool {
	switch state {
	case "running", "paused", "pmsuspended", "in shutdown":
		return true
	}
	return false
}

// VMNames lists every managed domain from the registry; empty when no VM is
// defined.
func VMNames(root string) []string {
	ents, _ := os.ReadDir(VMsDir(root))
	var names []string
	for _, ent := range ents {
		if name, ok := strings.CutSuffix(ent.Name(), ".xml"); ok && !ent.IsDir() {
			names = append(names, name)
		}
	}
	return names
}

// undoPreconditions refuses to undo while a guest or the GPU still hold
// the vfio setup (research §C2): restoring host config under a defined VM or
// a vfio-bound GPU strands the host half torn down. Domain state comes from
// virsh, not the filesystem, so non-default libvirt URIs are covered; a host
// without a reachable libvirt has no domains to hold anything.
func (e *Engine) undoPreconditions() error {
	for _, vm := range VMNames(e.Root) {
		switch state := DomainState(vm); {
		case DomainLive(state):
			return fmt.Errorf("VM %s is %s — shut it down first: virsh shutdown %s", vm, state, vm)
		case state != "":
			// vm undefine also drops the journaled define step, so a later full
			// undo does not replay virsh undefine against a missing domain
			return fmt.Errorf("VM %s is still defined — remove it first: orthogonals vm undefine --vm-name %s --yes (or virsh undefine %s --nvram --tpm)", vm, vm, vm)
		}
	}
	devs, _ := hw.ScanPCI(e.Root) // missing sysfs under root = nothing bound
	for _, d := range devs {
		if d.Vendor == hw.VendorNVIDIA && d.Driver == "vfio-pci" {
			return fmt.Errorf("GPU %s is bound to vfio-pci — reattach it to the host driver first: virsh nodedev-reattach pci_%s (or reboot)",
				d.Address, strings.NewReplacer(":", "_", ".", "_").Replace(d.Address))
		}
	}
	return nil
}

// printDrift reports kernel/driver updates since apply (research §C2) — undo
// still proceeds; this is context for "undo broke my host" bug reports.
func (e *Engine) printDrift(m *Manifest) {
	if cur := hw.KernelVersion(e.Root); m.Kernel != "" && cur != "" && cur != m.Kernel {
		fmt.Fprintf(e.Out, "note: host kernel has since updated %s → %s\n", m.Kernel, cur)
	}
	if m.NVIDIAVersion == "" {
		return
	}
	n := hw.DetectNVIDIA(e.Root)
	cur := "not loaded"
	if n.Loaded {
		cur = fmt.Sprintf("%s (%s)", n.Version, n.Flavor)
	}
	if was := fmt.Sprintf("%s (%s)", m.NVIDIAVersion, m.NVIDIAFlavor); cur != was {
		fmt.Fprintf(e.Out, "note: NVIDIA driver has since updated %s → %s\n", was, cur)
	}
}

// Undo replays the manifest in reverse: file and unit restores first, paired
// undo commands after, so regeneration commands (dracut) see their restored
// inputs. Data steps (disk image, caches) are kept unless purge, which also
// removes the state dir and config behind a typed confirmation.
func (e *Engine) Undo(force, purge bool, confirm io.Reader) error {
	m, err := Load(e.Root)
	if err != nil {
		return err
	}
	if len(m.Records) == 0 && !purge {
		fmt.Fprintln(e.Out, "nothing to undo")
		return nil
	}
	if err := e.undoPreconditions(); err != nil {
		return err
	}
	e.printDrift(m)
	if purge && e.Yes {
		fmt.Fprint(e.Out, "--purge deletes the VM disk image, ISO cache, state, and config.\ntype \"purge\" to confirm: ")
		line, _ := bufio.NewReader(confirm).ReadString('\n')
		if strings.TrimSpace(line) != "purge" {
			return errors.New("purge not confirmed, nothing done")
		}
	}

	all := m.Records
	done := map[string]bool{}
	saveProgress := func() error {
		keep := make([]Record, 0, len(all))
		for _, r := range all {
			if !done[r.ID] {
				keep = append(keep, r)
			}
		}
		left := *m // keep the apply-time stamps with whatever records remain
		left.Records = keep
		return left.save(e.Root)
	}

	var restores, cmds []Record
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Kind == KindRunCmd {
			cmds = append(cmds, all[i])
		} else {
			restores = append(restores, all[i])
		}
	}
	kept := 0
	needReboot := false
	for _, rec := range append(restores, cmds...) {
		if rec.Data && !purge {
			fmt.Fprintf(e.Out, "%s: data step kept (undo --purge removes it)\n", rec.ID)
			kept++
			continue
		}
		ok, err := e.undoRecord(rec, force)
		if err != nil {
			return fmt.Errorf("undo %s: %w", rec.ID, err)
		}
		if !ok {
			kept++
			continue
		}
		if rec.Reboot {
			needReboot = true
		}
		if e.Yes {
			done[rec.ID] = true
			if err := saveProgress(); err != nil {
				return err
			}
		}
	}

	if !e.Yes {
		if needReboot {
			fmt.Fprintln(e.Out, "undo will restore boot configuration — a reboot will be required")
		}
		if purge {
			fmt.Fprintf(e.Out, "would remove %s and %s\n",
				StateDir(e.Root), filepath.Join(e.Root, EtcDir))
		}
		fmt.Fprintln(e.Out, "dry run — re-run with --yes to undo")
		return nil
	}
	if needReboot {
		fmt.Fprintln(e.Out, "reboot required: boot configuration was restored")
	}
	// the up pipeline position is stale once anything was undone; a leftover
	// "verified" state would make a later `up --yes` claim setup is complete
	_ = os.Remove(StatePath(e.Root))
	if purge {
		if kept > 0 {
			// purging now would delete the backups of the very files that
			// were just skipped for drift — unrecoverable
			return fmt.Errorf("%d step(s) were skipped (changed since apply) — their backups are kept; re-run `undo --force --purge` to restore them and purge", kept)
		}
		if err := os.RemoveAll(StateDir(e.Root)); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(e.Root, EtcDir)); err != nil {
			return err
		}
		fmt.Fprintln(e.Out, "purged state, config, and caches")
		return nil
	}
	if kept == 0 {
		_ = os.Remove(ManifestPath(e.Root))
		if err := os.RemoveAll(backupDir(e.Root)); err != nil {
			return err
		}
		_ = os.Remove(StateDir(e.Root)) // only removes when empty; caches stay
		fmt.Fprintln(e.Out, "undo complete")
	} else {
		fmt.Fprintf(e.Out, "undo finished, %d step(s) kept in the manifest\n", kept)
	}
	return nil
}

// UndoID reverses one journaled record and drops it from the manifest —
// `orthogonals vm undefine` uses it to remove the domain while the rest of
// the host setup stays applied. found reports whether the record existed.
func (e *Engine) UndoID(id string, force bool) (found bool, err error) {
	m, err := Load(e.Root)
	if err != nil {
		return false, err
	}
	rec := m.find(id)
	if rec == nil {
		return false, nil
	}
	ok, err := e.undoRecord(*rec, force)
	if err != nil {
		return true, fmt.Errorf("undo %s: %w", id, err)
	}
	if !ok || !e.Yes {
		return true, nil
	}
	keep := make([]Record, 0, len(m.Records))
	for _, r := range m.Records {
		if r.ID != id {
			keep = append(keep, r)
		}
	}
	m.Records = keep
	return true, m.save(e.Root)
}

// undoRecord reverses one record; ok reports whether it can leave the manifest.
func (e *Engine) undoRecord(rec Record, force bool) (bool, error) {
	switch rec.Kind {
	case KindWriteFile:
		return e.undoWriteFile(rec, force)
	case KindRunCmd:
		return e.undoRunCmd(rec)
	case KindEnableUnit:
		return e.undoEnableUnit(rec)
	}
	return false, fmt.Errorf("unknown record kind %q", rec.Kind)
}

func (e *Engine) undoWriteFile(rec Record, force bool) (bool, error) {
	full := filepath.Join(e.Root, rec.Path)
	cur, err := os.ReadFile(full)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return false, err
	}
	if missing && !rec.Existed {
		fmt.Fprintf(e.Out, "%s: already removed\n", rec.Path)
		if e.Yes {
			removeDirs(e.Root, rec.MadeDirs)
		}
		return true, nil
	}
	if (missing || sha256hex(cur) != rec.NewSHA256) && !force {
		fmt.Fprintf(e.Err, "%s: changed since apply, skipping (undo --force restores anyway)\n", rec.Path)
		return false, nil
	}
	if !rec.Existed {
		if !e.Yes {
			fmt.Fprintf(e.Out, "would remove %s\n", rec.Path)
			return true, nil
		}
		if err := os.Remove(full); err != nil {
			return false, err
		}
		removeDirs(e.Root, rec.MadeDirs)
		fmt.Fprintf(e.Out, "removed %s\n", rec.Path)
		return true, nil
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would restore %s\n", rec.Path)
		return true, nil
	}
	orig, err := os.ReadFile(filepath.Join(backupDir(e.Root), rec.Backup))
	if err != nil {
		return false, err
	}
	if err := e.writeFile(full, orig, fs.FileMode(rec.OrigMode), rec.Restorecon); err != nil {
		return false, err
	}
	fmt.Fprintf(e.Out, "restored %s\n", rec.Path)
	return true, nil
}

func (e *Engine) undoRunCmd(rec Record) (bool, error) {
	if len(rec.UndoCmd) == 0 {
		fmt.Fprintf(e.Out, "%s: no undo command\n", rec.ID)
		return true, nil
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would run: %s\n", strings.Join(rec.UndoCmd, " "))
		return true, nil
	}
	if err := RunCmd(e.Out, rec.UndoCmd...); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) undoEnableUnit(rec Record) (bool, error) {
	var verb string
	switch rec.PriorState {
	case "enabled":
		verb = "enable"
	case "disabled":
		verb = "disable"
	default:
		fmt.Fprintf(e.Out, "%s: prior state %q not restorable, leaving unit as-is\n", rec.Unit, rec.PriorState)
		return true, nil
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would run: systemctl %s %s\n", verb, rec.Unit)
		return true, nil
	}
	if err := RunCmd(e.Out, "systemctl", verb, rec.Unit); err != nil {
		return false, err
	}
	return true, nil
}

func removeDirs(root string, dirs []string) {
	for _, d := range dirs {
		_ = os.Remove(filepath.Join(root, d)) // deepest first; harmlessly fails when non-empty
	}
}
