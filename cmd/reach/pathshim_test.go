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

// TestShellCommandArgStopsAtTheScript covers the second half of the wrapper
// bypass. A harness installed behind a wrapper is started as
// `bash /path/to/wrapper <the harness's own flags>`, and those flags are not
// bash's. codex's begin `-c model_providers.reach.name="reach"`; reach scanned
// the whole argv, took that for its command, and ran a TOML config override on
// somebody's server while never starting the wrapper it was asked to run.
func TestShellCommandArgStopsAtTheScript(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"plain -c", []string{"-c", "hostname"}, "hostname"},
		{"login shell cluster", []string{"-lc", "hostname"}, "hostname"},
		{"interactive cluster", []string{"-ic", "hostname"}, "hostname"},
		{"options before -c", []string{"-e", "-c", "hostname"}, "hostname"},

		{"a script is not a command", []string{"/path/to/wrapper"}, ""},
		{
			"the script's own flags are not bash's",
			[]string{"/path/to/wrapper", "-c", `model_providers.reach.name="reach"`, "exec"},
			"",
		},
		{"-- ends options", []string{"--", "-c", "hostname"}, ""},
		{"- is stdin, not an option", []string{"-", "-c", "hostname"}, ""},
		{"a long option is not a cluster", []string{"--rcfile", "x"}, ""},
		{"-c with nothing after it", []string{"-c"}, ""},
		{"an interactive shell", []string{}, ""},
		{"a version query", []string{"--version"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shellCommandArg(tc.args); got != tc.want {
				t.Errorf("shellCommandArg(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}
