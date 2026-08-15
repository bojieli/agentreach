#!/usr/bin/env bash
# Conformance tests: do the harness seams waldo depends on still have the shape
# waldo expects?
#
# waldo hooks undocumented implementation details in closed binaries. A harness
# upgrade can change them without notice. These tests exist so that breakage is
# discovered in seconds by running a command, rather than in the middle of a
# task by an agent quietly operating on the wrong machine.
set -uo pipefail
cd "$(dirname "$0")" && source ./lib.sh

PROBE="${WALDO_E2E_DIR}/conformance"
mkdir -p "$PROBE"

info "Harness seam conformance"

# ------------------------------------------------------------------ Claude Code
if command -v claude >/dev/null 2>&1; then
  ver="$(claude --version 2>/dev/null | head -1)"
  info "Claude Code: $ver"

  cat > "$PROBE/prefix" <<'SH'
#!/bin/bash
printf '%s\n' "ARGC=$#" > /tmp/waldo-conformance-argv
i=0; for a in "$@"; do printf 'ARGV[%d]=%s\n' "$i" "$a" >> /tmp/waldo-conformance-argv; i=$((i+1)); done
# Behave like a shell so the agent's turn completes.
exec /bin/bash -c "$1"
SH
  chmod +x "$PROBE/prefix"
  rm -f /tmp/waldo-conformance-argv

  clean_agent_env CLAUDE_CODE_SHELL_PREFIX="$PROBE/prefix" \
    timeout 300 claude -p "Run the shell command: echo conformance_probe" \
      --allowedTools Bash --permission-mode bypassPermissions \
      --model "${WALDO_E2E_MODEL:-haiku}" >/dev/null 2>&1

  if [[ -f /tmp/waldo-conformance-argv ]]; then
    argv="$(cat /tmp/waldo-conformance-argv)"
    ok "CLAUDE_CODE_SHELL_PREFIX is still honoured"
    assert_contains "$argv" "ARGC=1" \
      "prefix still receives the command as ONE argument"
    assert_contains "$argv" "eval " \
      "envelope still wraps the user command in eval"
    assert_contains "$argv" "pwd -P" \
      "envelope still uses the pwd -P cwd protocol"
    assert_contains "$argv" "shell-snapshots" \
      "envelope still sources a local shell snapshot (waldo strips this)"
  else
    bad "CLAUDE_CODE_SHELL_PREFIX is no longer honoured" \
        "waldo's Claude Code adapter needs updating for $ver"
  fi
else
  info "Claude Code not installed — skipping"
fi

# ------------------------------------------------------------------------ Codex
if command -v codex >/dev/null 2>&1; then
  ver="$(codex --version 2>/dev/null | head -1)"
  info "Codex: $ver"

  mkdir -p "$PROBE/pathshim"
  cat > "$PROBE/pathshim/bash" <<'SH'
#!/bin/bash
echo "CODEX_PATHSHIM_HIT" >&2
exec /bin/bash "$@"
SH
  chmod +x "$PROBE/pathshim/bash"

  out="$(PATH="$PROBE/pathshim:$PATH" timeout 60 codex sandbox -- bash -c 'echo inner' 2>&1)"
  assert_contains "$out" "CODEX_PATHSHIM_HIT" \
    "Codex still resolves bash through PATH (execvp), so a shim intercepts it"

  if codex features list >/dev/null 2>&1; then
    feats="$(codex features list 2>/dev/null)"
    assert_contains "$feats" "shell_tool" "Codex still exposes the shell_tool flag"
    assert_contains "$feats" "unified_exec" "Codex still exposes unified_exec"
  fi
else
  info "Codex not installed — skipping"
fi

# ------------------------------------------------------------------- Kimi Code
if command -v kimi >/dev/null 2>&1; then
  info "Kimi Code: $(kimi --version 2>/dev/null | head -1)"
  ok "Kimi present (adapter in progress)"
else
  info "Kimi Code not installed — skipping"
fi

summary
