# CLAUDE.md

orthogonals: a Go CLI that turns a Fedora desktop (Intel iGPU + one NVIDIA
dGPU) into a Looking Glass Windows 11 VM host — detect → preflight → apply →
vm → media → install → verify, all orchestrated by `up`.

Rationale for a single function lives in a comment at that function. What is
here is what no one file can say: cross-package rules, tooling traps, and
warnings against the obvious-but-wrong.

## Build / test

- `make build` / `make test` (vet + unit) / `make lint` (must be clean).
- `make test-integration` — T3, container. `make test-vm` — T4, throwaway
  Fedora Cloud VM. `make test-vfio` — T5, guest with an emulated IOMMU.
  `make test-desk` — T5b, read-only against your hardware.
- `make coverage` / `make coverage-pct`. Goldens: `go test ./internal/<pkg> -update`.

The host-mutation tiers are **one set of tests, many machines**: the fmf tests
in `test/tmt/` are shared and a plan in `plans.fmf` only chooses `provision:`.
Tests carry a `tier:` tag — **3** runs anywhere a `--root` prefix suffices,
**4** needs real root/kernel/reboot, **5** needs hardware CI cannot rent.
Needs `tmt` plus the matching provision plugin.

Traps in this tooling, none of which fail loudly:

- A test that skips itself outside its tier makes the **tier script fail on
  that skip** — `go test` exits 0 on skip, so the tier reports success having
  proved nothing (`test/tmt/media.sh`).
- Scripts run under `set -euo pipefail`: **never pipe into `grep -q`**. grep
  closes the pipe on first match, the producer dies of SIGPIPE, and pipefail
  turns a match into a failed pipeline. Redirect to a file and grep that.
- Check `tmt lint`'s **exit status**; grepping its output for `warn`/`fail`
  hides YAML parse errors.
- fmf `summary`/`description` containing `: ` needs a block scalar.
- `shellcheck -x test/tmt/*.sh` — **`-x` matters**, they all source `lib.sh`.

## Architecture

Packages: `internal/{cli,hw,preflight,steps,bls,hostcfg,hooks,domain,media,
orchestrate,artifacts,virt,sysd,utils}`, bounded on the distro/vendor seams.

- **Pure Go, no cgo** (`CGO_ENABLED=0`): go-libvirt, go-systemd + godbus,
  x/sys, iso9660, cobra.
- **Anything the standard library can do is done in Go, never shelled out
  to.** exec survives only where the binary IS the vendor API on Fedora:
  dracut, semanage, restorecon, usermod, modprobe (load side), nvidia-smi,
  notify-send, `gio`, and lspci/journalctl for the diagnostics bundle. The
  qemu hook is a Go subcommand behind a two-line shim, not a shell script;
  libvirtd invokes it, not users, and it journals nothing, so `--yes` does not
  gate it.
- The command tree is **factory-built** (`newRootCmd`), never a package
  global. Templates render via `embed.FS` beside their package, and **every
  rendered artifact has a golden test**.
- Dropping to the desktop user is the credential **and** the environment: sudo
  hands the op root's `HOME` and getenv takes the first match, so
  `steps.desktopEnv` **replaces** the inherited `HOME`/`XDG_*`/
  `DBUS_SESSION_BUS_ADDRESS` rather than appending — otherwise gvfs writes its
  metadata under `/root` as a user who cannot.
- `internal/utils` is the one package everything may depend on; it **imports
  nothing from this module** and must stay that way. A helper earns a place
  there by having a **second consumer**, not by looking generic — single-caller
  helpers stay with their caller.
- **`utils.Exists` returns `(bool, error)`**: absent and unreadable are
  different answers, and two bugs came from folding EACCES into "not there"
  (`/etc/libvirt` is 0700). Dropping the error is fine; it must be written at
  the call site. New code that stats a path and treats every error as absence
  is a bug.
- `internal/virt` and `internal/sysd` are narrow client surfaces — no
  virsh/systemctl exec, no output parsing. **sysd dials one connection per
  call and hangs up**, where virt caches one: go-systemd ties a connection's
  lifetime to the context it was dialled with, so a cached one is closed by
  the call that opened it and an ordinary restart fails with `context deadline
  exceeded`.
- `internal/bls` edits `/boot/loader/entries` **and `/etc/kernel/cmdline`** —
  kernel-install writes a new kernel's entry from that file, so args stopping
  at the entries are dropped by the next `dnf upgrade kernel` and the host
  boots with no IOMMU.
- **SPICE listens on a unix socket, never a TCP port** — an address listen has
  no password, so any local user could attach to an auto-logon Administrator
  console. The per-VM directory mode is the access control, not the socket's:
  QEMU binds it world-readable.
- **`cgroup_device_acl` replaces libvirt's compiled-in default rather than
  extending it, and Fedora's commented sample omits `/dev/kvm`** —
  uncommenting it verbatim, which the Looking Glass docs instruct, leaves the
  guest with no KVM. hostcfg writes an explicit closed list; `test/tmt/kvmfr.sh`
  starts a real domain to prove it.
- The Looking Glass buffer has two backends. `hw.KVMFRAvailable` asks whether
  the module is *built for the running kernel*, never whether it is loaded —
  `up` crosses a reboot, so a loaded-state test would downgrade every host on
  the second leg. Built is not loadable, hence `preflight.KVMFRWillLoad`.
  The hook loads the module on demand and never unloads it, which is why there
  is no modprobe.d, modules-load.d, udev rule or semanage entry for it.
- **Shared folders cost the domain its private memory**: virtiofsd maps guest
  RAM out of process, so any share forces `memfd` + `<access mode='shared'/>`
  on the hugepage backing. Only the dirs are registered — `domain.NewShares`
  derives tag, drive letter and guest service name from their **order**, so
  those cannot disagree. virtio-win's `VirtioFsSvc` **mounts exactly one
  device** (its tag comes from the service command line, not the registry), so
  provisioning reconfigures it for share one and clones a pinned service per
  extra share — with `New-Service`, never `sc.exe`, which PowerShell re-quotes
  into a binPath ending at `C:\Program` (a test bans it from the rendered
  script). Those services are made during provisioning only: a share added to
  an installed guest reaches the domain but nothing mounts it.

### Per-VM settings

**Every `vm define` knob is one field of `domain.Settings`**, marshalled whole
into the domain metadata and unmarshalled by the next define, so a flag-less
converge reproduces what was registered. Four rules keep it that way:

1. A zero-valued flag means **keep what is registered**, never "use the
   default" — so **no flag may declare a default**. Defaults belong in
   `domain.NewProfile`, or in `resolveSettings` when they come from the host.
2. `Settings.Over` merges by reflection: a new field is sticky with no call
   site touched.
3. `NewProfile` writes every default it fills **back into the record**, so
   nothing is re-derived from a host that may have changed.
4. A credential field carries **`secret:"true"`**; `domain.SecretElements()`
   is what the diagnostics bundle redacts by. The redactor once kept its own
   copy of the element names, a rename retired them, and three green tests
   shipped the guest password into bug reports — their fixtures were
   hand-written in the retired spelling. **Anything asserting on registry XML
   builds its fixture with `Profile.SettingsXML()`.**

`TestVMSettingsAllSticky` walks the struct by reflection and fails on a field
with no table entry: a knob cannot ship unproven. Two knobs are not plain
copies — `--gpu-rom` registers the *installed* copy, and the disk falls back to
the journal, which outlives the domain XML an undefine removes.

### Apply engine

**Every host mutation routes through `internal/steps`**, journaled to
`/var/lib/orthogonals/manifest.json` with original bytes backed up so `undo`
restores byte-identically. Dry-run is the default and never dials a daemon;
`--yes` gates all mutation. Step kinds: write_file, run_cmd (argv),
enable_unit, and **op** (a compiled-in registry entry with JSON args, so undo
works from a fresh process).

- The journal is **write-ahead**: every kind saves its record *before* the
  mutation and drops it if the mutation fails, so a process killed mid-step
  never strands an unjournaled change. `test/fault` SIGKILLs a real apply at
  every progress point.
- A kill also strands the temp file `utils.WriteAtomic` renames from, so every
  write sweeps `utils.TempPrefix` leftovers first, and undo paths that *remove*
  rather than write call `utils.SweepTemps` themselves.
- A journaled step whose command/op/path — or **kind**, when a release moves a
  step from run_cmd to op — diverges from current settings is **refused**,
  never silently skipped or rebound.
- **The journal is not proof the host still carries the change**: a step
  something else can undo (a kernel update regenerating BLS entries) sets
  `Recheck` from live state and re-runs, keeping the *journaled* undo.
- Under `--root` with no injected clients, daemon-touching steps journal and
  print "skipped under --root" — the container tier's contract; `make test-vm`
  covers them live.

### Pipeline

`up` is a persisted state machine (`state.json`) calling `runApply`/
`runVMDefine`/`runMedia` directly with options structs — no argv round-trip —
and stops cleanly at the reboot boundary. Post-install stage transitions are
`vm define --stage` re-renders converging through the define op's Input drift;
no device surgery. On a completed install it runs a converge pass instead.

All download pins live in `internal/artifacts/artifacts.go` — the single bump
place. Host packages are RPM `Requires:`, never installed at apply time. The
Looking Glass client is its own RPM built on COPR from a pinned submodule;
bump with `make lg-bump LG=<tag>`. The lockfiles are `//go:embed`-ed and
committed — nothing is hand-edited in Go.

## Testing conventions

No mocking frameworks. Four seams: the `--root` path prefix; argv-logging fake
binaries on PATH; in-process client fakes (`virt/virttest`, `sysd/sysdtest`,
`media/mediatest`); and package-level func vars for the syscall/notify
boundaries. Swap with `t.Cleanup` restore, never `t.Parallel` while swapped.

- **A unit test must NEVER dial the developer machine's real libvirt or
  systemd, nor issue a real `delete_module`.**
- Where a privileged path is unreachable from a unit run, split the *decision*
  into a pure function and test that; a tier proves the kernel honours it.
- **`hwtest.Roots` is the fixture registry** — the single source of every
  synthetic topology. Adding an entry there extends `internal/cli`'s
  detect/preflight golden set automatically.
- The fixtures are hand-written, so **the desk tier keeps them honest**: it
  requires every attribute they synthesize to exist on the machine they claim
  to model.
- **PCI identity is overlaid per file, never per directory** — a directory
  bind-mount fakes `iommu_group`, `unbind` and `rescan` by construction. Bind
  mounts do not survive a reboot, and `remove` + `rescan` takes the overlays
  with it, so `overlay_identity` runs again after both.
- `test/vfiohost` provisions the VFIO guest in Go, **not** tmt: testcloud
  cannot request an emulated IOMMU. Its defaults are load-bearing, not taste —
  read the comments there before changing one.
- Coverage gate 80%+, read from `make coverage-pct` — **never**
  `go tool cover -func | tail -1`, which misses every `var x = func(…)` seam
  from its listing *and* its total. Unit tests alone cap around 83%;
  `internal/virt` and `internal/sysd` need a live daemon, so a coverage number
  quoted without saying which tiers ran is meaningless. A tier directory left
  from an older binary merges cleanly and silently pads the denominator.
- **Coverage is not why the VFIO tier exists.** `internal/hooks` was at 86.6%
  before it, and the CWD bug in the holder gate was inside that 86.6%.
