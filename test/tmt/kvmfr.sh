#!/bin/bash
# The kvmfr module against a real kernel. Three things no --root fixture can
# answer:
#
#   1. the out-of-tree module still compiles and loads on this kernel;
#   2. its ioctl and DMABUF export work, via upstream's own module/test.c;
#   3. the cgroup_device_acl list orthogonals writes is complete for the libvirt
#      installed here — a real qemu opens /dev/kvmfr0 through it.
set -euo pipefail
cd "$(dirname "$0")"
export ORTHOGONALS_NEEDS_BINARY=0 # go test plus a probe domain, not the binary
# shellcheck source=lib.sh
source ./lib.sh

require_root "the kvmfr tier"

MODULE_SRC=$TREE/packaging/third_party/LookingGlass/module
[ -f "$MODULE_SRC/kvmfr.c" ] ||
	fail "$MODULE_SRC/kvmfr.c is missing — run: git submodule update --init --recursive"

step "build tools for an out-of-tree module"
# kvmfr-dkms lives in COPR, so the plan's spec-derived install skips it; the
# module is built from the submodule instead.
dnf install -y --setopt=retries=10 --setopt=timeout=60 \
	gcc make "kernel-devel-$(uname -r)" >/dev/null
pass "gcc, make and kernel-devel-$(uname -r)"

step "build kvmfr against $(uname -r)"
BUILD=$WORK/module
mkdir -p "$BUILD"
cp "$MODULE_SRC"/* "$BUILD/"
make -C "/lib/modules/$(uname -r)/build" "M=$BUILD" modules >"$WORK/build.log" 2>&1 ||
	{ cat "$WORK/build.log" >&2; fail "kvmfr does not build against $(uname -r)"; }
pass "kvmfr.ko built"

step "install it where modprobe and depmod can find it"
# dkms lands modules in extra/; the same by hand, so the hook's own modprobe and
# hw.KVMFRAvailable's modules.dep lookup are what get exercised.
make -C "/lib/modules/$(uname -r)/build" "M=$BUILD" INSTALL_MOD_DIR=extra \
	modules_install >>"$WORK/build.log" 2>&1 ||
	{ cat "$WORK/build.log" >&2; fail "modules_install failed"; }
depmod -a
cleanup() {
	rmmod kvmfr 2>/dev/null || true
	rm -f "/lib/modules/$(uname -r)/extra/kvmfr.ko"* /run/orthogonals-kvmfr-size
	depmod -a
	[ -f "$WORK/qemu.conf.orig" ] && cp "$WORK/qemu.conf.orig" /etc/libvirt/qemu.conf
	systemctl restart virtqemud.service 2>/dev/null || true
}
trap cleanup EXIT
pass "installed into /lib/modules/$(uname -r)/extra"

step "the hook's own module setup, against this kernel"
SIZE_MB=32
id "$USER_NAME" >/dev/null 2>&1 || useradd --create-home "$USER_NAME"
# Drives EnsureKVMFR itself rather than a shell re-implementation that could
# pass while the shipping code fails.
ORTHOGONALS_TIER_KVMFR=1 ORTHOGONALS_TEST_USER=$USER_NAME \
	go_tier kvmfr-module ./internal/hooks -run TestEnsureKVMFRAgainstTheRealKernel \
	-- TestEnsureKVMFRAgainstTheRealKernel
[ -c /dev/kvmfr0 ] || fail "/dev/kvmfr0 is not a character device"
pass "$(ls -lZ /dev/kvmfr0)"

step "ioctl and DMABUF export (upstream's module/test.c)"
gcc -I"$BUILD" -o "$BUILD/kvmfrtest" "$BUILD/test.c" -Wall -O1
# lib.sh's WORK is root's mktemp -d, so 0700: the desktop user cannot traverse
# it. The permission under test is the device node's, not the path's.
chmod a+rx "$WORK" "$BUILD"
runuser -u "$USER_NAME" -- "$BUILD/kvmfrtest" >"$WORK/test.out" 2>&1 ||
	{ cat "$WORK/test.out" >&2; fail "the module's own test program failed"; }
# test.expected has no leading "Size:" line: that one varies with static_size_mb.
tail -n +2 "$WORK/test.out" >"$WORK/test.trimmed"
diff -u "$MODULE_SRC/test.expected" "$WORK/test.trimmed" ||
	fail "DMABUF round-trip differs from upstream's expected output"
pass "GETSIZE + DMABUF_CREATE + mmap round-trip"

step "convert the libvirt this guest actually shipped"
cp /etc/libvirt/qemu.conf "$WORK/qemu.conf.orig"
ORTHOGONALS_TIER_KVMFR=1 go_tier kvmfr-acl ./internal/hostcfg -run TestDeviceACLAgainstTheRealLibvirt \
	-- TestDeviceACLAgainstTheRealLibvirt
systemctl restart virtqemud.service
pass "cgroup_device_acl written and virtqemud restarted"

step "a real qemu opens /dev/kvmfr0 through that list"
# The node keeps the ownership and label EnsureKVMFR gave it; anything fixed up
# here would be a bug in the hook.
[ -c /dev/kvm ] ||
	fail "this guest has no /dev/kvm — the tier needs nested virtualisation, because a
	TCG domain is confined as svirt_tcg_t and would test a policy nothing ships with"
cat >"$WORK/probe.xml" <<EOF
<domain type='kvm' xmlns:qemu='http://libvirt.org/schemas/domain/qemu/1.0'>
  <name>kvmfr-probe</name>
  <memory unit='MiB'>256</memory>
  <vcpu>1</vcpu>
  <os><type arch='x86_64' machine='q35'>hvm</type></os>
  <devices>
    <controller type='pci' index='0' model='pcie-root'/>
    <!-- Mirrors the rendered domain: index the root port above the bridge and
         libvirt emits the bridge first, which qemu refuses. -->
    <controller type='pci' index='1' model='pcie-root-port'/>
    <controller type='pci' index='2' model='pcie-to-pci-bridge'>
      <address type='pci' domain='0x0000' bus='0x01' slot='0x00' function='0x0'/>
    </controller>
  </devices>
  <qemu:commandline>
    <qemu:arg value='-object'/>
    <qemu:arg value='{"qom-type":"memory-backend-file","id":"lg","mem-path":"/dev/kvmfr0","size":$((SIZE_MB * 1024 * 1024)),"share":true}'/>
    <qemu:arg value='-device'/>
    <qemu:arg value='{"driver":"ivshmem-plain","id":"shmem0","memdev":"lg","bus":"pci.2","addr":"0x1"}'/>
  </qemu:commandline>
</domain>
EOF
# type='kvm' deliberately: libvirt confines a KVM domain as svirt_t, what a real
# VM gets, while TCG gets svirt_tcg_t — a tighter policy that cannot map an
# svirt_image_t chr_file. It also makes /dev/kvm load-bearing: drop that ACL
# entry and this domain loses KVM outright.
if ! virsh --connect qemu:///system create "$WORK/probe.xml" >"$WORK/probe.log" 2>&1; then
	# Permission denied here is either the cgroup device filter or svirt; the
	# two need different fixes.
	cat "$WORK/probe.log" >&2
	echo "--- device ---" >&2
	ls -lZ /dev/kvmfr0 >&2
	echo "--- active cgroup_device_acl ---" >&2
	sed -n '/^cgroup_device_acl/,/^]/p' /etc/libvirt/qemu.conf >&2
	echo "--- selinux ---" >&2
	getenforce >&2 || true
	ausearch -m AVC -ts recent >"$WORK/avc.log" 2>&1 || true
	grep kvmfr "$WORK/avc.log" >&2 || echo "(no AVC mentioning kvmfr)" >&2
	fail "qemu could not open /dev/kvmfr0"
fi
pid=$(pgrep -f 'guest=kvmfr-probe' | head -1 || true)
[ -n "$pid" ] || fail "the probe domain reported started but has no qemu process"
host_dev=$(stat -c '%t:%T' /dev/kvmfr0)
ns_dev=$(stat -c '%t:%T' "/proc/$pid/root/dev/kvmfr0" 2>/dev/null || echo none)
virsh --connect qemu:///system destroy kvmfr-probe >/dev/null 2>&1 || true
# qemu opens the backend with O_CREAT, so a missing device leaves the guest
# writing frames nobody reads.
[ "$ns_dev" = "$host_dev" ] ||
	fail "qemu saw $ns_dev in its private /dev, not the host's $host_dev — it got a regular file"
pass "qemu mapped the real device ($host_dev)"

echo
echo "kvmfr: builds, loads, exports a DMABUF, and is reachable by qemu on $(uname -r)"
