#!/usr/bin/env bash
# written by orthogonals: build the Looking Glass client from the pinned
# source tarball and install it at /usr/local/bin/looking-glass-client.
# Run by the journaled lg-client-build apply step; undo removes the binary.
set -euo pipefail

URL="https://looking-glass.io/artifact/B7/source"
SHA256="09e506660ccc1b9691d06caa70179b52ffb4393299895cff3c2f0e74fcd69985"
TARBALL="/var/lib/orthogonals/cache/looking-glass-B7.tar.gz"
BUILD="/var/lib/orthogonals/build/looking-glass-B7"
BIN="/usr/local/bin/looking-glass-client"

if [[ ! -f "$TARBALL" ]]; then
    mkdir -p "$(dirname "$TARBALL")"
    curl -fsSL -o "$TARBALL.part" "$URL"
    # verify before promoting: a truncated/corrupt download must never land at
    # the cache path, or every later run would re-verify the same bad bytes and
    # fail forever (the .part is re-fetched on the next run instead)
    echo "$SHA256  $TARBALL.part" | sha256sum --check --quiet -
    mv "$TARBALL.part" "$TARBALL"
fi
# pin check applies to the cached file too — a mismatch is a hard fail,
# never a silent re-fetch
echo "$SHA256  $TARBALL" | sha256sum --check --quiet -

rm -rf "$BUILD"
mkdir -p "$BUILD"
tar -xzf "$TARBALL" -C "$BUILD" --strip-components=1

# a pinned tarball must outlive compiler updates: strip -Werror, or every
# new GCC warning class breaks the build (no ENABLE_WERROR toggle in B7)
sed -i '/^  "-Werror"$/d' "$BUILD/client/CMakeLists.txt"
if grep -qF '"-Werror"' "$BUILD/client/CMakeLists.txt"; then
    echo "error: -Werror survived the strip — the pin changed shape, update lg-build.sh" >&2
    exit 1
fi

# ENABLE_BACKTRACE=OFF: Fedora's static libbfd.a wants ZSTD symbols the
# link line does not provide (PoC incident 4)
cmake -S "$BUILD/client" -B "$BUILD/client/build" \
    -DCMAKE_BUILD_TYPE=Release -DENABLE_BACKTRACE=OFF
make -C "$BUILD/client/build" -j"$(nproc)"
install -m 0755 "$BUILD/client/build/looking-glass-client" "$BIN"

# PoC incident 7: runtime libs of a hand-built binary can arrive as devel
# dependencies and vanish in a later cleanup — refuse an install that does
# not fully resolve
if ldd "$BIN" | grep -q "not found"; then
    echo "error: $BIN has unresolved shared libraries:" >&2
    ldd "$BIN" | grep "not found" >&2
    exit 1
fi

rm -rf "$BUILD"
echo "installed $BIN (Looking Glass B7)"
