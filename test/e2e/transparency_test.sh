#!/usr/bin/env bash
# End-to-end transparency tests: does a REAL coding agent, using its own native
# tools, actually operate on the remote target?
#
# These tests spend real model tokens. They are not run by `make test`; run them
# with `make e2e` when a harness is installed and authenticated.
set -uo pipefail
cd "$(dirname "$0")" && source ./lib.sh

WALDO_BIN="${WALDO_BIN:-$(cd ../.. && pwd)/waldo}"
MODEL="${WALDO_E2E_MODEL:-haiku}"

if [[ ! -x "$WALDO_BIN" ]]; then
  echo "waldo binary not found at $WALDO_BIN — run 'make build' first" >&2
  exit 1
fi

info "Setting up remote target (docker sshd)"
if ! setup_target; then echo "could not start test target" >&2; exit 1; fi
trap teardown_target EXIT

LOCAL_HOST="$(hostname)"
"$WALDO_BIN" up ssh://waldo-e2e/srv/app --name e2e >/dev/null 2>&1 || { echo "waldo up failed" >&2; exit 1; }
REMOTE_HOST="$("$WALDO_BIN" exec --session e2e -- hostname)"
export WALDO_SESSION=e2e

info "Target is up (local=$LOCAL_HOST remote=$REMOTE_HOST)"
if [[ "$LOCAL_HOST" == "$REMOTE_HOST" ]]; then
  echo "local and remote hostnames match; test cannot distinguish them" >&2; exit 1
fi

# ---------------------------------------------------------------- waldo itself
info "waldo core"
out="$("$WALDO_BIN" exec --session e2e -- 'hostname')"
assert_contains "$out" "$REMOTE_HOST" "exec runs on the target"

"$WALDO_BIN" exec --session e2e -- 'cd /tmp && pwd' >/dev/null
out="$("$WALDO_BIN" exec --session e2e -- 'pwd')"
assert_contains "$out" "/tmp" "cd persists between commands"
"$WALDO_BIN" exec --session e2e -- 'cd /srv/app' >/dev/null

"$WALDO_BIN" exec --session e2e -- 'exit 42' >/dev/null 2>&1
assert_contains "$?" "42" "exit status is passed through"

out="$("$WALDO_BIN" exec --session e2e -- 'ls /nonexistent' 2>&1)"
assert_contains "$out" "No such file" "stderr from the target reaches the caller"

# ------------------------------------------------------------------ Claude Code
if command -v claude >/dev/null 2>&1; then
  info "Claude Code (real agent, model=$MODEL)"
  shim="$WALDO_HOME/bin/waldo-shell-prefix"
  "$WALDO_BIN" env e2e >/dev/null   # ensures the shim exists

  out="$(clean_agent_env \
      WALDO_SESSION=e2e WALDO_SSH_CONFIG="$WALDO_SSH_CONFIG" WALDO_HOME="$WALDO_HOME" \
      CLAUDE_CODE_SHELL_PREFIX="$shim" \
      timeout 300 claude -p "Run the shell command 'hostname'. Then run 'cat /srv/app/README.md'. Report both outputs verbatim." \
        --allowedTools Bash --permission-mode bypassPermissions --model "$MODEL" 2>&1)"

  assert_contains "$out" "$REMOTE_HOST" "agent's Bash tool executed on the target"
  assert_not_contains "$out" "$LOCAL_HOST" "agent never saw the local machine"
  assert_contains "$out" "hello from remote" "agent read a file that exists only on the target"

  # The agent must be able to change directory and have it stick, which depends
  # on waldo reproducing the harness's local cwd bookkeeping.
  out="$(clean_agent_env \
      WALDO_SESSION=e2e WALDO_SSH_CONFIG="$WALDO_SSH_CONFIG" WALDO_HOME="$WALDO_HOME" \
      CLAUDE_CODE_SHELL_PREFIX="$shim" \
      timeout 300 claude -p "Run 'cd /etc' as one command. Then, as a SEPARATE second command, run 'pwd'. Report what the second command printed." \
        --allowedTools Bash --permission-mode bypassPermissions --model "$MODEL" 2>&1)"
  assert_contains "$out" "/etc" "cd persists across separate agent tool calls"

  # ---- mirror mode: the agent's NATIVE file tools must act on the target ----
  info "Claude Code — mirror mode (native Read/Edit on remote files)"
  "$WALDO_BIN" down e2e >/dev/null 2>&1
  "$WALDO_BIN" up ssh://waldo-e2e/srv/app --name e2e --mode mirror >/dev/null 2>&1

  "$WALDO_BIN" exec --session e2e -- \
    'printf "line one\nTARGET_VALUE = 41\nline three\n" > /srv/app/config.py' >/dev/null

  # The file must not exist locally, or the test proves nothing.
  if [[ -e /srv/app/config.py ]]; then
    bad "precondition" "/srv/app/config.py exists locally; cannot prove remoteness"
  else
    out="$(clean_agent_env \
        WALDO_SESSION=e2e WALDO_SSH_CONFIG="$WALDO_SSH_CONFIG" WALDO_HOME="$WALDO_HOME" \
        timeout 300 "$WALDO_BIN" claude -- \
          -p "Use the Read tool on /srv/app/config.py, then use the Edit tool to change TARGET_VALUE from 41 to 42." \
          --allowedTools "Read,Edit" --permission-mode bypassPermissions --model "$MODEL" 2>&1)"

    after="$("$WALDO_BIN" exec --session e2e -- 'cat /srv/app/config.py')"
    assert_contains "$after" "TARGET_VALUE = 42" \
      "native Edit tool wrote through to the target"
    assert_contains "$after" "line three" \
      "the rest of the target file was preserved"

    # Grep must be refused rather than silently searching a sparse mirror.
    out="$(printf '%s' '{"hook_event_name":"PreToolUse","tool_name":"Grep","tool_input":{"pattern":"x"},"cwd":"/srv/app"}' \
      | WALDO_SESSION=e2e WALDO_HOME="$WALDO_HOME" WALDO_SSH_CONFIG="$WALDO_SSH_CONFIG" "$WALDO_BIN" hook)"
    assert_contains "$out" '"permissionDecision":"deny"' \
      "Grep is denied in mirror mode (a sparse mirror would under-report)"
  fi
else
  info "Claude Code not installed — skipping agent tests"
fi

"$WALDO_BIN" down e2e >/dev/null 2>&1
summary
