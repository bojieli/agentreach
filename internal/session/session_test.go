package session

import (
	"os"
	"testing"
	"time"

	"github.com/bojieli/waldo/internal/waldo"
)

func withTempHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WALDO_HOME", dir)
}

func newTestSession(t *testing.T, name string) *Session {
	t.Helper()
	target, err := ParseTarget("ssh://box/srv/app")
	if err != nil {
		t.Fatal(err)
	}
	return &Session{
		Name: name, Target: target, Mode: ModeExec,
		Created: time.Now(), Tier: waldo.TierPOSIX, Timeout: time.Minute,
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
	if got.Target.Host != "box" || got.Mode != ModeExec || got.Tier != waldo.TierPOSIX {
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
	if !contains(err.Error(), "waldo up") {
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
	if entries, _ := os.ReadDir(os.Getenv("WALDO_HOME") + "/sessions"); len(entries) != 0 {
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
	got, err := List()
	if err != nil {
		t.Fatal(err)
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
