#!/bin/bash
# The only place the kernel, rather than a fixture, answers. The guest has an
# emulated IOMMU, so `intel_iommu=on` written by apply really populates
# /sys/kernel/iommu_groups and the qemu hook really evicts a device to
# vfio-pci. Every other test bind-mounts a directory over /sys/bus/pci/devices,
# where no bind, unbind, reset, or group operation can be real.
#
# Identity is the only synthetic thing left: per-file bind mounts over vendor,
# device and class make an ordinary virtio function look like an RTX 3080 to
# hw.ScanGPUs, while iommu_group, driver_override, unbind, drivers_probe,
# remove and rescan stay the kernel's own.
#
# Three arms, keyed off TMT_REBOOT_COUNT:
#   0  no IOMMU yet — apply, stop at the reboot boundary
#   1  real groups, real vfio-pci bind, real hook
#   2  confirm the host is pristine again
set -euo pipefail
cd "$(dirname "$0")"
# Real dracut, real semanage, real modprobe: a fake would pass while asserting
# nothing.
export ORTHOGONALS_REAL_TOOLS=1
# shellcheck source=lib.sh
source ./lib.sh

COUNT=${TMT_REBOOT_COUNT:-0}
OVERLAY=/run/orthogonals-overlay

require_root "the VFIO tier"

attr() { cat "/sys/bus/pci/devices/$1/$2" 2>/dev/null; }
driver_of() { basename "$(readlink -f "/sys/bus/pci/devices/$1/driver" 2>/dev/null)" 2>/dev/null; }
group_of() { basename "$(readlink -f "/sys/bus/pci/devices/$1/iommu_group" 2>/dev/null)" 2>/dev/null; }
nr_hugepages() { cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages; }

# iommu_group_count is how many groups the kernel currently publishes. find
# rather than ls: this script runs under pipefail, where `ls dir | wc -l` turns
# a missing directory into a failed pipeline instead of a count of nothing.
iommu_group_count() {
	find /sys/kernel/iommu_groups -mindepth 1 -maxdepth 1 -type d 2>/dev/null | wc -l
}

# find_devices locates the stand-in pair by its real identity — two functions of
# one virtio device — so a change to the provisioner's topology fails here
# rather than testing the wrong device. Must run before the overlays, which
# rewrite exactly the attributes it keys on.
find_devices() {
	GPU='' AUDIO='' IGPU=''
	local dev fn0 fn1
	for dev in /sys/bus/pci/devices/*.0; do
		fn0=$(basename "$dev")
		fn1=${fn0%.0}.1
		[ -e "/sys/bus/pci/devices/$fn1" ] || continue
		[ "$(attr "$fn0" vendor)" = 0x1af4 ] || continue
		GPU=$fn0 AUDIO=$fn1
		break
	done
	[ -n "$GPU" ] || fail "no multifunction virtio pair here — this is not the vfiohost guest"
	for dev in /sys/bus/pci/devices/*; do
		fn0=$(basename "$dev")
		case "$(attr "$fn0" class)" in 0x03*) [ "$fn0" = "$GPU" ] || IGPU=$fn0 ;; esac
	done
	[ -n "$IGPU" ] || fail "no display-class device to stand in for the iGPU"
	pass "stand-in dGPU $GPU + audio $AUDIO, iGPU $IGPU"
}

# overlay_identity makes the virtio pair answer as an NVIDIA dGPU and its HDMI
# audio function, and the video device as an Intel iGPU.
#
# Bind mounts do not survive a reboot, and a PCI remove + rescan unlinks the
# whole sysfs device directory — so this is called again after both.
overlay_one() { # device attribute value — separate arguments, because a PCI
	# address is full of colons and every packed-string separator is a trap.
	local target=/sys/bus/pci/devices/$1/$2
	printf '%s\n' "$3" >"$OVERLAY/$1.$2"
	if mountpoint -q "$target"; then
		umount "$target"
	fi
	mount --bind "$OVERLAY/$1.$2" "$target" ||
		fail "cannot overlay $1/$2 — the identity trick is the tier's foundation"
}

overlay_identity() {
	mkdir -p "$OVERLAY"
	overlay_one "$IGPU" vendor 0x8086
	overlay_one "$IGPU" device 0xa780
	overlay_one "$GPU" vendor 0x10de
	overlay_one "$GPU" device 0x2206
	overlay_one "$GPU" class 0x030000
	overlay_one "$AUDIO" vendor 0x10de
	overlay_one "$AUDIO" device 0x1aef
	overlay_one "$AUDIO" class 0x040300
	[ "$(attr "$GPU" vendor)" = 0x10de ] || fail "the vendor overlay on $GPU did not take"
	pass "identity overlaid on $GPU, $AUDIO, $IGPU"
}

case "$COUNT" in
0)
	step "arm 0 — no IOMMU yet"
	# Booted without intel_iommu=on, so the kernel publishes no groups at all.
	[ "$(iommu_group_count)" = 0 ] ||
		fail "the guest already has IOMMU groups — it was not booted clean"
	pass "no IOMMU groups before apply"

	find_devices
	overlay_identity
	id "$USER_NAME" >/dev/null 2>&1 || fail "the desktop user $USER_NAME does not exist"

	run 0 detect --json
	grep -q '"0x2206"' "$WORK/out" || fail "detect does not see the overlaid dGPU identity"
	# The firmware exposes VT-d (QEMU emits a real DMAR table for the emulated
	# IOMMU) but the kernel is not using it, so this is a warn, not a fail.
	run 2 preflight

	touch "$WORK/placeholder.iso"
	run 0 up --yes --user "$USER_NAME" --vm-name "$VM_NAME" \
		--win11-iso "$WORK/placeholder.iso" --disk-size 10
	grep -qi 'reboot now' "$WORK/out" || fail "up did not stop at the reboot boundary"
	[ "$(pipeline_state)" = host-applied ] ||
		fail "pipeline state is $(pipeline_state), want host-applied"

	pass "host applied, pipeline stopped at the reboot boundary"
	tmt-reboot
	;;

1)
	step "arm 1 — the kernel answers"
	grep -q 'intel_iommu=on' /proc/cmdline ||
		fail "kernel args are not live after the reboot: $(cat /proc/cmdline)"

	# The assertion this test exists for: everywhere else these groups are a
	# bind-mounted fixture, here the kernel published them because apply wrote
	# intel_iommu=on and the machine rebooted into it.
	groups=$(iommu_group_count)
	[ "$groups" -gt 0 ] ||
		fail "intel_iommu=on is live but the kernel published no IOMMU groups"
	pass "the kernel published $groups IOMMU groups because apply asked it to"

	find_devices
	# The whole-group rule against real kernel grouping: the pair must share a
	# group and be alone in it apart from the root port they sit behind.
	gpu_group=$(group_of "$GPU")
	[ -n "$gpu_group" ] || fail "$GPU has no IOMMU group despite an active IOMMU"
	[ "$(group_of "$AUDIO")" = "$gpu_group" ] ||
		fail "$GPU and $AUDIO landed in different IOMMU groups"
	strangers=""
	for member in /sys/kernel/iommu_groups/"$gpu_group"/devices/*; do
		addr=$(basename "$member")
		case "$addr" in "$GPU" | "$AUDIO") continue ;; esac
		case "$(attr "$addr" class)" in 0x0604*) continue ;; esac
		strangers="$strangers $addr"
	done
	[ -z "$strangers" ] || fail "IOMMU group $gpu_group also holds$strangers"
	pass "IOMMU group $gpu_group holds exactly $GPU and $AUDIO"

	overlay_identity

	run 2 preflight
	grep -q 'IOMMU active, host address width 39 bits' "$WORK/out" ||
		fail "preflight did not read 39 bits out of the emulated VT-d CAP register"
	grep -q 'address-width' "$WORK/out" || fail "the address-width check did not run"
	pass "the 39-bit warn path came from a real CAP register, not a fixture"

	# vm define needs /dev/kvm: without it libvirt offers no kvm domain type
	# and autoselecting the secure-boot EFI firmware fails. The guest CPU is
	# host-passthrough, so the node only misses when the host that booted this
	# guest offers no nested virtualization.
	[ -e /dev/kvm ] ||
		fail "no /dev/kvm in the guest — the host offers no nested virtualization"

	touch "$WORK/placeholder.iso"
	# No bind mount stands between VerifyBoot's iommuActive check and the kernel.
	rc=$(run_any up --yes --user "$USER_NAME" --vm-name "$VM_NAME" \
		--win11-iso "$WORK/placeholder.iso" --disk-size 10)
	sed 's/^/  | /' "$WORK/out"
	[ "$rc" != 0 ] || fail "up succeeded despite a placeholder Windows ISO"
	[ "$(pipeline_state)" = vm-defined ] ||
		fail "pipeline state is $(pipeline_state), want vm-defined"
	pass "boot verification passed against a real IOMMU; VM defined"

	step "the qemu hook evicts a real device to vfio-pci"
	pool_before=$(nr_hugepages)
	run 0 hook --user "$USER_NAME" qemu "$VM_NAME" prepare begin -
	for dev in "$GPU" "$AUDIO"; do
		[ "$(driver_of "$dev")" = vfio-pci ] ||
			fail "$dev is on '$(driver_of "$dev")', not vfio-pci"
	done
	[ -e "/dev/vfio/$gpu_group" ] || fail "/dev/vfio/$gpu_group did not appear"
	pool_held=$(nr_hugepages)
	[ "$pool_held" -gt "$pool_before" ] ||
		fail "the hook did not reserve hugepages (pool still $pool_held)"
	# CPU isolation must be skipped here. This guest has uniform cores, so the
	# domain profile pins every CPU it has — vCPUs plus the emulator and
	# iothread — leaving no housekeeping core to confine the host to. Only a
	# hybrid P-core/E-core machine takes the other branch, and no guest can look
	# hybrid: that needs /sys/devices/cpu_core, which no VM has.
	[ ! -e /run/orthogonals-cpuset ] ||
		fail "the hook isolated CPUs on a host whose pinning leaves none spare"
	grep -q 'no cores reserved for the host' /var/log/orthogonals/hooks.log ||
		fail "the hook skipped CPU isolation without saying why"
	pass "both functions on vfio-pci, /dev/vfio/$gpu_group open, pool $pool_before→$pool_held"

	step "release returns the device to the host"
	# Reattach cannot succeed here: there is no NVIDIA driver to reload, so it
	# falls through to its PCI remove + rescan recovery and reports the failure.
	rc=$(run_any hook --user "$USER_NAME" qemu "$VM_NAME" release end -)
	[ "$rc" != 0 ] || fail "release succeeded without an NVIDIA driver to reload"
	grep -qi 'rescan' /var/log/orthogonals/hooks.log ||
		fail "the reattach path never reached PCI remove + rescan"
	[ -e "/sys/bus/pci/devices/$GPU" ] ||
		fail "$GPU did not come back after the PCI remove + rescan"
	[ "$(driver_of "$GPU")" != vfio-pci ] || fail "$GPU is still bound to vfio-pci"
	[ ! -e "/dev/vfio/$gpu_group" ] || fail "/dev/vfio/$gpu_group outlived the release"
	[ "$(nr_hugepages)" = "$pool_before" ] ||
		fail "the hugepage pool was left at $(nr_hugepages), not the $pool_before it started at"
	pass "device re-enumerated, vfio node gone, hugepage pool restored"

	# remove + rescan unlinked the sysfs directories, and the bind mounts with
	# them. Nothing below sees the dGPU identity until they are back.
	overlay_identity

	step "the holder gate refuses a busy GPU"
	# A regular file, not a char device: mknod would succeed but opening it
	# fails with ENXIO, because no driver in this guest is registered for major
	# 195. The gate reads /proc/<pid>/fd symlink targets and matches on the
	# /dev/nvidia prefix, so a plain file exercises exactly the same path.
	: >/dev/nvidia0
	exec 9<>/dev/nvidia0
	rc=$(run_any hook --user "$USER_NAME" qemu "$VM_NAME" prepare begin -)
	exec 9>&-
	[ "$rc" != 0 ] || fail "the hook handed over the GPU while a process held /dev/nvidia0"
	grep -qi 'busy' "$WORK/out" || fail "the refusal did not tell the user the GPU was busy"
	[ "$(driver_of "$GPU")" != vfio-pci ] ||
		fail "the hook moved $GPU to vfio-pci despite refusing"
	rm -f /dev/nvidia0
	pass "handover refused with the device left on its host driver"

	step "libvirtd fires the installed hook shim"
	# A minimal TCG domain standing in for the Windows guest: it owns the same
	# hostdev group and carries the managed VM's name, so libvirtd runs
	# /etc/libvirt/hooks/qemu and the shim dispatches for real. This proves the
	# hook path and libvirt's claim on the vfio group, nothing about Windows.
	bdf=${GPU#*:}  # 0000:01:00.0 -> 01:00.0
	bus=0x${bdf%%:*}
	# The same domain, re-rendered: libvirt refuses to redefine a name under a
	# new UUID, and the hook only dispatches for the managed VM's name.
	uuid=$(virsh --connect qemu:///system domuuid "$VM_NAME" | tr -d '[:space:]')
	[ -n "$uuid" ] || fail "cannot read the defined domain's UUID"
	cat >"$WORK/inner.xml" <<XML
<domain type='qemu'>
  <name>$VM_NAME</name>
  <uuid>$uuid</uuid>
  <memory unit='MiB'>512</memory>
  <vcpu>1</vcpu>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <devices>
    <controller type='pci' index='1' model='pcie-root-port'/>
    <controller type='pci' index='2' model='pcie-root-port'/>
    <hostdev mode='subsystem' type='pci' managed='no'>
      <source><address domain='0x0000' bus='$bus' slot='0x00' function='0x0'/></source>
    </hostdev>
    <hostdev mode='subsystem' type='pci' managed='no'>
      <source><address domain='0x0000' bus='$bus' slot='0x00' function='0x1'/></source>
    </hostdev>
  </devices>
</domain>
XML
	: >/var/log/orthogonals/hooks.log
	virsh --connect qemu:///system define "$WORK/inner.xml" >/dev/null ||
		fail "libvirt refused the stand-in hostdev domain"
	virsh --connect qemu:///system start "$VM_NAME" >"$WORK/start.log" 2>&1 || {
		sed 's/^/  | /' "$WORK/start.log" >&2
		sed 's/^/  hook| /' /var/log/orthogonals/hooks.log >&2
		fail "the stand-in domain would not start"
	}
	grep -qi 'handover\|already on vfio-pci' /var/log/orthogonals/hooks.log ||
		fail "libvirtd started the domain without running the orthogonals hook shim"
	for dev in "$GPU" "$AUDIO"; do
		[ "$(driver_of "$dev")" = vfio-pci ] ||
			fail "$dev is on '$(driver_of "$dev")' while the domain holds it"
	done
	# Read the same file the product reads, rather than depending on getenforce
	# being installed. Permissive here would make the claim above vacuous.
	[ "$(cat /sys/fs/selinux/enforce)" = 1 ] ||
		fail "SELinux is not enforcing — this step proves less than it claims"
	pass "libvirtd ran the shim and claimed /dev/vfio/$gpu_group under enforcing SELinux"

	virsh --connect qemu:///system destroy "$VM_NAME" >/dev/null || true
	overlay_identity

	step "undo"
	# undo refuses while a VM is defined, and keeps the disk as a data record
	# unless purged — so the VM goes first, with --purge.
	run 0 vm undefine --vm-name "$VM_NAME" --purge --yes
	run 0 undo --yes
	grep -q 'undo complete' "$WORK/out" ||
		fail "undo kept records: $(sed -n 's/.*\(undo finished.*\)/\1/p' "$WORK/out")"
	pass "undo completed with no kept records"

	tmt-reboot
	;;

2)
	step "arm 2 — confirm the host is pristine"
	[ "$(iommu_group_count)" = 0 ] ||
		fail "undo left the IOMMU on: the kernel still publishes groups"
	goss_check pristine.yaml "after undo and reboot"
	pass "the IOMMU is off again and every applied artifact is gone"
	echo
	echo "vfio: apply turned a real IOMMU on, the hook moved a real device to vfio-pci, and undo put it all back"
	;;

*)
	fail "unexpected TMT_REBOOT_COUNT=$COUNT"
	;;
esac
