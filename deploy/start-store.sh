#!/bin/bash
# Starts the limiter store (Valkey) with a chosen store profile (docs/operator-guide.md
# "Store profiles"). Every argument after the profile goes to valkey-server unchanged, so pass
# --port, --dir and --aclfile as the profile files leave them out.
#
# Usage: deploy/start-store.sh [--profile durable|everysec|barrier|buffered] --port N --dir D --aclfile F
#   durable   appendfsync always (default; the only profile SPEC STA-16 calls durable)
#   everysec  appendfsync everysec: a power cut or OS crash can lose ~1-2 s of decisions
#   barrier   appendfsync always with F_BARRIERFSYNC (macOS; needs deploy/build-valkey-barrier.sh):
#             a power cut can lose the last few writes
#   buffered  appendfsync no: a power cut or OS crash can lose ~30 s or more
# The profile can also come from RL_STORE_PROFILE; --profile wins.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
profile="${RL_STORE_PROFILE:-durable}"
if [[ "${1:-}" == "--profile" ]]; then
	[[ $# -ge 2 ]] || { echo "start-store: --profile needs a value" >&2; exit 2; }
	profile="$2"
	shift 2
fi

server=valkey-server
case "$profile" in
durable | everysec | buffered) ;;
barrier)
	server="$root/.data/valkey-barrier/valkey-server"
	[[ -x "$server" ]] || { echo "start-store: $server is missing; run deploy/build-valkey-barrier.sh" >&2; exit 2; }
	;;
*)
	echo "start-store: unknown profile \"$profile\" (want durable, everysec, barrier or buffered)" >&2
	exit 2
	;;
esac

exec "$server" "$root/deploy/valkey-$profile.conf" "$@"
