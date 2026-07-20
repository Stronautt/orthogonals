<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img src="docs/logo.svg" width="640" alt="orthogonals">
  </picture>
</p>

<p align="center"><em>Same machine, orthogonal axes: Windows at full GPU speed, Linux never pauses.</em></p>

<p align="center">
  <a href="https://github.com/stronautt/orthogonals/actions/workflows/ci.yml"><img src="https://github.com/stronautt/orthogonals/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://copr.fedorainfracloud.org/coprs/stronautt/orthogonals/package/orthogonals/"><img src="https://copr.fedorainfracloud.org/coprs/stronautt/orthogonals/package/orthogonals/status_image/last_build.png" alt="COPR build" /></a>
  <a href="https://github.com/stronautt/orthogonals/releases/latest"><img src="https://img.shields.io/github/v/release/stronautt/orthogonals" alt="Latest release"></a>
  <img src="https://img.shields.io/badge/license-GPL--3.0-blue" alt="License: GPL-3.0">
</p>

<p align="center">
  <img src="docs/demo.gif" alt="orthogonals demo: one command from a plain Fedora install to Windows 11 in a window" width="768">
</p>

## Introduction

> [!CAUTION]
> This is a pre-alpha project, currently aimed at enthusiasts. It reconfigures
> your host at a low level (kernel parameters, GPU drivers, libvirt). You use it
> entirely at your own risk — the author takes no responsibility for any damage
> to your PC.

Many Linux users keep a Windows partition because some games and some
professional software run only on Windows. Dual boot works, but every switch
costs a reboot, and while Windows runs you lose your whole Linux session.
Orthogonals replaces the dual boot: it turns a Linux distro with an iGPU and
one dGPU into a host for a VM that owns the physical graphics card.
Windows runs in a window on your desktop, through [Looking Glass](https://looking-glass.io/),
at **~97.5%** of native GPU performance (measured with Geekbench on an RTX 3080).

The tool is a single Go binary with one main command. `orthogonals up` takes
the machine from a plain distro install to a working VM: it detects the
hardware, checks that the setup can work, configures the host, defines the
VM, builds unattended install media from your own ISO, and verifies the result.
Every host change is journaled with a backup of the original bytes, so `orthogonals undo`
restores the system exactly as it was.

## Supported hardware

`orthogonals preflight` checks all of this before anything is changed and
explains every refusal.

| Component | Requirement |
|---|---|
| OS | Fedora Workstation. Immutable variants (Silverblue, Kinoite, Bazzite) are not supported. |
| Machine | Desktop. Laptops are not supported. |
| Desktop GPU | Intel iGPU. It drives the Linux desktop. |
| Passthrough GPU | One NVIDIA dGPU, alone in its IOMMU group. |
| Firmware | VT-d (IOMMU) enabled. |
| RAM | 16 GiB or more. The guest needs at least 8 GiB. |

Three setups are refused on purpose. Single-GPU machines: the only GPU cannot
drive your desktop and be handed to the VM at the same time. AMD dGPUs: their
reset bugs need extra handling, planned for a later version. A dGPU that
shares its IOMMU group with other devices: orthogonals never applies the ACS
override kernel patch, because it breaks the isolation that makes passthrough
safe. Preflight tells you which case you hit and why.

The project was extracted from a machine that runs this setup daily:

- Fedora 44, GNOME on Wayland, Secure Boot on, LUKS root
- Intel Core i5-13600K, with the UHD 770 iGPU driving two monitors
- NVIDIA GeForce RTX 3080
- ASUS PRIME Z790-A WIFI, 32 GB RAM

This board has a 39-bit IOMMU, a common limit on consumer Alder Lake and
Raptor Lake boards that crashes QEMU with default firmware settings.
orthogonals detects the limit and applies the fix automatically.

## Quickstart

### 1. Prepare the machine

Your Linux desktop must run on the iGPU, because a graphics card that drives
a monitor cannot be handed to a VM. This is the only physical change
orthogonals asks for, and it takes nothing away from the NVIDIA card: while
no VM runs, games and GPU apps still use it (see
[Which GPU runs your apps](#which-gpu-runs-your-apps)).

1. Shut the PC down and move every monitor cable from the graphics card to
   the motherboard video outputs. After the move your desktop looks and
   behaves the same — the only difference is which chip sends the image to
   the monitor.
2. Boot Fedora and make sure the NVIDIA driver from
   [RPM Fusion](https://rpmfusion.org/Howto/NVIDIA) is installed and
   `nvidia-smi` works.
3. Download a Windows 11 ISO from
   [microsoft.com](https://www.microsoft.com/software-download/windows11).
   The standard multi-edition ISO works; **it must include the Pro edition.**

No BIOS visit is needed up front. `orthogonals preflight` checks the
firmware side and names the exact option on the rare board where one
matters:

- VT-d (IOMMU) disabled → fails and names the switch to enable. On most
  boards it is already on.
- iGPU disabled by the firmware (a common default when a graphics card is
  installed) → fails and names the option, usually "iGPU Multi-Monitor".
- Graphics card still set as the firmware's primary display → warns with an
  optional "Primary Display: CPU Graphics" change. Everything works without
  it; it only keeps the GRUB boot menu visible on your monitors.

`orthogonals detect` shows which connectors have displays, and preflight
names the exact cable to move if a monitor is still plugged into the
graphics card.

### 2. Install and run

```sh
sudo dnf copr enable stronautt/orthogonals
sudo dnf install orthogonals

orthogonals detect       # hardware inventory (read-only, no root needed)
orthogonals preflight    # go or no-go, with reasons (read-only)

sudo orthogonals up --win11-iso ~/Downloads/Win11.iso        # dry run
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso  # real run
```

The dry run prints every change the real run would make and touches nothing.

On the first real run, `up` installs the virtualization packages, configures
the host, and then asks you to reboot. This reboot happens once, for the host
setup only. After the reboot, run the same command again: it builds the
install media, defines the VM, installs Windows unattended, installs the
NVIDIA driver and Looking Glass inside the guest, and verifies the whole
pipeline. When it finishes, a "Windows 11" entry sits in your app grid; one
click starts the VM and opens the Looking Glass window.

> [!TIP]
> The guest account is `user` and its password is `password`. Change it inside Windows.

The host setup is a one-time step. Once it is done, you can create any number
of extra VMs without another reboot:

```sh
sudo orthogonals up --yes --vm-name gaming --display-name "Gaming" \
    --win11-iso ~/Downloads/Win11.iso
```

### 3. Upgrading

After upgrading the package, re-run `up` on the completed install:

```sh
sudo dnf upgrade orthogonals
sudo orthogonals up          # dry run: shows exactly what the new version changes
sudo orthogonals up --yes    # converge
```

On a finished setup, `up` converges instead of reinstalling: host configs are
rewritten only where the new version renders them differently, and the VM
definition is re-applied to libvirt only when its XML actually changed —
keeping the installed guest's display setup, credentials, TPM, and Secure
Boot state. No `--win11-iso` is needed once a VM is installed. A running VM
picks up a changed definition on its next boot.

Changed *settings* (`--disk`, `--disk-size`, `--binding`) are a different
story from a new version's defaults: those are refused with "journaled
command differs" — see [the troubleshooting entry](#apply-or-vm-refuses-journaled-command-differs-from-the-current-settings).
Fixes inside the Windows guest (provisioning scripts, guest-side config)
reach existing VMs only through a reinstall (`vm undefine --purge`, then
`up`).

### 4. Undo everything

```sh
sudo orthogonals undo        # dry run: lists everything it would restore
sudo orthogonals undo --yes  # restore the host
```

orthogonals is built to leave no trace. `undo` walks the change journal in
reverse and restores every file byte-for-byte, removes the kernel arguments,
regenerates the initramfs, and deletes the libvirt hooks. Your VM disks, the
ISO cache, and the config are kept so a later `up` can reuse them;
`undo --purge` deletes those too. Packages installed through package manager stay,
because removing shared system packages could break software you installed
in the meantime; remove them manually if you want them gone. If a system
update changed one of the managed files after apply, `undo` skips that file
and tells you; `--force` restores it anyway.

## How it works on the host

Two rules shape the whole design. Every host change goes through a journal
that `undo` can replay in reverse. And when something is wrong, orthogonals
refuses and explains instead of forcing and hoping.

### What it changes

The full list is printed by the dry run before anything happens.

- Kernel arguments, via grubby: `intel_iommu=on iommu=pt`.
- A dracut config that adds the vfio modules to the initramfs. This is the
  reason for the one reboot.
- An SELinux file-context rule and a tmpfiles entry for the Looking Glass
  shared-memory file.
- libvirt hooks that hand the GPU over: when a VM starts, the NVIDIA driver
  releases the card and vfio-pci takes it; on shutdown the reverse. If any
  process still holds the card, the VM refuses to start and the hook names
  the process. A failed start can never unbind the driver from a card the
  host is using. A sleep inhibitor is active while the VM runs, because
  sleeping a host with an active passthrough VM can hard-lock it.
- systemd units: `nvidia-persistenced` disabled (it holds `/dev/nvidia0`
  open, which would block every handover), `libvirt-guests` enabled (host
  shutdown shuts the guest down cleanly), `switcheroo-control` enabled.
- The Looking Glass client, built from the SHA256-pinned B7 source tarball,
  plus a launcher, desktop entry, and `~/Desktop` shortcut link per VM.
- Your desktop user joins the `libvirt` group, so the one-click launcher can
  start the VM without a password prompt.

**By default the binding is dynamic:** the NVIDIA card is a normal host GPU
whenever the VM is off, with CUDA, NVENC, and PRIME render offload all
working. `--binding=static` parks the card on vfio-pci at boot instead: the
host can never use it, but there is no driver rebind cycle to go wrong.

### Which GPU runs your apps

The goal is minimal friction on the host: no wrapper scripts and no custom
launchers. Orthogonals configures the stock desktop mechanisms so that apps
which want a powerful GPU get the NVIDIA card on their own:

- Steam and its games use the dGPU automatically. Steam's desktop entry
  ships the freedesktop `PrefersNonDefaultGPU=true` key, and GNOME honors it.
- Vulkan games, including everything under Proton and DXVK, need nothing.
  Both GPUs are visible and game engines pick the discrete one themselves,
  from any launch path.
- Any other app: right-click it in the GNOME app grid and pick "Launch using
  Discrete Graphics Card", or run `switcherooctl launch <app>` from a
  terminal.
- To pin an app to the dGPU permanently, copy its `.desktop` file to
  `~/.local/share/applications/` and add `PrefersNonDefaultGPU=true` (on
  KDE: `X-KDE-RunOnDiscreteGpu=true`).

The desktop session itself is kept off the NVIDIA card so a VM start always
finds it free. Some desktop apps need an extra push for that: Chromium and
GTK4 apps render their user interface with Vulkan, so a browser, editor, or
terminal would hold `/dev/nvidia*` for its whole lifetime while doing nothing
useful with it. orthogonals pins this known list (browsers, Electron apps
such as VS Code and Slack, GTK4 apps, Zed) to the iGPU through environment
settings and desktop-entry overrides.

One consequence to know: while any process holds the dGPU, the VM cannot
start. The gate refuses the handover, names the process, and sends a desktop
notification. It never kills anything. Close the app and start the VM again.

### More than one VM

The host setup is shared and VMs are additive: `up --vm-name <name>` creates
another VM with its own disk (`/var/lib/libvirt/images/<name>.qcow2`), its
own launcher, and its own desktop entry, with no reboot. Only one VM can run
at a time, because there is one dGPU; starting a second one is refused with a
message naming the VM that holds the card.

VMs you created yourself with virt-manager or virsh are not affected. The
hooks act only on VMs that orthogonals registered, so your existing VMs keep
running exactly as before.

To remove one VM, run `orthogonals vm undefine --vm-name <name>` (add
`--purge` to delete its disk too). To reinstall a VM from scratch while
keeping the host setup:

```sh
sudo orthogonals vm undefine --purge --yes
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso
```

## Command reference

Three global flags work with every command, before or after the command name.
`--yes` applies changes; **dry run is the default for every mutating command**,
printing what would happen and touching nothing. `--json` switches to machine-readable
output. `--root` prefixes all filesystem access (the testing seam; you will not need it).

`up` is the only command most users run; the rest are its building blocks,
useful on their own for inspection and repair.

### `orthogonals up`

Runs the whole pipeline (detect, preflight, apply, vm, media, install,
verify) as a persisted state machine, so it resumes where it left off after
the one host-setup reboot or after any interruption. It accepts the flags of
all the stages it runs, and on a resume the omitted flags keep the values the
first run applied.

```sh
sudo orthogonals up --win11-iso ~/Downloads/Win11.iso        # dry run: prints remaining stages
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso  # run the pipeline
sudo orthogonals up --yes --vm-name gaming --ram 24 \
    --win11-iso ~/Downloads/Win11.iso                        # extra VM on a prepared host
```

- `--win11-iso`: your Windows 11 installation ISO (required until media is built).
- `--binding`: `dynamic` (default) or `static`, see [What it changes](#what-it-changes).
- `--user`: desktop user that owns the Looking Glass shm file (default: the user invoking sudo).
- `--vm-name`: libvirt domain name (default `win11`).
- `--display-name`: desktop shortcut name (default "Windows 11" for the default VM, else the VM name).
- `--ram`: guest RAM in GiB (default: all of host RAM minus 8 GiB for the host).
- `--disk`: qcow2 path (default `/var/lib/libvirt/images/<vm-name>.qcow2`).
- `--disk-size`: disk size in GiB (default 100).
- `--resolution`: maximum guest resolution `WxH` (default 3840x2160).
- `--guest-user`, `--guest-password`: guest admin account (default `user` / `password`).
- `--locale`: guest locale and keyboard, e.g. `uk-UA` (default: the ISO's default language).
- `--nvidia-installer`: your own NVIDIA Windows driver installer, instead of the pinned download.

### `orthogonals detect`

Prints a read-only hardware inventory: GPUs, IOMMU groups, RAM, firmware. It
needs no root and has no flags beyond the globals.

```sh
orthogonals detect          # human-readable summary
orthogonals detect --json   # full inventory, the same JSON that goes in a bundle
```

### `orthogonals preflight`

Answers go or no-go without changing anything. It prints every check with a
fix for each failure, and the exit code reflects the overall status, so it
works in scripts.

```sh
orthogonals preflight && echo "good to go"
```

### `orthogonals apply`

Runs the host-setup stage alone: kernel arguments, vfio initramfs, SELinux
and tmpfiles rules, libvirt hooks, systemd units, the Looking Glass client.
Every change is journaled, and the first real run ends by asking for a
reboot.

```sh
sudo orthogonals apply                  # dry run: the full change list
sudo orthogonals apply --yes --binding=static
```

- `--binding`: `dynamic` (default) or `static`.
- `--user`: desktop user that owns the Looking Glass shm file (default: the user invoking sudo).

### `orthogonals vm define|undefine`

Creates or removes one VM on a prepared host: the domain XML, its disk, its
launcher and desktop entry. `undefine` keeps the disk unless you pass
`--purge`.

```sh
sudo orthogonals vm define --yes --vm-name gaming --display-name "Gaming"
sudo orthogonals vm undefine --yes --vm-name gaming --purge
```

- `--vm-name`, `--display-name`, `--ram`, `--disk`, `--disk-size`, `--resolution`: as in `up`.
- `--win11-iso`: attach the install CD (needed for a VM that will install Windows).
- `--purge`: with `undefine`, also delete the disk image and reset the `up` pipeline for a from-scratch reinstall.

### `orthogonals media`

Build the unattended install media from your ISO: the answer file, guest
provisioning scripts, the Virtual Display Driver, the NVIDIA guest driver,
and the Looking Glass host binary. Credentials, locale, and resolution stick
across rebuilds: an explicit flag wins, then the value from the previous run,
then the default.

```sh
sudo orthogonals media --yes --win11-iso ~/Downloads/Win11.iso --locale uk-UA
```

- `--win11-iso`: required.
- `--guest-user`, `--guest-password`, `--locale`, `--resolution`, `--nvidia-installer`: as in `up`.

### `orthogonals verify`

Checks the pipeline end to end for one VM: bindings, hooks, domain, guest
display. On failure it points you at `bundle`.

```sh
sudo orthogonals verify                    # the sole managed VM
sudo orthogonals verify --vm-name gaming   # required when several VMs exist
```

### `orthogonals status`

A lightweight health check of bindings, kernel arguments, hooks, and the
SELinux rule. It exits 0 when the applied setup is intact and 1 when
something (a kernel update, a manual change) has undone part of it.

```sh
sudo orthogonals status
```

### `orthogonals recover`

The escape hatch for a botched GPU handover, when `nvidia-smi` fails after
VM shutdown. It reloads the driver, re-enumerates the card, and tells you
when only a reboot will fix it. This is runtime repair, so nothing is
journaled.

```sh
sudo orthogonals recover --yes
```

### `orthogonals undo`

Walks the change journal in reverse and restores the host byte-for-byte; see
[Undo everything](#3-undo-everything).

```sh
sudo orthogonals undo --yes
```

- `--force`: restore files even if a system update changed them after apply.
- `--purge`: also remove the VM disks, ISO cache, state, and config.

### `orthogonals bundle`

Writes a redacted diagnostics tar.gz for a bug report; see
[How do I report a bug?](#how-do-i-report-a-bug) The optional argument names
the output file (default `orthogonals-bundle.tar.gz`).

```sh
orthogonals bundle my-report.tar.gz
```

### `orthogonals version`

Prints the binary version.

## Security notes

- No ACS override, ever. orthogonals refuses unsafe IOMMU groups instead of
  patching around them, because the patch removes the isolation between
  passthrough devices and the rest of the machine.
- Fail-safe hooks. A hook failure means the VM does not start. The reattach
  hook is guarded against the failed-start case, so it can never take the
  GPU away from running host apps.
- Nothing proprietary is bundled. You supply the Windows ISO. The NVIDIA
  guest driver is downloaded on your machine at media-build time, pinned to a
  known-good version by checksum (or pass `--nvidia-installer` for one you
  downloaded yourself). Looking Glass (GPLv2) and the Virtual Display Driver
  (MIT) come from their official releases, SHA256-pinned.
- Windows 11 requirements are met legitimately, with OVMF Secure Boot, an
  emulated TPM 2.0, and the host CPU model. There are no registry bypass
  hacks for Windows updates to break.
- Looking Glass uses `/dev/shm` instead of the kvmfr kernel module on
  purpose: kvmfr is an unsigned out-of-tree module, which contradicts the
  undoable-host idea and breaks under Secure Boot. The `/dev/shm` path is
  slightly slower and leaves nothing in your kernel.

## Troubleshooting

Start with these three commands:

```sh
sudo orthogonals status    # health check: bindings, kernel args, hooks
orthogonals bundle         # redacted diagnostics bundle for a bug report
journalctl -b | grep gpu   # hook output from the current boot
```

Every answer below comes from a real incident on the tested machine.

### The VM refuses to start and names a process holding the GPU

Why: this is the handover gate working as designed. Something on the host is
using the NVIDIA card.

Fix: close the named app and start the VM again. Steam must exit fully
(Steam menu, then Exit). To check by hand:
`sudo lsof /dev/nvidia0 /dev/nvidia-uvm /dev/nvidiactl /dev/nvidia-modeset`.
Do not bypass the gate; a forced start aborts anyway, just with a worse
error.

### QEMU crashes at VM boot with "vfio: DMA mapping failed"

Why: the guest firmware placed PCI device memory above what a 39-bit IOMMU
can address. Common on consumer Alder Lake and Raptor Lake boards.

Fix: the domain XML orthogonals generates already carries the working fix (a
`-fw_cfg` firmware argument). If you edited the XML by hand and lost the
`<qemu:commandline>` block, run `orthogonals vm define` again.

### Looking Glass says "can't open backing store /dev/shm/looking-glass: Permission denied"

Why: the shared-memory file lost its SELinux label, usually after a relabel
or manual recreation.

Fix: `sudo restorecon -v /dev/shm/looking-glass`, then check that `ls -Z`
shows `svirt_tmpfs_t` and the file is owned `<your user>:qemu` with mode
0660. `orthogonals status` verifies the rule exists.

### nvidia-smi says "No supported GPUs were found"

Why: while the VM runs, this is normal; the card belongs to vfio-pci. After
VM shutdown it means the reattach did not complete.

Fix: run `sudo orthogonals recover --yes`. It reloads the driver
and re-enumerates the card. If it reports a reboot is required, reboot.

### When is a reboot the answer?

Do not fight these; reboot: `orthogonals recover` fails, `dmesg` shows a
vfio or NVIDIA oops or `Xid` errors, `modprobe -r nvidia` hangs in D state,
or a failed VM start left virtqemud hung. Also run one VM start-stop cycle
after every NVIDIA driver or kernel update before trusting the setup; the
dynamic rebind is the least-exercised path in the NVIDIA driver.

### The host does not reach the desktop after apply and reboot

Fix: at the GRUB menu, edit the boot entry and delete the kernel arguments
apply added (it prints the exact list; with dynamic binding it is
`intel_iommu=on iommu=pt`). That disables passthrough for one boot without
changing any configuration. Then run `orthogonals undo` from the working
desktop.

### Clicks land in the wrong place in the guest, or the screen goes black

Why: two guest displays are active with absolute mouse coordinates, which
happens before guest provisioning finishes.

Fix: let provisioning finish; the final configuration keeps only the Virtual
Display Driver monitor. If you interrupted it, `orthogonals up` resumes. If
the SPICE setup display freezes during the Windows installer (a known
Windows 11 and SPICE issue), just wait; the unattended install continues
underneath and `up` reports progress from the guest side.

### Windows Setup never starts and the firmware asks to "Press any key to boot from CD"

Fix: open the VM console (`virt-viewer <vm-name>`) and press a key while the
prompt retries. This only happens on the first boot, while the disk is still
blank. Once Windows is installed, the disk boots first and the prompt never
returns.

### The Looking Glass client is missing libXss.so.1 or libXpresent.so.1

Why: these are runtime dependencies of the built client that arrived with
development packages; a package cleanup removed them.

Fix: `sudo dnf install libXScrnSaver libXpresent`. After any cleanup near
the client, check `ldd $(command -v looking-glass-client) | grep "not found"`.

### Building Looking Glass by hand fails with undefined ZSTD_* references

Why: Fedora's static libbfd needs zstd symbols that the Looking Glass
backtrace feature links against.

Fix: build with `-DENABLE_BACKTRACE=OFF`. orthogonals builds it that way.

### apply or vm refuses: "journaled command differs from the current settings"

Why: you changed a setting (`--binding`, `--vm-name`, `--disk`, a GPU swap)
that a journaled step already applied with a different value. Re-running
would stack the new configuration onto the old one, so the engine refuses.

Fix: undo the affected scope, then re-apply. For VM steps:
`orthogonals vm undefine --yes`. For host steps: `orthogonals undo --yes`,
plus a reboot when boot configuration is involved.

### Can a failed VM start take the GPU away from host apps?

No. libvirt fires the release hook even for a failed start, but the
installed reattach hook exits early unless the card is actually on vfio-pci.
If you replace the hooks with your own, keep that guard.

### The host went to sleep while the VM was running

Sleeping a host with an active passthrough VM can hard-lock it, which is why
the hooks hold a sleep inhibitor for the VM's lifetime. If the host slept
anyway (a forced power button, for example), reboot before using the VM or
the card again.

### How do I report a bug?

Attach the output of `orthogonals bundle`: a redacted tar.gz with the detect
JSON, `lspci -nnk`, the vfio and NVIDIA journal lines, the installed
orthogonals configs, and the libvirt hook log. Hostname, serial numbers,
machine-id, MAC addresses, and UUIDs are redacted; guest credentials are
stripped.

## Author

orthogonals is written and maintained by
Pavlo Hrytsenko <pavlo.o.hrytsenko@gmail.com>, 2026.

The project is licensed under the GNU General Public License v3.0; see
[LICENSE](LICENSE).

Contributions are highly welcome. Useful ways to help: bug reports with an
`orthogonals bundle` attached, reports from hardware other than the tested
board (especially different IOMMU layouts), and pull requests.
