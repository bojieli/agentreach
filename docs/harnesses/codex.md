# Codex

Works against **0.148.0**, through a seam Codex itself provides: its
exec-server protocol. Re-verify any version with `reach harness verify codex`.

## The finding that shapes everything

Codex has no file tools. Measured at the wire — a mock model server recorded
the `tools` array of codex 0.148.0's first Responses-API request — the model
is offered `exec_command`, `write_stdin`, and five tools that touch no
filesystem at all. There is no `read_file`, no `write_file`, no `edit`, no
`grep`. Even `apply_patch` is not a wire tool: when the model uses it, it runs
as a command *inside* `exec_command`.

So Codex's entire tool surface funnels through one protocol. Intercept that
protocol and there is nothing left to deny, nothing to mirror, and no gap for
a wrong-machine write to slip through. This is the cleanest fit reach has —
cleaner than Claude Code, where whole tool families must be denied or mirrored
around the fact that they cannot be redirected.

## The seam

Codex 0.148 supports *remote environments*: `$CODEX_HOME/environments.toml`
points it at an external exec-server, a JSON-RPC endpoint that receives every
process spawn and filesystem operation. reach is that server. `reach codex`
builds a managed `CODEX_HOME` — the operator's real one is never touched —
whose `environments.toml` selects reach and sets `include_local = false`, so
the local machine is not merely avoided, it is absent from the list.

Every `process/*` call runs on the target through the session transport; every
`fs/*` call is served from the target through reach's fileops. If the session
is broken the server fails loudly. There is no code path that falls back to
local execution, because the local machine is not a value the protocol can
name.

The launch guard is unchanged in shape: `reach harness verify codex` drives
one real turn against an offline mock model — no API key, no tokens — and
checks where the scripted command actually ran. Because old verdicts measured
a different seam, the verdict cache is schema-versioned and discards them
rather than trusting them in either direction.

## The lesson the PATH shim taught

This document once claimed the PATH shim worked against Codex ≤ 0.147. It did
not, and the error is worth recording because of *why* it survived.

The conformance check probed `codex sandbox -- bash -c …`. That resolves the
*user-supplied* program through `PATH`, so it stayed green no matter what the
shell tool itself did. Meanwhile codex had been resolving the login shell
through the account database (`getpwuid_r`) and spawning it by absolute path —
invisible to `PATH` — since before the code was split out of `codex-core` in
February 2026. Behavioural re-measurement in August 2026 found the shim
bypassed on every version probed: 0.140.0, 0.146.1, 0.147.0, 0.148.0.

Two design rules came out of that, and they now apply to every harness:

- **A claim about a harness needs an experiment that cannot lie.** Reading
  docs, reading source, and probing adjacent code paths all lie comfortably.
  The probe drives a real turn and checks which machine answered.
- **Prefer the harness's own remote-execution design over intercepting its
  process.** A shim fights the binary; a protocol cooperates with it. When
  codex grew a supported seam, reach moved to it and deleted a sandbox
  workaround, a network exemption, and a class of version fragility in one
  motion.

## Not the seam

Hooks — Codex's `PreToolUse` fires for the shell tool only, and the only
decision it honours is `deny`; `updatedInput` is parsed and rejected (upstream
issue #18491). Config — `ConfigToml` has no shell-path key; `zsh_path` is
internal to the under-development zsh-fork backend. Interposition — the macOS
binary is hardened-runtime signed without
`com.apple.security.cs.disable-library-validation`, so
`DYLD_INSERT_LIBRARIES` is unavailable. Each of these was checked, not
assumed; each is a dead end; the exec-server environment is the seam.
