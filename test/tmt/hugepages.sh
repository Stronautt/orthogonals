#!/bin/bash
# The 2M hugepage pool the qemu hook reserves before every VM start. Under a
# --root prefix nr_hugepages is an ordinary file that reads back whatever was
# written to it, so the retry-on-readback loop cannot be exercised there. Here
# the kernel allocates real physical pages and reports what it got.
set -euo pipefail
cd "$(dirname "$0")"
export ORTHOGONALS_NEEDS_BINARY=0 # runs `go test`, not the built binary
# shellcheck source=lib.sh
source ./lib.sh

require_root "the hugepage tier"
[ -e /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages ] ||
	fail "this kernel has no 2M hugepage pool"

prior=$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages)
step "hugepage reservation against the running kernel (pool starts at $prior)"

go_tier hugepages ./internal/hooks -run 'AgainstTheRealPool' \
	-- TestReserveHugepagesAgainstTheRealPool TestReserveHugepagesShortfallAgainstTheRealPool

# The tests restore the pool in t.Cleanup; prove it from the outside too, since
# a run that leaves a guest's memory pinned poisons everything after it.
now=$(cat /sys/kernel/mm/hugepages/hugepages-2048kB/nr_hugepages)
[ "$now" = "$prior" ] || fail "the pool was left at $now, not the $prior it started at"

echo
echo "hugepages: reserved and released against a real kernel pool"
