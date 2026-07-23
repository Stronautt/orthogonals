#!/bin/bash
# The central promise, asserted across the fixture matrix: whatever apply does
# to a host, undo puts back byte for byte. Every fixture preflight lets through
# runs the full cycle — dry-run, apply, re-apply, simulated reboot, undo.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

# fixture : expected kernel args : laptop (RTD3 artifacts expected)
SCENARIOS=(
	"reference:intel_iommu=on iommu=pt:no"
	"laptop:intel_iommu=on iommu=pt:yes"
	"laptop-amd:iommu=pt:yes"
	"no-audio:intel_iommu=on iommu=pt:no"
	"bridge:intel_iommu=on iommu=pt:no"
	"wide-iommu:intel_iommu=on iommu=pt:no"
)

RTD3_PATHS=(/etc/modprobe.d/nvidia-rtd3.conf /etc/udev/rules.d/80-orthogonals-nvidia-pm.rules)

for scenario in "${SCENARIOS[@]}"; do
	IFS=: read -r kind kargs laptop <<<"$scenario"
	step "$kind"
	root=$WORK/$kind
	prep_root "$kind" "$root"
	snapshot_into "$root" "$kind"

	# --- detect + preflight ---------------------------------------------------
	run 0 detect --json --root "$root"
	grep -q '"0x2206"' "$WORK/out" || fail "$kind: detect JSON missing the NVIDIA device id"
	# preflight warns (2) on every fixture; a fail (1) belongs in refusal.sh
	run 2 preflight --root "$root"

	# --- dry run touches nothing ----------------------------------------------
	run 0 apply --root "$root" --user testuser
	grep -q 'dry run' "$WORK/out" || fail "$kind: apply without --yes must announce the dry run"
	assert_restored "$root" "$kind" "$kind dry-run"

	# --- apply --yes -----------------------------------------------------------
	run 0 apply --root "$root" --user testuser --yes
	for path in \
		/etc/dracut.conf.d/vfio.conf \
		/etc/udev/rules.d/61-mutter-ignore-nvidia.rules \
		/etc/environment.d/50-orthogonals-igpu.conf \
		/etc/tmpfiles.d/looking-glass.conf \
		/etc/libvirt/hooks/qemu \
		/var/lib/orthogonals/manifest.json; do
		[ -e "$root$path" ] || fail "$kind: missing $path after apply --yes"
	done
	[ "$(stat -c '%a' "$root/etc/libvirt/hooks/qemu")" = 755 ] ||
		fail "$kind: qemu hook is not executable"
	grep -q 'hook --user testuser qemu' "$root/etc/libvirt/hooks/qemu" ||
		fail "$kind: qemu hook is not the orthogonals shim"

	for path in "${RTD3_PATHS[@]}"; do
		if [ "$laptop" = yes ]; then
			[ -e "$root$path" ] || fail "$kind: laptop apply did not install $path"
		else
			[ -e "$root$path" ] && fail "$kind: desktop apply installed the laptop-only $path"
		fi
	done
	if [ "$laptop" = yes ]; then
		grep -q 'NVreg_DynamicPowerManagement=0x02' "$root/etc/modprobe.d/nvidia-rtd3.conf" ||
			fail "$kind: RTD3 modprobe option missing"
	fi

	for entry in "$root"/boot/loader/entries/*.conf; do
		grep -q "$kargs" "$entry" || fail "$kind: $entry missing kernel args '$kargs'"
	done
	if [ "$kargs" = "iommu=pt" ]; then
		grep -rq intel_iommu "$root/boot/loader/entries/" &&
			fail "$kind: AMD host got intel_iommu"
	fi
	grep -qi 'reboot required' "$WORK/out" || fail "$kind: apply --yes missing the reboot notice"
	grep -q 'skipped under --root' "$WORK/out" ||
		fail "$kind: daemon-touching steps must report the --root skip"
	records=$(manifest_records "$root")

	# --- re-apply is idempotent, and keeps demanding the reboot until it lands -
	run 0 apply --root "$root" --user testuser --yes
	grep -qi 'reboot required' "$WORK/out" ||
		fail "$kind: re-apply before the reboot must still demand it"
	[ "$(manifest_records "$root")" = "$records" ] ||
		fail "$kind: re-apply grew the manifest beyond $records records"

	# --- simulate the reboot: kargs live on the kernel command line ------------
	printf 'BOOT_IMAGE=vmlinuz root=/dev/sda1 ro %s\n' "$kargs" >"$root/proc/cmdline"
	run 0 apply --root "$root" --user testuser --yes
	grep -qi 'reboot required' "$WORK/out" &&
		fail "$kind: no-op re-apply after the reboot must not demand one"
	rm "$root/proc/cmdline"

	# --- undo ------------------------------------------------------------------
	run 0 undo --root "$root" --yes
	grep -q 'undo complete' "$WORK/out" || fail "$kind: undo did not report completion"
	[ -e "$root/var/lib/orthogonals/manifest.json" ] && fail "$kind: manifest survived undo"
	grep -rq intel_iommu=on "$root/boot/loader/entries" &&
		fail "$kind: undo left IOMMU kernel args in the BLS entries"
	assert_restored "$root" "$kind" "$kind undo"

	pass "$kind ($records records, kargs='$kargs', byte-identical restore)"
done

echo
echo "lifecycle: all ${#SCENARIOS[@]} fixtures completed the full cycle"
