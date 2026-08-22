package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/session"
)

// knownCommands decides whether a word is a command or a hostname, so a
// command missing from it would be looked up in DNS and a word listed in it
// that dispatch does not handle would exit zero having done nothing. Both are
// silent, and both are one forgotten line away, so the map and the switch are
// read out of the source and compared.
func TestDispatchAndKnownCommandsAgree(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				name, err := strconv.Unquote(lit.Value)
				if err == nil {
					cases[name] = true
				}
			}
			return true
		})
		return false
	})
	if len(cases) == 0 {
		t.Fatal("found no cases in dispatch; this test can no longer see what it checks")
	}
	for name := range cases {
		if !knownCommands[name] {
			t.Errorf("dispatch handles %q but knownCommands omits it: it would be read as a hostname", name)
		}
	}
	for name := range knownCommands {
		if !cases[name] {
			t.Errorf("knownCommands lists %q but dispatch does not handle it: it would exit 0 doing nothing", name)
		}
	}
}

func TestResolveTargetSpec(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	if err := os.WriteFile(config, []byte("Host build-box\n  HostName 10.0.0.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REACH_SSH_CONFIG", config)

	for _, tc := range []struct {
		spec      string
		kind      session.Kind
		host      string
		workspace string
	}{
		{"ssh://box/srv/app", session.KindSSH, "box", "/srv/app"},
		{"box:/srv/app", session.KindSSH, "box", "/srv/app"},
		{"local:///tmp/x", session.KindLocal, "", "/tmp/x"},
		// A bare alias is a target because the operator's own ssh config says
		// it is a machine. The absent path is resolved by Probe.
		{"build-box", session.KindSSH, "build-box", ""},
		{"root@build-box", session.KindSSH, "build-box", ""},
	} {
		got, err := resolveTargetSpec(tc.spec)
		if err != nil {
			t.Errorf("resolveTargetSpec(%q): %v", tc.spec, err)
			continue
		}
		if got.Kind != tc.kind || got.Host != tc.host || got.Workspace != tc.workspace {
			t.Errorf("resolveTargetSpec(%q) = %+v", tc.spec, got)
		}
	}
}

// A mistyped command must not become a connection attempt.
func TestResolveTargetSpecRejectsAWordThatIsNoMachine(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config")
	// `Host *` matches everything, which is why it is ignored: honouring it
	// would make every typo a hostname, and nearly every ssh config has one.
	if err := os.WriteFile(config, []byte("Host *\n  ServerAliveInterval 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REACH_SSH_CONFIG", config)

	if _, err := resolveTargetSpec("stauts"); err == nil {
		t.Error("a word no ssh config names was accepted as a target")
	}
	// A dotted name cannot be a reach command, so it goes to ssh, which is the
	// thing that actually knows whether the name resolves.
	if _, err := resolveTargetSpec("build.example.com"); err != nil {
		t.Errorf("a dotted name was refused: %v", err)
	}
	if _, err := resolveTargetSpec("10.0.0.9"); err != nil {
		t.Errorf("an address was refused: %v", err)
	}
}

func TestSplitTargetArgs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		fresh bool
		mode  string
		rest  []string
	}{
		{"bare command", []string{"claude"}, false, "exec", []string{"claude"}},
		{"session flag first", []string{"--fresh", "claude"}, true, "exec", []string{"claude"}},
		{"flag with a value", []string{"--mode", "mirror", "claude"}, false, "mirror", []string{"claude"}},
		// Everything from the command onwards is the command's, including
		// flags reach itself defines.
		{"command flags pass through", []string{"claude", "--resume", "--mode", "x"},
			false, "exec", []string{"claude", "--resume", "--mode", "x"}},
		{"exec passthrough", []string{"exec", "--", "go", "test", "./..."},
			false, "exec", []string{"exec", "--", "go", "test", "./..."}},
		{"no command", nil, false, "exec", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, rest, err := splitTargetArgs(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if o.fresh != tc.fresh || o.mode != tc.mode {
				t.Errorf("options = %+v", o)
			}
			if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
				t.Errorf("rest = %v, want %v", rest, tc.rest)
			}
		})
	}
}

// A flag reach used to have must say it is gone. Left to the flag package it
// would be "flag provided but not defined: -untrusted" and a usage dump, which
// reads like reach forgot a flag it still supports — and the operator would be
// left believing they had asked for something.
func TestRemovedFlagExplainsItself(t *testing.T) {
	_, _, err := splitTargetArgs([]string{"--untrusted", "claude"})
	if err == nil {
		t.Fatal("--untrusted was accepted")
	}
	for _, want := range []string{"removed", "--fileops=helper"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message does not mention %q: %v", want, err)
		}
	}

	// After the command, the flag is the command's to refuse.
	if _, rest, err := splitTargetArgs([]string{"claude", "--untrusted"}); err != nil {
		t.Errorf("reach answered for a flag that belongs to claude: %v", err)
	} else if len(rest) != 2 {
		t.Errorf("rest = %v, want the command and its flag", rest)
	}

	// `up` owns its whole line, so position does not matter there.
	if err := removedFlag([]string{"ssh://box/srv/app", "--untrusted"}, false); err == nil {
		t.Error("reach up accepted --untrusted after its target")
	}
}

func TestDeriveSessionName(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"ssh://build-box/srv/app", "build-box-app"},
		{"ssh://build-box", "build-box"},
		{"ssh://build.example.com/opt/site", "build.example.com-site"},
		{"root@10.0.0.9:/srv/app", "10.0.0.9-app"},
		{"docker://c1/work", "c1-work"},
		{"local:///tmp/x", "local-x"},
	} {
		target, err := session.ParseTarget(tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got := deriveSessionName(target); got != tc.want {
			t.Errorf("deriveSessionName(%q) = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// Two targets that derive the same name are still two targets. Handing them
// one session would point an agent at a machine its operator did not name.
func TestPickSessionNameKeepsTargetsApart(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())

	save := func(name, spec string) {
		t.Helper()
		target, err := session.ParseTarget(spec)
		if err != nil {
			t.Fatal(err)
		}
		s := &session.Session{
			Name: name, Target: target, Mode: session.ModeExec,
			Created: time.Now(), Tier: reach.TierPOSIX, Timeout: time.Minute,
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	target := func(spec string) *session.Target {
		t.Helper()
		got, err := session.ParseTarget(spec)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Nothing saved yet: the derived name is free.
	if got := pickSessionName("box-app", target("ssh://box/srv/app")); got != "box-app" {
		t.Errorf("first use took %q", got)
	}
	save("box-app", "ssh://box/srv/app")

	// The same target again is the same session, which is what makes running
	// a second command against a box free.
	if got := pickSessionName("box-app", target("ssh://box/srv/app")); got != "box-app" {
		t.Errorf("reuse took %q, want box-app", got)
	}
	// A path with no path given matches whatever the session resolved.
	if got := pickSessionName("box", target("ssh://box")); got != "box" {
		t.Errorf("a pathless target took %q, want its own name", got)
	}
	// A different directory on the same host derives the same name and must
	// not take it.
	if got := pickSessionName("box-app", target("ssh://box/opt/app")); got != "box-app-2" {
		t.Errorf("a second target took %q, want box-app-2", got)
	}
}

func TestSameTarget(t *testing.T) {
	parse := func(spec string) *session.Target {
		t.Helper()
		got, err := session.ParseTarget(spec)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	for _, tc := range []struct {
		have, want string
		same       bool
	}{
		{"ssh://box/srv/app", "ssh://box/srv/app", true},
		{"ssh://box/srv/app", "ssh://box/opt/app", false},
		{"ssh://box/srv/app", "ssh://other/srv/app", false},
		{"ssh://root@box/srv/app", "ssh://box/srv/app", false},
		{"ssh://box:22/srv/app", "ssh://box/srv/app", false},
		// An unstated workspace matches the one the session resolved: asking
		// the host again for a directory it already answered for costs a round
		// trip to learn nothing.
		{"ssh://box/home/me", "ssh://box", true},
	} {
		if got := sameTarget(parse(tc.have), parse(tc.want)); got != tc.same {
			t.Errorf("sameTarget(%q, %q) = %v", tc.have, tc.want, got)
		}
	}
}

// A flag the operator typed disagreeing with the session on disk means the
// session has to be built again. A flag they did not type is a default, and a
// default must never overrule a session created deliberately.
func TestOptionsAgree(t *testing.T) {
	s := &session.Session{
		Mode: session.ModeMirror,
		Tier: reach.TierPipe, Pinned: true, Timeout: time.Minute,
	}
	defaults := &sessionOptions{mode: string(session.ModeExec), set: map[string]bool{}}
	if !optionsAgree(s, defaults) {
		t.Error("untyped defaults disagreed with an existing session")
	}
	same := &sessionOptions{
		mode: string(session.ModeMirror), tier: "pipe", timeout: time.Minute,
		set: map[string]bool{"mode": true, "fileops": true, "timeout": true},
	}
	if !optionsAgree(s, same) {
		t.Error("flags matching the session were reported as a disagreement")
	}
	for name, o := range map[string]*sessionOptions{
		"mode":    {mode: string(session.ModeExec), set: map[string]bool{"mode": true}},
		"fileops": {tier: "posix", set: map[string]bool{"fileops": true}},
		"timeout": {timeout: time.Hour, set: map[string]bool{"timeout": true}},
	} {
		if optionsAgree(s, o) {
			t.Errorf("--%s was typed with a different value and the session was reused anyway", name)
		}
	}
}

// Binding a session must not open a connection for a command that never
// leaves this machine: on a host with a hardware token, `reach build-box log`
// would ask for a touch to read a local file.
func TestCommandNeedsTarget(t *testing.T) {
	for _, tc := range []struct {
		rest []string
		want bool
	}{
		{nil, true}, // no command: this is `reach up`, and the connection is the point
		{[]string{"claude"}, true},
		{[]string{"exec", "--", "ls"}, true},
		{[]string{"doctor"}, true},
		{[]string{"log"}, false},
		{[]string{"status"}, false},
		{[]string{"env"}, false},
	} {
		if got := commandNeedsTarget(tc.rest); got != tc.want {
			t.Errorf("commandNeedsTarget(%v) = %v, want %v", tc.rest, got, tc.want)
		}
	}
}

// The whole point of the form, end to end: one command line, no `up`, and the
// command runs on the target with a session named after it left behind for
// `reach status` and `reach down`.
func TestTargetFirstBindsASessionAndRunsTheCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a local:// target is unsupported on Windows by design")
	}
	t.Setenv("REACH_HOME", t.TempDir())
	t.Setenv("REACH_SESSION", "")
	quiet(t)

	ws := t.TempDir()
	marker := filepath.Join(ws, "here")
	if err := os.WriteFile(marker, []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dispatch(context.Background(), []string{"local://" + ws, "exec", "--", "ls", "here"}); code != 0 {
		t.Fatalf("dispatch exited %d", code)
	}

	name := deriveSessionName(&session.Target{Kind: session.KindLocal, Workspace: ws})
	s, err := session.Load(name)
	if err != nil {
		t.Fatalf("no session was left behind under %q: %v", name, err)
	}
	if s.Target.Workspace != ws {
		t.Errorf("session workspace = %q, want %q", s.Target.Workspace, ws)
	}
	if os.Getenv("REACH_SESSION") != name {
		t.Errorf("REACH_SESSION = %q, want %q: the command would resolve a different session",
			os.Getenv("REACH_SESSION"), name)
	}
}

// A word that is neither a command nor a machine has to say both things. It is
// almost always a mistyped command, and a message about hostnames alone would
// send the operator looking in the wrong place.
func TestUnknownWordIsReportedAsBothReadings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REACH_SSH_CONFIG", filepath.Join(dir, "config"))

	stderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	code := dispatch(context.Background(), []string{"stauts"})
	os.Stderr = stderr
	_ = w.Close()
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()
	msg := string(buf[:n])

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	for _, want := range []string{"unknown command", "reach help", "ssh://"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n%s", want, msg)
		}
	}
}
