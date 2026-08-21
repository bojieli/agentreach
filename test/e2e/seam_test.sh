#!/usr/bin/env bash
# Harness seam tests: does each installed agent's shell tool really route
# through waldo to the remote target?
#
# Drives `waldo harness verify <harness> [--task-prefix CMD]` against a real
# SSH target (docker sshd).  The probe points the harness at an embedded mock
# model (no API key, no tokens), scripts one shell tool call, and checks WHERE
# it ran:
#
#   remote hostname -> shell is intercepted; waldo works
#   local hostname  -> harness resolved its shell by absolute path and bypassed
#                      waldo's PATH shim
#
# Three task variants are probed for each harness:
#   exec  — "echo <marker>; hostname"                      (shell execution)
#   ro    — "cat /srv/app/README.md && echo <marker>; hostname"   (file read)
#   rw    — "echo <marker> > /tmp/... && cat ... && rm ... ; hostname" (write+read)
#
# All three go through the same shell seam; probing all three confirms that
# reading and writing on the target work, not just running commands.
#
# Multiple installed versions of the same harness are probed in parallel.
# The WALDO_HARNESS_VERSIONS_DIR env var names a directory of the form:
#
#   <dir>/<harness>/<version>/bin/<harness>
#
# Every binary found there is probed alongside the PATH-resolved one.
#
# Known-broken versions:
#   - Codex >= 0.148: resolves login shell via getpwuid_r → absolute path
#   - Kimi Code 0.37.2: spawns shell by absolute path
set -uo pipefail
cd "$(dirname "$0")" && source ./lib.sh

WALDO_BIN="${WALDO_BIN:-$(cd ../.. && pwd)/waldo}"
SESSION="seam-test"
VERSIONS_DIR="${WALDO_HARNESS_VERSIONS_DIR:-}"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not installed — skipping harness seam tests"; exit 0
fi
if [[ ! -x "$WALDO_BIN" ]]; then
  (cd ../.. && make build) || { echo "could not build waldo" >&2; exit 1; }
fi

info "Setting up remote target (docker sshd)"
if ! setup_target; then echo "could not start test target" >&2; exit 1; fi
cleanup() {
  "$WALDO_BIN" down "$SESSION" >/dev/null 2>&1 || true
  teardown_target
}
trap cleanup EXIT

"$WALDO_BIN" up ssh://waldo-e2e/srv/app --name "$SESSION" >/dev/null 2>&1 \
  || { echo "waldo up failed" >&2; exit 1; }
REMOTE_HOST="$("$WALDO_BIN" exec --session "$SESSION" -- hostname)"
LOCAL_HOST="$(hostname)"
info "Target is up (local=$LOCAL_HOST remote=$REMOTE_HOST)"
if [[ "$LOCAL_HOST" == "$REMOTE_HOST" ]]; then
  echo "local and remote hostnames match; test cannot distinguish them" >&2; exit 1
fi

RESULT_DIR="$WALDO_E2E_DIR/seam-results"
rm -rf "$RESULT_DIR"
mkdir -p "$RESULT_DIR"

# probe HARNESS TASK VERSION BINARY_PATH
#
# Runs one probe and writes a result file to RESULT_DIR.
# TASK: exec | ro | rw
# VERSION: label for display (e.g. "0.148.0" or "PATH")
# BINARY_PATH: path to the harness binary (empty = PATH lookup)
probe() {
  local harness="$1" task="$2" version="$3" binary="${4:-}"
  local key="${harness}_${version}_${task}"
  local log="$RESULT_DIR/${key}.log"
  local out_file="$RESULT_DIR/${key}.result"

  local prefix_args=()
  local task_label
  case "$task" in
    exec)
      task_label="exec (echo+hostname)"
      ;;
    ro)
      task_label="read-only (cat remote file)"
      prefix_args=(--task-prefix "cat /srv/app/README.md")
      ;;
    rw)
      task_label="read-write (write+read remote file)"
      local tmp_path="/tmp/waldo-probe-rw-$RANDOM.txt"
      prefix_args=(--task-prefix \
        "echo waldo_probe_write > $tmp_path && cat $tmp_path && rm -f $tmp_path")
      ;;
    *)
      echo "unknown task $task" >&2; echo "error skip skip" > "$out_file"; return 0
      ;;
  esac

  local bin_args=()
  [[ -n "$binary" ]] && bin_args=(--binary "$binary")

  local out rc=0
  out="$(timeout 240 "$WALDO_BIN" harness verify "$harness" \
      --session "$SESSION" "${prefix_args[@]}" "${bin_args[@]}" 2>&1)" || rc=$?

  local verdict="error"
  if   [[ "$out" == *"verdict: ok"*       ]]; then verdict="ok"
  elif [[ "$out" == *"verdict: BYPASSED"* ]]; then verdict="bypassed"
  fi

  echo "$verdict $harness $version $task $task_label" > "$out_file"
  printf '%s\n' "$out" > "$log"
  return 0
}

# Enumerate binaries to probe for a harness:
#   1. The PATH-resolved binary (if installed)
#   2. Any versioned binaries under VERSIONS_DIR/<harness>/*/bin/<harness>
# Each entry is "version:path" (version="" means PATH-resolved).
harness_binaries() {
  local harness="$1"
  if command -v "$harness" >/dev/null 2>&1; then
    echo "PATH:"
  fi
  if [[ -n "$VERSIONS_DIR" && -d "$VERSIONS_DIR/$harness" ]]; then
    for vdir in "$VERSIONS_DIR/$harness"/*/; do
      local ver
      ver="$(basename "$vdir")"
      local bin="$vdir/bin/$harness"
      [[ -x "$bin" ]] && echo "$ver:$bin"
    done
  fi
}

# Queue probes for all harnesses × all tasks × all versions in parallel.
declare -a PIDS=()
HARNESSES=(claude codex kimi goose gemini)
TASKS=(exec ro rw)

for harness in "${HARNESSES[@]}"; do
  mapfile -t BINARIES < <(harness_binaries "$harness")
  if [[ ${#BINARIES[@]} -eq 0 ]]; then
    info "$harness not installed — skipping"
    for task in "${TASKS[@]}"; do
      echo "skip $harness PATH $task (not installed)" \
        > "$RESULT_DIR/${harness}_PATH_${task}.result"
    done
    continue
  fi
  for entry in "${BINARIES[@]}"; do
    ver="${entry%%:*}"
    bin="${entry#*:}"
    [[ "$ver" == "PATH" ]] && label="$harness (PATH)" || label="$harness v$ver"
    info "$label: queuing exec/ro/rw probes"
    for task in "${TASKS[@]}"; do
      probe "$harness" "$task" "$ver" "$bin" &
      PIDS+=($!)
    done
  done
done

# Wait for all parallel probes.
for pid in "${PIDS[@]}"; do
  wait "$pid" || true
done

# Report results ordered by harness → version → task.
info "Results"
for harness in "${HARNESSES[@]}"; do
  mapfile -t BINARIES < <(harness_binaries "$harness" 2>/dev/null)
  if [[ ${#BINARIES[@]} -eq 0 ]]; then
    BINARIES=("PATH:")
  fi
  for entry in "${BINARIES[@]}"; do
    ver="${entry%%:*}"
    for task in "${TASKS[@]}"; do
      rf="$RESULT_DIR/${harness}_${ver}_${task}.result"
      lf="$RESULT_DIR/${harness}_${ver}_${task}.log"
      [[ ! -f "$rf" ]] && continue
      line="$(cat "$rf")"
      verdict="${line%% *}"
      rest="${line#* }"           # strip verdict
      rest="${rest#* * }"         # strip harness and version
      task_label="${rest#* }"     # strip task code, keep human label

      case "$verdict" in
        ok)
          ok "$harness v$ver [$task] ($task_label) — routes to target"
          ;;
        bypassed)
          bad "$harness v$ver [$task] ($task_label) — bypasses target" \
              "shell shim not intercepted; see $lf"
          ;;
        skip)
          printf '  \033[33mSKIP\033[0m %s\n' "$rest"
          ;;
        *)
          bad "$harness v$ver [$task] probe inconclusive" \
              "probe errored; see $lf"
          ;;
      esac
    done
  done
done

summary
