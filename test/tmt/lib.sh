#!/bin/bash
# Shared helpers for the tmt host-mutation tests. Sourced, never executed.
#
# Every test drives the same real binary; only the machine underneath changes.
# Under --root the binary confines its writes to a prefix, so a container run
# can exercise the full apply/undo bookkeeping with no privileges.

BIN=${ORTHOGONALS_BIN:-/usr/local/bin/orthogonals}
FIXTURE_BIN=${ORTHOGONALS_FIXTURE_BIN:-/usr/local/bin/orthogonals-fixture}

# TREE is anchored to this file rather than to the caller's CWD, so a test can
# cd freely and still find the repo. The rest are unused by container runs and
# harmless there.
TREE=${TMT_TREE:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
USER_NAME=${ORTHOGONALS_TEST_USER:-orthtest}
VM_NAME=orthtest-vm
GOSS=${GOSS_BIN:-/usr/local/bin/goss}
STATE=/var/lib/orthogonals/state.json

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "ok: $*"; }
step() { echo; echo "=== $*"; }

require_root() { [ "$(id -u)" = 0 ] || fail "${1:-this tier} must run as root, got uid $(id -u)"; }

# Most tests drive the built binary against a synthetic --root tree. desk.sh
# is a Go test against the real host and needs neither, so it opts out rather
# than demanding a build it will not use.
if [ "${ORTHOGONALS_NEEDS_BINARY:-1}" = 1 ]; then
	[ -x "$BIN" ] || fail "orthogonals binary not found at $BIN (set ORTHOGONALS_BIN)"
	[ -x "$FIXTURE_BIN" ] || fail "fixture builder not found at $FIXTURE_BIN"
fi

# Coverage from the real binary, collected into the directory tmt pulls back
# off the guest. Harmless when the binary was not built with -cover.
if [ -n "${TMT_TEST_DATA:-}" ]; then
	export GOCOVERDIR="$TMT_TEST_DATA/coverage"
	mkdir -p "$GOCOVERDIR"
fi

# Fake vendor tools, for the tests that must not mutate the machine they run
# on. libvirt and systemd are deliberately not faked: the binary speaks their
# APIs and under --root those steps journal and skip.
#
# A test that exists to exercise the REAL tools sets ORTHOGONALS_REAL_TOOLS=1
# before sourcing this file: a fake dracut would pass while asserting nothing.
FAKEBIN=$WORK/fakebin
mkdir -p "$FAKEBIN"
for name in dracut semanage restorecon bash nvidia-smi usermod systemctl; do
	printf '#!/bin/sh\necho "$*" >> "%s/%s.log"\nexit 0\n' "$FAKEBIN" "$name" >"$FAKEBIN/$name"
	chmod 0755 "$FAKEBIN/$name"
done

if [ "${ORTHOGONALS_REAL_TOOLS:-0}" = 1 ]; then
	RUN_PATH=$PATH
else
	RUN_PATH=$FAKEBIN:$PATH
fi

# run scopes RUN_PATH to the binary, never to this script — bash itself is one
# of the stubs.
run() { # expected-rc args...
	local want=$1 rc=0
	shift
	env PATH="$RUN_PATH" "$BIN" "$@" >"$WORK/out" 2>&1 || rc=$?
	[ "$rc" = "$want" ] || {
		sed 's/^/  | /' "$WORK/out" >&2
		fail "orthogonals $* exited $rc, want $want"
	}
}

# run_any accepts any exit status and returns it, for assertions that only care
# that the command refused.
run_any() { # args... -> echoes rc
	local rc=0
	env PATH="$RUN_PATH" "$BIN" "$@" >"$WORK/out" 2>&1 || rc=$?
	echo "$rc"
}

tree_state() { # dir -> path/mode/type listing on stdout
	(cd "$1" && find . -exec stat -c '%n %a %F' {} + | sort)
}

prep_root() { # kind dest — build a fixture plus the dirs every real host has
	local kind=$1 dest=$2
	"$FIXTURE_BIN" "$dest" "$kind" || fail "cannot build the $kind fixture"
	[ -e "$dest/sys/bus/pci/devices" ] || fail "$kind fixture tree looks empty"
	mkdir -p "$dest/etc" "$dest/var/lib" "$dest/usr/local/bin" "$dest/usr/share/applications"
}

snapshot_into() { # dir label — record a tree for a later assert_restored
	cp -a "$1" "$WORK/$2-pre"
	tree_state "$1" >"$WORK/$2-pre.state"
}

assert_restored() { # dir label context
	local dir=$1 label=$2 context=$3
	diff -r --no-dereference "$WORK/$label-pre" "$dir" ||
		fail "$context: tree differs from the pre-apply snapshot"
	tree_state "$dir" >"$WORK/$label-post.state"
	diff -u "$WORK/$label-pre.state" "$WORK/$label-post.state" ||
		fail "$context: file modes or types differ from the pre-apply snapshot"
}

manifest_records() { # root -> count
	python3 -c 'import json,sys; print(len(json.load(open(sys.argv[1]))["records"]))' \
		"$1/var/lib/orthogonals/manifest.json"
}

# pipeline_state reads the `up` state machine's persisted stage.
pipeline_state() { python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["state"])' "$STATE"; }

# goss_check asserts host state against a gossfile, parameterized on the run's
# desktop user and VM name.
goss_check() { # gossfile context
	local file=$1 context=$2
	[ -x "$GOSS" ] || fail "goss not installed at $GOSS"
	printf 'user: %s\nvm: %s\n' "$USER_NAME" "$VM_NAME" >"$WORK/vars.yaml"
	"$GOSS" --vars "$WORK/vars.yaml" -g "$TREE/test/goss/$file" validate --format documentation ||
		fail "$context: host state does not match $file"
}

# go_tier drives a Go test package under one contract: every
# named test must report PASS. That subsumes the trap a bare `go test` hides — a
# skip, or a build tag that compiled nothing, still exits 0 — since a skipped
# test did not pass. Coverage from these privileged paths reaches the merged
# figure through -test.gocoverdir.
go_tier() { # label go-test-arg... -- required-test-name...
	local label=$1 args=()
	shift
	while [ $# -gt 0 ] && [ "$1" != -- ]; do
		args+=("$1")
		shift
	done
	shift # drop the --
	(cd "$TREE" && go test "${args[@]}" -coverpkg=./internal/... -count=1 -v \
		-args -test.gocoverdir="${GOCOVERDIR:-$(mktemp -d)}") 2>&1 | tee "$WORK/$label.log"
	local t
	for t in "$@"; do
		grep -q "^--- PASS: $t" "$WORK/$label.log" ||
			fail "$t did not PASS — the $label tier exists to run it"
	done
}
