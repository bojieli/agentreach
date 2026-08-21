# Codex

Works against **≤ 0.147**. **Broken by 0.148.0** — waldo refuses to launch it
(see below). Re-verify any version with `waldo harness verify codex`.

## Seam

Codex ≤ 0.147 resolves its shell with `execvp`, so an executable named `bash`
earlier on `PATH` intercepts it. waldo installs one and prepends its directory.
No fork, no `chsh`, no configuration the harness has to support.

Confirmed by intercepting a real `codex sandbox` exec.

## The 0.148 regression

Codex 0.148.0 resolves the login shell through the account database
(`getpwuid_r`) and spawns it by **absolute path** — `/bin/zsh -lc …` on a stock
macOS install, `/bin/bash -lc …` on most Linux ones. Nothing on `PATH` can
intercept an absolute path, so every shell command runs on the local machine
while the agent believes — and reports — that it acted on the target. That is
the failure waldo exists to prevent, so `waldo codex` **refuses to launch** a
version measured to bypass the shim. `--force` overrides, with a warning, for
operators who accept local execution.

How this was pinned down, because "the shim is on PATH" is not evidence:

- Codex session rollouts record each tool call's argv:
  `"command": ["/bin/zsh", "-lc", …]` — absolute, not a PATH lookup.
- The tagged 0.148.0 source (`codex-rs/shell-command/src/shell_detect.rs`)
  prefers the `getpwuid_r` shell whenever it exists, falling back to
  `which` — PATH — only when the account shell is missing or unrecognised.
- There is no config key, environment variable, or hook that changes this
  (the `zsh_path` config is internal, for the under-development zsh-fork
  escalation backend; `PreToolUse` hooks honour only `deny`).
- The macOS binary is hardened-runtime signed without
  `com.apple.security.cs.disable-library-validation`, so
  `DYLD_INSERT_LIBRARIES` interposition is not available either.

The conformance suite's original Codex check missed all of this: it probed
`codex sandbox -- bash -c …`, which resolves the *user-supplied* program via
execvp and stayed green while the shell tool's own resolution changed
underneath it. The replacement is behavioural and cannot lie in that way:
`waldo harness verify codex` (also run by `make conformance` via
`test/e2e/seam_test.sh`) points Codex at an offline mock model — the Responses
API is the only wire API ≥ 0.148 still speaks — scripts one shell tool call
that echoes a marker and the hostname, and checks where it ran. The verdict is
cached per Codex version in `$WALDO_HOME/harness-verdicts.json`, and the launch
guard consults that cache.

Over the Responses wire, 0.148.0 advertises its shell tool as `exec_command`
(args `{"cmd": …}`); `--disable unified_exec` swaps it for `shell_command`.
Both are probed.

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
