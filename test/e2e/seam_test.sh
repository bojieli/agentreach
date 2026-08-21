#!/usr/bin/env bash
# Harness seam tests: does each installed agent's shell tool really route
# through waldo to the remote target?
#
# This drives `waldo harness verify <harness>` — waldo's built-in, offline seam
# probe — against a real SSH target (docker sshd). The probe points the harness
# at an embedded mock model (no API key, no tokens), scripts one shell tool
# call that echoes a marker and the hostname, and checks WHERE it ran:
#
#   remote hostname -> the harness's shell is intercepted; waldo works
#   local hostname  -> the harness resolved its shell by absolute path and
#                      bypassed waldo's PATH shim entirely
#
# Known-broken versions (measured with this probe):
#   - Codex >= 0.148 resolves the login shell via getpwuid_r and spawns it by
#     absolute path (e.g. /bin/zsh -lc). Codex <= 0.147 used execvp("bash")
#     and is intercepted fine.
#   - Kimi Code 0.37.2 spawns its shell by absolute path as well.
#
# On those versions this test FAILS. That is the finding: a green run means the
# installed harness works with waldo; a red run means `waldo <harness>` would
# (without the launch guard) have let the agent operate on the wrong machine
# while believing — and reporting — otherwise. The guard refuses known-broken
# versions; this test is what tells you whether an upgrade changed anything.
set -uo pipefail
cd "$(dirname "$0")" && source ./lib.sh

WALDO_BIN="${WALDO_BIN:-$(cd ../.. && pwd)/waldo}"
SESSION="seam-test"

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

# The probe enforces a fresh measurement: WALDO_HOME was reset by setup_target,
# so no cached verdict can substitute for measuring this exact binary.
for harness in codex kimi; do
  if ! command -v "$harness" >/dev/null 2>&1; then
    info "$harness not installed — skipping"
    continue
  fi
  ver="$("$harness" --version 2>&1 | head -1)"
  info "$harness ($ver): probing the shell seam (offline mock model)"

  out="$(timeout 240 "$WALDO_BIN" harness verify "$harness" --session "$SESSION" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | sed 's/^/    /'

  assert_contains "$out" "verdict:" "$harness seam probe reached a verdict"
  if [[ "$out" == *"verdict: ok"* ]]; then
    ok "$harness shell commands route through waldo to the target"
  elif [[ "$out" == *"verdict: BYPASSED"* ]]; then
    bad "$harness shell commands route through waldo to the target" \
        "$harness $ver bypasses waldo's shell shim — commands would run LOCALLY
        while the agent believes it acts on the target. The launch guard now
        refuses this combination; re-run after a harness upgrade. See
        docs/harnesses/$harness.md."
  else
    bad "$harness seam probe is conclusive" \
        "probe errored instead of measuring (rc=$rc); the harness's wire
        protocol may have changed — the probe needs updating."
  fi
done

summary
