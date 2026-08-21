# Goose (Block)

Open-source Rust agent by Block.  Installed as `goose` CLI.  Version tested: current main.

## The shell seam

Goose's developer extension resolves its shell in
`crates/goose/src/agents/platform_extensions/developer/shell.rs`:

```rust
fn unix_shell() -> String {
    if let Ok(shell) = std::env::var("GOOSE_SHELL") {
        return shell;
    }
    if which::which("bash").is_ok() { "bash".to_string() } else { "sh".to_string() }
}
```

`GOOSE_SHELL` is a first-class, documented override — no patching, no shim
gymnastics.  `reach goose` sets `GOOSE_SHELL` to the absolute path of the
PATH shim's `bash`.  Every command the `shell` tool runs goes through reach
and executes on the session target.

Unlike Codex and Kimi, this is a supported, stable env var that Goose's own
test suite exercises — it is not an accidental bypass of a bypass.

## File tools: local only

Goose's developer extension also exposes `file_read`, `file_write`, and
`file_edit` tools that call Rust's `std::fs` directly.  `GOOSE_SHELL` has no
effect on these.

Options for full coverage:

1. **Goose ACP mode** (`goose acp`): Goose runs as an ACP (Agent Client
   Protocol) JSON-RPC server on stdio; the ACP client advertises file
   capabilities and handles file reads/writes.  reach would act as the ACP
   client, routing every file operation to the target.  This gives 100%
   coverage but requires reach to implement the ACP client protocol — not yet
   done.

2. **Shell-only posture**: Disable `file_read`, `file_write`, and `file_edit`
   (not yet configurable in stock Goose) and have the agent use shell commands
   for file access.  `GOOSE_SHELL` covers that path.

Current default: `reach goose` sets `GOOSE_SHELL` and warns that the file
tools act locally.

## File tools: denied via available_tools

Goose's developer extension also exposes `file_read`, `file_write`,
`file_edit`, `tree`, and `read_image` tools that call Rust's `std::fs`
directly.  `GOOSE_SHELL` has no effect on these.

`reach goose` builds a managed `GOOSE_PATH_ROOT` whose
`config/config.yaml` sets `available_tools: [shell]` on the developer
extension.  Goose enforces this allowlist at the extension layer: only tools
named in `available_tools` are advertised to the model.  With the list set
to `[shell]`, the file tools are not offered; the model uses shell commands
for file access instead, which run on the target.

The managed config is written from the operator's real goose
`config.yaml` (provider settings, model selection) with the `extensions:`
block replaced by reach's controlled version.  If the operator configures
their provider via env vars (the common case), no copying is needed.

## Seam coverage

| Tool surface | Mechanism | Status |
|---|---|---|
| `shell` (shell commands) | `GOOSE_SHELL` env var | **✓ remote** |
| `file_read` | `available_tools: [shell]` in config.yaml | **denied** (use shell) |
| `file_write` | `available_tools: [shell]` in config.yaml | **denied** (use shell) |
| `file_edit` | `available_tools: [shell]` in config.yaml | **denied** (use shell) |
| `tree` | `available_tools: [shell]` in config.yaml | **denied** (use shell) |
| `read_image` | `available_tools: [shell]` in config.yaml | **denied** |

## Probe

The seam guard runs Goose against a mock model and instructs a `shell` tool
call of `echo <marker>; hostname`.  A match with the target hostname is a
VerdictOK.  The `GOOSE_SHELL` seam is stable across Goose versions, so the
verdict is expected to stay OK and rarely needs re-measurement.

## Implementation status

`reach goose` is implemented.  `reach harness verify goose` probes the
`GOOSE_SHELL` seam end-to-end against the live session target.

## ACP roadmap

Implementing reach as a Goose ACP client would give 100% tool coverage — all
file reads and writes would route to the target — without needing
`available_tools` restriction.  The ACP wire format is JSON-RPC over stdio,
well-documented in Goose's repo.  File capabilities in ACP:
`fs.read_text_file`, `fs.write_text_file`.  The `terminal` capability covers
shell commands (overlapping with `GOOSE_SHELL`).  When the ACP client is
implemented, `reach goose` can drop `GOOSE_SHELL` and rely entirely on ACP.
