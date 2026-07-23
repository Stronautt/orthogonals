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
  orchestrate,artifacts,virt,sysd}`. Package boundaries are the
  distro/vendor seams. `internal/virt` and `internal/sysd` are the narrow
  client surfaces for libvirt and systemd — no virsh/systemctl exec, no
  output parsing. `internal/bls` edits `/boot/loader/entries` directly (the
  native replacement for grubby). **Anything the standard library can do is
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
- **Every host mutation routes through the apply engine** (`internal/steps`):
  journaled to `/var/lib/orthogonals/manifest.json` with original bytes
  backed up, so `undo` restores byte-identically. The journal is
  **write-ahead**: every step kind saves its record *before* the mutation
  runs (`Engine.journal`) and drops it again if the mutation fails
  (`Engine.rollbackOnError`), so a process killed mid-step never strands an
  unjournaled change — and a failed step is retried rather than mistaken for
  one already applied. `test/fault` enforces this by SIGKILLing a real apply
  at every one of its progress points. Dry-run is the default and
  never dials a daemon; `--yes` gates all mutation. Step kinds: write_file,
  run_cmd (argv), enable_unit, and **op** — a named entry in the compiled-in
  ops registry (`internal/steps/ops.go`) with JSON args journaled like argv,
  so undo works from a fresh process. A journaled step whose
  command/op/path diverges from the current settings is refused (undo
  first), never silently skipped or rebound; a step that declares `Input`
  content re-runs when that content drifts (how the define-domain op
  converges on a new render). Under `--root` with no injected clients,
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
  `cli.execProcess`/`executablePath`) — swap with `t.Cleanup` restore, never
  `t.Parallel` while swapped. A unit test must NEVER dial the developer
  machine's real libvirt or systemd, nor issue a real `delete_module`.
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
- Coverage gate: 80%+. `make coverage` merges the unit profile with the real
  binary's, as driven by whichever tiers have run — **88.6%** with container,
  VM, and VFIO. Two measurement traps: a bare per-package `-coverprofile`
  reports ~75% because it cannot see the `*test` helper packages being
  exercised from other packages' tests (hence `-coverpkg=./internal/...`), and
  unit tests alone cap out around 83% because `internal/virt` and
  `internal/sysd` need a live daemon. The host tiers are what lift those, so a
  coverage number quoted without saying which tiers ran is meaningless.
  `internal/virt` (53%) is the remaining floor — the paths only a live Windows
  guest reaches (agent commands, SPICE display, key injection).
  **Coverage is not why the VFIO tier exists.** `internal/hooks` was at 86.6%
  before it, and the CWD bug in the holder gate was sitting inside that 86.6%.
