# Security model

reach's premise is that **the target is not trusted**. It exists precisely for
the case where you want an agent to work on a machine you would not put your
credentials on — a client's server, a shared box, a production host someone
else administers.

Please report vulnerabilities privately via GitHub Security Advisories on this
repository rather than opening a public issue.

## What reach guarantees

**No credential reaches the target.** The agent, its API key or OAuth token,
and its conversation all stay on your machine. reach sends commands and file
bytes; it never sends authentication material. This is the property that makes
reach different from running the agent remotely.

**Nothing is installed by default.** Tier 0 uses only a POSIX shell and writes
nothing to the target's disk. The one tier that does write a binary
(`--fileops=helper`) is never selected by autonegotiation and is refused
outright on a session marked `--untrusted`.

**SSH agent forwarding is off, unconditionally.** reach never passes
`ForwardAgent=yes`. A forwarded agent socket on a host with a hostile root lets
that host authenticate as you against every other system you can reach — it
converts one compromised server into all of them. There is no flag to enable
this in reach; use plain `ssh` if you have a reason to.

**Local paths are stripped where reach can do it safely.** Claude Code's
command envelope sources a shell snapshot from your home directory. Forwarding
it would disclose your username and directory layout to the target for no
benefit, so reach removes it. reach does *not* attempt to rewrite arbitrary
commands: a false positive that silently mangled a command would be worse than
the leak it prevented.

**Connections do not outlive sessions.** `reach down` tears down the
multiplexed SSH master. A tool whose premise is leaving no trace should not
leave a live connection to someone else's server behind it.

## What reach does not protect you from

**Prompt injection from target output.** This is the sharpest risk in this
design, and it is not fully solvable at reach's layer.

File contents, logs, error messages and command output from the target flow
into the context of an agent that holds your credentials and can write to your
local disk. A compromised target can therefore attempt to instruct your agent.
Nothing about running the agent locally prevents this; if anything, reach makes
it more relevant, because the whole point is pointing an agent at machines you
do not control.

Mitigations, in order of effectiveness:

- Keep secrets out of the agent's local environment. reach does not need them
  and neither should the session.
- Prefer a session per target. reach binds a session to exactly one target, so
  output from one host cannot reach a shell aimed at another.
- Review file writes the agent makes to your **local** machine while a remote
  session is attached. Those are the ones an injected instruction would target.
- Treat a compromised target's output the way you would treat a hostile pull
  request: something to read, not something to obey.

**A malicious target lying about its own state.** reach reports what the target
says. A compromised host can return whatever it likes for `hostname`, file
contents or exit statuses. reach verifies transport integrity, not target
honesty.

**Your own SSH configuration.** reach delegates destination resolution to your
`ssh` client so that jump hosts, certificates and hardware tokens keep working.
That also means reach inherits whatever your config does, including any
`ForwardAgent yes` you have set for a host in `~/.ssh/config`. reach passes
`ForwardAgent=no` explicitly on its own command line, which takes precedence,
but if you have unusual `Match exec` directives you should read them.

## Denied file tools are a safety control, not a preference

In `exec` mode with Claude Code, reach denies `Read`, `Edit`, `Write`, `Glob`
and `Grep`. Those tools have no interception seam and would keep operating on
your **local** filesystem while the agent believes it is working on the target.

The failure modes are asymmetric and both bad: reading a local file the agent
thinks is remote produces confident, wrong analysis; writing a local file the
agent thinks is remote destroys your own work. Denying the tools makes both
impossible rather than unlikely.

`--allow-local-file-tools` disables this. It exists for operators who have
arranged the paths to coincide and understand what they are doing.

## Reporting

Security reports: GitHub Security Advisories on this repository. Please include
the reach version, the harness and version, and the target's `uname -sm`.
