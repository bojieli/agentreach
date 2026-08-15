package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/waldo/internal/envelope"
	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// exitTransportFailure is returned when waldo could not run the command at
// all. It is deliberately distinct from any status the command itself might
// produce, so an agent can tell "your command failed" from "waldo could not
// reach the target".
const exitTransportFailure = 125

func cmdExec(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	cmdline := strings.Join(pos, " ")
	if strings.TrimSpace(cmdline) == "" {
		fmt.Fprintln(os.Stderr, "usage: waldo exec [--session N] -- <command>")
		return 2
	}
	return runOnTarget(ctx, sessionNameFromEnv(*name), cmdline, "")
}

// runShellPrefix is the CLAUDE_CODE_SHELL_PREFIX entrypoint.
//
// Claude Code invokes the prefix program with the entire command envelope as a
// single argument. waldo takes that envelope apart, forwards only the portable
// part, and reproduces locally the bookkeeping the harness expects to find on
// the local filesystem.
func runShellPrefix(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "waldo shell-prefix: expected a command argument")
		return 2
	}
	// ssh and the harness both pass the command as one element, but join
	// defensively so a harness that splits it still works.
	raw := strings.Join(args, " ")

	p := envelope.ParseClaudeCode(raw)
	return runOnTarget(context.Background(), sessionNameFromEnv(""), p.Command, p.CwdFile)
}

// runOnTarget executes one command in a session and mirrors the harness's
// working-directory bookkeeping.
func runOnTarget(ctx context.Context, sessionName, command, cwdFile string) int {
	s, err := session.Load(sessionName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return exitTransportFailure
	}
	t, err := s.Transport()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return exitTransportFailure
	}
	// The transport is not closed here: closing tears down the multiplexed
	// master, and this process runs once per tool call. Reuse across calls is
	// the entire performance story.

	cwd := s.Cwd()

	// Ask the target for its resulting directory in the same round trip. The
	// harness tracks `cd` between calls by reading a local file, so waldo has
	// to know where the command ended up regardless of whether the harness
	// asked for it.
	const cwdMarker = "__waldo_cwd__"
	command = fmt.Sprintf("%s\n__waldo_rc=$?; printf '%s%%s\\n' \"$(pwd -P)\" >&2; exit $__waldo_rc", command, cwdMarker)

	stderr := &cwdCapturingWriter{out: os.Stderr, marker: cwdMarker}
	code, err := transport.RunStream(ctx, t, waldo.ExecRequest{
		Command: command,
		Dir:     cwd,
		Timeout: s.Timeout,
	}, os.Stdout, stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return exitTransportFailure
	}

	if newCwd := stderr.Captured(); newCwd != "" {
		_ = s.SetCwd(newCwd)
		// Reproduce the harness's own bookkeeping on the local filesystem.
		// Without this, `cd` silently stops persisting between tool calls.
		if cwdFile != "" {
			_ = os.WriteFile(cwdFile, []byte(newCwd+"\n"), 0o600)
		}
	}
	return code
}
