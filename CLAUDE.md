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
- `make coverage` / `make coverage-pct`. Goldens: `make goldens` — the one
  `-update` flag lives in `internal/testsupport`, so `go test ./... -update`
  fails on every package that renders nothing.

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

Packages under `internal/` are bounded on the distro/vendor seams.

- **Pure Go, no cgo** (`CGO_ENABLED=0`): go-libvirt, go-systemd + godbus,
  x/sys, iso9660, cobra. **Anything the standard library can do is done in Go,
  never shelled out to** — exec survives only where the binary IS the vendor
  API on Fedora (dracut, semanage, restorecon, systemd-tmpfiles, usermod,
  udevadm, lspci, journalctl, **dkms's build side**, modprobe's load side,
  nvidia-smi) or the desktop's (`gio`, notify-send). Where only a *reading* of
  that tool was wanted, the file it reads is read instead: `mokutil --test-key`
  is a MokListRT parse and `dkms status` a walk of `/var/lib/dkms`, so neither
  binary is a dependency.
- The command tree is **factory-built** (`newRootCmd`), never a package
  global. Templates render via `embed.FS` beside their package, and **every
  rendered artifact has a golden test**.
- `internal/utils` is the one package everything may depend on; it **imports
  nothing from this module** and must stay that way. A helper earns a place
  there by having a **second consumer**, not by looking generic — single-caller
  helpers stay with their caller. Its `Exists` returns `(bool, error)` because
  absent and unreadable are different answers (`/etc/libvirt` is 0700):
  dropping the error at a call site is fine, folding every error into "not
  there" is the bug it exists to prevent.
- `internal/virt` and `internal/sysd` are narrow client surfaces — no
  virsh/systemctl exec, no output parsing. **sysd dials one connection per
  call and hangs up**, where virt caches one: go-systemd ties a connection's
  lifetime to the context it was dialled with, so a cached one is closed by
  the call that opened it and an ordinary restart fails with `context deadline
  exceeded`.
- `internal/bls` writes kernel args to **three** targets: the BLS entries,
  `/etc/kernel/cmdline`, and `/etc/default/grub`. The first two are regenerated
  from the third, so args that stop short of it are dropped by the next package
  update and the host boots with no IOMMU.
- The qemu hook is a Go subcommand behind a two-line shim, not a shell script.
  libvirtd invokes it, not users, and it journals nothing, so `--yes` does not
  gate it. It loads the kvmfr module on demand and never unloads it — hence no
  modprobe.d, modules-load.d, udev rule or semanage entry for kvmfr.
- **Shared folders force shared memory backing**: virtiofsd maps guest RAM out
  of process, so any share renders a `memfd` `<memoryBacking>` the domain
  otherwise does without. virtio-win's `VirtioFsSvc` mounts exactly one device,
  so provisioning clones a pinned service per extra share — with `New-Service`,
  never `sc.exe`, which PowerShell re-quotes into a binPath ending at
  `C:\Program` (a test bans it from the rendered script).
- **Guest-side state is written during provisioning only**: `provision.ps1`
  runs from the installer media under a logon task the cleanup stage
  unregisters, and `final` drops the cdroms, so an installed guest cannot
  re-run it at all — a share added later reaches the domain but nothing in the
  guest mounts it. **Editing `provision.ps1` means applying the change to
  installed guests by hand.**
- **The Looking Glass host service stops itself for good** when its host
  program fails to start. The stop is clean, so the SCM runs no recovery action
  and nothing in the guest starts it again. Provisioning orders its start
  behind the `IVSHMEM` driver, and `vm launch` starts it through the guest
  agent. **Nothing restarts it on a timer**: whether frames flow is observable
  host-side only, so no in-guest check can tell a settled capture from a wedged
  one.

### Per-VM settings

**Every `vm define` knob is one field of `domain.Settings`**, marshalled whole
into the domain metadata and unmarshalled by the next define, so a flag-less
converge reproduces what was registered. Three rules keep it that way:

1. A zero-valued flag means **keep what is registered**, never "use the
   default" — so **no flag may declare a default**. Defaults belong in
   `domain.NewProfile`, or in `resolveSettings` when they come from the host.
2. `NewProfile` writes every default it fills **back into the record**, so
   nothing is re-derived from a host that may have changed.
3. A credential field carries **`secret:"true"`**, and the diagnostics bundle
   redacts by `domain.SecretElements()`. **Anything asserting on registry XML
   builds its fixture with `Profile.SettingsXML()`** — hand-written fixtures in
   a spelling a rename had retired once kept three tests green while the guest
   password shipped in bug reports.

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

- The journal is **write-ahead**, and `test/fault` SIGKILLs a real apply at
  every progress point to keep it that way. A kill also strands the temp file
  `utils.WriteAtomic` renames from, so every write sweeps `utils.TempPrefix`
  leftovers first and undo paths that *remove* rather than write call
  `utils.SweepTemps` themselves.
- A journaled step whose command/op/path — or **kind**, when a release moves a
  step from run_cmd to op — diverges from current settings is **refused**,
  never silently skipped or rebound.
- Under `--root` with no injected clients, daemon-touching steps journal and
  print "skipped under --root" — the container tier's contract; `make test-vm`
  covers them live.

### Pipeline

`up` is a persisted state machine (`state.json`) calling `runApply`/
`runVMDefine`/`runMedia` directly with options structs — no argv round-trip —
and stops cleanly at the reboot boundary. Post-install stage transitions are
`vm define --stage` re-renders converging through the define op's Input drift;
no device surgery. On a completed install it runs a converge pass instead.

### Pins and packaging

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
- **A fixture that varies one fact goldens one fact.** `internal/cli` and
  `internal/domain` golden the reference host in full and every other case as
  its departure from it (`testsupport.GoldenDelta`), because a full golden
  apiece rewrote the same hunk into 7–11 files for every shared change. The
  stored departure carries no line numbers, so a change to what the cases share
  moves the baseline alone; failure messages are rendered with full context
  separately. A case indistinguishable from the reference stores **no file**
  and must be declared in `blindTo` with the reason — that assertion fails in
  both directions, so a command that starts seeing a fixture, or quietly goes
  blind to one, is a test failure rather than a silent golden reshuffle.
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
  quoted without saying which tiers ran is meaningless.
- Tier data from another build merges cleanly and pads the denominator rather
  than raising the numerator, so `make coverage` **fails** when merging adds an
  `internal/` block — the tier's only honest addition is `main.go`. Its two
  causes are a stale `/var/tmp/orthogonals-tmt-*` and a tier binary built by
  another Go than the one merging, so **no CI job names a Go version**: every
  `setup-go` reads `go-version-file: go.mod`, and the workflow-lint job fails
  on one that does not.
- **Coverage is not why the host tiers exist**: `internal/hooks` was at 86.6%
  when the VFIO tier found the CWD bug in its holder gate.
