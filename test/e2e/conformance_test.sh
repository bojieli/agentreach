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

  # The claim this seam rests on: Claude Code cannot be monkey-patched from
  # inside, so its native file tools cannot be re-pointed and waldo must either
  # deny them or materialise real files. It ships as a Node SEA, and the macOS
  # build is stripped, so the V8 symbol check that settles it on Linux proves
  # nothing there. Run the experiment instead — and run a control, because a
  # preload that silently fails to fire would "confirm" this for the wrong
  # reason.
  preload="$PROBE/preload.js"
  marker="$PROBE/preload-fired"
  cat > "$preload" <<'JS'
require("fs").writeFileSync(process.env.WALDO_PRELOAD_MARKER, "fired");
JS
  rm -f "$marker"

  control="skipped"
  if command -v node >/dev/null 2>&1; then
    WALDO_PRELOAD_MARKER="$marker" NODE_OPTIONS="--require $preload" \
      node -e '' >/dev/null 2>&1
    if [[ -f "$marker" ]]; then control="works"; else control="broken"; fi
    rm -f "$marker"
  fi

  if [[ "$control" == "broken" ]]; then
    bad "NODE_OPTIONS preload control failed" \
        "the probe cannot detect a preload even under plain node, so its result about claude would be meaningless"
  else
    clean_agent_env WALDO_PRELOAD_MARKER="$marker" NODE_OPTIONS="--require $preload" \
      timeout 60 claude --version >/dev/null 2>&1
    if [[ -f "$marker" ]]; then
      bad "NODE_OPTIONS=--require is now honoured by Claude Code" \
          "in-process patching may be possible again; waldo's denial of the native file tools should be revisited"
    else
      ok "NODE_OPTIONS=--require is still ignored (control: node preload $control)"
    fi
  fi
  rm -f "$marker"

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

  # NOTE: `codex sandbox` resolves the *user-supplied* program via execvp, which
  # says nothing about how the shell TOOL resolves its shell. Codex >= 0.148
  # spawns the login shell by absolute path (getpwuid_r -> /bin/zsh -lc), and
  # this check stayed green while the real seam broke. The behavioral probe in
  # seam_test.sh below is the check that cannot lie; this one only tracks the
  # execvp-era path Codex <= 0.147 relied on.
  out="$(PATH="$PROBE/pathshim:$PATH" timeout 60 codex sandbox -- bash -c 'echo inner' 2>&1)"
  assert_contains "$out" "CODEX_PATHSHIM_HIT" \
    "codex sandbox still resolves commands through PATH (execvp)"

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
  ok "Kimi present (shell seam measured by the behavioral probe below)"
else
  info "Kimi Code not installed — skipping"
fi

# --------------------------------------------------- behavioral seam probes
# Static checks above only detect shape changes in what waldo hooks into; they
# cannot see a harness switching from execvp("bash") to an absolute shell path,
# which is exactly the Codex 0.148 regression. The behavioral probe drives each
# installed harness against an offline mock model and a real SSH target and
# measures where a scripted command actually ran. It fails on known-broken
# harness versions — that red is the point.
info "Behavioral seam probes (offline mock model + docker sshd)"
if ./seam_test.sh; then
  ok "harness seam probes (see seam_test.sh output above)"
else
  bad "harness seam probes" \
      "see seam_test.sh output above — a BYPASSED verdict means the installed
      harness version runs commands locally while appearing remote"
fi

summary
