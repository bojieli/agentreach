#!/usr/bin/env bash
# Measure what each file-operation tier actually costs.
#
# The numbers in docs/TRANSPORTS.md come from this script. They exist because
# the tier ordering that looks obvious on paper — richer protocol, faster tier —
# is wrong for waldo: every tool call is a new process, so an interpreter or a
# binary starting up is pure overhead that batching never gets to amortise.
# Anyone changing the negotiation order should re-run this rather than reason
# about it.
#
# waldo is driven through its CLI on purpose, one process per operation, because
# that is exactly how a harness drives it. A library-level benchmark would
# measure a shape waldo does not have.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
ROOT="$PWD"
BENCH_DIR="${WALDO_BENCH_DIR:-/tmp/waldo-bench}"
PORT="${WALDO_BENCH_SSH_PORT:-22433}"

info() { printf '\033[1m%s\033[0m\n' "$1"; }
die()  { printf '\033[31m%s\033[0m\n' "$1" >&2; exit 1; }

command -v go >/dev/null || die "go is required"
go build -o "$ROOT/waldo" ./cmd/waldo || die "build failed"

# ---------------------------------------------------------------- test target
# A user-owned sshd, so this runs anywhere sshd is installed and needs neither
# root nor a container runtime nor a network.
find_one() { for p in "$@"; do [[ -x "$p" ]] && { echo "$p"; return 0; }; done; return 1; }

SSHD=$(find_one /usr/sbin/sshd /usr/local/sbin/sshd /opt/homebrew/sbin/sshd) \
  || die "no sshd found"
SFTP_SERVER=$(find_one /usr/libexec/sftp-server /usr/lib/openssh/sftp-server \
  /usr/libexec/openssh/sftp-server /usr/lib/ssh/sftp-server) \
  || die "no sftp-server found"

rm -rf "$BENCH_DIR"; mkdir -p "$BENCH_DIR/workspace"
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
Subsystem sftp $SFTP_SERVER
EOF

cat > "$BENCH_DIR/ssh_config" <<EOF
Host waldo-bench
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
cleanup() {
  "$ROOT/waldo" agent uninstall --session bench >/dev/null 2>&1
  kill "$SSHD_PID" 2>/dev/null
}
trap cleanup EXIT

export WALDO_SSH_CONFIG="$BENCH_DIR/ssh_config"
export WALDO_HOME="$BENCH_DIR/home"

for _ in $(seq 40); do
  ssh -F "$WALDO_SSH_CONFIG" waldo-bench true 2>/dev/null && break
  sleep 0.25
done
ssh -F "$WALDO_SSH_CONFIG" waldo-bench true 2>/dev/null \
  || die "sshd did not come up: $(cat "$BENCH_DIR/sshd.log" 2>/dev/null)"

WS="$BENCH_DIR/workspace"
head -c 1024 /dev/urandom > "$WS/small.bin"
head -c 20971520 /dev/urandom > "$WS/large.bin"

now() { python3 -c 'import time;print(time.time())'; }
since() { python3 -c "import time;print('%.2f'%(time.time()-$1))"; }

SMALL_READS="${WALDO_BENCH_SMALL_READS:-40}"

info "waldo tier benchmark — one process per operation, warm ControlMaster"
printf '\n%-8s %18s %14s %14s\n' tier "${SMALL_READS} x 1KiB read" "20MiB read" "20MiB write"

for tier in posix sftp pipe agent; do
  if ! "$ROOT/waldo" up "ssh://waldo-bench$WS" --fileops="$tier" --name bench >/dev/null 2>&1; then
    printf '%-8s %18s\n' "$tier" "unavailable"
    continue
  fi

  t=$(now)
  for _ in $(seq "$SMALL_READS"); do
    "$ROOT/waldo" fs read "$WS/small.bin" --session bench >/dev/null
  done
  small=$(since "$t")

  t=$(now)
  "$ROOT/waldo" fs read "$WS/large.bin" --session bench > "$BENCH_DIR/read.$tier"
  bigread=$(since "$t")
  cmp -s "$WS/large.bin" "$BENCH_DIR/read.$tier" \
    || printf '  !! %s corrupted the large read\n' "$tier"

  t=$(now)
  "$ROOT/waldo" fs write "$WS/w.$tier" --session bench < "$WS/large.bin"
  bigwrite=$(since "$t")
  cmp -s "$WS/large.bin" "$WS/w.$tier" \
    || printf '  !! %s corrupted the large write\n' "$tier"

  printf '%-8s %17ss %13ss %13ss\n' "$tier" "$small" "$bigread" "$bigwrite"
done

printf '\nOver loopback, so round trips are free. A real link penalises tier 0\n'
printf 'further: its per-chunk round trips are sequential and its payloads are\n'
printf '33%% larger, while sftp pipelines eight reads deep.\n'
