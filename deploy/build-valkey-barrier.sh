#!/bin/bash
# Builds the Valkey binary for the barrier store profile (deploy/valkey-barrier.conf):
# Valkey 9.1.2, the same release as Homebrew's, with deploy/valkey-barrier.patch applied.
# The patch changes one call: with appendfsync always on macOS, each AOF write is synced with
# F_BARRIERFSYNC instead of F_FULLFSYNC. On Linux the patch changes nothing.
#
# Usage: deploy/build-valkey-barrier.sh
# Output: .data/valkey-barrier/valkey-server (the path deploy/start-store.sh and the
# orchestrator's --store-profile barrier use). Needs curl, shasum, patch, make and a C compiler.
set -euo pipefail

version=9.1.2
sha256=19c23908e7d57e8d91ef85b41f5646307582f10f4f0fb999bbf89ed24ec9c983 # Homebrew's checksum
url="https://github.com/valkey-io/valkey/archive/refs/tags/${version}.tar.gz"

root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/.data/valkey-barrier"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

curl -fsSL -o "$work/valkey.tar.gz" "$url"
echo "$sha256  $work/valkey.tar.gz" | shasum -a 256 -c -
tar -xzf "$work/valkey.tar.gz" -C "$work"
src="$work/valkey-$version"
patch -d "$src" -p1 <"$root/deploy/valkey-barrier.patch"
make -C "$src" -j "$(sysctl -n hw.ncpu 2>/dev/null || nproc)" valkey-server >"$work/build.log" 2>&1 ||
	{ tail -40 "$work/build.log" >&2; exit 1; }

mkdir -p "$out"
cp "$src/src/valkey-server" "$out/valkey-server"
"$out/valkey-server" --version
echo "built $out/valkey-server"
