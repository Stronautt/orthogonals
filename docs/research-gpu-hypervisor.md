# Research: paths to a "GPU hypervisor" — reducing onboarding friction

Date: 2026-07-20. Question: can the plug-monitor-into-motherboard / reconfigure-BIOS
friction be removed, up to the ideal of Linux acting as a hypervisor for the GPU
(host and Windows guest both using the dGPU, no physical reconfiguration)?

Four paths were researched in parallel. Verdicts first:

| Path | Verdict |
|---|---|
| 1. Concurrent sharing (SR-IOV / vGPU slicing of the dGPU) | **Blocked at vendor level** on consumer NVIDIA; not solvable by this project |
| 2. Paravirtualized GPU (no passthrough at all) | **Nonexistent** for gaming-class Windows guests in 2026; ceiling is ~DX11 at 30–60% native |
| 3. Live handover (host desktop *on* the dGPU, VM steals it) | **Blocked** at the NVIDIA driver level (no DRM hot-unplug) and at the connector level |
| 4. Dynamic handover with iGPU-primary session + software removal of the BIOS step | **Feasible today** — this is the real opportunity; nobody has packaged it well |

The irreducible constraint across all paths: **a monitor whose only cable goes to a
vfio-bound dGPU is dark**. No software can scan out through a connector owned by
vfio-pci. The monitor cable to the motherboard (or a dual-input monitor) is physics,
not friction.

---

## Path 1 — True GPU hypervisor: slicing the dGPU between host and guest

- **NVIDIA official vGPU**: datacenter/pro SKUs only + paid license; GeForce excluded
  by device-ID whitelisting. On Ada/Hopper the mechanism moved from mdev to SR-IOV +
  a vendor-specific VFIO framework (kernel 6.8+, `iommufd`); consumer RTX 40/50 cards
  do not expose SR-IOV capability at all.
- **vgpu_unlock / vgpu_unlock-rs**: works only where GeForce silicon has a
  vGPU-qualified sibling — Maxwell/Pascal/Turing. Consumer Ampere/Ada are not
  unlockable; no patches exist for vGPU 18.1+/19.x drivers. GSP firmware signing
  closed the hooking surface. Legacy, dying. Even when it works, the host loses the
  GPU (the vGPU manager driver has no display/CUDA path).
- **Intel**: the one vendor with real consumer partitioning. `i915-sriov-dkms` gives
  SR-IOV on Alder/Raptor/Meteor Lake **iGPUs** (up to 7 VFs, Windows guest uses the
  stock Intel driver, host keeps the PF). Upstream xe supports Lunar/Panther Lake
  iGPUs and Arc **Pro** dGPUs only (consumer B580/B570 fused off). Useful for giving
  a guest QuickSync/desktop accel — not gaming.
- **AMD**: GIM/MxGPU supports only ancient Tonga; modern SR-IOV is Instinct/cloud
  only. Dead for consumers.
- **Hyper-V GPU-P** is the proof this is possible *in principle* with consumer
  GeForce — but it lives inside the closed WDDM/dxgkrnl stack on both sides, which
  is exactly why it's Windows-host-only. LibVF.IO / Arc Compute GVM (the last
  attempt at a Linux equivalent) has been inactive since 2023.

**Conclusion**: the dGPU cannot be space-multiplexed on this hardware class, and
nothing on the 2026 horizon changes that. The only sliceable device in the target
topology is the Intel iGPU (possible future feature: an SR-IOV VF for a second/light
VM — not a replacement for passthrough).

Sources: [PolloLoco vGPU guide](https://gitlab.com/polloloco/vgpu-proxmox),
[vgpu_unlock-rs](https://github.com/mbilker/vgpu_unlock-rs),
[kubevirt#17642 (new VFIO framework)](https://github.com/kubevirt/kubevirt/issues/17642),
[NVIDIA vGPU 19 release notes](https://docs.nvidia.com/vgpu/19.0/grid-vgpu-release-notes-generic-linux-kvm/index.html),
[i915-sriov-dkms](https://github.com/strongtz/i915-sriov-dkms),
[Phoronix: SR-IOV only Arc Pro](https://www.phoronix.com/news/Intel-SR-IOV-Only-For-Arc-Pro),
[MS GPU-P docs](https://learn.microsoft.com/en-us/windows-hardware/drivers/display/gpu-paravirtualization),
[LibVF.IO](https://github.com/Arc-Compute/LibVF.IO).

## Path 2 — Paravirtualized GPU (no device handover at all)

- **viogpu3d** ([virtio-win PR #943](https://github.com/virtio-win/kvm-guest-drivers-windows/pull/943))
  is the only active effort to give a Windows KVM guest an accelerated paravirt GPU:
  a WDDM 1.3 driver with OpenGL + **D3D10 only**, unmerged, one developer, known
  crashes. WDDM 1.3 caps it; no open-source D3D12 UMD exists anywhere.
- **Venus** (Vulkan over virtio) now supports NVIDIA proprietary ≥570.86 as a *host*
  driver, but the guest driver is Mesa/Linux-only. ~60% of native.
- **DRM native context** reaches near-native in benchmarks but requires a Mesa host
  driver (NVIDIA proprietary excluded) and a Linux guest. Both are categorically
  unavailable to a Windows guest.
- Commercial ceiling: VMware Workstation (DX11, no DX12, roughly iGPU-class perf) and
  Parallels on Mac (DX11-to-Metal, mid-tier games). Win11 24H2-era titles increasingly
  require DX12 + anticheat hostile to virtual GPUs.
- Interesting inversion that *does* work today: a **Linux** guest with Venus or native
  context running Windows games under Proton — but that abandons the Windows-guest
  requirement.

**Conclusion**: no viable paravirt path for the project's use case; ceiling ~30–60%
of native at DX11. Passthrough + Looking Glass remains the only near-native
architecture. Worth re-checking viogpu3d/native-context yearly.

Sources: [Collabora: state of gfx virtualization, Jan 2025](https://www.collabora.com/news-and-blog/blog/2025/01/15/the-state-of-gfx-virtualization-using-virglrenderer/),
[Mesa Venus docs](https://docs.mesa3d.org/drivers/venus.html),
[QEMU virtio-gpu docs](https://www.qemu.org/docs/master/system/devices/virtio/virtio-gpu.html).

## Path 3 — Live handover: host desktop composited on the dGPU, VM steals it

- nvidia/nvidia-open share a DRM shim with **no hot-unplug support**: sysfs `unbind`
  blocks until every fd on `/dev/nvidia*` and `/dev/dri/*` closes. mutter releases a
  DRM device only on udev *remove* — which never fires while mutter holds the fd.
  Deadlock by design; no compositor has a "release this GPU" API.
- Only amdgpu has invested in DRM hot-unplug (`drm_dev_unplug()`), and it is still
  best-effort.
- The single-GPU-passthrough community recipe (kill display manager, unbind, start
  VM, reverse on stop) is unchanged since 2019 and remains the least reliable
  variant: session teardown per VM start, no console when it hangs.
- Even if unbind worked, monitors cabled to the dGPU go dark under vfio — see the
  irreducible constraint above.

**Conclusion**: the strong form ("desktop runs on the dGPU, VM takes it live") is
blocked at the driver level. The survivable configuration is: **session on the iGPU,
dGPU held only by revocable clients** (PRIME offload apps, CUDA, NVENC) — which is
Path 4.

Sources: [ArchWiki PRIME](https://wiki.archlinux.org/title/PRIME),
[nvidia unload thread](https://forums.developer.nvidia.com/t/nvidia-kernel-module-refuses-to-unload-no-matter-what/256803),
[joeknock90 single-GPU issues](https://github.com/joeknock90/Single-GPU-Passthrough/issues/25),
[dynamic bind origin (2016)](https://arseniyshestakov.com/2016/03/31/how-to-pass-gpu-to-vm-and-back-without-x-restart/).

## Path 4 — What is actually achievable: kill the BIOS step, kill the dummy plug, add dynamic mode

### 4a. The BIOS primary-display change is software-removable

- Kernel ≥6.0 fixed the historic blocker: vfio-pci now evicts the firmware
  framebuffer on bind (`aperture_remove_conflicting_pci_devices()`, commit
  `d173780620792`). Booting with the dGPU as BIOS-primary and binding it to vfio no
  longer fails with "BAR 0: can't reserve". Proxmox never asks users to change BIOS
  primary display — strongest evidence the step is removable.
- What replaces the BIOS step in software:
  - Preflight reads `/sys/bus/pci/devices/*/boot_vga` to detect which GPU is
    firmware-primary.
  - Pin the session to the iGPU regardless of `boot_vga`: udev
    `TAG+="mutter-device-ignore"` on the dGPU DRM device (KWin:
    `KWIN_DRM_DEVICES=`). mutter otherwise prefers the boot_vga device — this
    one-line tag is why the BIOS change was ever needed.
  - Fallback for stubborn kernels/boards: `initcall_blacklist=sysfb_init` kernel arg
    (modern replacement for `video=efifb:off`); costs the pre-KMS console, so
    preflight must assert i915 is in the initramfs (LUKS prompt).
  - Guest-side VBIOS-shadow hangs (firmware POSTed the dGPU, OVMF sees a dirty ROM):
    rare on Turing+; fallback is a dumped `romfile` in the domain XML.
- Bonus: the BIOS path is itself unreliable across vendors (ASUS disables the iGPU
  by default with a dGPU installed; some boards ignore Primary Display with CSM
  off) — the software path is not just lower-friction, it is *more* portable.

### 4b. The dummy-plug / guest-display friction — already solved here

Looking Glass needs an active display target in the guest, not a physical monitor.
orthogonals already installs the Virtual Display Driver during guest provisioning,
so nothing needs to be cabled to the dGPU and no dummy plug exists in this design.
The LG IDD driver (LGIdd) was deferred out of B7 stable; swap to it when it ships
if it proves better than the third-party VDD.

### 4c. Dynamic handover mode ("GPU hypervisor, time-multiplexed")

The configuration that survives handover, end to end:

1. Session composited on the iGPU (udev tag pins it, regardless of BIOS/cabling of
   boot_vga). dGPU bound to nvidia at boot, fully usable by the host: PRIME render
   offload for games, CUDA, NVENC.
2. VM start hook: enumerate open fds on the dGPU (`/dev/nvidia*`, its
   `/dev/dri/*` nodes); refuse handover with a precise "these processes hold the
   GPU" error (loud failure, per project policy) or optionally stop
   nvidia-persistenced/powerd; `virsh nodedev-detach` / driverctl to vfio-pci;
   sysfs `remove`+`rescan` as the re-attach fallback.
3. VM stop hook: rebind to nvidia (`driverctl --nosave` semantics — note the known
   wedge when rebinding after boot-bound-to-nvidia; needs testing per driver
   branch), dGPU returns to host PRIME/CUDA duty. Turing+ resets reliably; the HDA
   audio function is the residual reset risk.
4. Guest display via Looking Glass on the iGPU-driven monitor, virtual display
   driver in the guest.

This removes: the BIOS trip, the reboot-per-mode, the dummy plug, and the "dGPU is
dead weight when the VM is off" cost. It does NOT remove the monitor-on-motherboard
cable, and PRIME offload means the *compositor* runs on the iGPU (offloaded apps get
full dGPU perf; the desktop itself is iGPU-class).

### 4d. What no one has done (the ambition, honestly scoped)

The missing primitive for the strong form is **revocation**: a way for the kernel or
compositor to revoke GPU fds so a device can be reclaimed from unwilling clients
(the way a hypervisor reclaims a CPU). That requires upstream work, not
project-level engineering:

- DRM-level fd revocation / hot-unplug in nvidia-open (or NVK maturing to the point
  the desktop stack is Mesa-native, where native-context and amdgpu-style unplug
  work become applicable).
- A compositor "release/reacquire GPU" protocol (mutter/kwin have none today).

A realistic novel contribution at the project level: a **GPU lease manager** daemon
that owns the dGPU's lifecycle — tracks holders, brokers handover requests,
gracefully asks registered clients to drop (SIGTERM contract), and exposes
attach/detach as a D-Bus API. That is genuinely new packaging of Path 4c, and it is
the credible stepping stone toward the upstream revocation story.

---

## Roadmap outcome (updated after codebase review)

Codebase exploration corrected the initial framing: most of the "roadmap" already
existed before this research.

1. **BIOS step — removed.** The `udev-mutter-ignore` rule and environment.d iGPU
   pins were already shipped; this work added `boot_vga` + DRM-connector detection,
   the `display-topology` and `boot-vga` preflight checks, and the README rewrite.
   The `initcall_blacklist=sysfb_init` and romfile fallbacks turned out to be
   unnecessary (kernel ≥6.0 evicts the firmware framebuffer on vfio bind; the
   domain XML already runs with `<rom bar='off'/>`).
2. **Guest display — already shipped.** The VDD is installed during provisioning;
   no dummy plug or dGPU cabling needed. Swap to LG IDD when it ships stable.
3. **Dynamic mode — already shipped and the default binding.** Hook-driven
   detach/attach with fd-holder reporting (`fuser`, refuses and names holders)
   existed; this work added the PCI remove+rescan fallback to the reattach hook.
4. **Hard-class detection — shipped** via the `gpu-topology` (no iGPU / AMD /
   multi-GPU fails), `display-topology` (per-connector cable instruction), and
   `boot-vga` (optional-BIOS warn) checks.
5. **Watch list (yearly)**: viogpu3d / Windows native-context, LG IDD, NVK/nouveau
   hot-unplug, Intel xe SR-IOV breadth, any GeForce SR-IOV movement (none expected).
