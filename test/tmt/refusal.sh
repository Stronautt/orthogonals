#!/bin/bash
# A host preflight rejects must be left exactly as apply found it. The refusal
# path runs after detect and fact-gathering, so "it refused" and "it refused
# without writing anything" are two separate claims.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

# fixture : the check that must fail : a phrase its message has to carry
SCENARIOS=(
	"dirty-group:iommu-group:whole group"
	"no-igpu:gpu-topology:single-GPU host"
	"dual-nvidia:duplicate-gpu-ids:share vendor:device"
	"foreign-vfio:foreign-vfio:pre-existing vfio configuration"
	"no-reset:gpu-reset:no sysfs reset file"
)

for scenario in "${SCENARIOS[@]}"; do
	IFS=: read -r kind check phrase <<<"$scenario"
	step "$kind"
	root=$WORK/$kind
	prep_root "$kind" "$root"
	snapshot_into "$root" "$kind"

	# preflight fails (1), and names the check it failed on
	run 1 preflight --root "$root" --json
	python3 - "$WORK/out" "$check" <<'EOF'
import json, sys
report = json.load(open(sys.argv[1]))
want = sys.argv[2]
assert report["status"] == "fail", f'overall status is {report["status"]}, want fail'
failed = [c["name"] for c in report["checks"] if c["status"] == "fail"]
assert want in failed, f"{want} did not fail; failing checks were {failed}"
EOF

	# the human-readable report explains why
	run 1 preflight --root "$root"
	grep -qi "$phrase" "$WORK/out" || fail "$kind: preflight report never mentions '$phrase'"

	# apply refuses, and leaves nothing behind
	rc=$(run_any apply --root "$root" --user testuser --yes)
	[ "$rc" != 0 ] || fail "$kind: apply --yes succeeded on a host preflight failed"
	grep -q "preflight $check" "$WORK/out" ||
		fail "$kind: apply did not name the failing check $check"
	[ -e "$root/var/lib/orthogonals/manifest.json" ] &&
		fail "$kind: refused apply still wrote a manifest"
	assert_restored "$root" "$kind" "$kind refused apply"

	pass "$kind (preflight fails on $check, apply refused and wrote nothing)"
done

# A /etc/default/grub this build will not edit is a refusal of its own. The
# alternative to naming the line is appending an assignment past it, which wins
# under sh and drops whatever the host booted with at the next regeneration.
step "grub-export"
root=$WORK/grub-export
prep_root reference "$root"
sed -i 's/^GRUB_CMDLINE_LINUX=/export GRUB_CMDLINE_LINUX=/' "$root/etc/default/grub"
grep -q '^export GRUB_CMDLINE_LINUX=' "$root/etc/default/grub" ||
	fail "grub-export: the fixture no longer has a GRUB_CMDLINE_LINUX line to export"
snapshot_into "$root" grub-export

run 1 preflight --root "$root"
grep -q 'grub-defaults' "$WORK/out" ||
	fail "grub-export: preflight never named the grub-defaults check"
grep -q 'boot-entries.*fail' "$WORK/out" &&
	fail "grub-export: a grub value must not fail the boot-entries check"

rc=$(run_any apply --root "$root" --user testuser --yes)
[ "$rc" != 0 ] || fail "grub-export: apply --yes succeeded on a grub file it cannot edit"
[ -e "$root/var/lib/orthogonals/manifest.json" ] &&
	fail "grub-export: refused apply still wrote a manifest"
assert_restored "$root" grub-export "grub-export refused apply"
pass "grub-export (an unmanageable GRUB_CMDLINE_LINUX refuses apply and changes nothing)"

echo
echo "refusal: every rejected host was left untouched"
