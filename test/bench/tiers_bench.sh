#!/usr/bin/env bash
# Measure what each file-operation tier actually costs.
#
# The numbers in docs/TRANSPORTS.md come from this script. They exist because
# the tier ordering that looks obvious on paper — richer protocol, faster tier —
# is wrong for reach: every tool call is a new process, so an interpreter or a
# binary starting up is pure overhead that batching never gets to amortise.
# Anyone changing the negotiation order should re-run this rather than reason
# about it.
#
# reach is driven through its CLI on purpose, one process per operation, because
# that is exactly how a harness drives it. A library-level benchmark would
# measure a shape reach does not have.
#
# By default it measures against an sshd on loopback, which needs nothing and
# measures CPU. Point it at a real host to measure what actually varies:
#
#   REACH_BENCH_SSH_HOST=my-box make bench
#
# It creates one directory there and removes it, along with anything the agent
# helper tier installed, when it exits.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="$PWD"
BENCH_DIR="${REACH_BENCH_DIR:-/tmp/reach-bench}"
PORT="${REACH_BENCH_SSH_PORT:-22433}"
LARGE_MB="${REACH_BENCH_LARGE_MB:-20}"
SMALL_READS="${REACH_BENCH_SMALL_READS:-20}"

info() { printf '\033[1m%s\033[0m\n' "$1"; }
die()  { printf '\033[31m%s\033[0m\n' "$1" >&2; exit 1; }
now()  { python3 -c 'import time;print(time.time())'; }
since() { python3 -c "import time;print('%.2f'%(time.time()-$1))"; }

command -v go >/dev/null || die "go is required"
go build -o "$ROOT/reach" ./cmd/reach || die "build failed"

rm -rf "$BENCH_DIR"; mkdir -p "$BENCH_DIR"
export REACH_HOME="$BENCH_DIR/home"

# on_target runs a command where the files live, so fixtures are created the
# same way in both modes.
if [[ -n "${REACH_BENCH_SSH_HOST:-}" ]]; then
  # ---------------------------------------------------- a host you already have
  HOST="$REACH_BENCH_SSH_HOST"
  WS="${REACH_BENCH_WORKSPACE:-/tmp}/reach-bench-$$"
  LABEL="$HOST"
  ssh "$HOST" "mkdir -p '$WS'" || die "cannot prepare $WS on $HOST"
  on_target() { ssh "$HOST" "$1"; }
  cleanup() {
    "$ROOT/reach" helper uninstall --session bench >/dev/null 2>&1
    ssh "$HOST" "rm -rf '$WS'" >/dev/null 2>&1
  }
else
  # ------------------------------------------------- an sshd of our own, local
  find_one() { for p in "$@"; do [[ -x "$p" ]] && { echo "$p"; return 0; }; done; return 1; }
  SSHD=$(find_one /usr/sbin/sshd /usr/local/sbin/sshd /opt/homebrew/sbin/sshd) \
    || die "no sshd found"
  mkdir -p "$BENCH_DIR/workspace"
  ssh-keygen -q -t ed25519 -N '' -f "$BENCH_DIR/id"
  ssh-keygen -q -t ed25519 -N '' -f "$BENCH_DIR/hostkey"
  cp "$BENCH_DIR/id.pub" "$BENCH_DIR/authorized_keys"
  chmod 600 "$BENCH_DIR/authorized_keys" "$BENCH_DIR/hostkey"

  cat > "$BENCH_DIR/sshd_config" <<EOF
Port $PORT
ListenAddress 127.0.0.1
HostKey $BENCH_DIR/hostkey
PidFile $BENCH_DIR/sshd.pid
AuthorizedKeysFile $BENCH_DIR/authorized_keys
StrictModes no
UsePAM no
PasswordAuthentication no
LogLevel ERROR
EOF
  cat > "$BENCH_DIR/ssh_config" <<EOF
Host reach-bench
  HostName 127.0.0.1
  Port $PORT
  User ${USER:-$(id -un)}
  IdentityFile $BENCH_DIR/id
  IdentitiesOnly yes
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
EOF

  "$SSHD" -D -f "$BENCH_DIR/sshd_config" -E "$BENCH_DIR/sshd.log" &
  SSHD_PID=$!
  export REACH_SSH_CONFIG="$BENCH_DIR/ssh_config"
  HOST="reach-bench"
  WS="$BENCH_DIR/workspace"
  LABEL="loopback sshd"
  on_target() { bash -c "$1"; }
  cleanup() {
    "$ROOT/reach" helper uninstall --session bench >/dev/null 2>&1
    kill "$SSHD_PID" 2>/dev/null
  }

  for _ in $(seq 40); do
    ssh -F "$REACH_SSH_CONFIG" "$HOST" true 2>/dev/null && break
    sleep 0.25
  done
  ssh -F "$REACH_SSH_CONFIG" "$HOST" true 2>/dev/null \
    || die "sshd did not come up: $(cat "$BENCH_DIR/sshd.log" 2>/dev/null)"
fi
trap cleanup EXIT

on_target "head -c 1024 /dev/urandom > '$WS/small.bin'" || die "cannot write fixtures"
on_target "head -c $((LARGE_MB * 1024 * 1024)) /dev/urandom > '$WS/large.bin'" || die "cannot write fixtures"

info "reach tier benchmark against $LABEL — one process per operation"
printf '\n%-8s %18s %14s %14s\n' tier "${SMALL_READS} x 1KiB read" "${LARGE_MB}MiB read" "${LARGE_MB}MiB write"

for tier in posix pipe helper; do
  if ! "$ROOT/reach" up "ssh://$HOST$WS" --fileops="$tier" --name bench >/dev/null 2>&1; then
    printf '%-8s %18s\n' "$tier" "unavailable"
    continue
  fi

  t=$(now)
  for _ in $(seq "$SMALL_READS"); do
    "$ROOT/reach" fs read "$WS/small.bin" --session bench >/dev/null
  done
  small=$(since "$t")

  t=$(now)
  "$ROOT/reach" fs read "$WS/large.bin" --session bench > "$BENCH_DIR/read.$tier"
  bigread=$(since "$t")
  # A fast wrong answer is not a result. Every timing is checked for content.
  if [[ "$(wc -c < "$BENCH_DIR/read.$tier")" -ne $((LARGE_MB * 1024 * 1024)) ]]; then
    printf '  !! %s returned the wrong number of bytes\n' "$tier"
  fi

  t=$(now)
  "$ROOT/reach" fs write "$WS/w.$tier" --session bench < "$BENCH_DIR/read.$tier"
  bigwrite=$(since "$t")
  if ! on_target "cmp -s '$WS/large.bin' '$WS/w.$tier'"; then
    printf '  !! %s corrupted the large write\n' "$tier"
  fi
  on_target "rm -f '$WS/w.$tier'"
  rm -f "$BENCH_DIR/read.$tier"

  printf '%-8s %17ss %13ss %13ss\n' "$tier" "$small" "$bigread" "$bigwrite"
done

if [[ -z "${REACH_BENCH_SSH_HOST:-}" ]]; then
  printf '\nOver loopback, so round trips are free. Re-run with REACH_BENCH_SSH_HOST\n'
  printf 'against a real host to see what latency does to each tier.\n'
fi
