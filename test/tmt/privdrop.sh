#!/bin/bash
# The credential switch in notify.Send, executed for real. The hook runs as
# root and delivers notifications as the desktop user; every unit run does that
# with its own account, where uid equals euid and the branch is skipped. Needs
# root and a second account — the vm plan creates orthtest.
set -euo pipefail
cd "$(dirname "$0")"
ORTHOGONALS_NEEDS_BINARY=0
export ORTHOGONALS_REAL_TOOLS=1
# shellcheck source=lib.sh
source ./lib.sh

require_root "the privilege-drop tier"
id "$USER_NAME" >/dev/null 2>&1 || fail "the desktop user $USER_NAME does not exist"

go_tier privdrop ./internal/notify -run TestSendDropsToAnotherUser \
	-- TestSendDropsToAnotherUser

echo
echo "privdrop: the notification really is delivered as $USER_NAME, not as root"
