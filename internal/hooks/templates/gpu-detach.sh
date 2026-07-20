#!/usr/bin/env bash
# Evict the passthrough GPU from the host and bind it to vfio-pci.
# Any failure exits non-zero => libvirt refuses to start the VM (fail-safe).
# Refuse-and-list, never kill: a busy GPU aborts the start and names holders.
set -euo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin   # libvirt hooks run with a minimal env
GPU="{{.GPU}}"
AUD="{{.Audio}}"   # empty when the dGPU has no audio function
NOTIFY_USER="{{.User}}"
LOG="{{.LogPath}}"

DEVS=("$GPU")
if [ -n "$AUD" ]; then DEVS+=("$AUD"); fi

mkdir -p "$(dirname "$LOG")"
log() {
    echo "$(date -Is) gpu-detach: $*" >> "$LOG"
    echo "gpu-detach: $*" >&2
}

# Desktop notification to the logged-in user (the hook runs as root).
notify_user() {
    uid="$(id -u "$NOTIFY_USER" 2>/dev/null || echo 1000)"
    sudo -u "$NOTIFY_USER" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" \
        notify-send -u "${2:-critical}" -i video-display "Windows VM" "$1" 2>/dev/null || true
}

# Any failure past this point aborts the VM start; tell the desktop why.
on_exit() {
    rc=$?
    if [ $rc -ne 0 ]; then
        log "failed (exit $rc) — VM start aborted"
        if [ -z "${NOTIFIED:-}" ]; then
            notify_user "GPU handover failed — VM not started. See: $LOG"
        fi
    fi
}
trap on_exit EXIT

driver_of() {
    if [ -e "/sys/bus/pci/devices/$1/driver" ]; then
        basename "$(readlink -f "/sys/bus/pci/devices/$1/driver")"
    fi
}

# CPU governor: performance while the VM owns the GPU. All CPUs on purpose —
# the E-cores carry the emulator, iothread, and Looking Glass client. Every
# write is guarded: a cpufreq quirk must never block the VM. The reattach
# hook restores the saved governor; /run clears on reboot in step with the
# governors themselves. Called only where the start can no longer fail:
# libvirt skips release/end when prepare fails, and nothing would restore.
GOV_SAVE=/run/orthogonals-governor
boost_governor() {
    gov0=/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor
    [ -f "$gov0" ] || return 0    # no cpufreq on this host
    [ -f "$GOV_SAVE" ] || cat "$gov0" > "$GOV_SAVE" 2>/dev/null || return 0
    for g in /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor; do
        echo performance > "$g" 2>/dev/null || true
    done
    log "cpu governor performance (restore target: $(cat "$GOV_SAVE"))"
}

# Already on vfio-pci (static binding, or a previous failed start)? Done.
if [ "$(driver_of "$GPU")" = "vfio-pci" ]; then
    log "GPU already on vfio-pci — nothing to do"
    boost_governor
    exit 0
fi
log "handover start: ${DEVS[*]}"

# Stage 1 — stop the persistence daemon BEFORE the holder gate: it holds
# /dev/nvidia* open, and driver updates re-enable it behind our back. It is
# ours to stop, not a holder to refuse on — checking first would block every
# VM start with our own daemon listed as the culprit.
systemctl stop nvidia-persistenced.service 2>/dev/null || true
log "nvidia-persistenced stopped"

# Stage 2 — FAIL-SAFE GATE: refuse handover while anything holds the GPU.
# fuser catches GL clients that nvidia-smi misses. Advisory for a friendly
# message; the HARD gate is stage 3: modprobe -r fails on a busy module.
fuser -v /dev/nvidia* >> "$LOG" 2>&1 || true
holders="$(fuser /dev/nvidia* 2>/dev/null | tr -s ' \t' '\n' | sort -u || true)"
if [ -n "$holders" ]; then
    log "GPU is busy — refusing handover. Holders (close them, never killed):"
    ps -o pid=,user=,comm= -p $holders 2>/dev/null | tee -a "$LOG" >&2 || true
    apps="$(ps -o comm= -p $holders 2>/dev/null | sort -u | tr '\n' ' ' || true)"
    notify_user "GPU is busy — close these apps, then start the VM again:
${apps}"
    NOTIFIED=1
    exit 1
fi
log "holder gate passed"
notify_user "VM is starting — the GPU is being handed over, first screen in ~20 seconds." normal

# Stage 3 — unload NVIDIA modules. Order matters; fails here (=> VM refused)
# if anything still holds the driver.
for m in nvidia_drm nvidia_modeset nvidia_uvm nvidia; do
    if lsmod | grep -q "^$m "; then modprobe -r "$m"; fi
done
log "nvidia modules unloaded"

# Stage 4 — bind every passthrough function to vfio-pci via driver_override.
modprobe vfio-pci
for dev in "${DEVS[@]}"; do
    echo vfio-pci > "/sys/bus/pci/devices/$dev/driver_override"
    if [ -e "/sys/bus/pci/devices/$dev/driver" ]; then
        echo "$dev" > "/sys/bus/pci/devices/$dev/driver/unbind"
    fi
    echo "$dev" > /sys/bus/pci/drivers_probe
done
log "bound to vfio-pci"

# Stage 5 — verify or abort.
for dev in "${DEVS[@]}"; do
    drv="$(driver_of "$dev")"
    if [ "$drv" != "vfio-pci" ]; then
        log "$dev ended on '${drv:-none}', not vfio-pci — aborting VM start"
        exit 1
    fi
done

# switcheroo-control enumerates GPUs only at startup; restart it so the
# desktop's dGPU menu reflects the rebind.
systemctl try-restart switcheroo-control.service 2>/dev/null || true
boost_governor
log "GPU handed to vfio-pci"
