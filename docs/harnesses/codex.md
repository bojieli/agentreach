# Codex

Verified against **0.147.0**. Re-verify with `make conformance`.

## Seam

Codex resolves its shell with `execvp`, so an executable named `bash` earlier on
`PATH` intercepts it. waldo installs one and prepends its directory. No fork, no
`chsh`, no configuration the harness has to support.

Confirmed by intercepting a real `codex sandbox` exec.

## Why Codex is the best fit

Codex routes file reads, writes and `apply_patch` edits **through its shell
tool** rather than through native file tools. Intercepting the shell therefore
redirects Codex's entire tool surface — there is no mirror mode to configure,
no denied tools, and no gap.

## The sandbox conflict

Codex sandboxes the commands it runs, and that sandbox blocks network syscalls.
waldo's shim has to open an SSH connection, so under the default policy every
command fails with `Operation not permitted`.

`waldo codex` therefore sets:

```toml
sandbox_mode = "workspace-write"
sandbox_workspace_write.network_access = true
```

This keeps Codex's filesystem sandbox and allows only the network. Codex's local
sandbox is doing less work than usual in this configuration — the commands
execute on the target, so the meaningful boundary is the target itself — but
there is no reason to give up the part that still applies.
`--danger-full-access` disables it entirely if you need that.

## Not the seam

Hooks. Codex's `PreToolUse` fires for the shell tool only, and the only decision
it honours is `deny`; `updatedInput` is parsed and rejected (upstream issue
#18491). Hooks cannot rewrite a command, so shell substitution is used instead.
