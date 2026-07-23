#!/bin/bash
# The loop-device paths, which no other test can reach: attaching a loop device
# and mounting a filesystem both need CAP_SYS_ADMIN in the initial user
# namespace, and iso9660/udf are not FS_USERNS_MOUNT.
set -euo pipefail
cd "$(dirname "$0")"
export ORTHOGONALS_NEEDS_BINARY=0 # runs `go test`, not the built binary
# shellcheck source=lib.sh
source ./lib.sh

require_root "the media tier"
[ -e /dev/loop-control ] || fail "no /dev/loop-control in this guest"

step "loop-device round trip against a real ISO"
go_tier media ./internal/media -run 'TestMountISO|TestValidateWin11ISOAgainstARealISO' \
	-- TestMountISORoundTrip TestValidateWin11ISOAgainstARealISO

echo
echo "media: BuildISO output mounted and read back through the kernel"
