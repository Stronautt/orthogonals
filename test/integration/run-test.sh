#!/bin/bash
# Integration tier: runs the built orthogonals binary through
# detect → preflight → apply --yes → undo --yes against synthetic --root trees
# (baked by `go run ./test/fixture` — the single source of every topology) with
# fake system binaries on PATH. Asserts exit codes, manifest correctness, and
# byte-identical filesystem restore for the reference desktop plus two laptop
# fixtures. Designed for a clean fedora:44 container (make test-integration) but
# runs anywhere bash + python3 + GNU diffutils exist.
set -euo pipefail

BIN=${ORTHOGONALS_BIN:-/usr/local/bin/orthogonals}
WORK=$(mktemp -d)
ROOT=$WORK/root
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

[ -x "$BIN" ] || fail "orthogonals binary not found at $BIN (set ORTHOGONALS_BIN)"

FIXTURE=${ORTHOGONALS_FIXTURE:-/usr/local/share/orthogonals-fixture}
FIXTURE_LAPTOP=${ORTHOGONALS_FIXTURE_LAPTOP:-/usr/local/share/orthogonals-fixture-laptop}
FIXTURE_LAPTOP_AMD=${ORTHOGONALS_FIXTURE_LAPTOP_AMD:-/usr/local/share/orthogonals-fixture-laptop-amd}

# --- fake system binaries (argv-logging stubs, like the Go test seam) --------
# libvirt/systemd are not faked: the binary speaks their APIs, and under --root
# those steps journal and skip ("skipped under --root") — test-vm covers them
# live. Only the exec'd vendor tools need stubs here.

FAKEBIN=$WORK/fakebin
mkdir -p "$FAKEBIN"
for name in dracut semanage restorecon bash nvidia-smi usermod; do
	printf '#!/bin/sh\necho "$*" >> "%s/%s.log"\nexit 0\n' "$FAKEBIN" "$name" >"$FAKEBIN/$name"
	chmod 0755 "$FAKEBIN/$name"
done

binlog() { cat "$FAKEBIN/$1.log" 2>/dev/null || true; }

run() { # expected-rc args... (fake PATH scoped to the binary, not this script)
	local want=$1 rc=0
	shift
	env PATH="$FAKEBIN:$PATH" "$BIN" "$@" >"$WORK/out" 2>&1 || rc=$?
	[ "$rc" = "$want" ] || {
		sed 's/^/  | /' "$WORK/out" >&2
		fail "orthogonals $* exited $rc, want $want"
	}
}

tree_state() { # dir → content-addressed listing (path, mode, type) on stdout
	(cd "$1" && find . -exec stat -c '%n %a %F' {} + | sort)
}

prep_root() { # fixture-src dest
	local fixture=$1 dest=$2
	[ -e "$fixture/sys/bus/pci/devices/0000:01:00.0" ] ||
		fail "fixture tree missing at $fixture (regenerate: go run ./test/fixture <dir> <kind>)"
	mkdir -p "$dest"
	cp -a "$fixture/." "$dest/"
	# base dirs every real host has (undo only removes dirs apply itself created)
	mkdir -p "$dest/etc" "$dest/var/lib" "$dest/usr/local/bin" "$dest/usr/share/applications"
	# BLS entries: apply edits these directly for the IOMMU kernel args (the
	# native replacement for grubby). Two entries prove the op touches all.
	mkdir -p "$dest/boot/loader/entries"
	local v
	for v in 6.15.0 6.14.0; do
		printf 'title Fedora Linux (%s)\nlinux /vmlinuz-%s\noptions root=UUID=aaaa ro rhgb quiet\n' \
			"$v" "$v" >"$dest/boot/loader/entries/fedora-$v.conf"
	done
}

# ============================================================================
# reference desktop (i5-13600K + RTX 3080): the thorough scenario
# ============================================================================

prep_root "$FIXTURE" "$ROOT"

# --- detect ------------------------------------------------------------------

run 0 detect --json --root "$ROOT"
grep -q '"0x2206"' "$WORK/out" || fail "detect JSON missing the RTX 3080 device id"
grep -q '"iommu_address_width": *39' "$WORK/out" || fail "detect JSON missing 39-bit IOMMU width"
pass "detect"

# --- preflight (39-bit + Secure Boot + inactive default net ⇒ warns, exit 2)

run 2 preflight --root "$ROOT"
grep -qi 'warn' "$WORK/out" || fail "preflight exit 2 but no warning in output"
pass "preflight (warn)"

# --- apply: dry-run touches nothing -----------------------------------------

cp -a "$ROOT" "$WORK/pre"
tree_state "$ROOT" >"$WORK/pre.state"

run 0 apply --root "$ROOT" --user testuser
grep -q 'dry run' "$WORK/out" || fail "apply without --yes must announce the dry run"
diff -r --no-dereference "$WORK/pre" "$ROOT" >/dev/null || fail "dry-run apply modified the tree"
pass "apply dry-run"

# --- apply --yes -------------------------------------------------------------

run 0 apply --root "$ROOT" --user testuser --yes
for path in \
	/etc/dracut.conf.d/vfio.conf \
	/etc/udev/rules.d/61-mutter-ignore-nvidia.rules \
	/etc/environment.d/50-orthogonals-igpu.conf \
	/etc/tmpfiles.d/looking-glass.conf \
	/etc/libvirt/hooks/qemu \
	/var/lib/orthogonals/manifest.json; do
	[ -e "$ROOT$path" ] || fail "missing $path after apply --yes"
done
# the reference is a desktop: no laptop-only RTD3 artifacts
for path in /etc/modprobe.d/nvidia-rtd3.conf /etc/udev/rules.d/80-orthogonals-nvidia-pm.rules; do
	[ -e "$ROOT$path" ] && fail "desktop apply installed the laptop-only $path"
done
[ "$(stat -c '%a' "$ROOT/etc/libvirt/hooks/qemu")" = 755 ] || fail "qemu hook is not executable"
grep -q 'hook --user testuser qemu' "$ROOT/etc/libvirt/hooks/qemu" ||
	fail "qemu hook is not the orthogonals shim"
for gone in orthogonals-gpu-detach.sh orthogonals-gpu-reattach.sh; do
	[ -e "$ROOT/etc/libvirt/hooks/$gone" ] && fail "apply still installs $gone"
done
for e in "$ROOT"/boot/loader/entries/*.conf; do
	grep -q 'intel_iommu=on iommu=pt' "$e" || fail "BLS entry $e missing the vfio kernel args"
done
binlog dracut | grep -q -- '-f --regenerate-all' || fail "dracut regenerate was not invoked"
grep -qi 'reboot required' "$WORK/out" || fail "apply --yes missing the reboot notice"

python3 - "$ROOT/var/lib/orthogonals/manifest.json" <<'EOF'
import json, sys
m = json.load(open(sys.argv[1]))
recs = m["records"]
assert recs, "manifest has no records"
ids = [r["id"] for r in recs]
assert len(ids) == len(set(ids)), "duplicate record ids in manifest"
assert any(r.get("reboot") for r in recs), "no reboot-flagged (boot config) record"
kinds = {r["kind"] for r in recs}
assert {"write_file", "run_cmd", "op", "enable_unit"} <= kinds, f"missing step kinds, got {kinds}"
ops = {r["op"] for r in recs if r["kind"] == "op"}
assert "libvirt-socket-reload" in ops and "net-autostart" in ops, f"missing op records, got {ops}"
ka = next(r for r in recs if r["id"] == "kernel-args")
assert ka["kind"] == "op" and ka["op"] == "kernel-args-add" and ka.get("reboot"), f"kernel-args not an op: {ka}"
EOF
grep -q 'skipped under --root' "$WORK/out" ||
	fail "daemon-touching steps must report the --root skip"
# unmanaged-domain pass-through: the hook exits 0 and never dials a daemon
# (kept after the $WORK/out asserts above — `run` overwrites that capture)
run 0 hook --root "$ROOT" --user testuser qemu ghost prepare begin -
RECORDS=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["records"]))' \
	"$ROOT/var/lib/orthogonals/manifest.json")
pass "apply --yes ($RECORDS manifest records)"

# --- re-apply is idempotent --------------------------------------------------

# journaled but not yet live on the running kernel: the notice must persist
run 0 apply --root "$ROOT" --user testuser --yes
grep -qi 'reboot required' "$WORK/out" || fail "re-apply before the reboot must still demand it"
RECORDS2=$(python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["records"]))' \
	"$ROOT/var/lib/orthogonals/manifest.json")
[ "$RECORDS" = "$RECORDS2" ] || fail "re-apply grew the manifest: $RECORDS → $RECORDS2"

# simulate the reboot: kargs live in /proc/cmdline → no reboot demand
printf 'BOOT_IMAGE=vmlinuz root=/dev/sda1 ro intel_iommu=on iommu=pt\n' >"$ROOT/proc/cmdline"
run 0 apply --root "$ROOT" --user testuser --yes
grep -qi 'reboot required' "$WORK/out" && fail "no-op re-apply after the reboot must not demand one"
rm "$ROOT/proc/cmdline" # keep the tree comparable to the pre-apply snapshot
pass "re-apply idempotent"

# --- undo --yes: byte-identical restore --------------------------------------

run 0 undo --root "$ROOT" --yes
grep -q 'undo complete' "$WORK/out" || fail "undo did not report completion"
[ -e "$ROOT/var/lib/orthogonals/manifest.json" ] && fail "manifest survived undo"
diff -r --no-dereference "$WORK/pre" "$ROOT" ||
	fail "tree differs from pre-apply snapshot after undo"
tree_state "$ROOT" >"$WORK/post.state"
diff -u "$WORK/pre.state" "$WORK/post.state" ||
	fail "file modes/types differ from pre-apply snapshot after undo"
grep -rq intel_iommu=on "$ROOT/boot/loader/entries" && fail "undo left IOMMU kargs in the BLS entries"
pass "undo --yes (byte-identical restore)"

# ============================================================================
# laptop scenarios: chassis passes, RTD3 artifacts install, vendor-correct
# kernel args, byte-identical undo
# ============================================================================

laptop_scenario() { # name fixture-src expected-kargs muxless(yes/no)
	local name=$1 fixture=$2 kargs=$3 muxless=$4
	local root=$WORK/$name
	prep_root "$fixture" "$root"
	cp -a "$root" "$WORK/$name-pre"
	tree_state "$root" >"$WORK/$name-pre.state"

	run 0 detect --json --root "$root"
	grep -q '"chassis_type": *10' "$WORK/out" || fail "$name: detect JSON is not a laptop chassis"

	run 2 preflight --root "$root"
	grep -qi 'laptop' "$WORK/out" || fail "$name: preflight does not recognize the laptop chassis"
	if [ "$muxless" = yes ]; then
		grep -q -- '--gpu-rom' "$WORK/out" || fail "$name: MUXless preflight must mention --gpu-rom"
	fi

	run 0 apply --root "$root" --user testuser --yes
	for path in /etc/modprobe.d/nvidia-rtd3.conf /etc/udev/rules.d/80-orthogonals-nvidia-pm.rules; do
		[ -e "$root$path" ] || fail "$name: laptop apply did not install $path"
	done
	grep -q 'NVreg_DynamicPowerManagement=0x02' "$root/etc/modprobe.d/nvidia-rtd3.conf" ||
		fail "$name: RTD3 modprobe option missing"
	local e
	for e in "$root"/boot/loader/entries/*.conf; do
		grep -q "$kargs" "$e" || fail "$name: BLS entry $e missing kernel args '$kargs'"
	done
	if [ "$kargs" = "iommu=pt" ]; then
		grep -rq intel_iommu "$root"/boot/loader/entries/ && fail "$name: AMD host got intel_iommu"
	fi

	run 0 undo --root "$root" --yes
	diff -r --no-dereference "$WORK/$name-pre" "$root" || fail "$name: tree differs after undo"
	tree_state "$root" >"$WORK/$name-post.state"
	diff -u "$WORK/$name-pre.state" "$WORK/$name-post.state" || fail "$name: file modes differ after undo"
	pass "$name (kargs='$kargs', RTD3 installed, byte-identical restore)"
}

laptop_scenario laptop "$FIXTURE_LAPTOP" "intel_iommu=on iommu=pt" yes
laptop_scenario laptop-amd "$FIXTURE_LAPTOP_AMD" "iommu=pt" no

echo "integration test: all checks passed"
