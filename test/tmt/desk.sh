#!/bin/bash
# detect, preflight, and status against the machine this runs on. Read-only —
# it never applies anything, so it is safe on a daily driver.
#
# Every other test answers to a fixture; this one asks whether the fixtures
# resemble a real machine, so it needs real hardware and never runs in CI.
set -euo pipefail
cd "$(dirname "$0")"
# No binary and no synthetic root: this is a Go test against /. Real tools
# too, so preflight's `tools` check reports what is actually installed.
export ORTHOGONALS_NEEDS_BINARY=0 ORTHOGONALS_REAL_TOOLS=1
# shellcheck source=lib.sh
source ./lib.sh

[ "$(id -u)" != 0 ] || echo "note: running as root — preflight can read /boot, so it sees more than a desktop user would" >&2

step "read-only checks against this machine"
# tmt's local provisioner hands the test an empty HOME, and without one Go
# cannot locate its module cache. Recover it from the passwd entry.
[ -n "${HOME:-}" ] || HOME=$(getent passwd "$(id -u)" | cut -d: -f6)
export HOME

go_tier desk -tags desk ./test/desk \
	-- TestJSONContractOnRealHardware TestPreflightContractHoldsOnRealHardware \
	TestFixtureAttributesExistOnRealHardware

echo
echo "desk: the fixtures still match the hardware they claim to model"
