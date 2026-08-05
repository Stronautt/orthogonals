<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/logo-dark.svg">
    <img src="docs/logo.svg" width="640" alt="orthogonals">
  </picture>
</p>

<p align="center"><em>Same machine, orthogonal axes: Windows at full GPU speed, Linux never pauses.</em></p>

<p align="center">
  <a href="https://github.com/stronautt/orthogonals/actions/workflows/ci.yml"><img src="https://github.com/stronautt/orthogonals/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/stronautt/orthogonals/actions/workflows/regression.yml"><img src="https://github.com/stronautt/orthogonals/actions/workflows/regression.yml/badge.svg" alt="Regression"></a>
  <a href="https://github.com/stronautt/orthogonals/actions/workflows/codeql.yml"><img src="https://github.com/stronautt/orthogonals/actions/workflows/codeql.yml/badge.svg" alt="CodeQL"></a>
  <a href="https://copr.fedorainfracloud.org/coprs/stronautt/orthogonals/package/orthogonals/"><img src="https://copr.fedorainfracloud.org/coprs/stronautt/orthogonals/package/orthogonals/status_image/last_build.png" alt="COPR build" /></a>
  <a href="https://github.com/stronautt/orthogonals/releases/latest"><img src="https://img.shields.io/github/v/release/stronautt/orthogonals" alt="Latest release"></a>
  <img src="https://img.shields.io/badge/license-GPL--3.0-blue" alt="License: GPL-3.0">
</p>

<p align="center">
  <img src="docs/demo.gif" alt="orthogonals demo: one command from a plain Fedora install to Windows 11 in a window" width="768">
</p>

## Introduction

> [!CAUTION]
> Use this at your own risk. orthogonals is pre-alpha software aimed at
> enthusiasts, and it reconfigures your host at a low level: kernel
> parameters, GPU drivers, libvirt. The author takes no responsibility for
> damage to your PC.

Many Linux users keep a Windows partition around, because some games and some
professional software run nowhere else. Dual boot works, but every switch
costs a reboot, and your whole Linux session goes with it.

orthogonals replaces that arrangement. It turns a Linux desktop with an iGPU
and one dGPU into a host for a VM that owns the physical graphics card.
Windows then runs in a window on your desktop through
[Looking Glass](https://looking-glass.io/), at **~97.5%** of native GPU
performance (measured with Geekbench on an RTX 3080).

The tool is a single Go binary with one main command. `orthogonals up` takes
the machine from a plain distro install to a working VM:

- detects the hardware
- checks that the setup can work
- configures the host
- defines the VM
- builds unattended install media from your own ISO
- verifies the result

Every host change is journaled with a backup of the original bytes, so
`orthogonals undo` puts the system back exactly as it was.

## Supported hardware

`orthogonals preflight` checks all of this before anything is changed, and
explains every refusal.

| Component | Requirement |
|---|---|
| OS | Fedora Workstation. Immutable variants (Silverblue, Kinoite, Bazzite) are not supported. |
| Machine | Desktop, or a hybrid-graphics laptop (experimental, see below). |
| Host GPU | Intel or AMD iGPU. It drives the Linux desktop. |
| Passthrough GPU | One NVIDIA dGPU, alone in its IOMMU group. |
| Firmware | IOMMU enabled: VT-d on Intel, AMD-Vi on AMD. |
| RAM | 16 GiB or more. The guest needs at least 8 GiB. |

Three setups are refused on purpose. A single-GPU machine cannot work, since
the one card cannot drive your desktop and belong to the VM at the same time.
AMD dGPUs are out for now, because their reset bugs need extra handling that
is planned for a later version. And a dGPU that shares its IOMMU group with
other devices is refused because orthogonals never applies the ACS override
kernel patch, which breaks the isolation that makes passthrough safe.
preflight tells you which case you hit and why.

> [!WARNING]
> **Laptop support is fully experimental.** It is built and tested only
> against synthetic fixtures, and no real hybrid-graphics laptop has run it
> yet. Laptops vary far more than desktops do: display MUX, power gating,
> per-model firmware. Treat a laptop run as unproven and be ready to `undo`.
> Reports from real hardware are very welcome.

A hybrid laptop works the same way: the internal panel stays on the iGPU and
the NVIDIA dGPU goes to the guest. The BIOS graphics mode has to be
**hybrid/Optimus**, not discrete-only. preflight reads the ASUS display MUX
and the Dell, Lenovo and HP firmware settings where the kernel exposes them,
and names the exact switch where it cannot. A MUXless dGPU (a 3D controller
with no display outputs) often needs its own vBIOS: pass `--gpu-rom <file>`
and orthogonals installs it into the VM. On a laptop, apply also turns on
NVIDIA RTD3 runtime power management, so the dGPU still suspends for battery
when no VM runs.

The project was extracted from a machine that runs this setup daily:

- Fedora 44, GNOME on Wayland, Secure Boot on, LUKS root
- Intel Core i5-13600K, with the UHD 770 iGPU driving two monitors
- NVIDIA GeForce RTX 3080
- ASUS PRIME Z790-A WIFI, 32 GB RAM

That board has a 39-bit IOMMU, a common limit on consumer Alder Lake and
Raptor Lake boards, and it crashes QEMU with default firmware settings.
orthogonals detects the limit and applies the fix automatically.

## Quickstart

### 1. Prepare the machine

Your Linux desktop has to run on the iGPU, because a graphics card that
drives a monitor cannot also belong to a VM. This is the only physical change
orthogonals asks for, and it costs you nothing on the NVIDIA card: while no
VM runs, games and GPU apps still use it (see
[Which GPU runs your apps](#which-gpu-runs-your-apps)).

1. Shut the PC down and move every monitor cable from the graphics card to
   the motherboard video outputs. Your desktop looks and behaves the same
   afterwards; the only difference is which chip sends the image to the
   monitor. A laptop has no cable to move, since the internal panel already
   runs on the iGPU in hybrid mode. Just make sure the BIOS graphics mode is
   hybrid/Optimus, which preflight checks for you.
2. Boot Fedora and make sure the NVIDIA driver from
   [RPM Fusion](https://rpmfusion.org/Howto/NVIDIA) is installed and that
   `nvidia-smi` works.
3. Download a Windows 11 ISO from
   [microsoft.com](https://www.microsoft.com/software-download/windows11).
   The standard multi-edition ISO works. **It must include the Pro edition.**

No BIOS visit is needed up front. `orthogonals preflight` covers the firmware
side and names the exact option on the rare board where one matters:

- IOMMU (VT-d on Intel, AMD-Vi on AMD) disabled. preflight fails and names
  the switch to turn it on, though most boards have it on already. Where the
  kernel exposes the BIOS settings (Dell, Lenovo and HP business models), it
  names the exact attribute under `/sys/class/firmware-attributes` to flip.
- iGPU disabled by the firmware, a common default when a graphics card is
  installed. preflight fails and names the option, usually "iGPU
  Multi-Monitor".
- The graphics card still set as the firmware's primary display. preflight
  warns and suggests "Primary Display: CPU Graphics". Everything works
  without it; the change only keeps the GRUB boot menu visible on your
  monitors.
- On a laptop, a graphics mode set to discrete-only rather than
  hybrid/Optimus. preflight reads the ASUS `gpu_mux_mode` knob and prints the
  one-liner to switch it; on other laptops it names the BIOS "GPU Mode" or
  "Optimus" setting.

`orthogonals detect` shows which connectors have displays, and preflight
names the exact cable to move if a monitor is still plugged into the graphics
card.

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
the host, and then asks you to reboot. That reboot happens once, for the host
setup only. Run the same command again afterwards and the second pass does
the rest:

- builds the install media
- defines the VM
- installs Windows unattended
- installs the NVIDIA driver and Looking Glass inside the guest
- verifies the whole pipeline

When it finishes, a "Windows 11" entry sits in your app grid. One click
starts the VM and opens the Looking Glass window.

> [!TIP]
> The guest account is `user` and its password is `password`. Change it
> inside Windows.

The host setup is a one-time step. Once it is done, you can create as many
extra VMs as you want without another reboot:

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

On a finished setup, `up` converges instead of reinstalling. Host configs are
rewritten only where the new version renders them differently, and the VM
definition goes back to libvirt only when its XML actually changed, so the
installed guest keeps its display setup, credentials, TPM and Secure Boot
state. Once a VM is installed you no longer need `--win11-iso`. A running VM
picks up a changed definition on its next boot.

Changed *settings* (`--disk`, `--disk-size`, `--binding`) are a different
story from a new version's defaults. Those are refused with "journaled
command differs" (see
[the troubleshooting entry](#apply-or-vm-refuses-journaled-command-differs-from-the-current-settings)).
Fixes inside the Windows guest, such as provisioning scripts and guest-side
config, reach an existing VM only through a reinstall: `vm undefine --purge`,
then `up`.

### 4. Undo everything

```sh
sudo orthogonals undo        # dry run: lists everything it would restore
sudo orthogonals undo --yes  # restore the host
```

orthogonals is built to leave no trace. `undo` walks the change journal in
reverse: it restores every file byte-for-byte, removes the kernel arguments,
regenerates the initramfs and deletes the libvirt hooks.

Your VM disks, the ISO cache and the config are kept so a later `up` can
reuse them; `undo --purge` deletes those too. Packages installed through the
package manager stay, because removing shared system packages can break
software you installed in the meantime. Remove them by hand if you want them
gone. And if a system update changed one of the managed files after apply,
`undo` skips that file and tells you; `--force` restores it anyway.

## How it works on the host

Two rules shape the whole design. Every host change goes through a journal
that `undo` can replay in reverse. And when something is wrong, orthogonals
refuses and explains instead of forcing the change and hoping.

### What it changes

The full list is printed by the dry run before anything happens.

- Kernel arguments: `intel_iommu=on iommu=pt` on a VT-d (Intel) platform,
  `iommu=pt` on AMD-Vi.
- A dracut config that adds the vfio modules to the initramfs. This is the
  reason for the one reboot.
- On a laptop only: a modprobe.d option and udev rules that enable NVIDIA
  RTD3, so the dGPU still runtime-suspends to D3cold for battery when no VM
  runs. `nvidia-powerd` is disabled too, since it holds the card open and
  blocks the handover.
- An SELinux file-context rule and a tmpfiles entry for the Looking Glass
  shared-memory file.
- libvirt hooks that hand the GPU over. When a VM starts, the NVIDIA driver
  releases the card and vfio-pci takes it; on shutdown the reverse. If any
  process still holds the card, the VM refuses to start and the hook names
  the process. A failed start can never unbind the driver from a card the
  host is using. A sleep inhibitor stays active while the VM runs, because
  sleeping a host with an active passthrough VM can hard-lock it.
- systemd units: `nvidia-persistenced` off (it holds `/dev/nvidia0` open,
  which would block every handover), `libvirt-guests` on (host shutdown then
  shuts the guest down cleanly), `switcheroo-control` on.
- The Looking Glass client, installed from the pinned `looking-glass-client`
  RPM (B7, matching the guest host), plus a desktop entry and a `~/Desktop`
  shortcut per VM.
- Your desktop user joins the `libvirt` group, so the one-click launcher can
  start the VM without a password prompt.

**By default the binding is dynamic:** whenever the VM is off, the NVIDIA
card is a normal host GPU, and CUDA, NVENC and PRIME render offload all work.
`--binding=static` parks the card on vfio-pci at boot instead. The host can
then never touch it, but there is no driver rebind cycle to go wrong.

### Which GPU runs your apps

The goal is minimal friction on the host: no wrapper scripts, no custom
launchers. orthogonals configures the stock desktop mechanisms so that apps
which want a fast GPU get the NVIDIA card on their own:

- Steam and its games use the dGPU automatically. Steam's desktop entry ships
  the freedesktop `PrefersNonDefaultGPU=true` key, and GNOME honors it.
- Vulkan games, including everything under Proton and DXVK, need nothing.
  Both GPUs are visible and game engines pick the discrete one themselves,
  from any launch path.
- Any other app: right-click it in the GNOME app grid and pick "Launch using
  Discrete Graphics Card", or run `switcherooctl launch <app>` from a
  terminal.
- To pin an app to the dGPU permanently, copy its `.desktop` file to
  `~/.local/share/applications/` and add `PrefersNonDefaultGPU=true` (on KDE:
  `X-KDE-RunOnDiscreteGpu=true`).

The desktop session itself is kept off the NVIDIA card so a VM start always
finds it free. Some apps need an extra push for that. Chromium and GTK4 apps
draw their interface with Vulkan, so a browser, editor or terminal will hold
`/dev/nvidia*` for its whole lifetime while doing nothing useful with it.
orthogonals pins that known list (browsers, Electron apps such as VS Code and
Slack, GTK4 apps, Zed) to the iGPU through environment variables and
desktop-entry overrides.

One consequence to know: while any process holds the dGPU, the VM cannot
start. The gate refuses the handover, names the process and sends a desktop
notification. It never kills anything. Close the app and start the VM again.

### More than one VM

The host setup is shared and VMs are additive: `up --vm-name <name>` creates
another VM with its own disk (`/var/lib/libvirt/images/<name>.qcow2`), its
own launcher and its own desktop entry, with no reboot. Only one VM can run
at a time, because there is one dGPU; starting a second one is refused with a
message naming the VM that holds the card.

VMs you created yourself with virt-manager or virsh are not affected. The
hooks act only on VMs that orthogonals registered, so your existing VMs keep
running exactly as before.

To remove one VM, run `orthogonals vm undefine --vm-name <name>` and add
`--purge` to delete its disk too. To reinstall a VM from scratch while
keeping the host setup:

```sh
sudo orthogonals vm undefine --purge --yes
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso
```

## Command reference

Three global flags work with every command, before or after the command name.
`--yes` applies changes: **dry run is the default for every command that
changes the host**, printing what would happen and touching nothing. `--json`
switches to machine-readable output. `--root` prefixes all filesystem access
(the testing seam; you will not need it).

`up` is the only command most users run. The rest are its building blocks,
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
- `--share`: host directory to export to the guest over virtiofs, as a drive
  letter counting down from `Z:`; repeat for more, `--share ""` clears them.
  Only shares present at install time get mounted.
- `--resolution`: maximum guest resolution `WxH` (default 3840x2160).
- `--guest-user`, `--guest-password`: guest admin account (default `user` / `password`).
- `--locale`: guest locale and keyboard, for example `uk-UA` (default: the ISO's default language).
- `--gpu-rom`: an extracted GPU vBIOS ROM, for a MUXless laptop dGPU that gives no guest output.
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
and tmpfiles rules, libvirt hooks and systemd units. Every change is
journaled, and the first real run ends by asking for a reboot.

```sh
sudo orthogonals apply                  # dry run: the full change list
sudo orthogonals apply --yes --binding=static
```

- `--binding`: `dynamic` (default) or `static`.
- `--user`: desktop user that owns the Looking Glass shm file (default: the user invoking sudo).

### `orthogonals vm define|undefine`

Creates or removes one VM on a prepared host: the domain XML, its disk, its
launcher and its desktop entry. `undefine` keeps the disk unless you pass
`--purge`.

```sh
sudo orthogonals vm define --yes --vm-name gaming --display-name "Gaming"
sudo orthogonals vm undefine --yes --vm-name gaming --purge
```

- `--vm-name`, `--display-name`, `--ram`, `--disk`, `--disk-size`, `--resolution`, `--share`, `--gpu-rom`: as in `up`.
- `--win11-iso`: attach the install CD, which a VM that will install Windows needs.
- `--purge`: with `undefine`, also delete the disk image and reset the `up` pipeline for a from-scratch reinstall.

### `orthogonals media`

Builds the unattended install media from your ISO: the answer file, guest
provisioning scripts, the Virtual Display Driver, the NVIDIA guest driver and
the Looking Glass host binary. Credentials, locale and resolution stick
across rebuilds. An explicit flag wins, then the value from the previous run,
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

A lightweight health check of bindings, kernel arguments, hooks and the
SELinux rule. It exits 0 when the applied setup is intact and 1 when
something, a kernel update or a manual change, has undone part of it.

```sh
sudo orthogonals status
```

### `orthogonals recover`

The escape hatch for a botched GPU handover, when `nvidia-smi` fails after VM
shutdown. It reloads the driver, re-enumerates the card and tells you when
only a reboot will fix it. This is runtime repair, so nothing is journaled.

```sh
sudo orthogonals recover --yes
```

### `orthogonals undo`

Walks the change journal in reverse and restores the host byte-for-byte; see
[Undo everything](#4-undo-everything).

```sh
sudo orthogonals undo --yes
```

- `--force`: restore files even if a system update changed them after apply.
- `--purge`: also remove the VM disks, ISO cache, state and config.

### `orthogonals bundle`

Writes a redacted diagnostics tar.gz for a bug report; see
[How do I report a bug?](#how-do-i-report-a-bug). The optional argument names
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
  hook is guarded against the failed-start case, so it can never take the GPU
  away from running host apps.
- Nothing proprietary is bundled. You supply the Windows ISO. The NVIDIA
  guest driver is downloaded on your machine when the media is built, pinned
  by checksum to a known-good version, or pass `--nvidia-installer` for one
  you downloaded yourself. Looking Glass (GPLv2) and the Virtual Display
  Driver (MIT) come from their official releases, SHA256-pinned.
- Windows 11 requirements are met legitimately, with OVMF Secure Boot, an
  emulated TPM 2.0 and the host CPU model. There are no registry bypass hacks
  for Windows updates to break.
- Looking Glass uses the kvmfr kernel module when it is available and
  `/dev/shm` when it is not. With kvmfr your iGPU pulls frames straight out
  of the buffer over DMA instead of the client copying them. Measured on the
  reference host at 2560x1440, that removed one full-frame write per frame
  (~800 MiB/s of memory bandwidth), cut the client's CPU from 7.4% of a core
  to 3.0% and halved the iGPU's clock. On an iGPU desktop that bandwidth is
  the scarce resource, which is why upstream treats kvmfr as a requirement
  rather than a tuning knob.
  - It is a DKMS module, so every kernel update rebuilds it and signs it with
    the key your host already uses for its other out-of-tree modules. Secure
    Boot needs no new enrollment in the normal case, and `preflight` answers
    that per host instead of guessing.
  - It loads only while a VM runs, sized to that VM, and never at boot.
  - If it ever fails to build, the VM refuses to start and says so on screen;
    `sudo orthogonals up` re-renders the domain back onto `/dev/shm`.
- The frame buffer is readable by the desktop user and the `qemu` group with
  either backend (`0660 <user>:qemu`). That is unchanged between the two
  paths.

## Troubleshooting

Start with these three commands:

```sh
sudo orthogonals status    # health check: bindings, kernel args, hooks
orthogonals bundle         # redacted diagnostics bundle for a bug report
journalctl -b | grep gpu   # hook output from the current boot
```

Every answer below comes from a real incident on the tested machine.

### The VM refuses to start and says the kvmfr module is unavailable

Why: a kernel update landed and DKMS could not rebuild the module, so the
domain names a device that does not exist. The hook refuses the start rather
than letting QEMU create a plain file where the device should be, which would
leave the guest writing frames nothing reads.

```sh
dkms status                # what failed to build
sudo orthogonals status    # which VM wants kvmfr, and for which kernel
sudo orthogonals up        # re-render the domain onto /dev/shm and carry on
```

`up` puts you back on the slower path immediately; fixing the build and
running `up` again moves you back. Removing the `kvmfr-dkms` package lands in
the same place.

### The VM refuses to start and names a process holding the GPU

Why: this is the handover gate working as designed. Something on the host is
using the NVIDIA card.

Fix: close the named app and start the VM again. Steam must exit fully (Steam
menu, then Exit). To check by hand:
`sudo lsof /dev/nvidia0 /dev/nvidia-uvm /dev/nvidiactl /dev/nvidia-modeset`.
Do not bypass the gate; a forced start aborts anyway, just with a worse
error.

### QEMU crashes at VM boot with "vfio: DMA mapping failed"

Why: the guest firmware placed PCI device memory above what a 39-bit IOMMU
can address. Common on consumer Alder Lake and Raptor Lake boards.

Fix: the domain XML orthogonals generates already carries the working fix, a
`-fw_cfg` firmware argument. If you edited the XML by hand and lost the
`<qemu:commandline>` block, run `orthogonals vm define` again.

### Looking Glass says "can't open backing store /dev/shm/looking-glass: Permission denied"

Why: the shared-memory file lost its SELinux label, usually after a relabel
or a manual recreation.

Fix: `sudo restorecon -v /dev/shm/looking-glass`, then check that `ls -Z`
shows `svirt_tmpfs_t` and that the file is owned `<your user>:qemu` with mode
0660. `orthogonals status` verifies the rule exists.

### nvidia-smi says "No supported GPUs were found"

Why: while the VM runs, this is normal, since the card belongs to vfio-pci.
After VM shutdown it means the reattach did not complete.

Fix: run `sudo orthogonals recover --yes`. It reloads the driver and
re-enumerates the card. If it reports that a reboot is required, reboot.

### When is a reboot the answer?

Do not fight these. Reboot when:

- `orthogonals recover` fails
- `dmesg` shows a vfio or NVIDIA oops, or `Xid` errors
- `modprobe -r nvidia` hangs in D state
- a failed VM start left virtqemud hung

Also run one VM start-stop cycle after every NVIDIA driver or kernel update
before trusting the setup; the dynamic rebind is the least-exercised path in
the NVIDIA driver.

### The host does not reach the desktop after apply and reboot

Fix: at the GRUB menu, edit the boot entry and delete the kernel arguments
apply added. It prints the exact list, and with dynamic binding it is
`intel_iommu=on iommu=pt`. That disables passthrough for one boot without
changing any configuration. Then run `orthogonals undo` from the working
desktop.

### Clicks land in the wrong place in the guest, or the screen goes black

Why: two guest displays are active with absolute mouse coordinates, which
happens before guest provisioning finishes.

Fix: let provisioning finish, and the final configuration keeps only the
Virtual Display Driver monitor. If you interrupted it, `orthogonals up`
resumes. If the SPICE setup display freezes during the Windows installer, a
known Windows 11 and SPICE issue, wait it out: the unattended install
continues underneath and `up` reports progress from the guest side.

### Windows Setup never starts and the firmware asks to "Press any key to boot from CD"

Fix: open the VM console (`virt-viewer <vm-name>`) and press a key while the
prompt retries. This only happens on the first boot, while the disk is still
blank. Once Windows is installed, the disk boots first and the prompt never
returns.

### The guest boots but the passed-through GPU shows no image (laptop)

Why: a MUXless laptop dGPU, a 3D controller with no display outputs, can hang
OVMF on its option ROM or stay dark without its own vBIOS. Preflight's `mux`
check warns when it detects this topology.

Fix: extract the dGPU vBIOS and pass it, so orthogonals installs it and
renders `<rom file=...>` in the domain: `orthogonals up --gpu-rom <rom.bin>
...` (or `vm define --gpu-rom`). The ROM is stored under
`/var/lib/orthogonals/vbios` and kept across converges.

### apply or vm refuses: "journaled command differs from the current settings"

Why: you changed a setting (`--binding`, `--vm-name`, `--disk`, a GPU swap)
that a journaled step already applied with a different value. Re-running
would stack the new configuration onto the old one, so the engine refuses.

Fix: undo the affected scope, then re-apply. For VM steps:
`orthogonals vm undefine --yes`. For host steps: `orthogonals undo --yes`,
plus a reboot when boot configuration is involved.

### Apply refuses because of a line in /etc/default/grub

orthogonals edits `GRUB_CMDLINE_LINUX` there so that a `grub2-mkconfig`,
which any kernel or grub2 update can run, rebuilds the boot entries with the
IOMMU arguments still in them. It edits that variable only where it can read
the line back exactly as the shell would, and refuses by line number
otherwise:

- an assignment form it does not model (`export GRUB_CMDLINE_LINUX=…`, a lone
  `GRUB_CMDLINE_LINUX+=…`, a bare mention of the variable elsewhere in the
  file)
- a value carrying `$`, a backtick or a backslash, which the shell would
  expand
- a value it cannot split into arguments: a trailing `# comment`, an
  unbalanced quote, or an escaped quote such as `acpi_osi=\"Windows 2009\"`

The refusal is deliberate. The alternative to naming the line is appending a
fresh `GRUB_CMDLINE_LINUX=` assignment, which wins under `sh` and silently
drops whatever the host booted with (`rd.luks.uuid=`, `resume=`,
`rd.md.uuid=`) at the next regeneration. Rewrite that one line as a plain
quoted assignment and re-run; `orthogonals preflight` prints the line and the
reason.

`GRUB_CMDLINE_LINUX_DEFAULT` is left alone: it is additive to
`GRUB_CMDLINE_LINUX`, so arguments written there already reach the boot path.

### Can a failed VM start take the GPU away from host apps?

No. A handover that fails rolls itself back before it reports, so the card is
already on the host driver by the time the error reaches you. If the process
is killed mid-handover instead, `/run/orthogonals-handover` records that one
started: libvirt fires the release hook even for a failed start, and the
reattach hook undoes the handover whenever that marker is there or the card
is on vfio-pci. It does nothing when neither is true, so a start refused
before it touched anything costs the desktop nothing. If you replace the
hooks with your own, keep both halves of that guard.

A handover is also refused outright, before it changes anything, if the
passthrough devices have no IOMMU group. Without one the kernel cannot bind
them to vfio-pci, and starting the eviction would only strand the card.

### The host went to sleep while the VM was running

Sleeping a host with an active passthrough VM can hard-lock it, which is why
the hooks hold a sleep inhibitor for the VM's lifetime. If the host slept
anyway, after a forced power button for example, reboot before using the VM
or the card again.

### How do I report a bug?

Attach the output of `orthogonals bundle`: a redacted tar.gz with the detect
JSON, `lspci -nnk`, the vfio and NVIDIA journal lines, the installed
orthogonals configs and the libvirt hook log. Hostname, serial numbers,
machine-id, MAC addresses and UUIDs are redacted; guest credentials are
stripped.

## Author

<img src="docs/favicon.svg" width="128" align="left" hspace="24" vspace="6" alt="orthogonals">

orthogonals is written and maintained by Pavlo Hrytsenko
&lt;pavlo.o.hrytsenko@gmail.com&gt;, 2026.

The project is licensed under the GNU General Public License v3.0; see
[LICENSE](LICENSE).

Contributions are highly welcome. Useful ways to help: bug reports with an
`orthogonals bundle` attached, reports from hardware other than the tested
board (especially different IOMMU layouts), and pull requests.
