#!/bin/bash
# System tier (local only, not CI): boots a throwaway Fedora Cloud VM,
# installs the built binary, runs a REAL apply (real grubby/dracut/SELinux)
# → reboot → asserts live configs → undo → asserts pristine → destroys the VM.
#
# The VM has no GPU, so the reference GPU topology (i5-13600K + RTX 3080) is
# bind-mounted over /sys/bus/pci/devices, /sys/class/iommu and /proc/driver
# inside the guest before detect/apply run — detection is synthetic, every
# mutation (kernel args, initramfs, SELinux contexts, packages, LG build)
# is the real thing.
#
# Requires: libvirt (qemu:///system) + virt-install + curl on this machine.
# Downloads a Fedora Cloud image (~500 MiB, cached) and installs the full
# virt stack inside the guest — expect 15–30 minutes on the first run.
set -euo pipefail

REL=${FEDORA_RELEASE:-44}
VM=${VM_NAME:-orthogonals-systest}
CACHE=${CACHE_DIR:-$HOME/.cache/orthogonals-test}
# /var/tmp + 0711: the qemu user (uid 107) must traverse into the disk dir,
# and $HOME/.cache is typically 0700 — the VM disks live here, not there
WORK=$(mktemp -d /var/tmp/orthogonals-systest.XXXXXX)
chmod 0711 "$WORK"
REPO=$(cd "$(dirname "$0")/../.." && pwd)
VIRSH="virsh --connect qemu:///system"
SSH_TRIES=90 # x SSH_POLL seconds: first boot + cloud-init can take minutes
SSH_POLL=5
REBOOT_WAIT=10 # let the guest actually go down before polling ssh again

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "=== $*"; }

cleanup() {
	$VIRSH destroy "$VM" >/dev/null 2>&1 || true
	$VIRSH undefine "$VM" --nvram >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# --- build + image -----------------------------------------------------------

step "build RPM + reference fixture"
# the guest installs the RPM, not a bare binary: host packages are RPM
# dependencies, and this tier is what proves the Requires list is complete
(cd "$REPO" && make rpm >/dev/null)
VER=$(cat "$REPO/VERSION")
cp "$REPO/dist/x86_64/orthogonals-$VER"-*.x86_64.rpm "$WORK/orthogonals.rpm"
# the same tree hwtest.ReferenceRoot builds — the single source of the
# synthetic topology; only sys/ and proc/ ride into the guest
(cd "$REPO" && go run ./test/fixture "$WORK/fixture")
tar -C "$WORK/fixture" -cf "$WORK/fixture.tar" sys proc

step "fetch Fedora $REL cloud image (cached in $CACHE)"
mkdir -p "$CACHE"
if [ -z "${IMAGE:-}" ]; then
	LISTING="https://download.fedoraproject.org/pub/fedora/linux/releases/$REL/Cloud/x86_64/images/"
	NAME=$(curl -fsL "$LISTING" | grep -oE 'Fedora-Cloud-Base-Generic[^"<>]*\.qcow2' | head -1) || NAME=""
	if [ -n "$NAME" ]; then
		IMAGE=$CACHE/$NAME
		[ -f "$IMAGE" ] || curl -fL -o "$IMAGE" "$LISTING$NAME"
	else
		# mirror listing flaked: fall back to whatever the cache already has
		IMAGE=$(ls -t "$CACHE"/Fedora-Cloud-Base-Generic*.qcow2 2>/dev/null | head -1)
		[ -n "$IMAGE" ] || fail "cannot list $LISTING and no cached image — pass IMAGE=/path/to/Fedora-Cloud.qcow2"
	fi
fi
# base copied (reflink: free on btrfs) so the backing chain never points into
# $HOME, which qemu cannot traverse
cp --reflink=auto "$IMAGE" "$WORK/base.qcow2"
chmod 0644 "$WORK/base.qcow2"
qemu-img create -f qcow2 -b "$WORK/base.qcow2" -F qcow2 "$WORK/disk.qcow2" 25G >/dev/null

# --- guest payloads ----------------------------------------------------------

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id"
SSH_OPTS=(-i "$WORK/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	-o ServerAliveInterval=30 -o ConnectTimeout=10 -o LogLevel=ERROR)

# no packages: preinstalled — installing the orthogonals RPM must pull every
# host tool via its Requires. nvidia-persistenced stub: apply disables that
# unit, and the VM has no driver.
cat >"$WORK/user-data" <<EOF
#cloud-config
users:
  - name: fedora
    sudo: 'ALL=(ALL) NOPASSWD:ALL'
    ssh_authorized_keys:
      - $(cat "$WORK/id.pub")
write_files:
  - path: /etc/systemd/system/nvidia-persistenced.service
    content: |
      [Unit]
      Description=stub for the orthogonals system test (no NVIDIA driver in the VM)
      [Service]
      ExecStart=/usr/bin/true
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl daemon-reload
EOF

# fake sysfs: the shipped reference topology bind-mounted over the live paths
# (idempotent, does not survive reboot — only detect/preflight/apply need it)
cat >"$WORK/fakesys.sh" <<'EOF'
#!/bin/bash
set -euo pipefail
FAKE=/opt/orthogonals-fakesys
rm -rf "$FAKE"
mkdir -p "$FAKE"
tar -C "$FAKE" -xf /tmp/fixture.tar
# a bad extraction must fail loudly here, not as a cryptic detect miss
[ -e "$FAKE/sys/bus/pci/devices/0000:01:00.0" ] || { echo "fixture tree incomplete" >&2; exit 1; }

remount() {
	mountpoint -q "$2" && umount "$2"
	mount --bind "$1" "$2"
}
remount "$FAKE/sys/bus/pci/devices" /sys/bus/pci/devices
remount "$FAKE/sys/class/iommu" /sys/class/iommu
remount "$FAKE/sys/devices/system/cpu" /sys/devices/system/cpu
remount "$FAKE/proc/driver" /proc/driver
# file bind: the cpu/memory sizing preflight checks read the reference
# machine's values, not the 4 GiB test guest's
remount "$FAKE/proc/meminfo" /proc/meminfo
EOF

# --- boot the VM -------------------------------------------------------------

step "create VM $VM"
virt-install --connect qemu:///system --name "$VM" --memory 4096 --vcpus 6 \
	--disk "path=$WORK/disk.qcow2,format=qcow2" --import \
	--osinfo "fedora-unknown" --cloud-init "user-data=$WORK/user-data" \
	--network network=default --graphics none --noautoconsole >/dev/null

get_ip() { $VIRSH domifaddr "$VM" 2>/dev/null | awk '/ipv4/ {sub(/\/.*/, "", $4); print $4; exit}'; }
wait_ssh() {
	local i ip
	for i in $(seq 1 "$SSH_TRIES"); do
		ip=$(get_ip)
		if [ -n "$ip" ] && ssh "${SSH_OPTS[@]}" "fedora@$ip" true 2>/dev/null; then
			echo "$ip"
			return
		fi
		sleep "$SSH_POLL"
	done
	fail "VM never became reachable over ssh"
}

step "wait for ssh + cloud-init"
IP=$(wait_ssh)
vm() { ssh "${SSH_OPTS[@]}" "fedora@$IP" "$@"; }
vm 'sudo cloud-init status --wait >/dev/null'

step "install RPM (pulls the full dependency set) + fake sysfs"
scp "${SSH_OPTS[@]}" "$WORK/orthogonals.rpm" "$WORK/fakesys.sh" "$WORK/fixture.tar" "fedora@$IP:/tmp/"
vm 'sudo dnf install -y -q /tmp/orthogonals.rpm' || fail "RPM install failed"
vm 'command -v virsh && command -v dracut && command -v semanage' >/dev/null ||
	fail "RPM dependencies did not pull the host tools"
# grubby is no longer an orthogonals dependency (BLS edits are native now), but
# the base cloud image ships it — we use it below as an independent verifier
# that our native /boot/loader/entries edits match what grubby reads.
vm 'command -v grubby' >/dev/null || fail "grubby absent — needed as the BLS cross-check"
vm 'sudo bash /tmp/fakesys.sh'

# --- detect / preflight / real apply -----------------------------------------

step "detect + preflight"
vm 'sudo orthogonals detect --json | grep -q 0x2206' || fail "detect misses the fake RTX 3080"
rc=0; vm 'sudo orthogonals preflight' || rc=$?
[ "$rc" = 2 ] || fail "preflight exited $rc, want 2 (warns only) — see output above"

step "apply --yes (real grubby/dracut/semanage — takes a while)"
vm 'sudo orthogonals apply --yes --user fedora' || fail "apply failed"
vm 'test -f /var/lib/orthogonals/manifest.json' || fail "no manifest after apply"
# cross-check: grubby (reading the same BLS entries our op wrote) sees the args
vm 'sudo grubby --info=ALL | grep -q intel_iommu=on' ||
	fail "grubby does not see the native BLS kernel-arg edit"

# a guest-initiated reboot can land as a domain poweroff with this cloud
# image (observed live; on_reboot=restart notwithstanding) — start it again
# if it went down. `virsh start` on a running domain fails harmlessly.
reboot_vm() {
	vm 'sudo reboot' || true
	sleep "$REBOOT_WAIT"
	$VIRSH start "$VM" >/dev/null 2>&1 || true
	IP=$(wait_ssh)
}

step "reboot + assert live configs"
reboot_vm
vm 'grep -q intel_iommu=on /proc/cmdline && grep -q iommu=pt /proc/cmdline' ||
	fail "vfio kernel args not live after reboot"
vm 'test -f /etc/dracut.conf.d/vfio.conf' || fail "vfio.conf missing"
vm 'sudo lsinitrd 2>/dev/null | grep -q vfio_pci || sudo lsinitrd 2>/dev/null | grep -q vfio-pci' ||
	fail "vfio-pci not in the regenerated initramfs"
vm 'sudo semanage fcontext -l | grep -q looking-glass' || fail "SELinux fcontext missing"
vm 'sudo test -x /etc/libvirt/hooks/qemu' || fail "qemu hook missing or not executable"
vm 'grep -q "hook --user fedora qemu" /etc/libvirt/hooks/qemu' || fail "qemu hook is not the orthogonals shim"
vm 'test -e /etc/libvirt/hooks/orthogonals-gpu-detach.sh' && fail "gpu-detach.sh should be gone (logic is Go now)"
vm 'test -x /usr/bin/looking-glass-client' || fail "looking-glass-client not installed"
# vm launch against a nonexistent domain proves the subcommand + libvirt wiring
# (clean nonzero, "no such VM") without needing a defined guest
vm 'sudo orthogonals vm launch --vm-name ghost 2>&1 | grep -q "no such VM"' ||
	fail "vm launch did not reach libvirt / report the missing domain"
# the hook fires end to end against the real daemon SELinux context: an
# unmanaged domain must exit 0 (no NVIDIA in the guest, so a managed detach is
# not exercised — the unit transcripts cover that)
vm 'sudo /usr/bin/orthogonals hook --user fedora qemu ghost prepare begin -' ||
	fail "hook dispatch failed for an unmanaged domain"
# the sleep inhibitor: start it as a transient unit, confirm logind holds the
# block, then stop it and confirm the lock releases (exercises logind fd
# passing + SIGTERM release under enforcing SELinux)
vm 'sudo systemd-run --unit=orth-inhibit-smoke /usr/bin/orthogonals hook inhibit smoke' ||
	fail "could not start the inhibitor transient unit"
sleep 2
vm 'systemd-inhibit --list | grep -q orthogonals' || fail "logind did not register the orthogonals sleep inhibitor"
vm 'sudo systemctl stop orth-inhibit-smoke 2>/dev/null; sleep 1; ! systemd-inhibit --list | grep -q orthogonals' ||
	fail "sleep inhibitor did not release on stop"

# --- undo + assert pristine ---------------------------------------------------

step "undo --yes"
vm 'sudo orthogonals undo --yes' || fail "undo failed"
vm 'sudo grubby --info=ALL | grep -q intel_iommu=on' && fail "kernel args survived undo"
vm 'test -e /etc/dracut.conf.d/vfio.conf' && fail "vfio.conf survived undo"
vm 'sudo test -e /etc/libvirt/hooks/qemu' && fail "qemu hook survived undo"
vm 'test -e /var/lib/orthogonals/manifest.json' && fail "manifest survived undo"
vm 'sudo semanage fcontext -l | grep -q looking-glass' && fail "SELinux fcontext survived undo"

step "final reboot + pristine cmdline"
reboot_vm
vm 'grep -q intel_iommu=on /proc/cmdline' && fail "kernel args still live after undo + reboot"

echo
echo "system test: all checks passed (VM will be destroyed)"
