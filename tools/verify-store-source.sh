#!/usr/bin/env bash
# make verify-store-source (DESIGN §A3.0 durability bullet, §C8; IMPLEMENTATION_PLAN S1
# step 7). Fetches the store's own src/config.h and src/aof.c from github.com/valkey-io/valkey
# at the installed version (plan choice: the pin is the installed version) into .out/, and
# checks that on macOS appendfsync always ends in a full sync:
#   config.h: #define valkey_fsync(fd) fcntl(fd, F_FULLFSYNC) in the __APPLE__ branch;
#   aof.c:    a valkey_fsync(server.aof_fd) call on the AOF_FSYNC_ALWAYS path.
# Prints the matched line numbers (DESIGN cites 132 and 1371; information only). Needs the
# network, so it is not part of slice-check (plan choice). Run it when the pin changes.
#
# With a directory argument it checks DIR/config.h and DIR/aof.c instead, without the
# binary or the network: the way to try the matchers on altered copies.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"

if [[ $# -gt 0 ]]; then
	out="$1"
	label="$out/"
else
	# The binary the tests use (DESIGN §C1): RL_STORE_BIN, else valkey-server.
	bin="${RL_STORE_BIN:-}"
	if [[ -z "$bin" ]]; then
		bin="$(command -v valkey-server || true)"
	fi
	if [[ -z "$bin" ]]; then
		echo "verify-store-source: no valkey-server on PATH (make setup)" >&2
		exit 1
	fi
	line="$("$bin" --version)"
	version="$(sed -n 's/^Valkey server v=\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\) .*/\1/p' <<<"$line")"
	if [[ -z "$version" ]]; then
		echo "verify-store-source: $bin is not a Valkey server: $line" >&2
		exit 1
	fi

	out="$root/.out/store-source/$version"
	label="Valkey $version src/"
	mkdir -p "$out"
	base="https://raw.githubusercontent.com/valkey-io/valkey/$version/src"
	for f in config.h aof.c; do
		curl -fsSL --retry 2 --max-time 60 -o "$out/$f" "$base/$f"
	done
fi

# The define inside a preprocessor branch whose condition names __APPLE__.
config_line="$(awk '
	/^[ \t]*#[ \t]*if/ {
		depth++
		if ($0 ~ /__APPLE__/) apple = depth
		next
	}
	/^[ \t]*#[ \t]*elif/ {
		apple = ($0 ~ /__APPLE__/) ? depth : (apple == depth ? 0 : apple)
		next
	}
	/^[ \t]*#[ \t]*else/ {
		if (apple == depth) apple = 0
		next
	}
	/^[ \t]*#[ \t]*endif/ {
		if (apple == depth) apple = 0
		depth--
		next
	}
	apple && /^[ \t]*#[ \t]*define[ \t]+valkey_fsync\(fd\)[ \t]+fcntl\(fd,[ \t]*F_FULLFSYNC\)/ {
		print NR ": " $0
		exit
	}
' "$out/config.h")"

# The call inside the body of a block that an AOF_FSYNC_ALWAYS condition opens, and only
# there: brace depth counts from the condition's "{". A line that starts with "}" counts
# its closes before its opens, so "} else if (...) {" or "} else {" ends the ALWAYS
# branch instead of carrying the search into the next branch of the chain (such a line
# may open another ALWAYS block, so the start rule still sees it).
aof_line="$(awk '
	inside {
		line = $0
		opens = gsub(/\{/, "{", line)
		closes = gsub(/\}/, "}", line)
		if ($0 ~ /^[ \t]*\}/ && depth - closes <= 0) {
			inside = 0
		} else {
			if ($0 ~ /valkey_fsync\(server\.aof_fd\)/) {
				print NR ": " $0
				exit
			}
			depth += opens - closes
			if (depth <= 0) inside = 0
			next
		}
	}
	match($0, /aof_fsync[ \t]*==[ \t]*AOF_FSYNC_ALWAYS/) {
		rest = substr($0, RSTART + RLENGTH)
		opens = gsub(/\{/, "{", rest)
		closes = gsub(/\}/, "}", rest)
		if (opens == 0) next
		if (rest ~ /valkey_fsync\(server\.aof_fd\)/) {
			print NR ": " $0
			exit
		}
		depth = opens - closes
		inside = depth > 0
	}
' "$out/aof.c")"

status=0
if [[ -n "$config_line" ]]; then
	echo "verify-store-source: ${label}config.h:$config_line"
else
	echo "verify-store-source: ${label}config.h: no '#define valkey_fsync(fd) fcntl(fd, F_FULLFSYNC)' in an __APPLE__ branch" >&2
	status=1
fi
if [[ -n "$aof_line" ]]; then
	echo "verify-store-source: ${label}aof.c:$aof_line"
else
	echo "verify-store-source: ${label}aof.c: no valkey_fsync(server.aof_fd) call on the AOF_FSYNC_ALWAYS path" >&2
	status=1
fi
exit "$status"
