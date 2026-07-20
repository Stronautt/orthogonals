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

- Stdlib only — no third-party Go dependencies; CLI uses `flag` with
  per-subcommand `newFlagSet` (no cobra).
- Packages: `internal/{cli,hw,preflight,steps,hostcfg,hooks,domain,media,
  orchestrate,artifacts}`. Package boundaries are the distro/vendor seams;
  no interfaces until a second implementation exists.
- **Every host mutation routes through the apply engine** (`internal/steps`):
  journaled to `/var/lib/orthogonals/manifest.json` with original bytes
  backed up, so `undo` restores byte-identically. Dry-run is the default;
  `--yes` gates all mutation. A journaled step whose command/path diverges
  from the current settings is refused (undo first), never silently skipped
  or rebound; a run_cmd that declares `Input` content re-runs when that
  content drifts (how `virsh define` converges on a new release's XML).
- `up` is a persisted state machine (`state.json`) that re-invokes the
  subcommand funcs via argv; it stops cleanly at the reboot boundary. On a
  completed install it runs a converge pass (apply + vm define) instead.
- All download pins (URL/version/SHA256) and the host package list live in
  `internal/artifacts/artifacts.go` — the single bump place.
- Templates render via `embed.FS` next to their package; every rendered
  artifact has a golden test.

## Testing conventions

- No mocking frameworks. Two seams only: the `--root` path prefix for all
  filesystem access, and fake binaries (argv-logging shell scripts)
  prepended to PATH. Fake `systemctl` must answer `is-enabled` (echo a
  state), or the engine treats units as not installed.
- `internal/hw/hwtest` provides `ReferenceRoot` (a PoC-mirroring fixture
  host) and sysfs builders.
- Coverage gate: 80%+ (currently ~86% on internal/...).
