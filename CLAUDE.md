# CLAUDE.md

orthogonals: a Go CLI that turns a Fedora desktop (Intel iGPU + one NVIDIA
dGPU) into a Looking Glass Windows 11 VM host — detect → preflight → apply →
vm → media → install → verify, all orchestrated by `up`.

## Build / test

- `make build` — build the binary.
- `make test` — `go vet` + full unit suite.
- `make lint` — golangci-lint (staticcheck included); must be clean.
- `make test-integration` — container tier (podman, falls back to docker):
  synthetic sysfs root + argv-logging fake binaries in a fedora:44 image.
- `make test-vm` — local-only system tier in a throwaway Fedora Cloud VM.
- Golden files regenerate with `go test ./internal/<pkg> -update`.

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
  native replacement for grubby). exec remains only where the binary IS the
  vendor API on Fedora (dracut, semanage, restorecon, usermod, modprobe
  load-side, nvidia-smi, notify-send, the desktop-link script, and
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
  backed up, so `undo` restores byte-identically. Dry-run is the default and
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
  host) and sysfs builders.
- Coverage gate: 80%+ (currently ~86% on internal/...).
