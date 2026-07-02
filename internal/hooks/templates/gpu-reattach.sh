#!/usr/bin/env bash
# Return the passthrough GPU to the NVIDIA driver after VM shutdown.
# No set -e on purpose: run every stage, then let the nvidia-smi health check
# decide — a mid-script death would skip the recovery guidance.
set -uo pipefail
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
GPU="{{.GPU}}"
AUD="{{.Audio}}"   # empty when the dGPU has no audio function
NOTIFY_USER="{{.User}}"
LOG="{{.LogPath}}"

DEVS=("$GPU")
if [ -n "$AUD" ]; then DEVS+=("$AUD"); fi

mkdir -p "$(dirname "$LOG")"
log() {
    echo "$(date -Is) gpu-reattach: $*" >> "$LOG"
    echo "gpu-reattach: $*" >&2
}

notify_user() {
    uid="$(id -u "$NOTIFY_USER" 2>/dev/null || echo 1000)"
    sudo -u "$NOTIFY_USER" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/${uid}/bus" \
        notify-send -u critical -i video-display "Windows VM" "$1" 2>/dev/null || true
}

# GUARD: libvirt fires release/end even for a FAILED start. If the GPU never
# left the nvidia driver, unbinding it here would yank it from live apps
# (PoC incident 9). Only proceed when it is actually on vfio-pci.
if [ "$(basename "$(readlink -f "/sys/bus/pci/devices/$GPU/driver")" 2>/dev/null)" != "vfio-pci" ]; then
    log "GPU not on vfio-pci (failed/refused start) — nothing to undo"
    exit 0
fi
log "reattach start: ${DEVS[*]}"

# Stage 1 — release the devices from vfio-pci.
for dev in "${DEVS[@]}"; do
    echo "" > "/sys/bus/pci/devices/$dev/driver_override"
    if [ -e "/sys/bus/pci/devices/$dev/driver" ]; then
        echo "$dev" > "/sys/bus/pci/devices/$dev/driver/unbind"
    fi
done
log "released from vfio-pci"

# Stage 2 — reload NVIDIA modules and rebind.
modprobe nvidia
modprobe nvidia_uvm
modprobe nvidia_drm   # PRIME render offload needs the DRM node back too
for dev in "${DEVS[@]}"; do
    echo "$dev" > /sys/bus/pci/drivers_probe
done
log "nvidia modules loaded, devices probed"

# switcheroo-control enumerates GPUs only at startup; restart it so the
# desktop's dGPU menu reflects the rebind.
systemctl try-restart switcheroo-control.service 2>/dev/null || true

# Stage 3 — health check: healthy or surface the failure.
if timeout 15 nvidia-smi --query-gpu=name,memory.used --format=csv,noheader >> "$LOG" 2>&1; then
    log "GPU back on host, healthy"
else
    log "nvidia-smi failed after reattach — recovery: sudo orthogonals recover --yes; last resort: reboot"
    notify_user "GPU reattach failed — run: sudo orthogonals recover --yes (see $LOG)"
    exit 1
fi
