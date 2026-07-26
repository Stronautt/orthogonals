# CLAUDE.md

orthogonals: a Go CLI that turns a Fedora desktop (Intel iGPU + one NVIDIA
dGPU) into a Looking Glass Windows 11 VM host — detect → preflight → apply →
vm → media → install → verify, all orchestrated by `up`.

## Build / test

- `make build` — build the binary.
- `make test` — `go vet` + full unit suite.
- `make lint` — golangci-lint (staticcheck included); must be clean.
- `make test-integration` — container tier (T3) via **fmf/tmt**: synthetic
  sysfs roots + argv-logging fake binaries in a fedora:44 container.
- `make test-vm` — system tier (T4) in a throwaway Fedora Cloud VM.
- `make test-vfio` — VFIO tier (T5) in a guest with an **emulated IOMMU**, the
  only tier where the kernel rather than a fixture answers.
- `make test-desk` — desk tier (T5b), read-only, against **your** hardware.
- `make coverage` — merges unit coverage with the real binary's, as driven by
  whichever tiers have been run.
- Golden files regenerate with `go test ./internal/<pkg> -update`.

The host-mutation tiers are **one set of tests, many machines**: the fmf tests
in `test/tmt/` are shared, and a plan in `plans.fmf` only chooses `provision:`
(`container` locally and in CI, `virtual` for the VM tier, `connect` for the
VFIO tier, `local` for the desk tier, overridden by Testing Farm). Tests carry
a `tier:` tag and plans filter on it — **tier 3** runs anywhere a `--root`
prefix is enough, **tier 4** needs real root, a real kernel, or a reboot,
**tier 5** needs hardware CI cannot rent: an emulated IOMMU, or your own desk.
Tier 5 holds two unrelated tiers each mapping to one test, so its plans select
by test name (the tier filter would be redundant). A test that cannot run
outside its tier must skip itself,
and the tier script must then **fail on that skip**: `go test` exits 0 on a
skip, so a tier that exists to run something would otherwise report success
having proved nothing (`test/tmt/media.sh`). Requires `tmt` plus the matching
provision plugin (`tmt+provision-container` / `tmt+provision-virtual`).
The tier scripts run under `set -euo pipefail`, so **never pipe into
`grep -q`**: grep exits on the first match and closes the pipe, the producer
dies of SIGPIPE, and pipefail turns a successful match into a failed pipeline.
Redirect to a file and grep that. `tmt lint` must be clean — check its **exit
status**, since grepping its output for `warn`/`fail` hides YAML parse errors. fmf `summary`/`description` values
containing `: ` need a block scalar, or YAML reads the text as a mapping key.
The scripts are clean under `shellcheck -x test/tmt/*.sh` — **`-x` matters**,
since they all source `lib.sh` and without it shellcheck sees none of it.

## Architecture

- Pure-Go dependencies only, no cgo (static `CGO_ENABLED=0` builds):
  `digitalocean/go-libvirt` (libvirt RPC over the local socket),
  `coreos/go-systemd` + `godbus` (systemd/D-Bus), `x/sys` (mount/loop
  ioctls), `kdomanski/iso9660` (provision ISO writer). CLI is `spf13/cobra`:
  a factory-built command tree (`internal/cli/root.go`, `newRootCmd`, never a
  package global) with `--json/--yes/--root` as root persistent flags; `Run`
  maps RunE results to exit codes (an `exitCode` error carries a specific
  code, else 2 for cobra usage errors). `vm` and `hook` are real subcommand
  trees; the RPM ships generated shell completions.
- Packages: `internal/{cli,hw,preflight,steps,bls,hostcfg,hooks,domain,media,
  orchestrate,artifacts,virt,sysd,utils}`. Package boundaries are the
  distro/vendor seams, with one exception: `internal/utils` holds the
  primitives every seam needs — filesystem (`WriteAtomic`, `WriteSync`,
  `SyncDir`, `Exists`, `ReadTrim`, `ReadUint`, `LinkBase`, `CopyFile`,
  `SweepTemps`, `IsTerminal`), hashing (`SHA256Hex`, `FileSHA256`), escaping and encoding
  (`XMLEscape`, `PowerShellEscape`, `WriteJSON`), and the binary size units
  (`BytesPerKiB/MiB/GiB` plus `GiB()` for display). It
  imports nothing from this module and must stay that way — it is the only
  package everything else may depend on. **`utils.Exists` returns
  `(bool, error)`** because absent and unreadable are different answers: two
  bugs came from a `bool` helper folding EACCES into "not there"
  (`/etc/libvirt` is 0700). Dropping the error is fine, but it has to be
  written at the call site. A helper earns a place here by having a second
  consumer, not by looking generic: single-caller helpers stay with their
  caller, where the local name is the documentation (`hostcfg.uncomment`,
  `hw.splitTrim`, `steps.splitLines`). `internal/virt` and `internal/sysd` are the narrow
  client surfaces for libvirt and systemd — no virsh/systemctl exec, no
  output parsing. `internal/bls` edits `/boot/loader/entries` directly (the
  native replacement for grubby) **and `/etc/kernel/cmdline` with it** —
  kernel-install writes a new kernel's entry from that file, so args that stop
  at the entries are dropped by the next `dnf upgrade kernel` and the host
  boots with no IOMMU. Its `Wanted` splits the two questions an edit needs:
  tokens some target already carries (never undo those) and tokens some target
  lacks (still to write). **Anything the standard library can do is
  done in Go, never shelled out to** — the `~/Desktop` shortcut is an op
  (`steps.OpDesktopLink`) using `MkdirAll`/`Symlink`/`Lchown`, not a
  `runuser … sh -c` script. exec remains only where the binary IS the vendor
  API on Fedora (dracut, semanage, restorecon, usermod, modprobe load-side,
  nvidia-smi, notify-send, `gio` for GNOME file metadata, and
  lspci/journalctl for the diagnostics bundle).
- **The libvirt qemu hook is Go, not shell.** apply installs a two-line shim
  at `/etc/libvirt/hooks/qemu` that execs `orthogonals hook qemu …`; the GPU
  detach/reattach/holder-gate/governor logic lives in `internal/hooks`
  (runtime.go), shared with `orthogonals recover`. The sleep inhibitor is a
  transient systemd unit running `orthogonals hook inhibit` (a logind
  Inhibit fd held until SIGTERM). `hook` is an internal subcommand invoked by
  libvirtd, not users; it journals nothing, so `--yes` does not gate it.
  Per-VM launch is `orthogonals vm launch` over `internal/virt` (no shell
  launcher; the desktop entry execs the binary).
- **SPICE listens on a unix socket, never a TCP port** — an address listen has
  no password, so any local user could attach to an auto-logon Administrator
  console. The domain renders
  `<listen type='socket' socket='/run/orthogonals/<vm>/spice.sock'/>`. The
  per-VM directory (`0730 <user>:qemu`, a tmpfiles.d fragment `vm define`
  writes) is the access control: QEMU binds the socket world-readable, so its
  own mode restricts nothing. The `started/begin` hook chowns it to 0600 on top
  of that, and is log-only for the same reason. `parseSpiceDisplay` returns the
  path with port `"0"` — the zero is how Looking Glass is told `-c` names a
  socket. SELinux type is `qemu_var_run_t`, the policy's type for
  `/var/lib/libvirt/qemu`; `svirt_var_run_t` does not exist, and `test/desk`
  checks it against the running policy.
- **The Looking Glass buffer has two backends and the render picks one.**
  `hw.KVMFRAvailable` asks whether the module is *built for the running kernel*
  (via `modules.dep`), never whether it is loaded — `up` crosses a reboot
  between apply and `vm define`, so a loaded-state test would downgrade every
  host to `/dev/shm` on the second leg. Built is not loadable, though, so the
  render also asks `preflight.KVMFRWillLoad`: under Secure Boot with no key dkms
  can sign with, the module exists and is rejected at load, and a kvmfr domain
  would trap the user — the hook's refusal names `orthogonals up`, which renders
  kvmfr again. Declining at `vm define` is what makes that remedy terminate.
  libvirt's `<shmem>` can only name a file under `/dev/shm`, so kvmfr goes
  through `<qemu:commandline>`, which in turn means qemu.conf must list the
  device: **setting `cgroup_device_acl`
  replaces libvirt's compiled-in default rather than extending it, and Fedora's
  commented sample omits `/dev/kvm`** — uncommenting it verbatim, which is what
  the Looking Glass docs instruct, leaves the guest with no KVM. `hostcfg`
  writes an explicit closed list and `test/tmt/kvmfr.sh` starts a real domain to
  prove it. The module is loaded **by the hook, on demand**, sized from the
  domain being started (`domain.KVMFRSizeMiB`) and never unloaded; that is why
  there is no modprobe.d, modules-load.d, udev rule or `semanage` entry for it —
  the hook chowns and labels the node itself — **after `udevadm settle`**, since
  udev stamps its own `device_t` while processing the add event and that write
  can land after the hook's `svirt_image_t`, leaving qemu denied at map time with
  nothing but an AVC to show for it; the label is read back for the same reason.
  A failure there refuses the start,
  notifies the desktop, and carries `hooks.KVMFRErrPrefix` so `vm launch` prints
  it verbatim; matching on text is forced by the hook → libvirtd → RPC boundary,
  where nothing else survives. `vm launch` passes `-f` for a `/dev/shm` domain,
  because the client prefers `/dev/kvmfr0` whenever it exists and would
  otherwise attach to a stale buffer and wait forever.
- **Every host mutation routes through the apply engine** (`internal/steps`):
  journaled to `/var/lib/orthogonals/manifest.json` with original bytes
  backed up, so `undo` restores byte-identically. The journal is
  **write-ahead**: every step kind saves its record *before* the mutation
  runs (`Engine.journal`) and drops it again if the mutation fails
  (`Engine.rollbackOnError`), so a process killed mid-step never strands an
  unjournaled change — and a failed step is retried rather than mistaken for
  one already applied. `test/fault` enforces this by SIGKILLing a real apply
  at every one of its progress points. **A kill also strands the temp file
  `utils.WriteAtomic` renames from** — SIGKILL runs no deferred cleanup — so
  every write sweeps `utils.TempPrefix` leftovers from its target directory
  first, and the two undo paths that *remove* rather than write call
  `utils.SweepTemps` themselves (before `removeDirs`, or the leftover keeps the
  directory non-empty and that leaks too). bls sweeps on its no-op path for the
  same reason: undoing a kernel-args edit that never landed changes nothing, so
  it never reaches `WriteAtomic`. Dry-run is the default and
  never dials a daemon; `--yes` gates all mutation. Step kinds: write_file,
  run_cmd (argv), enable_unit, and **op** — a named entry in the compiled-in
  ops registry (`internal/steps/ops.go`) with JSON args journaled like argv,
  so undo works from a fresh process. A journaled step whose
  command/op/path — or whose **kind**, when a release moves a step from
  run_cmd to op — diverges from the current settings is refused, never
  silently skipped or rebound. The refusal names both sides by their own
  kind (a record read through the new kind's fields prints an empty "was:")
  and quotes `undo --step <id>`, which reverses that one step and leaves the
  rest of the manifest applied; a step that declares `Input`
  content re-runs when that content drifts (how the define-domain op
  converges on a new render). **The journal is not proof the host still
  carries the change**: a step whose effect something else can undo (a kernel
  update regenerating the BLS entries) sets `Recheck` from live state and
  re-runs — and a re-run keeps the *journaled* undo, since deriving one from a
  host the first run already changed would ask for less than was added. Under
  `--root` with no injected clients,
  daemon-touching steps journal and print "skipped under --root" — the
  container tier's contract; `make test-vm` covers them live.
- The domain's pipeline position is a Stage (install → novideo → final)
  read back from its rendered XML (`domain.CurrentStage`); the up
  pipeline's post-install transitions are `vm define --stage` re-renders
  that converge through the define op's Input drift — no device surgery.
- `up` is a persisted state machine (`state.json`) that drives the stages by
  calling the subcommand logic funcs (`runApply`/`runVMDefine`/`runMedia`)
  directly with options structs — no argv round-trip; it stops cleanly at the
  reboot boundary. On a completed install (stage final) it runs a converge
  pass (apply + vm define) instead.
- All download pins (URL/version/SHA256) live in
  `internal/artifacts/artifacts.go` — the single bump place. Host packages
  are RPM `Requires:` in `packaging/orthogonals.spec`, installed with the
  package itself, never at apply time. The Looking Glass client is its own
  RPM (`packaging/looking-glass-client.spec`), built on COPR from the pinned
  git submodule (`packaging/third_party/LookingGlass`, `make srpm-lg`) and
  pulled in as a versioned `Requires:` — never compiled on the user's host.
  Bumping the LG release is `make lg-bump LG=<tag>` (or edit
  `internal/artifacts/looking-glass.version`, then `make lg-bump`): it moves the
  submodule and regenerates the host-SHA lockfile (`looking-glass.sha256`). Both
  are `//go:embed`-ed committed lockfiles the specs and Makefile derive from —
  nothing is hand-edited in Go.
- Templates render via `embed.FS` next to their package; every rendered
  artifact has a golden test.

## Testing conventions

- No mocking frameworks. Four seams: the `--root` path prefix for all
  filesystem access; fake binaries (argv-logging shell scripts) on PATH for
  the still-exec'd vendor tools; in-process client fakes (`virt/virttest`,
  `sysd/sysdtest`, `media/mediatest`) injected through `Engine.Virt`/
  `Engine.Sysd` or cli's `newVirt`/`newSysd` vars; and package-level func
  vars for syscall/notify boundaries the hook runtime crosses
  (`hooks.DeleteModule`, `hooks.deviceDriver`, `notify.Send`,
  `notify.lookupUser`, `steps.pathOwner`,
  `cli.execProcess`/`executablePath`) — swap with `t.Cleanup` restore, never
  `t.Parallel` while swapped. A unit test must NEVER dial the developer
  machine's real libvirt or systemd, nor issue a real `delete_module`.
  Where a privileged path cannot be reached from a unit run at all, the
  *decision* is split into a pure function and tested there
  (`notify.credential`, `steps.trustCmd`) and a tier proves the kernel honours
  it (`test/tmt/privdrop.sh`, `test/tmt/inhibit.sh`). Dropping to the desktop
  user is the credential **and** the environment: sudo hands the op root's
  `HOME`, getenv takes the first match, so `steps.desktopEnv` replaces the
  inherited `HOME`/`XDG_*`/`DBUS_SESSION_BUS_ADDRESS` instead of appending —
  otherwise gvfs writes its metadata tree under `/root` as a user who cannot.
- `internal/hw/hwtest` provides `ReferenceRoot` (a PoC-mirroring fixture
  host) and sysfs builders. **`hwtest.Roots` is the fixture registry** — the
  single source of every synthetic topology, consumed by `test/fixture`, the
  testscript `fixture` command, and the JSON-contract table. Adding an entry
  there automatically extends `internal/cli`'s detect/preflight golden set,
  so a new topology needs one builder and a `-update` run, nothing else.
  Those fixtures are hand-written, so **the desk tier (`test/desk`, build tag
  `desk`) is what keeps them honest**: it walks the reference fixture and
  requires every attribute it synthesizes to exist on the machine it claims to
  model. It found the audio function carrying a `reset` file no HDA device has,
  and a `detect --json` schema that rejected real Thunderbolt PCI addresses
  (domains run past four hex digits, e.g. `10000:e0:06.0`).
- **PCI identity is overlaid per file, never per directory.** The VFIO tier
  bind-mounts single files over `vendor`, `device`, and `class` so an ordinary
  virtio function answers as an RTX 3080, while `iommu_group`,
  `driver_override`, `unbind`, `drivers_probe`, `remove` and `rescan` stay the
  kernel's own — the older directory bind-mount makes every one of those fake
  by construction. Two consequences the scripts must honour: bind mounts do not
  survive a reboot, and a PCI `remove` + `rescan` unlinks the whole sysfs
  device directory and takes the overlays with it, so `overlay_identity` is
  called again after both (`test/tmt/vfio.sh`).
- The VFIO guest is provisioned by `test/vfiohost`, in Go over `go-libvirt`,
  **not** by tmt: testcloud cannot request an emulated IOMMU (tmt's hardware
  matrix supports `iommu` on `beaker` only), and that is the one thing the
  guest exists to have. Its defaults are load-bearing, not taste — an explicit
  `<topology>` because the default gives every vCPU its own core and leaves the
  domain profile nothing to assign; 14 GiB because that is the smallest guest
  whose own `/proc/meminfo` clears preflight's floor without faking it; and a
  host-passthrough CPU because `vm define` needs `/dev/kvm` (without it libvirt
  offers no `kvm` domain type, so autoselecting the rendered domain's
  secure-boot EFI firmware fails) and only the host's own silicon can provide
  it: KVM nests only the host's virt extension and kvm_amd refuses non-AMD
  vendor strings, so a named CPU model loses `/dev/kvm` on whichever fleet it
  doesn't match. The guest needs no particular CPU vendor — the kernel-arg
  choice keys on the firmware's ACPI IOMMU table (DMAR/IVRS), with CPU vendor
  only the no-table fallback preflight quotes as the remedy. The inner TCG
  test domain still needs no nesting of its own.
- Coverage gate: 80%+, read from `make coverage-pct` — **never**
  `go tool cover -func | tail -1`. `make coverage` merges the unit profile with
  the real binary's, as driven by whichever tiers have run — **88.6%** with
  container, VM, and VFIO; 83.1% with container alone. Four measurement traps:
  a bare per-package `-coverprofile` reports ~75% because it cannot see the
  `*test` helper packages being exercised from other packages' tests (hence
  `-coverpkg=./internal/...`); unit tests alone cap out around 83% because
  `internal/virt` and `internal/sysd` need a live daemon; `go tool cover -func`
  accumulates over `FuncDecl`s only, so every package-level
  `var x = func(…)` seam is missing from its listing *and its total* — and two
  of those seams are the privilege-dropping exec paths (`notify.Send`,
  `steps.markTrusted`), hence the statement sum in `coverage-pct`; and a tier
  directory left from an older binary merges cleanly but contributes blocks
  describing source this tree no longer has, landing in the denominator and
  nowhere else (worth several points, silently — `make coverage` now warns when
  more than one build's covmeta is present). The host tiers are what lift the
  daemon packages, so a coverage number quoted without saying which tiers ran
  is meaningless.
  `internal/virt` (53%) is the remaining floor — the paths only a live Windows
  guest reaches (agent commands, SPICE display, key injection).
  **Coverage is not why the VFIO tier exists.** `internal/hooks` was at 86.6%
  before it, and the CWD bug in the holder gate was sitting inside that 86.6%.
