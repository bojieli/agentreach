# Harness Integration Research

Everything in this document was **verified empirically** against the named
version by running a real agent, not inferred from documentation. Each claim
carries the version it was checked against and the method used. Re-verify with
`make conformance` after upgrading a harness.

Last verified: 2026-08-15

---

## Why this document exists

waldo relies on interception seams that are, in most harnesses, undocumented
implementation details. A seam that silently changes shape is the single most
likely way this project breaks for users. So every seam is (a) verified by
experiment, (b) recorded here with its exact observed shape, and (c) covered by
a conformance test that fails loudly when the shape changes.

---

## Claude Code 2.1.233

### Distribution format

    $ file ~/.local/share/claude/versions/2.1.233
    ELF 64-bit LSB executable, dynamically linked, not stripped  (310 MB)
    $ strings ... | grep _ZN2v8    ->  V8 symbols present

Claude Code ships as a **Node SEA (Single Executable Application)** with V8
statically embedded. It is not a `node script.js` invocation.

Consequences, both verified:

| Technique | Works? | Evidence |
|---|---|---|
| `NODE_OPTIONS=--require preload.js` | **No** | preload's `console.error` never fired; `claude --version` ran clean |
| `LD_PRELOAD` fs interposition | Not viable as a general strategy | Works for Claude (dynamically linked) but *not* for Codex, which is `static-pie` — no dynamic symbol interposition possible |

This kills in-process monkey-patching of `node:fs`. Native `Read`/`Edit`/`Write`/
`Grep`/`Glob` cannot be re-pointed from inside the process.

### `CLAUDE_CODE_SHELL`

Decompiled validation logic (extracted from the bundle's string table):

```js
async function isExecutable(e) {
  try { return fs.accessSync(e, constants.X_OK), true }
  catch (t) { let { code } = await run(e, ["--version"], { timeout: 1000 }); return code === 0 }
}
async function resolveShell() {
  let e = env.CLAUDE_CODE_SHELL;
  if (e)
    if ((e.includes("bash") || e.includes("zsh")) && await isExecutable(e))
      return log(`Using shell override: ${e}`), e;
    else
      log(`CLAUDE_CODE_SHELL="${e}" is not a valid bash/zsh path, falling back to detection`);
  // ... falls back to $SHELL, then /bin, /usr/bin, /usr/local/bin probing
}
```

**Rules, exactly:**

1. The *path string* must contain the substring `bash` or `zsh` anywhere — not
   just the basename. `/opt/waldo/waldo-bash` qualifies.
2. It must be executable (`X_OK`), or exit 0 on `--version`.
3. On failure it falls back silently to shell detection — the agent keeps
   working against the **local** machine. A misconfigured shim is therefore a
   silent correctness hazard, which is why `waldo doctor` checks the name rule.

Invocation shape, observed:

    argv = ["-c", "<command>"]
    argv = ["-c", "-l", "<snapshot-builder-script>"]   # once per session

### `CLAUDE_CODE_SHELL_PREFIX`  ← the seam waldo uses

Observed invocation:

    ARGC=1
    ARGV[0]=<<<source /home/ubuntu/.claude/shell-snapshots/snapshot-bash-1786757311642-4kz5ln.sh 2>/dev/null || true && { shopt -u extglob || setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL; } >/dev/null 2>&1 || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true && eval 'echo PREFIX_MARKER_991' < /dev/null && pwd -P >| /tmp/claude-1003-cwd>>>

The prefix program receives the **entire command envelope as a single argv
element**. No `-c`, no name validation, and the prelude becomes shell-agnostic
when the variable is set:

```js
function prelude(shellPath) {
  if (env.CLAUDE_CODE_SHELL_PREFIX)
    return "{ shopt -u extglob || setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL; } >/dev/null 2>&1 || true";
  if (shellPath.includes("bash")) return "shopt -u extglob 2>/dev/null || true";
  if (shellPath.includes("zsh"))  return "setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL 2>/dev/null || true";
  return null;
}
```

That the prelude is deliberately written to work under *either* shell is strong
evidence this variable exists for wrapping execution in a foreign environment
(container/remote), which is exactly waldo's use case. This is the seam waldo
targets: one argument, no parsing of flags, stable contract.

### The command envelope

Every Bash tool call arrives wrapped:

    source <LOCAL_SNAPSHOT>.sh 2>/dev/null || true
      && { shopt -u extglob || setopt ... ; } >/dev/null 2>&1 || true
      && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; } >/dev/null 2>&1 || true
      && eval '<USER COMMAND>' < /dev/null
      && pwd -P >| /tmp/claude-<rand>-cwd

Three parts matter to waldo:

**1. The snapshot (`source ...`)** — a file generated on the *local* machine
capturing the user's shell functions, aliases, `shopt`, and `PATH`. waldo
**strips** this segment rather than forwarding it, for two reasons:

- It references local absolute paths that do not exist remotely; sourcing it
  remotely is a silent no-op at best.
- It leaks the local username and directory layout to the remote host. On an
  untrusted client server that is an information disclosure waldo must not
  cause. See `docs/SECURITY.md`.

**2. `pwd -P >| /tmp/claude-<rand>-cwd`** — this is how Claude Code persists
`cd` between Bash calls. The path is **regenerated per invocation** (observed
`/tmp/claude-1003-cwd` then `/tmp/claude-a4c6-cwd`), and Claude Code reads it
back on the local filesystem after the command returns.

If the envelope is forwarded verbatim to a remote host, this file is written
*remotely* and the local read finds nothing — **`cd` silently stops
persisting**. waldo therefore strips this segment, tracks cwd itself, and
writes the resolved working directory to the local path the envelope named.
This is the single most important correctness detail in the Claude Code
adapter.

**3. Embedded tool shadowing** — the snapshot installs shell functions that
replace `rg`, `find`, and `grep` with Claude Code's *embedded* binaries:

```bash
function rg   { (exec -a rg    "$CLAUDE_CODE_EXECPATH" "$@") }
function find { (exec -a bfs   "$CLAUDE_CODE_EXECPATH" -S dfs -regextype findutils-default "$@") }
function grep { (exec -a ugrep "$CLAUDE_CODE_EXECPATH" -G --ignore-files --hidden -I ... "$@") }
```

Because waldo strips the snapshot, these functions are never defined remotely
and `rg`/`find`/`grep` resolve to the remote host's real binaries. That is the
desired behaviour — but it means **remote hosts without ripgrep get plain
grep semantics**, which `waldo doctor` reports.

### Native file tools

`Read`, `Edit`, `Write`, `Grep`, `Glob` do **not** route through any shell and
have no environment-variable seam. With MCP excluded by design and in-process
patching impossible (SEA), the only way to make them act on remote content is
to place real files at the paths they read. See "Materialisation" in
`ARCHITECTURE.md`.

---

## Codex CLI 0.147.0

Distribution: `ELF 64-bit LSB pie executable, static-pie linked, stripped` (247 MB), Rust.

`static-pie` means **no dynamic linking**, so `LD_PRELOAD`/`DYLD_INSERT_LIBRARIES`
interposition is impossible by construction.

Relevant configuration surface found in the binary: `unified_exec`,
`shell_command`, `shell_type`, `danger_full_access`, `workspace_write`,
`read_only`, `apply_patch`. Hook events exist but — per upstream docs and
issue #18491 — `PreToolUse` fires for the shell tool only and the only decision
Codex acts on is `deny`; `updatedInput` is parsed but rejected. So hooks cannot
rewrite a command, and the adapter uses shell-level substitution instead.

See `docs/harnesses/codex.md`.

## Kimi Code CLI 0.31.1

Distribution: dynamically linked ELF, 164 MB (Bun-compiled TypeScript), MIT.

Confirmed present in the binary: `PreToolUse`, `PostToolUse`, `plugin`.
Supports `-p/--prompt` and `--output-format stream-json`, which makes it
scriptable for E2E testing.

See `docs/harnesses/kimi.md`.

## opencode

Not installed on the verification machine at time of writing; adapter is built
against the documented plugin API (custom tools whose filename matches a
built-in take precedence, plus `tool.execute.before`/`after` hooks). Marked
**unverified** in `docs/harnesses/opencode.md` until an E2E run exists.

---

## Reproducing these results

    make conformance          # runs the probes below against installed harnesses

The probe rig works by pointing the harness's shell seam at a logging shim and
running a real prompt through the agent, then asserting on the captured argv.
Note that a nested `claude` run inherits `CLAUDECODE=1`,
`CLAUDE_CODE_CHILD_SESSION=1` and a possibly-invalid `ANTHROPIC_API_KEY` from
its parent; all of these must be unset or the run hangs or fails
authentication. `test/e2e/cleanenv.sh` does this.
