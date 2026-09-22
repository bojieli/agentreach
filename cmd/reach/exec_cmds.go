package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bojieli/agentreach/internal/audit"

	"github.com/bojieli/agentreach/internal/envelope"
	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/transport"
)

// exitTransportFailure is returned when reach could not run the command at
// all. It is deliberately distinct from any status the command itself might
// produce, so an agent can tell "your command failed" from "reach could not
// reach the target".
const exitTransportFailure = 125

func cmdExec(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	name := fs.String("session", "", "session name (default $REACH_SESSION)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	cmdline := strings.Join(pos, " ")
	if strings.TrimSpace(cmdline) == "" {
		fmt.Fprintln(os.Stderr, "usage: reach exec [--session N] -- <command>")
		return 2
	}
	return runOnTarget(ctx, sessionNameFromEnv(*name), cmdline, "")
}

// claudeProjectDirEnv is set by Claude Code in the environment of a hook
// command and not in the environment of a Bash tool call. It is documented as
// the absolute path to the project root, "available to hook commands", and
// that asymmetry is the second of the two signals isLocalCallback wants.
const claudeProjectDirEnv = "CLAUDE_PROJECT_DIR"

// runShellPrefix is the CLAUDE_CODE_SHELL_PREFIX entrypoint.
//
// Claude Code invokes the prefix program with the entire command envelope as a
// single argument. reach takes that envelope apart, forwards only the portable
// part, and reproduces locally the bookkeeping the harness expects to find on
// the local filesystem.
//
// Not everything arriving here is the agent's, though: see isLocalCallback.
func runShellPrefix(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "reach shell-prefix: expected a command argument")
		return 2
	}
	// ssh and the harness both pass the command as one element, but join
	// defensively so a harness that splits it still works.
	raw := strings.Join(args, " ")

	p := envelope.ParseClaudeCode(raw)
	if isLocalCallback(p) {
		// Run it the way Claude Code would have run it with no prefix in the
		// way: through a local shell, inheriting this process's stdin, stdout
		// and exit status, because the hook protocol is carried on all three —
		// JSON in, a decision out, a status that says whether there was one.
		return execRealShell([]string{"-c", raw})
	}
	return runOnTarget(context.Background(), sessionNameFromEnv(""), p.Command, p.CwdFile)
}

// isLocalCallback reports that this invocation is Claude Code running one of
// its own local commands — a hook — rather than a Bash tool call belonging to
// the agent.
//
// Claude Code routes hook commands through CLAUDE_CODE_SHELL_PREFIX as well as
// tool calls (observed on 2.1.278). Forwarded to the target they fail there,
// and the failure is not the cosmetic one it looks like:
//
//   - reach's own PreToolUse hook stops answering, so exec mode goes back to
//     refusing correct commands for naming paths this machine does not have —
//     the thing the hook exists to prevent.
//   - an operator's hooks stop running at all, on either machine.
//   - the hook's stdin payload — tool input, transcript path, working
//     directory — travels to the target, which on someone else's server is a
//     disclosure reach would be causing.
//
// Two signals, and both must agree, because the two ways of being wrong are
// nothing like each other. Calling a hook remote is what reach does today: it
// is broken, but it is broken where the operator can see it. Calling an
// agent's command local would run it on the operator's own machine while the
// agent believed it ran on the target, which is the failure reach exists to
// prevent. So the test is narrow on purpose and anything it is unsure of goes
// to the target: a bare command *and* the environment Claude Code gives only
// to hooks.
func isLocalCallback(p envelope.Parsed) bool {
	return !p.Recognised && os.Getenv(claudeProjectDirEnv) != ""
}

// recordExec appends one executed command to the session's audit log.
//
// The command recorded is the one the model asked for, not the one reach sent:
// the wrapper reach adds to recover the exit status and working directory is
// reach's bookkeeping, and putting it in the record would bury what the agent
// actually did under machinery it did not write.
func recordExec(s *session.Session, command, cwd string, code int, took time.Duration, err error) {
	dir, dirErr := session.Dir()
	if dirErr != nil {
		return
	}
	entry := audit.Entry{
		Target:  s.Target.Describe(),
		Action:  "exec",
		Command: command,
		Dir:     cwd,
		Code:    code,
		Millis:  took.Milliseconds(),
	}
	if err != nil {
		entry.Error = err.Error()
	}
	audit.Append(dir, s.Name, entry)
}

// runOnTarget executes one command in a session and mirrors the harness's
// working-directory bookkeeping.
func runOnTarget(ctx context.Context, sessionName, command, cwdFile string) int {
	s, err := session.Load(sessionName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return exitTransportFailure
	}
	t, err := s.Transport()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return exitTransportFailure
	}
	// The transport is not closed here: closing tears down the multiplexed
	// master, and this process runs once per tool call. Reuse across calls is
	// the entire performance story.

	cwd := s.Cwd()
	original := command

	// Ask the target for its resulting directory in the same round trip. The
	// harness tracks `cd` between calls by reading a local file, so reach has
	// to know where the command ended up regardless of whether the harness
	// asked for it.
	const cwdMarker = "__reach_cwd__"
	command = fmt.Sprintf("%s\n__reach_rc=$?; printf '%s%%s\\n' \"$(pwd -P)\" >&2; exit $__reach_rc", command, cwdMarker)

	stderr := &cwdCapturingWriter{out: os.Stderr, marker: cwdMarker}
	started := time.Now()
	code, err := transport.RunStream(ctx, t, reach.ExecRequest{
		Command: command,
		Dir:     cwd,
		Timeout: s.Timeout,
		// The agent gets the PATH the operator would have on that machine.
		// Without this, a tool the operator installed into ~/.local/bin or
		// ~/.cargo/bin is invisible to the agent, for no reason it could work
		// out from the error.
		Env: s.Caps.Env(),
	}, os.Stdout, stderr)

	// Record it either way. A command that failed to reach the target is as
	// much a part of the account as one that ran.
	recordExec(s, original, cwd, code, time.Since(started), err)

	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return exitTransportFailure
	}

	if newCwd := stderr.Captured(); newCwd != "" {
		// Nothing else carries the working directory between tool calls: each
		// one is its own process and its own connection. If this cannot be
		// recorded, the agent's `cd` stops persisting and its next command runs
		// somewhere else — so the failure is reported rather than swallowed,
		// on the stream the agent reads.
		if err := s.SetCwd(newCwd); err != nil {
			fmt.Fprintf(os.Stderr,
				"reach: could not record the working directory %s: %v\n"+
					"reach: later commands will keep running in %s until this is fixed\n",
				newCwd, err, cwd)
		}
		// Reproduce the harness's own bookkeeping on the local filesystem.
		// Without this, `cd` silently stops persisting between tool calls.
		if cwdFile != "" {
			if err := os.WriteFile(cwdFile, []byte(newCwd+"\n"), 0o600); err != nil {
				fmt.Fprintf(os.Stderr, "reach: could not update the harness's record of the working directory: %v\n", err)
			}
		}
	}
	return code
}
