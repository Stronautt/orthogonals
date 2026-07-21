package steps

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stronautt/orthogonals/internal/hw"
	"github.com/stronautt/orthogonals/internal/virt"
)

// DefaultVMName is the default VM name.
const DefaultVMName = "win11"

// CheckVMName rejects VM names unsafe to interpolate into scripts and XML.
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

// CheckUser rejects usernames unsafe to interpolate into root-run scripts.
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

// CheckExecPath rejects an executable path unsafe to interpolate into a shell.
func CheckExecPath(exe string) error {
	if exe == "" || exe[0] != '/' {
		return fmt.Errorf("bad executable path %q: must be absolute", exe)
	}
	for _, r := range exe {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == '/':
		default:
			return fmt.Errorf("bad executable path %q: use letters, digits, '_', '-', '.', '/'", exe)
		}
	}
	return nil
}

// EtcDir is the orthogonals config directory.
const EtcDir = "/etc/orthogonals"

// VMsDirPath is the managed-VM registry directory.
const VMsDirPath = EtcDir + "/vms"

// VMsDir is VMsDirPath under root.
func VMsDir(root string) string { return filepath.Join(root, VMsDirPath) }

// LibvirtRunDir holds libvirt's live-domain state XML.
const LibvirtRunDir = "/run/libvirt/qemu"

// VMNames lists every managed domain from the registry.
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

// undoPreconditions refuses to undo while a VM or the GPU still holds the vfio setup.
func (e *Engine) undoPreconditions(oc *OpClients) error {
	if vms := VMNames(e.Root); len(vms) > 0 && !e.skipUnderRoot(oc) {
		for _, vm := range vms {
			state, err := oc.Virt().DomainState(vm)
			if err != nil {
				continue
			}
			if virt.Live(state) {
				return fmt.Errorf("VM %s is %s — shut it down first: virsh shutdown %s", vm, state, vm)
			}
			return fmt.Errorf("VM %s is still defined — remove it first: orthogonals vm undefine --vm-name %s --yes (or virsh undefine %s --nvram --tpm)", vm, vm, vm)
		}
	}
	devs, _ := hw.ScanPCI(e.Root)
	for _, d := range devs {
		if d.Vendor == hw.VendorNVIDIA && d.Driver == "vfio-pci" {
			return fmt.Errorf("GPU %s is bound to vfio-pci — reattach it to the host driver first: virsh nodedev-reattach pci_%s (or reboot)",
				d.Address, strings.NewReplacer(":", "_", ".", "_").Replace(d.Address))
		}
	}
	return nil
}

// printDrift reports kernel/driver updates since apply.
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

// Undo replays the manifest in reverse, restoring the host.
func (e *Engine) Undo(force, purge bool, confirm io.Reader) error {
	m, err := Load(e.Root)
	if err != nil {
		return err
	}
	if len(m.Records) == 0 && !purge {
		fmt.Fprintln(e.Out, "nothing to undo")
		return nil
	}
	oc := e.newOpClients()
	defer oc.close()
	if err := e.undoPreconditions(oc); err != nil {
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
		left := *m
		left.Records = keep
		return left.save(e.Root)
	}

	var restores, cmds []Record
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Kind == KindRunCmd || all[i].Kind == KindOp {
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
		ok, err := e.undoRecord(rec, force, oc)
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
	_ = os.Remove(StatePath(e.Root))
	if purge {
		if kept > 0 {
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

// UndoID reverses one journaled record and drops it from the manifest.
func (e *Engine) UndoID(id string, force bool) (found bool, err error) {
	m, err := Load(e.Root)
	if err != nil {
		return false, err
	}
	rec := m.find(id)
	if rec == nil {
		return false, nil
	}
	oc := e.newOpClients()
	defer oc.close()
	ok, err := e.undoRecord(*rec, force, oc)
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

// undoRecord reverses one record.
func (e *Engine) undoRecord(rec Record, force bool, oc *OpClients) (bool, error) {
	switch rec.Kind {
	case KindWriteFile:
		return e.undoWriteFile(rec, force)
	case KindRunCmd:
		return e.undoRunCmd(rec, oc)
	case KindEnableUnit:
		return e.undoEnableUnit(rec, oc)
	case KindOp:
		return e.undoOp(rec, oc)
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

func (e *Engine) undoRunCmd(rec Record, oc *OpClients) (bool, error) {
	if rec.UndoOp != "" {
		return e.runUndoOp(rec.ID, rec.UndoOp, rec.UndoArgs, oc)
	}
	if len(rec.UndoCmd) == 0 {
		fmt.Fprintf(e.Out, "%s: no undo command\n", rec.ID)
		return true, nil
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would run: %s\n", strings.Join(rec.UndoCmd, " "))
		return true, nil
	}
	if err := runCmd(e.Out, rec.UndoCmd...); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) undoOp(rec Record, oc *OpClients) (bool, error) {
	if rec.UndoOp == "" {
		fmt.Fprintf(e.Out, "%s: no undo op\n", rec.ID)
		return true, nil
	}
	return e.runUndoOp(rec.ID, rec.UndoOp, rec.UndoArgs, oc)
}

// runUndoOp executes a journaled inverse op.
func (e *Engine) runUndoOp(id, undoOp string, args map[string]string, oc *OpClients) (bool, error) {
	entry, ok := ops[undoOp]
	if !ok {
		return false, fmt.Errorf("unknown undo op %q", undoOp)
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would: %s\n", opLine(undoOp, args))
		return true, nil
	}
	if entry.dials && e.skipUnderRoot(oc) {
		fmt.Fprintf(e.Out, "%s: undo skipped under --root (needs live libvirt/systemd — covered by test-vm)\n", id)
		return true, nil
	}
	if err := entry.fn(oc, e.Root, e.Out, args); err != nil {
		return false, err
	}
	return true, nil
}

func (e *Engine) undoEnableUnit(rec Record, oc *OpClients) (bool, error) {
	enable := false
	switch rec.PriorState {
	case "enabled":
		enable = true
	case "disabled":
	default:
		fmt.Fprintf(e.Out, "%s: prior state %q not restorable, leaving unit as-is\n", rec.Unit, rec.PriorState)
		return true, nil
	}
	verb := "disable"
	if enable {
		verb = "enable"
	}
	if !e.Yes {
		fmt.Fprintf(e.Out, "would: %s unit %s\n", verb, rec.Unit)
		return true, nil
	}
	if e.skipUnderRoot(oc) {
		fmt.Fprintf(e.Out, "%s: undo skipped under --root (needs live systemd — covered by test-vm)\n", rec.ID)
		return true, nil
	}
	var err error
	if enable {
		err = oc.Sysd().EnableUnit(rec.Unit)
	} else {
		err = oc.Sysd().DisableUnit(rec.Unit)
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func removeDirs(root string, dirs []string) {
	for _, d := range dirs {
		_ = os.Remove(filepath.Join(root, d))
	}
}
