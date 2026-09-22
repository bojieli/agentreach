package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/bojieli/agentreach/internal/envelope"
)

// Claude Code runs its hook commands through CLAUDE_CODE_SHELL_PREFIX, the same
// seam it uses for Bash tool calls. reach forwarded them to the target, where a
// hook naming a local path cannot exist:
//
//	bash: line 1: /Users/…/.local/bin/reach: No such file or directory
//
// That is reach's own PreToolUse hook failing — the one that tells Claude Code
// a command naming /srv/app is fine because /srv/app is on the target — so exec
// mode quietly lost the thing that makes it usable, and every operator hook
// went to the wrong machine with its stdin payload in tow.
//
// These drive the real binary through its shim alias, because the mechanism is
// argv[0] dispatch and process replacement; there is nothing to observe from
// inside the test process.

// prefixAlias builds reach and returns the path to a reach-shell-prefix alias
// of it, which is how Claude Code is pointed at reach.
func prefixAlias(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the alias is a symlink on POSIX; Windows copies the binary instead")
	}
	dir := t.TempDir()

	reachBin := filepath.Join(dir, "reach")
	build := exec.Command("go", "build", "-o", reachBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build reach: %v\n%s", err, out)
	}
	alias := filepath.Join(dir, shimName)
	if err := os.Symlink(reachBin, alias); err != nil {
		t.Fatal(err)
	}
	return alias
}

// hookEnv is the environment Claude Code hands a hook command: a session reach
// is engaged for, and CLAUDE_PROJECT_DIR, which it sets for hooks and not for
// tool calls.
func hookEnv(t *testing.T, reachHome string) []string {
	t.Helper()
	return append(os.Environ(),
		"REACH_HOME="+reachHome,
		"REACH_SESSION=absent",
		claudeProjectDirEnv+"="+t.TempDir(),
	)
}

// A hook must run here, on the machine whose paths it names. The session is
// deliberately missing: a hook does not need one, and needing one is exactly
// the bug — reach used to take the hook down the transport path, where a
// missing session is a hard failure and a present one is worse.
func TestHookCommandRunsOnTheLocalMachine(t *testing.T) {
	alias := prefixAlias(t)
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "hook-ran")

	cmd := exec.Command(alias, "touch "+marker)
	cmd.Env = hookEnv(t, home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the hook did not run: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("the hook produced nothing on this machine (%v); it was sent to the target\n%s",
			statErr, out)
	}
}

// A hook's exit status is its answer. Claude Code reads 0 as "no opinion" and
// 2 as "block", so a status reach invents in place of the hook's own would
// make the harness act on a decision nobody took.
func TestHookExitStatusIsThePrefixExitStatus(t *testing.T) {
	alias := prefixAlias(t)
	home := t.TempDir()

	cmd := exec.Command(alias, "exit 2")
	cmd.Env = hookEnv(t, home)
	var ee *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &ee) {
		t.Fatalf("a hook exiting 2 reported %v", err)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("hook exit status = %d, want 2", ee.ExitCode())
	}
}

// The dangerous direction. A hook wrongly sent to the target is visible and
// recoverable; an agent's command wrongly run here executes on the operator's
// own machine while the agent reports it as remote, which is the whole reason
// reach exists. So a tool call must go to the target even when the environment
// carries every hook signal there is — and with no session to reach, that means
// failing rather than falling through to this machine.
func TestToolCallNeverFallsThroughToTheLocalMachine(t *testing.T) {
	alias := prefixAlias(t)
	home := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran-locally")

	envelopeArg := `{ shopt -u extglob || setopt NO_EXTENDED_GLOB NO_BARE_GLOB_QUAL; } >/dev/null 2>&1 || true` +
		` && eval 'touch ` + marker + `' < /dev/null` +
		` && pwd -P >| ` + filepath.Join(t.TempDir(), "cwd")

	cmd := exec.Command(alias, envelopeArg)
	cmd.Env = hookEnv(t, home)
	out, err := cmd.CombinedOutput()

	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("a tool call with no reachable session exited cleanly: %v\n%s", err, out)
	}
	if ee.ExitCode() != exitTransportFailure {
		t.Errorf("tool call exit = %d, want %d (the transport failure)\n%s",
			ee.ExitCode(), exitTransportFailure, out)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("the agent's command ran on the operator's machine:\n%s", out)
	}
}

// Both signals, not either. A bare command with no hook environment is the
// shape a tool call takes if Claude Code ever drops the prelude, and guessing
// "local" there is the failure above.
func TestIsLocalCallbackNeedsBothSignals(t *testing.T) {
	bare := envelope.ParseClaudeCode("/opt/bin/notify")
	wrapped := envelope.ParseClaudeCode(
		`{ shopt -u extglob; } >/dev/null 2>&1 || true && eval 'ls' < /dev/null`)

	t.Setenv(claudeProjectDirEnv, "")
	if isLocalCallback(bare) {
		t.Error("a bare command with no hook environment was claimed as a hook")
	}

	t.Setenv(claudeProjectDirEnv, t.TempDir())
	if !isLocalCallback(bare) {
		t.Error("a bare command in a hook environment was not recognised as a hook")
	}
	if isLocalCallback(wrapped) {
		t.Error("an envelope was claimed as a hook because the environment looked like one")
	}
}
