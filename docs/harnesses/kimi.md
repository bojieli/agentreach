# Kimi Code

Probed against **0.34.0**. MIT-licensed, Bun-compiled TypeScript.

## Seam

`PATH` shim, same as Codex. No shell-override environment variable was found in
the binary.

## Status

**Command execution: working.** `waldo kimi` prepends the shim directory.

**File tools: not yet redirected.** Kimi's native `read_file`, `write_file` and
`multi_edit` still act on the local filesystem. `waldo kimi` prints a warning
saying so, and the agent should use shell commands for file access until an
adapter exists.

Kimi exposes `PreToolUse`/`PostToolUse` hooks, which is the same mechanism
waldo's Claude Code mirror mode uses. Wiring them up is the obvious next step;
the hook implementation in `cmd/waldo/hook.go` is harness-agnostic apart from
its JSON field names.

Because Kimi is open source, upstreaming a proper execution seam is realistic
rather than hypothetical — unlike the closed harnesses, where waldo must work
with whatever the binary happens to expose.
