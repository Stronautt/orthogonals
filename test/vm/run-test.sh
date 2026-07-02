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
WORK=$(mktemp -d)
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

step "build binary + reference fixture"
(cd "$REPO" && CGO_ENABLED=0 go build -o "$WORK/orthogonals" .)
# the same tree hwtest.ReferenceRoot builds — the single source of the
# synthetic topology; only sys/ and proc/ ride into the guest
(cd "$REPO" && go run ./test/fixture "$WORK/fixture")
tar -C "$WORK/fixture" -cf "$WORK/fixture.tar" sys proc

step "fetch Fedora $REL cloud image (cached in $CACHE)"
mkdir -p "$CACHE"
if [ -z "${IMAGE:-}" ]; then
	LISTING="https://download.fedoraproject.org/pub/fedora/linux/releases/$REL/Cloud/x86_64/images/"
	NAME=$(curl -fsL "$LISTING" | grep -oE 'Fedora-Cloud-Base-Generic[^"<>]*\.qcow2' | head -1) ||
		fail "cannot list $LISTING — pass IMAGE=/path/to/Fedora-Cloud.qcow2"
	IMAGE=$CACHE/$NAME
	[ -f "$IMAGE" ] || curl -fL -o "$IMAGE" "$LISTING$NAME"
fi
qemu-img create -f qcow2 -b "$IMAGE" -F qcow2 "$WORK/disk.qcow2" 25G >/dev/null

# --- guest payloads ----------------------------------------------------------

ssh-keygen -q -t ed25519 -N '' -f "$WORK/id"
SSH_OPTS=(-i "$WORK/id" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	-o ServerAliveInterval=30 -o ConnectTimeout=10 -o LogLevel=ERROR)

# hard preflight tools preinstalled so preflight runs before apply's dnf step;
# nvidia-persistenced stub: apply disables that unit, and the VM has no driver.
cat >"$WORK/user-data" <<EOF
#cloud-config
users:
  - name: fedora
    sudo: 'ALL=(ALL) NOPASSWD:ALL'
    ssh_authorized_keys:
      - $(cat "$WORK/id.pub")
packages: [libvirt-client, qemu-img, xorriso, lsof, wimlib-utils, policycoreutils-python-utils]
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
remount "$FAKE/proc/driver" /proc/driver
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

step "install binary + fake sysfs"
scp "${SSH_OPTS[@]}" "$WORK/orthogonals" "$WORK/fakesys.sh" "$WORK/fixture.tar" "fedora@$IP:/tmp/"
vm 'sudo install -m 0755 /tmp/orthogonals /usr/local/bin/orthogonals && sudo bash /tmp/fakesys.sh'

# --- detect / preflight / real apply -----------------------------------------

step "detect + preflight"
vm 'sudo orthogonals detect --json | grep -q 0x2206' || fail "detect misses the fake RTX 3080"
rc=0; vm 'sudo orthogonals preflight' || rc=$?
[ "$rc" = 2 ] || fail "preflight exited $rc, want 2 (warns only) — see output above"

step "apply --yes (real grubby/dracut/dnf/semanage — takes a while)"
vm 'sudo orthogonals apply --yes --user fedora' || fail "apply failed"
vm 'test -f /var/lib/orthogonals/manifest.json' || fail "no manifest after apply"

step "reboot + assert live configs"
vm 'sudo reboot' || true
sleep "$REBOOT_WAIT"
IP=$(wait_ssh)
vm 'grep -q intel_iommu=on /proc/cmdline && grep -q iommu=pt /proc/cmdline' ||
	fail "vfio kernel args not live after reboot"
vm 'test -f /etc/dracut.conf.d/vfio.conf' || fail "vfio.conf missing"
vm 'sudo lsinitrd 2>/dev/null | grep -q vfio_pci || sudo lsinitrd 2>/dev/null | grep -q vfio-pci' ||
	fail "vfio-pci not in the regenerated initramfs"
vm 'sudo semanage fcontext -l | grep -q looking-glass' || fail "SELinux fcontext missing"
vm 'test -x /etc/libvirt/hooks/qemu' || fail "qemu hook missing or not executable"
vm 'test -x /usr/local/bin/looking-glass-client' || fail "looking-glass-client not built"

# --- undo + assert pristine ---------------------------------------------------

step "undo --yes"
vm 'sudo orthogonals undo --yes' || fail "undo failed"
vm 'sudo grubby --info=ALL | grep -q intel_iommu=on' && fail "kernel args survived undo"
vm 'test -e /etc/dracut.conf.d/vfio.conf' && fail "vfio.conf survived undo"
vm 'test -e /etc/libvirt/hooks/qemu' && fail "qemu hook survived undo"
vm 'test -e /var/lib/orthogonals/manifest.json' && fail "manifest survived undo"
vm 'sudo semanage fcontext -l | grep -q looking-glass' && fail "SELinux fcontext survived undo"

step "final reboot + pristine cmdline"
vm 'sudo reboot' || true
sleep "$REBOOT_WAIT"
IP=$(wait_ssh)
vm 'grep -q intel_iommu=on /proc/cmdline' && fail "kernel args still live after undo + reboot"

echo
echo "system test: all checks passed (VM will be destroyed)"
