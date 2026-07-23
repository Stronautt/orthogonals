#!/bin/bash
# Crash consistency, against the same binary the container-run tests drive.
# The loop itself lives in Go (test/fault) so it runs identically under
# `go test` on a laptop and under tmt on a guest.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=lib.sh
source ./lib.sh

step "fault injection against $BIN"
cd "$TREE"
ORTHOGONALS_BIN=$BIN go test ./test/fault -v -count=1
echo
echo "fault: apply survived a kill at every progress point"
