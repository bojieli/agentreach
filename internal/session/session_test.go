package session

import (
	"os"
	"testing"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("REACH_HOME", dir)
}

func newTestSession(t *testing.T, name string) *Session {
	t.Helper()
	target, err := ParseTarget("ssh://box/srv/app")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Name: name, Target: target, Mode: ModeExec,
		Created: time.Now(), Tier: reach.TierPOSIX, Timeout: time.Minute,
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	withTempHome(t)
	s := newTestSession(t, "one")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load("one")
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Host != "box" || got.Mode != ModeExec || got.Tier != reach.TierPOSIX {
		t.Errorf("round trip lost data: %+v", got)
	}
}

func TestLoadMissingSessionExplainsHowToFixIt(t *testing.T) {
	withTempHome(t)
	_, err := Load("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	// An error an operator can act on directly is worth more than a correct
	// one they have to look up.
	if !contains(err.Error(), "reach up") {
		t.Errorf("error does not say how to fix it: %v", err)
	}
}

func TestInvalidNamesAreRejected(t *testing.T) {
	withTempHome(t)
	for _, bad := range []string{"", "../escape", "with/slash", "with space", ".hidden"} {
		s := newTestSession(t, bad)
		if err := s.Save(); err == nil {
			t.Errorf("accepted invalid session name %q — path traversal risk", bad)
		}
	}
}

func TestCwdDefaultsToWorkspaceAndPersists(t *testing.T) {
	withTempHome(t)
	s := newTestSession(t, "cwdtest")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got := s.Cwd(); got != "/srv/app" {
		t.Errorf("default cwd = %q want the workspace root", got)
	}
	if err := s.SetCwd("/srv/app/sub"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load("cwdtest")
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Cwd(); got != "/srv/app/sub" {
		t.Errorf("cwd did not persist: %q", got)
	}
}

func TestRemoveClearsCwdToo(t *testing.T) {
	withTempHome(t)
	s := newTestSession(t, "gone")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	_ = s.SetCwd("/srv/app/deep")
	if err := Remove("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("gone"); err == nil {
		t.Error("session still loadable after Remove")
	}
	// A stale cwd left behind would silently apply to a later session of the
	// same name.
	if entries, _ := os.ReadDir(os.Getenv("REACH_HOME") + "/sessions"); len(entries) != 0 {
		t.Errorf("state left behind after Remove: %v", entries)
	}
}

func TestListReturnsAllSessions(t *testing.T) {
	withTempHome(t)
	for _, n := range []string{"a", "b", "c"} {
		if err := newTestSession(t, n).Save(); err != nil {
			t.Fatal(err)
		}
	}
	got, broken, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 0 {
		t.Errorf("List() reported %d broken sessions, want 0", len(broken))
	}
	if len(got) != 3 {
		t.Errorf("List() returned %d sessions want 3", len(got))
	}
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// TestListIgnoresNonSessionJSON is the regression test for a crash: reach
// generates settings documents for harnesses, and when those lived beside
// session state, List() parsed them as sessions with a nil target and
// `reach status` panicked.
func TestListIgnoresNonSessionJSON(t *testing.T) {
	withTempHome(t)
	if err := newTestSession(t, "real").Save(); err != nil {
		t.Fatal(err)
	}
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	// A document that is valid JSON but is not a session.
	if err := os.WriteFile(dir+"/real.claude-settings.json",
		[]byte(`{"permissions":{"deny":["Read"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, broken, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(broken) != 0 {
		t.Errorf("a non-session document was reported as a broken session: %+v", broken)
	}
	if len(got) != 1 {
		t.Fatalf("List() returned %d entries want 1", len(got))
	}
	for _, s := range got {
		if s.Target == nil {
			t.Fatal("List() returned a session with a nil target; callers will panic")
		}
		// Exercise the call that crashed.
		_ = s.Target.Describe()
	}
}

func TestConfDirIsSeparateFromSessions(t *testing.T) {
	withTempHome(t)
	sd, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	cd, err := ConfDir()
	if err != nil {
		t.Fatal(err)
	}
	if sd == cd {
		t.Error("generated config shares a directory with session state; discovery will pick it up")
	}
}

// The control socket is keyed on the destination, so sessions pointed at one
// host share a connection. `reach down` has to know that before tearing it
// down: ending a connection another session is working over turns its next tool
// call into a reconnect, and in batch mode on a host that needs a password or a
// token, into a failure.
func TestSharesConnectionWith(t *testing.T) {
	withTempHome(t)

	save := func(name, target string) *Session {
		t.Helper()
		tgt, err := ParseTarget(target)
		if err != nil {
			t.Fatal(err)
		}
		s := &Session{
			Name: name, Target: tgt, Mode: ModeExec,
			Created: time.Now(), Tier: reach.TierPOSIX, Timeout: time.Minute,
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Same host, different directories: one connection, two sessions.
	build := save("build", "ssh://box/srv/app")
	docs := save("docs", "ssh://box/srv/docs")
	// A different destination, and a kind with no connection to share.
	save("other", "ssh://elsewhere/srv/app")
	save("here", "local:///tmp")

	if got := build.SharesConnectionWith(); len(got) != 1 || got[0] != "docs" {
		t.Errorf("build shares with %v, want [docs]", got)
	}
	if got := docs.SharesConnectionWith(); len(got) != 1 || got[0] != "build" {
		t.Errorf("docs shares with %v, want [build]", got)
	}
	if got := save("alone", "ssh://elsewhere/srv/other").SharesConnectionWith(); len(got) != 1 || got[0] != "other" {
		t.Errorf("alone shares with %v, want [other]", got)
	}

	local, err := Load("here")
	if err != nil {
		t.Fatal(err)
	}
	if got := local.SharesConnectionWith(); got != nil {
		t.Errorf("a local session reported sharing a connection with %v", got)
	}
}

// A different user or port is a different connection, because the control
// socket is keyed on all three.
func TestSharesConnectionDistinguishesCredentials(t *testing.T) {
	withTempHome(t)
	for _, tc := range []struct{ name, target string }{
		{"alice", "ssh://alice@box/srv/app"},
		{"bob", "ssh://bob@box/srv/app"},
		{"alt-port", "ssh://alice@box:2222/srv/app"},
	} {
		tgt, err := ParseTarget(tc.target)
		if err != nil {
			t.Fatal(err)
		}
		s := &Session{
			Name: tc.name, Target: tgt, Mode: ModeExec,
			Created: time.Now(), Tier: reach.TierPOSIX, Timeout: time.Minute,
		}
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Load("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.SharesConnectionWith(); len(got) != 0 {
		t.Errorf("a session shares a connection with %v across a different user or port", got)
	}
}
