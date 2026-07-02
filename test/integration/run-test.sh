#!/bin/bash
# Integration tier: runs the built orthogonals binary through
# detect → preflight → apply --yes → undo --yes against a synthetic --root
# tree (the PoC reference machine: i5-13600K + RTX 3080) with fake system
# binaries on PATH. Asserts exit codes, manifest correctness, and
# byte-identical filesystem restore. Designed for a clean fedora:44
# container (make test-integration) but runs anywhere bash + python3 +
# GNU diffutils exist.
set -euo pipefail

BIN=${ORTHOGONALS_BIN:-/usr/local/bin/orthogonals}
WORK=$(mktemp -d)
ROOT=$WORK/root
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }

[ -x "$BIN" ] || fail "orthogonals binary not found at $BIN (set ORTHOGONALS_BIN)"

# --- synthetic sysfs tree (hwtest.BuildReferenceRoot, baked into the image
# by `go run ./test/fixture` — the single source of the reference topology) --

FIXTURE=${ORTHOGONALS_FIXTURE:-/usr/local/share/orthogonals-fixture}
[ -e "$FIXTURE/sys/bus/pci/devices/0000:01:00.0" ] ||
	fail "fixture tree missing at $FIXTURE (regenerate: go run ./test/fixture <dir>, set ORTHOGONALS_FIXTURE)"
mkdir -p "$ROOT"
cp -a "$FIXTURE/." "$ROOT/"

# base dirs every real host has (undo only removes dirs apply itself created)
mkdir -p "$ROOT/etc" "$ROOT/var/lib" "$ROOT/usr/local/bin" "$ROOT/usr/share/applications"

# --- fake system binaries (argv-logging stubs, like the Go test seam) -------

FAKEBIN=$WORK/fakebin
mkdir -p "$FAKEBIN"
# rm is faked too: undo's paired commands (rm -f /usr/local/bin/...) target
# real host paths by design — the PATH seam keeps them off this machine.
for name in dnf grubby dracut semanage restorecon systemctl virsh bash \
	qemu-img xorriso lsof nvidia-smi wiminfo usermod rm; do
	printf '#!/bin/sh\necho "$*" >> "%s/%s.log"\nexit 0\n' "$FAKEBIN" "$name" >"$FAKEBIN/$name"
	chmod 0755 "$FAKEBIN/$name"
done
# is-enabled must answer, or the engine treats every unit as not installed
printf '#!/bin/sh\necho "$*" >> "%s/systemctl.log"\nif [ "$1" = "is-enabled" ]; then echo enabled; fi\nexit 0\n' \
	"$FAKEBIN" >"$FAKEBIN/systemctl"

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
[ -z "$(binlog grubby)" ] || fail "dry-run apply executed grubby"
pass "apply dry-run"

# --- apply --yes -------------------------------------------------------------

run 0 apply --root "$ROOT" --user testuser --yes
for path in \
	/etc/dracut.conf.d/vfio.conf \
	/etc/udev/rules.d/61-mutter-ignore-nvidia.rules \
	/etc/environment.d/50-orthogonals-igpu.conf \
	/etc/tmpfiles.d/looking-glass.conf \
	/etc/libvirt/hooks/qemu \
	/etc/libvirt/hooks/orthogonals-gpu-detach.sh \
	/etc/libvirt/hooks/orthogonals-gpu-reattach.sh \
	/var/lib/orthogonals/manifest.json; do
	[ -e "$ROOT$path" ] || fail "missing $path after apply --yes"
done
[ "$(stat -c '%a' "$ROOT/etc/libvirt/hooks/qemu")" = 755 ] || fail "qemu hook is not executable"
binlog grubby | grep -q -- '--update-kernel=ALL --args=intel_iommu=on iommu=pt' ||
	fail "grubby was not invoked with the vfio kernel args"
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
assert {"write_file", "run_cmd"} <= kinds, f"missing step kinds, got {kinds}"
EOF
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
binlog grubby | grep -q -- '--remove-args' || fail "undo did not run the paired grubby --remove-args"
pass "undo --yes (byte-identical restore)"

echo "integration test: all checks passed"
