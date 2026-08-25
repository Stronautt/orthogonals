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
> orthogonals is pre-alpha software for enthusiasts. It changes your kernel
> parameters, your GPU drivers and your libvirt setup. Use it at your own
> risk. The author takes no responsibility for damage to your PC.

Many Linux users keep a Windows partition for the games and the professional
software that runs nowhere else. Every switch costs a reboot, and the whole
Linux session goes with it.

orthogonals removes that reboot. It turns a Linux desktop with an iGPU and one
dGPU into the host of a VM that owns the physical graphics card. Windows then
runs in a window on your desktop through
[Looking Glass](https://looking-glass.io/), at **~97.5%** of native GPU speed
(measured with Geekbench on an RTX 3080).

The tool is one Go binary with one main command:

```sh
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso
```

`up` detects the hardware, reports whether the host meets the requirements,
configures the host, defines the VM, builds unattended install media from your
ISO, and verifies the result. Every host change goes into a journal with a
backup of the original bytes. `orthogonals undo` puts the host back as it was.

## Supported hardware

| Component | Requirement |
|---|---|
| OS | Fedora Workstation. Immutable variants (Silverblue, Kinoite, Bazzite) do not work. |
| Machine | A desktop, or a hybrid-graphics laptop (experimental). |
| Host GPU | An Intel or AMD iGPU. It drives the Linux desktop. |
| Passthrough GPU | One NVIDIA dGPU, alone in its IOMMU group. |
| Firmware | IOMMU on: VT-d on Intel, AMD-Vi on AMD. |
| RAM | 16 GiB or more. The guest gets 5/8 of host RAM and needs 8 GiB. |

`orthogonals preflight` reads all of this before anything changes, and it
explains every refusal. orthogonals refuses three setups on purpose:

- **A single-GPU machine.** One card cannot drive your desktop and belong to
  the VM at the same time.
- **An AMD dGPU.** The reset behavior of these cards needs extra handling that
  comes in a later version.
- **A dGPU that shares its IOMMU group.** orthogonals never applies the ACS
  override kernel patch, because the patch removes the isolation that makes
  passthrough safe.

> [!WARNING]
> **Laptop support is fully experimental.** Only synthetic fixtures cover it.
> No real hybrid-graphics laptop ran it yet. Laptops vary much more than
> desktops: display MUX, power gating, per-model firmware. Treat a laptop run
> as unproven and be ready to run `undo`. Reports from real hardware are very
> welcome.

On a laptop, the internal panel stays on the iGPU and the NVIDIA dGPU goes to
the guest. The BIOS graphics mode must be **hybrid/Optimus**, not
discrete-only. `apply` also enables NVIDIA RTD3 there, so the dGPU still
suspends for battery when no VM runs.

The project comes from a machine that runs this setup daily: Fedora 44 on
Wayland with Secure Boot and LUKS, an Intel Core i5-13600K with the UHD 770
iGPU on two monitors, an NVIDIA GeForce RTX 3080, and an ASUS PRIME Z790-A
WIFI board with 32 GB RAM. That board has a 39-bit IOMMU, a common limit on
consumer Alder Lake and Raptor Lake boards. It crashes QEMU with default
firmware settings, so orthogonals detects the limit and applies the fix.

## Quickstart

### 1. Prepare the machine

Your Linux desktop must run on the iGPU, because a graphics card that drives a
monitor cannot also belong to a VM. This is the only physical change. It costs
you nothing on the NVIDIA card: while no VM runs, games and GPU apps still use
it (see [Which GPU runs your apps](#which-gpu-runs-your-apps)).

1. Shut the PC down. Move every monitor cable from the graphics card to the
   motherboard video outputs.

   > The desktop looks and behaves the same afterwards. Only the chip that
   > sends the image to the monitor changes. A laptop has no cable to move,
   > because the internal panel already runs on the iGPU in hybrid mode.

2. Boot Fedora. Install the NVIDIA driver from
   [RPM Fusion](https://rpmfusion.org/Howto/NVIDIA) and make sure that
   `nvidia-smi` works.
3. Download a Windows 11 ISO from
   [microsoft.com](https://www.microsoft.com/software-download/windows11). The
   standard multi-edition ISO works. **It must include the Pro edition.**

You do not need to visit the BIOS first. `orthogonals preflight` covers the
firmware side and names the exact option on the rare board where one matters:

- **IOMMU off.** preflight fails and names the switch. On Dell, Lenovo and HP
  business models it names the exact attribute under
  `/sys/class/firmware-attributes`.
- **iGPU off**, a common default when a graphics card is installed. preflight
  fails and names the option, usually "iGPU Multi-Monitor".
- **The graphics card set as the primary display.** preflight warns and
  suggests "Primary Display: CPU Graphics". This change only keeps the GRUB
  boot menu visible on your monitors.
- **A laptop graphics mode set to discrete-only.** preflight reads the ASUS
  `gpu_mux_mode` knob, or it names the BIOS "GPU Mode" setting.

### 2. Install and run

```sh
sudo dnf copr enable stronautt/orthogonals
sudo dnf install orthogonals

orthogonals detect       # hardware inventory (read-only, no root needed)
orthogonals preflight    # go or no-go, with reasons (read-only)

sudo orthogonals up --win11-iso ~/Downloads/Win11.iso        # dry run
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso  # real run
```

The dry run prints every change that the real run makes, and touches nothing.

The first real run configures the host and then asks you to reboot. That
reboot happens once, for the host setup. Run the same command again. The
second pass builds the install media, defines the VM, installs Windows
unattended, installs the NVIDIA driver and Looking Glass in the guest, and
verifies the whole pipeline.

At the end, a "Windows 11" entry sits in your app grid. One click starts the VM
and opens the Looking Glass window.

> [!TIP]
> The guest account is `user` and its password is `password`. Change it inside
> Windows.

The host setup is a one-time step. After it is done, you can create more VMs
without another reboot:

```sh
sudo orthogonals up --yes --vm-name gaming --display-name "Gaming" \
    --win11-iso ~/Downloads/Win11.iso
```

### 3. Upgrade

```sh
sudo dnf upgrade orthogonals
sudo orthogonals up          # dry run: what the new version changes
sudo orthogonals up --yes    # converge
```

On a finished setup, `up` converges instead of a reinstall. It rewrites a host
file only where the new version renders it differently. It sends the VM
definition back to libvirt only when the XML changed. The installed guest
keeps its display setup, credentials, TPM and Secure Boot state. A running VM
picks up a changed definition on its next boot. After the install completes,
you no longer need `--win11-iso`.

Changed *settings* (`--disk`, `--disk-size`, `--binding`) are a different case
from the defaults of a new version. orthogonals refuses those with "journaled
command differs" (see
[the troubleshooting entry](#apply-or-vm-refuses-journaled-command-differs)).
Fixes inside the Windows guest reach an existing VM only through a reinstall:
run `vm undefine --purge`, then `up`.

### 4. Undo everything

```sh
sudo orthogonals undo        # dry run: what it restores
sudo orthogonals undo --yes  # restore the host
```

`undo` walks the change journal in reverse. It restores every file
byte-for-byte, removes the kernel arguments, regenerates the initramfs and
removes the libvirt hooks.

orthogonals keeps your VM disks, the ISO cache and the settings, so that a
later `up` can reuse them. `undo --purge` removes those too. Packages stay,
because the removal of shared system packages can break other software.
If you want them gone, remove them by hand. If a system update changed a
managed file after apply, `undo` skips that file and tells you. `--force`
restores it anyway.

## How it works on the host

Two rules shape the design. Every host change goes through a journal that
`undo` can replay in reverse. When something is wrong, orthogonals refuses and
explains. It never forces the change and hopes.

### What it changes

The dry run prints the full list before anything happens.

- **Kernel arguments**: `intel_iommu=on iommu=pt` on VT-d, `iommu=pt` on
  AMD-Vi.
- **A dracut config** that adds the vfio modules to the initramfs. This is the
  reason for the one reboot.
- **On a laptop only**: a modprobe.d option and udev rules that enable NVIDIA
  RTD3, so the dGPU still suspends to D3cold for battery when no VM runs.
  orthogonals disables `nvidia-powerd` too, because it holds the card open and
  blocks the handover.
- **An SELinux file-context rule and a tmpfiles entry** for the Looking Glass
  shared-memory file.
- **libvirt hooks** that hand the GPU over. On VM start, the NVIDIA driver
  releases the card and vfio-pci takes it. On shutdown the reverse happens. If
  a process still holds the card, the VM refuses to start and the hook names
  the process. A failed start can never unbind the driver from a card that the
  host uses. A sleep inhibitor stays active while the VM runs, because sleep
  with an active passthrough VM can hard-lock the host.
- **systemd units**: `nvidia-persistenced` off (it holds `/dev/nvidia0` open
  and blocks every handover), `libvirt-guests` on (host shutdown then shuts
  the guest down cleanly), `switcheroo-control` on.
- **The Looking Glass client**, from the pinned `looking-glass-client` RPM
  (B7, the same version as the guest host program), plus a desktop entry and a
  `~/Desktop` shortcut per VM.
- **Your desktop user joins the `libvirt` group**, so that the one-click
  launcher starts the VM without a password prompt.

**The binding is dynamic by default.** While the VM is off, the NVIDIA card is
a normal host GPU, and CUDA, NVENC and PRIME render offload all work.
`--binding=static` parks the card on vfio-pci at boot instead. The host can
then never touch it, but no rebind cycle can fail.

### Which GPU runs your apps

The goal is minimal friction on the host: no wrapper scripts, no custom
launchers. orthogonals configures the stock desktop mechanisms, so that the
apps that want a fast GPU get the NVIDIA card on their own.

- **Steam and its games** use the dGPU automatically. The Steam desktop entry
  ships the freedesktop `PrefersNonDefaultGPU=true` key, and GNOME obeys it.
- **Vulkan games** need nothing, and that covers everything under Proton and
  DXVK. Both GPUs are visible and the game engines pick the discrete one.
- **Any other app**: right-click it in the GNOME app grid and pick "Launch
  using Discrete Graphics Card", or run `switcherooctl launch <app>`.
- **To pin an app to the dGPU**, copy its `.desktop` file to
  `~/.local/share/applications/` and add `PrefersNonDefaultGPU=true`. On KDE,
  add `X-KDE-RunOnDiscreteGpu=true`.

The desktop session itself stays off the NVIDIA card, so that a VM start always
finds it free. Chromium and GTK4 apps draw their interface with Vulkan and hold
`/dev/nvidia*` for their whole lifetime. orthogonals pins that known list
(browsers, Electron apps such as VS Code and Slack, GTK4 apps, Zed) to the iGPU
with environment variables and desktop-entry overrides.

While a process holds the dGPU, the VM cannot start. The gate refuses the
handover, names the process and sends a desktop notification. It never kills
anything. Close the app and start the VM again.

### More than one VM

The host setup is shared and VMs are additive. `up --vm-name <name>` creates
another VM with its own disk, launcher and desktop entry, with no reboot. Only
one VM can run at a time, because there is one dGPU. orthogonals refuses a
second start and names the VM that holds the card. It does not touch the VMs
that you made yourself with virt-manager or virsh, because the hooks act only
on the VMs that orthogonals registered.

To remove one VM, run `orthogonals vm undefine --vm-name <name>`. Add
`--purge` to remove its disk too. To reinstall a VM from scratch and keep the
host setup:

```sh
sudo orthogonals vm undefine --purge --yes
sudo orthogonals up --yes --win11-iso ~/Downloads/Win11.iso
```

## Commands

Three global flags work with every command, before or after the command name.
**A dry run is the default for every command that changes the host**: it
prints what happens and touches nothing. `--yes` applies the changes. `--json`
prints machine-readable output. `--root` prefixes all filesystem access (a
testing seam).

`up` is the only command that most users run. The rest are its building
blocks, useful on their own to inspect and to repair. Run
`orthogonals <command> --help` for the flags of one command.

| Command | What it does |
|---|---|
| `up` | Runs the whole pipeline as a persisted state machine. It resumes where it stopped, after the host-setup reboot or after any interruption. It takes the flags of every stage that it runs. |
| `detect` | Prints a read-only hardware inventory: GPUs, IOMMU groups, RAM, firmware. No root needed. |
| `preflight` | Answers go or no-go and changes nothing. It prints a fix for each failure, and the exit code carries the result. |
| `apply` | Runs the host-setup stage alone: kernel arguments, vfio initramfs, SELinux and tmpfiles rules, libvirt hooks, systemd units. |
| `vm define` | Creates one VM, or converges an existing one: the domain XML, its disk, its launcher and its desktop entry. |
| `vm undefine` | Removes one VM definition. It keeps the disk unless you pass `--purge`. |
| `vm launch` | Starts the VM and then runs `looking-glass-client`. This is what the desktop shortcut runs. |
| `media` | Builds the unattended install media from your ISO: the answer file, the provisioning scripts, the Virtual Display Driver, the NVIDIA guest driver and the Looking Glass host binary. |
| `verify` | Verifies one VM end to end: bindings, hooks, domain, guest display. On failure it points you at `bundle`. |
| `status` | A lightweight health report of bindings, kernel arguments, hooks and the SELinux rule. It exits 0 when the setup is intact. |
| `recover` | Repairs the GPU state after a botched handover. It journals nothing, because this is runtime repair. |
| `undo` | Walks the journal in reverse and restores the host byte-for-byte. |
| `bundle` | Writes a redacted diagnostics tar.gz for a bug report. |

`up` and `vm define` take the same VM flags: `--vm-name`, `--display-name`,
`--ram`, `--disk`, `--disk-size`, `--resolution`, `--guest-user`,
`--guest-password`, `--locale`. Four need a word more:

| Flag | Meaning |
|---|---|
| `--win11-iso` | Your Windows 11 installation ISO. Required until the media exists. |
| `--share` | A host directory to export to the guest over virtiofs, as a drive letter that counts down from `Z:`. Repeat it for more. Only the shares that exist at install time get mounted. |
| `--binding` | `dynamic` (default) or `static`, see [What it changes](#what-it-changes). |
| `--gpu-rom` | An extracted GPU vBIOS ROM, for a MUXless laptop dGPU that gives no guest output. |

## Security notes

- **No ACS override, ever.** orthogonals refuses unsafe IOMMU groups instead
  of a patch around them. The patch removes the isolation between the
  passthrough devices and the rest of the machine.
- **Fail-safe hooks.** A hook failure means that the VM does not start. A
  guard protects the reattach hook against the failed-start case, so it can
  never take the GPU away from running host apps.
- **orthogonals bundles nothing proprietary.** You supply the Windows ISO. Your machine
  downloads the NVIDIA guest driver when it builds the media, pinned by
  checksum to a known-good version. Pass `--nvidia-installer` for your own
  copy. Looking Glass (GPLv2) and the Virtual Display Driver (MIT) come from
  their official releases, pinned by SHA256.
- **orthogonals meets the Windows 11 requirements legitimately**, with OVMF
  Secure Boot, an emulated TPM 2.0 and the host CPU model. There are no registry bypass
  hacks for a Windows update to break.
- **Looking Glass uses the kvmfr kernel module** when it is available, and
  `/dev/shm` when it is not. With kvmfr, your iGPU pulls frames out of the
  buffer over DMA. On the reference host at 2560x1440 that removed one
  full-frame write per frame (~800 MiB/s of memory bandwidth) and cut the CPU
  of the client from 7.4% of a core to 3.0%. kvmfr is a DKMS module. It loads
  only while a VM runs, sized to that VM, and never at boot. DKMS signs it
  with the key that the host already uses for its other out-of-tree modules,
  so Secure Boot needs no new enrollment in the normal case.
- **The frame buffer** is readable by the desktop user and the `qemu` group
  with both backends (`0660 <user>:qemu`).

## Troubleshooting

Start with these three commands:

```sh
sudo orthogonals status    # health report: bindings, kernel args, hooks
orthogonals bundle         # redacted diagnostics bundle for a bug report
journalctl -b | grep gpu   # hook output from the current boot
```

Every answer below comes from a real incident on the tested machine.

### The VM refuses to start and says that the kvmfr module is unavailable

A kernel update landed and DKMS did not rebuild the module, so the domain
names a device that does not exist. The hook refuses the start. Otherwise QEMU
creates a plain file where the device belongs, and the guest writes frames
that nothing reads.

```sh
dkms status                # what failed to build
sudo orthogonals status    # which VM wants kvmfr, and for which kernel
sudo orthogonals up        # re-render the domain onto /dev/shm and continue
```

`up` puts you back on the slower path at once. Fix the build and run `up`
again to move back. The removal of `kvmfr-dkms` lands in the same place.

### The VM refuses to start and names a process that holds the GPU

This is the handover gate at work. Something on the host uses the NVIDIA card.
Close the named app and start the VM again. Steam must exit in full (Steam
menu, then Exit). To find the holder by hand, run
`sudo lsof /dev/nvidia0 /dev/nvidia-uvm /dev/nvidiactl /dev/nvidia-modeset`.
Do not bypass the gate. A forced start aborts anyway, with a worse error.

### QEMU crashes at VM boot with "vfio: DMA mapping failed"

The guest firmware put PCI device memory above the range that a 39-bit IOMMU
can address. This is common on consumer Alder Lake and Raptor Lake boards. The
domain XML that orthogonals generates already carries the fix, a `-fw_cfg`
firmware argument. If you edited the XML by hand and lost the
`<qemu:commandline>` block, run `orthogonals vm define` again.

### Looking Glass says "can't open backing store /dev/shm/looking-glass: Permission denied"

The shared-memory file lost its SELinux label, usually after a relabel or a
manual recreation. Run `sudo restorecon -v /dev/shm/looking-glass`. Then make
sure that `ls -Z` prints `svirt_tmpfs_t`, and that the file is owned
`<your user>:qemu` with mode 0660. `orthogonals status` reports whether the
rule exists.

### nvidia-smi says "No supported GPUs were found"

While the VM runs, this is normal, because the card belongs to vfio-pci. After
VM shutdown it means that the reattach did not complete. Run
`sudo orthogonals recover --yes`. It reloads the driver and re-enumerates the
card. If it reports that a reboot is necessary, reboot.

### When is a reboot the answer?

Do not fight these. Reboot when:

- `orthogonals recover` fails
- `dmesg` prints a vfio or NVIDIA oops, or `Xid` errors
- `modprobe -r nvidia` hangs in D state
- a failed VM start left virtqemud hung
- the host slept while the VM ran, after a forced power button for example

Also run one VM start-stop cycle after every NVIDIA driver update and every
kernel update, before you trust the setup. The dynamic rebind is the
least-exercised path in the NVIDIA driver.

### The host does not reach the desktop after apply and reboot

At the GRUB menu, edit the boot entry and remove the kernel arguments that
apply added. `apply` prints the exact list. With dynamic binding it is
`intel_iommu=on iommu=pt`. This disables passthrough for one boot and changes
no settings. Then run `orthogonals undo` from the working desktop.

### Clicks land in the wrong place in the guest, or the screen goes black

Before guest provisioning completes, two guest displays are active with
absolute mouse coordinates. Let provisioning finish. The final
settings keep only the Virtual Display Driver monitor. If you interrupted it,
`orthogonals up` resumes. If the SPICE setup display freezes during the
Windows installer, a known Windows 11 and SPICE error, wait. The unattended
install continues underneath and `up` reports progress from the guest side.

### Windows Setup never starts and the firmware asks to "Press any key to boot from CD"

Open the VM console with `virt-viewer <vm-name>` and press a key while the
prompt retries. This happens only on the first boot, while the disk is blank.
After Windows is installed, the disk boots first and the prompt never returns.

### The guest boots but the passed-through GPU shows no image (laptop)

A MUXless laptop dGPU, a 3D controller with no display outputs, can hang OVMF
on its option ROM, or stay dark without its own vBIOS. The `mux` check of
preflight warns when it finds this topology. Extract the dGPU vBIOS and pass
it: `orthogonals up --gpu-rom <rom.bin> ...`, or `vm define --gpu-rom`.
orthogonals installs it under `/var/lib/orthogonals/vbios`, renders
`<rom file=...>` in the domain, and keeps it across converges.

### apply or vm refuses: "journaled command differs"

You changed a setting (`--binding`, `--vm-name`, `--disk`, a GPU swap) that a
journaled step already applied with a different value. A re-run stacks the new
settings onto the old ones, so the engine refuses. Undo the affected scope,
then apply again. For VM steps, run `orthogonals vm undefine --yes`. For host
steps, run `orthogonals undo --yes`, plus a reboot when boot settings are
involved.

### apply refuses because of a line in /etc/default/grub

orthogonals edits `GRUB_CMDLINE_LINUX` there, so that the boot entries keep
the IOMMU arguments after a `grub2-mkconfig` run. It edits that variable only
where it can read the line back exactly as the shell reads it. Otherwise it
refuses and names the line number:

- an assignment form that it does not model (`export GRUB_CMDLINE_LINUX=…`, a
  lone `GRUB_CMDLINE_LINUX+=…`, a bare mention of the variable elsewhere)
- a value that carries `$`, a backtick or a backslash, which the shell expands
- a value that it cannot split into arguments: a trailing `# comment`, an
  unbalanced quote, or an escaped quote such as `acpi_osi=\"Windows 2009\"`

Rewrite that one line as a plain quoted assignment and run the command again.
`orthogonals preflight` prints the line and the reason. The alternative to a
named line is a fresh assignment at the end of the file. That form wins under
`sh` and silently drops whatever the host booted with, such as
`rd.luks.uuid=` or `resume=`.

### How do I report a bug?

Attach the output of `orthogonals bundle`: a redacted tar.gz with the detect
JSON, `lspci -nnk`, the vfio and NVIDIA journal lines, the installed
orthogonals settings files and the libvirt hook log. It redacts the hostname,
the serial numbers, the machine-id, the MAC addresses and the UUIDs, and it
strips the guest credentials.

## Author

<img src="docs/favicon.svg" width="128" align="left" hspace="24" vspace="6" alt="orthogonals">

Pavlo Hrytsenko &lt;pavlo.o.hrytsenko@gmail.com&gt; writes and maintains
orthogonals, 2026. The GNU General Public License v3.0 covers the project. See
[LICENSE](LICENSE).

Contributions are very welcome: bug reports with an `orthogonals bundle`
attached, reports from hardware other than the tested board (especially
different IOMMU layouts), and pull requests.
