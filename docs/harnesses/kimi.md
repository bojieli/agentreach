# Kimi Code

Probed against **0.34.0** (PATH shim) and re-measured against **0.37.2**
(bypassed — see below). MIT-licensed, Bun-compiled TypeScript.

## Seam

`PATH` shim, same as Codex. No shell-override environment variable was found in
the binary; the documented `KIMI_SHELL_PATH` overrides only the Git Bash path
on Windows.

## Status

**Command execution: broken on 0.37.2, guarded.** Against 0.34.0 the shim
intercepted Kimi's shell. On 0.37.2 Kimi spawns its shell by absolute path,
so the shim never runs and every command executes locally while the agent
believes it acts on the target — the same regression class as Codex 0.148.
`waldo harness verify kimi` measures this offline (mock model over the
OpenAI chat-completions wire, one scripted `Bash` tool call, hostname check),
caches the verdict per Kimi version, and `waldo kimi` refuses to launch a
version measured to bypass the shim. `--force` overrides for operators who
accept local execution.

Kimi's `PreToolUse` hook honours only allow/deny — it cannot rewrite a tool
call's input — so a hook cannot reroute `Bash` commands either.

**File tools: not redirected.** Kimi's native `read_file`, `write_file` and
`multi_edit` act on the local filesystem regardless of the shell seam.
`waldo kimi` prints a warning saying so, and the agent should use shell
commands for file access until an adapter exists.

Because Kimi is open source, upstreaming a proper execution seam is realistic
rather than hypothetical — unlike the closed harnesses, where waldo must work
with whatever the binary happens to expose.
