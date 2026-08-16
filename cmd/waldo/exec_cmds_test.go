package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

// runOnTarget is what every tool call goes through, and its cwd bookkeeping is
// the part that cannot be checked by reading it: `cd` has to survive between
// two processes that share nothing but a file, and the harness has to find the
// result where it expects it. These use a local target, so they exercise the
// real path — envelope, transport, capture, persistence — without a network.

// localSession starts a session against a directory on this machine.
func localSession(t *testing.T, workspace string) *session.Session {
	t.Helper()
	// A local:// target says "this machine is the target", and Windows is
	// deliberately not one: localShell refuses it rather than pick up a stray
	// Git-for-Windows bash, under which MSYS path translation changes what every
	// absolute path means. The target cannot even be spelled here — a Windows
	// temp directory is C:\..., which is not a URL — so these tests have nothing
	// to assert on this platform. What waldo does *on* Windows, driving a remote
	// POSIX host, is covered by platform_test.go and the windows-cli CI job.
	if runtime.GOOS == "windows" {
		t.Skip("a local:// target is unsupported on Windows by design")
	}
	t.Setenv("WALDO_HOME", t.TempDir())

	target, err := session.ParseTarget("local://" + workspace)
	if err != nil {
		t.Fatal(err)
	}
	s := &session.Session{
		Name: "t", Target: target, Mode: session.ModeExec,
		Created: time.Now(), Tier: waldo.TierPOSIX, Timeout: 30 * time.Second,
	}
	if err := s.Probe(context.Background()); err != nil {
		t.Skipf("cannot probe a local target here: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCwd(workspace); err != nil {
		t.Fatal(err)
	}
	return s
}

// quiet sends a command's output somewhere other than the test log.
func quiet(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devnull, devnull
	t.Cleanup(func() {
		os.Stdout, os.Stderr = stdout, stderr
		_ = devnull.Close()
	})
}

// `cd` has to persist between tool calls. Each call is its own process and its
// own connection, so nothing carries it but the session file: without this the
// agent's second command runs somewhere other than where its first one left it,
// and nothing reports that.
func TestRunOnTargetPersistsCwdBetweenCalls(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := localSession(t, ws)
	quiet(t)

	if code := runOnTarget(context.Background(), s.Name, "cd sub", ""); code != 0 {
		t.Fatalf("`cd sub` exited %d", code)
	}
	after, err := session.Load(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(after.Cwd(), "/sub") {
		t.Fatalf("cwd is %q after `cd sub`, want it to end in /sub", after.Cwd())
	}

	// And again, to prove the second call starts where the first ended rather
	// than resetting to the workspace.
	if code := runOnTarget(context.Background(), s.Name, "cd deeper", ""); code != 0 {
		t.Fatalf("`cd deeper` exited %d", code)
	}
	after, err = session.Load(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(after.Cwd(), "/sub/deeper") {
		t.Errorf("cwd is %q, want it to end in /sub/deeper; the second call did not start where the first ended",
			after.Cwd())
	}
}

// Claude Code tracks the working directory by reading a file waldo is told to
// write. The target's directory is authoritative, but the harness never asks
// it — it reads that file, so a correct answer that does not reach the file is
// the same as a wrong one.
func TestRunOnTargetWritesTheHarnessCwdFile(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := localSession(t, ws)
	quiet(t)

	cwdFile := filepath.Join(t.TempDir(), "cwd")
	if code := runOnTarget(context.Background(), s.Name, "cd sub", cwdFile); code != 0 {
		t.Fatalf("exited %d", code)
	}

	data, err := os.ReadFile(cwdFile)
	if err != nil {
		t.Fatalf("the harness's cwd file was not written: %v", err)
	}
	got := strings.TrimSpace(string(data))
	if !strings.HasSuffix(got, "/sub") {
		t.Errorf("cwd file holds %q, want it to end in /sub", got)
	}
	// The harness reads this with a shell; a missing newline concatenates it
	// with whatever it reads next.
	if !strings.HasSuffix(string(data), "\n") {
		t.Errorf("cwd file %q does not end in a newline", data)
	}
}

// A command's exit status is the agent's only signal that it failed. Losing it
// — or replacing it with waldo's own — turns a failed build into a successful
// one as far as the agent can tell.
func TestRunOnTargetPreservesExitStatus(t *testing.T) {
	s := localSession(t, t.TempDir())
	quiet(t)

	for _, want := range []int{0, 1, 3, 42} {
		got := runOnTarget(context.Background(), s.Name, "exit "+strconv.Itoa(want), "")
		if got != want {
			t.Errorf("`exit %d` reported %d", want, got)
		}
	}
}

// A command that fails must not move the recorded directory. If it did, a
// failed `cd nowhere` would leave the session pointing somewhere the agent
// never asked for, and every later command would run there.
func TestRunOnTargetKeepsCwdWhenTheCommandFails(t *testing.T) {
	ws := t.TempDir()
	s := localSession(t, ws)
	quiet(t)

	before, err := session.Load(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if code := runOnTarget(context.Background(), s.Name, "cd /definitely/not/here", ""); code == 0 {
		t.Fatal("cd into a missing directory reported success")
	}
	after, err := session.Load(s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if after.Cwd() != before.Cwd() {
		t.Errorf("a failed cd moved the session from %q to %q", before.Cwd(), after.Cwd())
	}
}

// A session that does not exist has to be a clear failure, not a command that
// runs somewhere else. This is the top of the "which machine am I on" question.
func TestRunOnTargetFailsWithoutASession(t *testing.T) {
	t.Setenv("WALDO_HOME", t.TempDir())
	quiet(t)

	if code := runOnTarget(context.Background(), "no-such-session", "true", ""); code != exitTransportFailure {
		t.Errorf("exit %d for a missing session, want %d; a command must never "+
			"fall through to the local machine", code, exitTransportFailure)
	}
}
