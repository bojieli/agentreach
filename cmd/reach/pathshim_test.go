package main

import (
	"os"
	"testing"

	"github.com/bojieli/agentreach/internal/session"
)

func testSession(t *testing.T, workspace string) *session.Session {
	t.Helper()
	target, err := session.ParseTarget("ssh://box" + workspace)
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	return &session.Session{Name: "test", Target: target, Mode: session.ModeExec}
}

func TestMapEmbeddedCwdMapsWorkspaceRoot(t *testing.T) {
	t.Setenv("REACH_EXEC_WORKSPACE", "/Users/op/proj")
	sess := testSession(t, "/srv/app")
	got := mapEmbeddedCwd(sess, "cd '/Users/op/proj' && hostname")
	want := "cd /srv/app && hostname"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMapEmbeddedCwdMapsWorkspaceSubdir(t *testing.T) {
	t.Setenv("REACH_EXEC_WORKSPACE", "/Users/op/proj")
	sess := testSession(t, "/srv/app")
	got := mapEmbeddedCwd(sess, `cd "/Users/op/proj/sub/dir" && ls`)
	want := "cd /srv/app/sub/dir && ls"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMapEmbeddedCwdLeavesTargetPathsAlone(t *testing.T) {
	t.Setenv("REACH_EXEC_WORKSPACE", "/Users/op/proj")
	sess := testSession(t, "/srv/app")
	for _, cmd := range []string{
		"cd '/var/log' && ls",
		"cd /etc && cat hostname",
		"hostname",
		"cd && pwd",
		"echo 'cd /x && y'",
	} {
		if got := mapEmbeddedCwd(sess, cmd); got != cmd {
			t.Errorf("mapEmbeddedCwd(%q) = %q, want unchanged", cmd, got)
		}
	}
}

func TestMapEmbeddedCwdHandlesPrivateTmp(t *testing.T) {
	if _, err := os.Stat("/private/tmp"); err != nil {
		t.Skip("not macOS")
	}
	t.Setenv("REACH_EXEC_WORKSPACE", "/tmp")
	sess := testSession(t, "/srv/app")
	got := mapEmbeddedCwd(sess, "cd '/private/tmp' && hostname")
	want := "cd /srv/app && hostname"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMapEmbeddedCwdWithoutWorkspaceEnv(t *testing.T) {
	t.Setenv("REACH_EXEC_WORKSPACE", "")
	sess := testSession(t, "/srv/app")
	cmd := "cd '/Users/op/proj' && hostname"
	if got := mapEmbeddedCwd(sess, cmd); got != cmd {
		t.Errorf("got %q, want unchanged when REACH_EXEC_WORKSPACE is unset", got)
	}
}

func TestSplitCdPrefixShapes(t *testing.T) {
	cases := []struct {
		in       string
		dir, res string
		ok       bool
	}{
		{"cd '/a b' && ls", "/a b", "ls", true},
		{`cd "/a b" && ls`, "/a b", "ls", true},
		{"cd /a && ls", "/a", "ls", true},
		{"cd  /a  &&  ls -la", "/a", "ls -la", true},
		{"cd /a &&", "", "", false},
		{"cd /a; ls", "", "", false},
		{"cd", "", "", false},
		{"cdx /a && ls", "", "", false},
	}
	for _, c := range cases {
		dir, rest, ok := splitCdPrefix(c.in)
		if dir != c.dir || rest != c.res || ok != c.ok {
			t.Errorf("splitCdPrefix(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, dir, rest, ok, c.dir, c.res, c.ok)
		}
	}
}
