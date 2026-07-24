#!/bin/bash
# The logind sleep inhibitor the qemu hook holds for the life of a VM. Under a
# --root prefix there is no system bus to take a lock from, so every statement
# in internal/hooks/inhibit.go is unreachable outside this tier — and the
# failure is silent: the host suspends with the GPU handed to a guest.
set -euo pipefail
cd "$(dirname "$0")"
# Real logind, real D-Bus: a fake would assert nothing.
export ORTHOGONALS_REAL_TOOLS=1
# shellcheck source=lib.sh
source ./lib.sh

require_root "the inhibit tier"
command -v systemd-inhibit >/dev/null || fail "systemd-inhibit is not installed"

step "the hook takes a sleep inhibitor"
"$BIN" hook inhibit tier-test &
inhibitor=$!

# The lock is taken over D-Bus after the process is up, so poll. Redirected to
# a file, never piped into grep -q: under pipefail a match kills the producer.
held=0
for _ in $(seq 50); do
	systemd-inhibit --list >"$WORK/inhibit.txt" 2>&1 || true
	if grep -q orthogonals "$WORK/inhibit.txt"; then
		held=1
		break
	fi
	sleep 0.1
done
if [ "$held" -ne 1 ]; then
	sed 's/^/  | /' "$WORK/inhibit.txt" >&2
	kill "$inhibitor" 2>/dev/null || true
	fail "no orthogonals inhibitor appeared in systemd-inhibit --list"
fi
grep -q sleep "$WORK/inhibit.txt" ||
	fail "the inhibitor does not block sleep — a suspend would strand the GPU in the guest"
pass "logind reports an orthogonals sleep inhibitor"

step "SIGTERM releases it"
kill -TERM "$inhibitor"
wait "$inhibitor" || fail "hook inhibit exited non-zero on SIGTERM"
systemd-inhibit --list >"$WORK/after.txt" 2>&1 || true
grep -q orthogonals "$WORK/after.txt" &&
	fail "the inhibitor outlived the process holding it"
pass "the lock is released when the transient unit stops"

echo
echo "inhibit: the sleep lock is taken and released against real logind"
