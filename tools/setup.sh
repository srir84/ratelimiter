#!/usr/bin/env bash
# make setup (DESIGN §C8; IMPLEMENTATION_PLAN S0 step 5).
#
# Checks Go, Python, PostgreSQL and the store binary; initializes the harness
# PostgreSQL cluster in .data/pg (TCP on 127.0.0.1:5433 only, no Unix socket; role
# rl, database hits, deploy/pg-schema.sql); installs the pinned lint tools. Safe to
# rerun: a second run skips initdb, the role, the database and every install.
#
# Setup always leaves PostgreSQL stopped, whatever the outcome: a server already
# running on .data/pg (say, one a failed gate left behind) is stopped before setup
# starts its own, and on every exit setup stops any server running on .data/pg.
#
# Written for macOS's /bin/bash 3.2: no associative arrays, no ${x,,}, no mapfile.
set -euo pipefail

# An exported CDPATH would send the relative cd below into another directory.
unset CDPATH
cd "$(dirname "$0")/.."

readonly GOLANGCI_VERSION=2.14.0
# SHA-256 of the release archive golangci-lint-2.14.0-darwin-arm64.tar.gz.
readonly GOLANGCI_SHA256=5ef5f36a7147e91dc58ef9ef4d11bb7bad5ead0c76eb6c01327a73c641d1dcc3
readonly GOLANGCI_BIN=bin/tools/golangci-lint
readonly PG_DIR=.data/pg
readonly PG_LOG=.data/pg.log
readonly PG_PORT=5433
# Setup appends this line, then its settings, to postgresql.conf right after initdb. initdb
# writes PG_VERSION early, so PG_VERSION without this line means an interrupted initdb or append.
readonly PG_MARKER='# Harness settings, appended once by tools/setup.sh after initdb.'

warnings=0
dl_dir=""

say() { printf 'setup: %s\n' "$*"; }
warn() {
  printf 'setup: WARNING: %s\n' "$*" >&2
  warnings=$((warnings + 1))
}
die() {
  printf 'setup: ERROR: %s\n' "$*" >&2
  exit 1
}

# On any exit, success or failure: stop any PostgreSQL server running on .data/pg (setup
# always leaves it stopped, and fails if it cannot) and remove the download directory.
cleanup() {
  local status=$?
  if command -v pg_ctl >/dev/null 2>&1 && pg_ctl -D "$PG_DIR" status >/dev/null 2>&1; then
    if ! pg_ctl -D "$PG_DIR" -w stop >/dev/null 2>&1; then
      printf 'setup: ERROR: could not stop PostgreSQL; run: pg_ctl -D %s -w stop\n' "$PG_DIR" >&2
      if [ "$status" = 0 ]; then
        status=1
      fi
    fi
  fi
  if [ -n "$dl_dir" ]; then
    rm -rf "$dl_dir"
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ge MAJOR MINOR WANT_MAJOR WANT_MINOR: true when MAJOR.MINOR >= WANT_MAJOR.WANT_MINOR.
ge() {
  if (($1 != $3)); then
    (($1 > $3))
  else
    (($2 >= $4))
  fi
}

# offer_brew CMD REASON: a Homebrew install asks first on a TTY; otherwise it prints
# the command and setup continues with a warning (DESIGN §C8). True when CMD ran and succeeded.
offer_brew() {
  local cmd=$1 reason=$2 answer=""
  if [ -t 0 ] && [ -t 1 ]; then
    printf 'setup: %s\nsetup: run "%s" now? [y/N] ' "$reason" "$cmd"
    read -r answer || answer=""
    case $answer in
      y | Y | yes | Yes | YES)
        if eval "$cmd"; then
          return 0
        fi
        warn "\"$cmd\" failed"
        return 1
        ;;
    esac
  fi
  warn "$reason; to install, run: $cmd"
  return 1
}

# --- Go, Python, PostgreSQL ---------------------------------------------------

# Run in the repo, so go.mod's toolchain line applies (go1.27.1, downloaded on first use).
command -v go >/dev/null 2>&1 || die "go not found; install Go 1.24 or later"
go_line=$(go version) || die "go version failed (the first run downloads the go1.27.1 toolchain)"
re='go([0-9]+)\.([0-9]+)'
[[ $go_line =~ $re ]] || die "cannot parse the Go version from: $go_line"
ge "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" 1 24 || die "Go 1.24 or later required; got: $go_line"
say "$go_line"

command -v python3 >/dev/null 2>&1 || die "python3 not found; install Python 3.13 or later"
py_ver=$(python3 -c 'import sys; print("%d.%d.%d" % sys.version_info[:3])') || die "python3 failed to run"
re='^([0-9]+)\.([0-9]+)\.'
[[ $py_ver =~ $re ]] || die "cannot parse the Python version: $py_ver"
ge "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" 3 13 || die "Python 3.13 or later required; got $py_ver"
say "Python $py_ver"

for tool in pg_ctl initdb psql; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool not found; install PostgreSQL 14 or later"
done
re='\(PostgreSQL\) ([0-9]+)'
pg_line=$(pg_ctl --version) || die "pg_ctl --version failed"
[[ $pg_line =~ $re ]] || die "cannot parse the PostgreSQL version from: $pg_line"
pg_major=${BASH_REMATCH[1]}
((pg_major >= 14)) || die "PostgreSQL 14 or later required; got: $pg_line"
initdb_line=$(initdb --version) || die "initdb --version failed"
if ! [[ $initdb_line =~ $re ]] || [ "${BASH_REMATCH[1]}" != "$pg_major" ]; then
  die "initdb and pg_ctl come from different PostgreSQL versions: $initdb_line; $pg_line"
fi
say "$pg_line"

# --- Store binary (DESIGN §C1 order) ------------------------------------------

store_bin=""
store_line=""
find_store() {
  store_bin=""
  store_line=""
  if [ -n "${RL_STORE_BIN:-}" ]; then
    store_bin=$RL_STORE_BIN
  elif command -v valkey-server >/dev/null 2>&1; then
    store_bin=valkey-server
  elif command -v redis-server >/dev/null 2>&1; then
    store_bin=redis-server
  else
    return 1
  fi
  store_line=$("$store_bin" --version 2>/dev/null) || return 1
  local valkey_91='^Valkey server v=9\.1\.[0-9]+( |$)'
  [[ $store_line =~ $valkey_91 ]]
}
if find_store; then
  say "store: $store_bin: ${store_line%% sha=*}"
else
  if [ -n "$store_bin" ]; then
    reason="store binary $store_bin is not Valkey 9.1.x (${store_line:-no version output})"
  else
    reason="no store binary found (RL_STORE_BIN, valkey-server, redis-server)"
  fi
  if offer_brew "brew unlink redis && brew install valkey" "$reason"; then
    if find_store; then
      say "store: $store_bin: ${store_line%% sha=*}"
    else
      warn "after the install the store binary is still not Valkey 9.1.x: ${store_line:-none}"
    fi
  fi
fi

# --- Harness PostgreSQL cluster (.data/pg, port 5433) --------------------------

# The six DESIGN §C8 settings. The last turns off the Unix socket (owner decision
# 2026-10-04): every client connects over TCP to 127.0.0.1:5433, so the server writes
# nothing to /tmp.
pg_settings=(
  "max_connections = 300"
  "port = $PG_PORT"
  "listen_addresses = '127.0.0.1'"
  "fsync = on"
  "synchronous_commit = on"
  "unix_socket_directories = ''"
)
if [ -f "$PG_DIR/PG_VERSION" ]; then
  # Never removed automatically: the owner decides what happens to the directory.
  grep -qxF "$PG_MARKER" "$PG_DIR/postgresql.conf" 2>/dev/null ||
    die "the PostgreSQL cluster in $PG_DIR is incomplete: an earlier initdb or setup run was" \
      "interrupted before setup's settings were appended to postgresql.conf;" \
      "remove $PG_DIR (rm -rf $PG_DIR) and rerun make setup"
  say "PostgreSQL cluster $PG_DIR exists; skipping initdb"
else
  say "initializing the PostgreSQL cluster in $PG_DIR"
  initdb -D "$PG_DIR" -U postgres --auth=trust -E UTF8
  # Appended only right after initdb, so reruns add nothing; the marker goes first.
  {
    printf '\n%s\n' "$PG_MARKER"
    printf '%s\n' "${pg_settings[@]}"
  } >>"$PG_DIR/postgresql.conf"
fi
for setting in "${pg_settings[@]}"; do
  grep -qxF "$setting" "$PG_DIR/postgresql.conf" ||
    die "$PG_DIR/postgresql.conf lacks the line \"$setting\", which setup appends after initdb;" \
      "restore it, or remove $PG_DIR and rerun make setup"
done

# psql_as USER DB ARGS...: psql over loopback TCP, ignoring ~/.psqlrc, stopping on the first error.
psql_as() {
  local user=$1 db=$2
  shift 2
  psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$PG_PORT" -U "$user" -d "$db" "$@"
}

# Setup always leaves PostgreSQL stopped (the orchestrator starts it when a run needs it,
# DESIGN §B5). A server already running on .data/pg is a leftover, for example from a
# failed gate, so setup stops it and starts its own with the settings above.
if pg_ctl -D "$PG_DIR" status >/dev/null 2>&1; then
  say "PostgreSQL is already running on $PG_DIR; stopping it first (setup leaves it stopped)"
  pg_ctl -D "$PG_DIR" -w stop
fi
pg_ctl -D "$PG_DIR" -l "$PG_LOG" -w start

has_role=$(psql_as postgres postgres -tAc "SELECT 1 FROM pg_roles WHERE rolname = 'rl'")
if [ "$has_role" = 1 ]; then
  say "role rl exists; skipping"
else
  psql_as postgres postgres -c 'CREATE ROLE rl LOGIN'
  say "created role rl"
fi
has_db=$(psql_as postgres postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'hits'")
if [ "$has_db" = 1 ]; then
  say "database hits exists; skipping"
else
  psql_as postgres postgres -c 'CREATE DATABASE hits OWNER rl'
  say "created database hits (owner rl)"
fi
psql_as rl hits -f deploy/pg-schema.sql
say "applied deploy/pg-schema.sql as rl"

# Always stopped here, whoever started it (see above); the exit trap covers failures.
pg_ctl -D "$PG_DIR" -w stop

# --- Lint tools (AGENTS.md) -----------------------------------------------------

golangci_ok() {
  local out
  [ -x "$GOLANGCI_BIN" ] || return 1
  out=$("$GOLANGCI_BIN" version 2>/dev/null) || return 1
  # "golangci-lint has version 2.14.0 built with go1.27.0 from ..."
  [[ $out == *"has version $GOLANGCI_VERSION "* || $out == *"has version v$GOLANGCI_VERSION "* ]]
}
if golangci_ok; then
  say "golangci-lint $GOLANGCI_VERSION present in $GOLANGCI_BIN; skipping download"
else
  if [ "$(uname -s)-$(uname -m)" != Darwin-arm64 ]; then
    die "golangci-lint is pinned for darwin-arm64 only; this machine is $(uname -s)-$(uname -m)"
  fi
  asset="golangci-lint-$GOLANGCI_VERSION-darwin-arm64"
  url="https://github.com/golangci/golangci-lint/releases/download/v$GOLANGCI_VERSION/$asset.tar.gz"
  mkdir -p "$(dirname "$GOLANGCI_BIN")"
  dl_dir=$(mktemp -d "$(dirname "$GOLANGCI_BIN")/.download.XXXXXX")
  say "downloading $url"
  # Bounded, so a stalled download fails instead of hanging: 30 s to connect and at most
  # 10 minutes per attempt, with up to 3 retries.
  curl -fsSL --retry 3 --connect-timeout 30 --max-time 600 --proto '=https' \
    -o "$dl_dir/$asset.tar.gz" "$url"
  sum=$(shasum -a 256 "$dl_dir/$asset.tar.gz")
  sum=${sum%% *}
  [ "$sum" = "$GOLANGCI_SHA256" ] ||
    die "SHA-256 mismatch for $asset.tar.gz: got $sum, want $GOLANGCI_SHA256"
  tar -xzf "$dl_dir/$asset.tar.gz" -C "$dl_dir"
  chmod 0755 "$dl_dir/$asset/golangci-lint"
  mv -f "$dl_dir/$asset/golangci-lint" "$GOLANGCI_BIN"
  rm -rf "$dl_dir"
  dl_dir=""
  golangci_ok || die "$GOLANGCI_BIN does not report version $GOLANGCI_VERSION after the install"
  say "installed golangci-lint $GOLANGCI_VERSION into $GOLANGCI_BIN"
fi

lua_tools_ok() {
  local out
  out=$(luacheck --version 2>/dev/null) || return 1
  [[ $out == "Luacheck: 1.2."* ]] || return 1
  out=$(stylua --version 2>/dev/null) || return 1
  [[ $out == "stylua 2.5."* ]]
}
if lua_tools_ok; then
  say "luacheck 1.2.x and stylua 2.5.x present"
elif offer_brew "brew install luacheck stylua" "luacheck 1.2.x and stylua 2.5.x are required"; then
  lua_tools_ok || warn "after the install luacheck is not 1.2.x or stylua is not 2.5.x"
fi

# pin NAME: the version requirements-dev.txt pins for NAME (a NAME==VERSION line).
pin() { sed -n "s/^$1==\([^[:space:]#]*\).*/\1/p" requirements-dev.txt; }
for tool in pytest ruff mypy; do
  [ -n "$(pin "$tool")" ] || die "requirements-dev.txt has no $tool==<version> line"
done
dev_tools_ok() {
  local tool want out
  for tool in pytest ruff mypy; do
    want=$(pin "$tool")
    out=$(python3 -m "$tool" --version 2>/dev/null) || return 1
    # "pytest 9.0.2", "ruff 0.15.8", "mypy 2.1.0 (compiled: yes)"
    [[ $out == "$tool $want" || $out == "$tool $want "* ]] || return 1
  done
}
if dev_tools_ok; then
  say "pytest $(pin pytest), ruff $(pin ruff) and mypy $(pin mypy) match requirements-dev.txt"
else
  say "installing requirements-dev.txt"
  python3 -m pip install -r requirements-dev.txt
  dev_tools_ok || die "pytest, ruff or mypy still differ from requirements-dev.txt after pip install"
  say "pytest $(pin pytest), ruff $(pin ruff) and mypy $(pin mypy) installed"
fi

if ((warnings > 0)); then
  say "done, with $warnings warning(s) above"
else
  say "done"
fi
