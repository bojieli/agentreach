# Windows

AgentReach runs on Windows as the machine you sit at: the agent and `reach` live on your Windows machine, and the target is a remote POSIX host. A Windows machine is never a *target* — AgentReach's floor is a POSIX shell, and Windows does not provide one.

This document records what differs from the Linux/macOS path, what is verified and how, and what is not yet verified.

## What differs from Unix, and why

Every Windows difference lives in `cmd/reach/platform_windows.go` and
`internal/transport/localshell_windows.go`. Nothing else in reach branches on
the operating system, so the cost of this port is visible in one place rather
than spread through the harness adapters.

### There is no `execve`

Go stubs `syscall.Exec` on Windows to return `EWINDOWS` unconditionally. reach
launches the harness as a child process instead and forwards its exit status.

This is genuinely worse, not merely different: with `execve` the harness *is*
the process, so signals, job control and the exit status belong to it directly.
As a child it inherits the console — Ctrl-C reaches it — but reach remains in
the process tree, and a `reach` killed with `SIGKILL` would orphan it. The same
code path runs on Unix whenever `execve` fails, so it is not Windows-only and
not untested.

### Shims cannot be symlinks

reach intercepts a harness's shell by putting an executable named `bash` earlier
on `PATH`, and points `CLAUDE_CODE_SHELL_PREFIX` at an executable named
`reach-shell-prefix`. On Unix both are symlinks to the reach binary.

Creating a symlink on Windows requires Developer Mode or an administrator, which
is not an acceptable prerequisite for an ordinary user. reach uses a **hard
link**, falling back to a **copy** when that fails — across volumes, or on a
filesystem without link support.

The distinction matters for staleness. A hard link is the same file, so
`os.SameFile` proves a shim is current. A copy is not, so reach writes a
`.source` stamp beside it recording the origin path, size and modification time,
and re-copies when they no longer match. Getting this wrong means an *old* reach
running inside a tool call after an upgrade, which surfaces as the harness
misbehaving rather than as a reach problem.

Shims are named `.exe`, because Windows will not execute a file whose extension
is not in `PATHEXT`, and `argv[0]` dispatch strips the extension
case-insensitively — a harness may well spawn `BASH.EXE`.

### There is no execute bit

`os.Stat` on Windows reports `0444` or `0666` and nothing else. reach's original
harness lookup tested `mode&0o111`, which on Windows is a test that can never
pass: every harness would have been reported as not installed. Lookups now go
through `exec.LookPath`, which consults `PATHEXT`.

### The search path is spelled `Path`

This was the most dangerous of the three, because it fails silently in the
direction that matters. Windows environment variables are case-insensitive and
the search path is conventionally `Path`. Code matching `"PATH"` exactly finds
nothing, so reach would have *appended a second* variable rather than editing
the real one — and the harness would have launched without reach's shim
directory in front of it, found the genuine `bash`, and run the model's commands
on the operator's own machine while the agent believed it was working on the
target.

reach matches the key case-insensitively on Windows and preserves its original
spelling when rewriting, so a child process is never left choosing between two
variables that differ only in case.

### There is no `ControlMaster`

This is the one difference reach cannot paper over.

OpenSSH's connection multiplexing passes file descriptors over a Unix domain
socket. Windows has AF_UNIX sockets but no `SCM_RIGHTS`, so the feature is
absent from Win32-OpenSSH rather than merely unconfigured.

The consequence is that every command opens and authenticates its own
connection: roughly **130 ms instead of 7 ms**, and one authentication per tool
call rather than one per session. reach's "there is no daemon" argument rests on
ControlMaster providing connection reuse for free, and on Windows that premise
does not hold — so the cost is real and is stated rather than hidden.

reach does not hard-code this. `reach up` establishes a multiplexed connection
and asks the client to confirm it with `ssh -O check`:

- A version string tells you what was compiled in, not what a `Match` block or a
  restricted socket directory will permit at run time.
- If a future Windows OpenSSH gains multiplexing, reach will find and use it
  with no change here.

The result is recorded in the session and reported by `reach up` and
`reach doctor`.

**Run an `ssh-agent`.** reach runs `ssh` in batch mode for tool calls, so a
passphrase-protected key with no agent does not prompt — it fails, once per
command. Windows ships an `ssh-agent` service:

```powershell
Get-Service ssh-agent | Set-Service -StartupType Automatic
Start-Service ssh-agent
ssh-add $env:USERPROFILE\.ssh\id_ed25519
```

## What is verified, and how

Following this project's standard: an experiment, not a reading of the
documentation.

**On every commit, on a real `windows-latest` runner:**

- The full unit suite (`go test ./...`), including `cmd/reach/platform_test.go`,
  which asserts the four behaviours above: shim installation and staleness
  detection, `PATH` matching under all three spellings, shim-directory removal
  from a child's environment, and `PATHEXT`-based executability.
- A CLI smoke test: the binary runs, reports `windows/` in its version, and
  fails an unreachable target with an explanation rather than a hang or a panic.
- Cross-compilation for `windows/amd64` and `windows/arm64`.

**On every commit, on Linux and macOS:**

- `TestWorksWithoutMultiplexing` runs the exact connection path Windows is
  permanently on — no control socket, a fresh authentication per command, no
  master to tear down — against a real sshd, including the exit-255 case and a
  binary file round trip.

**Not verified, and stated as such:**

- **A live agent run from Windows.** The harness seams — whether Claude Code on
  Windows honours `CLAUDE_CODE_SHELL_PREFIX`, and whether Codex on Windows
  resolves its shell by name in a way a `PATH` shim intercepts — have been
  reasoned about but not observed. CI cannot do this: it needs a Windows machine
  with a licensed agent and a reachable POSIX host, which is not something a
  public runner has.
- **End-to-end Windows → Linux.** For the same reason. The manual procedure is
  below; running it is what would turn `docs/harnesses/*.md` from "expected" to
  "verified on Windows".

## Verifying it by hand

On a Windows machine with an agent installed and a POSIX host to reach:

```powershell
go build -o reach.exe ./cmd/reach

# Does it reach the host, and did it get multiplexing?
.\reach.exe up ssh://your-host/srv/app
.\reach.exe doctor

# Do commands run there rather than here?
.\reach.exe exec -- uname -a
.\reach.exe exec -- pwd

# Do file operations round-trip, on whichever tier was negotiated?
.\reach.exe exec -- 'printf "hello\0\xff" > /tmp/reach-check.bin'
.\reach.exe fs read /tmp/reach-check.bin | Format-Hex

# The one that matters: does the agent's own shell reach the target?
.\reach.exe claude
#   then ask it to run `hostname` and `ls /`, and confirm the answers are the
#   target's and not this machine's.

.\reach.exe down
```

If the agent's `hostname` returns your Windows machine's name, stop and open an
issue with the `harness-seam` template — that is the failure this project exists
to make impossible, and it is worth more as a bug report than anything else in
here.
