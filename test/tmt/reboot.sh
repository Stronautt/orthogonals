#!/bin/bash
# The `up` state machine across a real reboot. Every other test runs apply
# under a --root prefix against fake vendor tools; here dracut regenerates a
# real initramfs, the BLS edit reaches the running kernel, and the domain lands
# in a live libvirtd.
#
# Three arms, keyed off TMT_REBOOT_COUNT:
#   0  apply, then stop at the reboot boundary the pipeline announces
#   1  resume: boot verification passes, VM is defined, then undo
#   2  confirm the kernel arguments are gone
set -euo pipefail
cd "$(dirname "$0")"
# Real dracut, real semanage, real usermod: a fake would pass while asserting
# nothing.
export ORTHOGONALS_REAL_TOOLS=1
# shellcheck source=lib.sh
source ./lib.sh

COUNT=${TMT_REBOOT_COUNT:-0}

require_root "the reboot tier"

# The guest has no GPU, so the reference topology is bind-mounted over the live
# sysfs paths detect reads. Every mutation below is real; only the hardware
# detection is synthetic. Bind mounts do not survive a reboot, hence the re-run.
# proc/cpuinfo pins the vendor fallback: no ACPI IOMMU table in a plain guest,
# and AMD runners would otherwise drop intel_iommu=on from the applied args.
fake_sysfs() {
	local fake=/opt/orthogonals-fakesys
	rm -rf "$fake"
	mkdir -p "$fake"
	"$FIXTURE_BIN" "$fake" reference
	[ -e "$fake/sys/bus/pci/devices/0000:01:00.0" ] || fail "fixture tree incomplete at $fake"

	# The one fact this test cannot make real: a plain QEMU guest has no IOMMU,
	# so the kernel publishes no groups no matter what intel_iommu=on says.
	# vfio.sh is where that one becomes real too.
	mkdir -p "$fake/sys/kernel/iommu_groups/0" "$fake/sys/kernel/iommu_groups/1"
	[ -d /sys/kernel/iommu_groups ] ||
		fail "/sys/kernel/iommu_groups does not exist, so it cannot be overlaid"

	local pair
	for pair in \
		"sys/bus/pci/devices:/sys/bus/pci/devices" \
		"sys/class/iommu:/sys/class/iommu" \
		"sys/kernel/iommu_groups:/sys/kernel/iommu_groups" \
		"sys/devices/system/cpu:/sys/devices/system/cpu" \
		"proc/driver:/proc/driver" \
		"proc/cpuinfo:/proc/cpuinfo" \
		"proc/meminfo:/proc/meminfo"; do
		local src=${pair%%:*} dst=${pair#*:}
		mountpoint -q "$dst" && umount "$dst"
		mount --bind "$fake/$src" "$dst"
	done
}

case "$COUNT" in
0)
	step "arm 0 — apply the host configuration"
	fake_sysfs
	id "$USER_NAME" >/dev/null 2>&1 || fail "the desktop user $USER_NAME does not exist"

	run 0 detect --json
	grep -q '"0x2206"' "$WORK/out" || fail "detect does not see the bind-mounted RTX 3080"
	run 2 preflight

	# up refuses without --win11-iso before the media stage (cli/up.go), and CI
	# has no Windows ISO: a placeholder carries the pipeline as far as it can go.
	touch "$WORK/placeholder.iso"
	run 0 up --yes --user "$USER_NAME" --vm-name "$VM_NAME" \
		--win11-iso "$WORK/placeholder.iso" --disk-size 10
	grep -qi 'reboot now' "$WORK/out" || fail "up did not stop at the reboot boundary"

	[ "$(pipeline_state)" = host-applied ] ||
		fail "pipeline state is $(pipeline_state), want host-applied"
	# grubby is an independent reader of the entries the native editor wrote.
	# Captured to a file first: piping into `grep -q` under `set -o pipefail`
	# makes the producer die of SIGPIPE the moment grep matches, so a *match*
	# fails the pipeline.
	grubby --info=ALL >"$WORK/grubby.txt"
	grep -q intel_iommu=on "$WORK/grubby.txt" ||
		fail "grubby does not see the native BLS kernel-arg edit"
	grep -q intel_iommu=on /proc/cmdline &&
		fail "kernel args are live before the reboot — the fixture host was already configured"

	pass "host applied, pipeline stopped at the reboot boundary"
	tmt-reboot
	;;

1)
	step "arm 1 — resume after the reboot"
	grep -q 'intel_iommu=on' /proc/cmdline ||
		fail "kernel args are not live after the reboot: $(cat /proc/cmdline)"
	grep -q 'iommu=pt' /proc/cmdline || fail "iommu=pt missing from the live cmdline"
	pass "kernel arguments took effect"

	# Every driver apply asked dracut to force in must actually be there.
	# dracut exits 0 whether or not it found them, so without this a user only
	# discovers the gap after rebooting into a host that cannot bind the GPU.
	lsinitrd >"$WORK/initrd.txt" 2>/dev/null || fail "lsinitrd failed on the running kernel's initramfs"
	missing=""
	for module in vfio_pci vfio vfio_iommu_type1; do
		grep -qE "/${module//_/[_-]}\.ko" "$WORK/initrd.txt" || missing="$missing $module"
	done
	if [ -n "$missing" ]; then
		echo "--- vfio entries dracut did include ---" >&2
		grep -i vfio "$WORK/initrd.txt" >&2 || echo "(none)" >&2
		echo "--- force_drivers apply wrote ---" >&2
		cat /etc/dracut.conf.d/vfio.conf >&2
		fail "dracut left$missing out of the regenerated initramfs"
	fi
	pass "regenerated initramfs carries vfio, vfio_pci, vfio_iommu_type1"

	# force_drivers also writes rd.driver.pre=, so a correct initramfs loads
	# vfio_pci before userspace. Report rather than assume: if it is not loaded
	# the initramfs did not do its job, and the rest of the test still needs it.
	if [ -d /sys/module/vfio_pci ]; then
		pass "the regenerated initramfs loaded vfio_pci at boot"
	else
		echo "note: vfio_pci was not loaded at boot — loading it explicitly" >&2
		modprobe vfio_pci || fail "vfio_pci will not load in this guest"
	fi

	fake_sysfs
	touch "$WORK/placeholder.iso"

	# VerifyBoot now reads a real /proc/cmdline; DefineVM
	# reaches a live libvirtd. BuildMedia then stops on the placeholder ISO,
	# which is the expected terminus, so the exit status is not 0.
	rc=$(run_any up --yes --user "$USER_NAME" --vm-name "$VM_NAME" \
		--win11-iso "$WORK/placeholder.iso" --disk-size 10)
	sed 's/^/  | /' "$WORK/out"
	[ "$rc" != 0 ] || fail "up succeeded despite a placeholder Windows ISO"
	grep -qi 'guest media' "$WORK/out" || fail "up did not fail in the media stage; it failed earlier"

	[ "$(pipeline_state)" = vm-defined ] ||
		fail "pipeline state is $(pipeline_state), want vm-defined — the reboot did not advance it"
	pass "state machine crossed the reboot: host-applied → rebooted → vm-defined"

	# orthtest has never logged in, so there is no session bus and the GNOME
	# trust flag cannot be set. That must not stop the shortcut being created,
	# nor abort VM definition.
	grep -q 'not marked trusted' "$WORK/out" ||
		fail "the desktop shortcut reported no trust-flag note, so this guest did not exercise the sessionless path"
	link=/home/$USER_NAME/Desktop/$VM_NAME.orthogonals.desktop
	[ -L "$link" ] || fail "the desktop shortcut was not created at $link"
	[ -e "$link" ] || fail "the desktop shortcut at $link dangles"
	pass "shortcut created for a user with no session; the trust flag was reported, not fatal"

	goss_check applied.yaml "after up"

	# The hook dispatches against the real daemon under enforcing SELinux. An
	# unmanaged domain must pass straight through.
	run 0 hook --user "$USER_NAME" qemu ghost prepare begin -
	pass "qemu hook dispatches for an unmanaged domain"

	step "undo"
	# undo refuses while a VM is defined, and keeps the disk as a data record
	# unless purged — so the VM goes first, with --purge, or the manifest
	# survives and the pristine assertions cannot hold.
	run 0 vm undefine --vm-name "$VM_NAME" --purge --yes
	run 0 undo --yes
	grep -q 'undo complete' "$WORK/out" ||
		fail "undo kept records: $(sed -n 's/.*\(undo finished.*\)/\1/p' "$WORK/out")"
	pass "undo completed with no kept records"

	tmt-reboot
	;;

2)
	step "arm 2 — confirm the host is pristine"
	goss_check pristine.yaml "after undo and reboot"
	pass "kernel arguments and every applied artifact are gone"
	echo
	echo "reboot: up crossed two real reboots and left the host as it found it"
	;;

*)
	fail "unexpected TMT_REBOOT_COUNT=$COUNT"
	;;
esac
