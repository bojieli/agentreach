package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/waldo"
)

// A session file outlives the binary that wrote it. These tests cover what
// happens when the two disagree, which is the case nobody exercises by hand.

// writeRawSession puts a document on disk without going through Save, which is
// the only way to produce the shapes an older or newer build would have left.
func writeRawSession(t *testing.T, name string, doc map[string]any) {
	t.Helper()
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rawSession(name, tier string) map[string]any {
	return map[string]any{
		"name": name,
		"target": map[string]any{
			"kind": "ssh", "host": "h.invalid", "workspace": "/srv/app",
			"raw": "ssh://h.invalid/srv/app",
		},
		"mode": "exec", "tier": tier, "caps": map[string]any{},
	}
}

func TestSaveStampsTheSchemaVersion(t *testing.T) {
	withTempHome(t)
	s := newTestSession(t, "v")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if s.Version != SchemaVersion {
		t.Errorf("Save left Version at %d, want %d", s.Version, SchemaVersion)
	}

	dir, _ := Dir()
	data, err := os.ReadFile(filepath.Join(dir, "v.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["version"] != float64(SchemaVersion) {
		t.Errorf("document has version %v, want %d", doc["version"], SchemaVersion)
	}
}

// A document from the future must be refused, not partly understood.
// encoding/json drops unknown fields without a word, so an older binary would
// otherwise load a session with settings it cannot see — on a tool whose job is
// being certain which machine a command runs on.
func TestLoadRefusesANewerSchema(t *testing.T) {
	withTempHome(t)
	doc := rawSession("future", "posix")
	doc["version"] = SchemaVersion + 1
	writeRawSession(t, "future", doc)

	_, err := Load("future")
	if err == nil {
		t.Fatal("loaded a session written by a newer waldo; its unknown fields would be silently ignored")
	}
	for _, want := range []string{"newer waldo", "waldo down"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// Documents written before the version field existed have version 1's shape,
// so they load as written rather than being thrown away.
func TestLoadAcceptsAPreVersionDocument(t *testing.T) {
	withTempHome(t)
	writeRawSession(t, "old", rawSession("old", "pipe"))

	s, err := Load("old")
	if err != nil {
		t.Fatalf("a pre-version session should still load: %v", err)
	}
	if s.Tier != waldo.TierPipe {
		t.Errorf("tier is %v, want pipe", s.Tier)
	}
	if s.Version != SchemaVersion {
		t.Errorf("version is %d, want it brought up to %d", s.Version, SchemaVersion)
	}
}

// The bug this replaces: Load discarded ParseTier's error, so a session created
// with a tier that no longer exists loaded as posix — with Pinned still true.
// waldo would then run a tier other than the one it was instructed to use and
// report the tier it was told, which is the exact failure this project refuses.
func TestLoadRefusesARemovedTierRatherThanSilentlyDowngrading(t *testing.T) {
	withTempHome(t)
	for _, tc := range []struct{ tier, want string }{
		{"sftp", "removed"},
		{"agent", "now called helper"},
		{"nonsense", "unknown fileops tier"},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			doc := rawSession("s", tc.tier)
			doc["pinned"] = true
			writeRawSession(t, "s", doc)

			s, err := Load("s")
			if err == nil {
				t.Fatalf("loaded tier %q as %v with pinned=%v; waldo would run a tier "+
					"it was not asked for and report the one it was told",
					tc.tier, s.Tier, s.Pinned)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not explain %q: %v", tc.tier, err)
			}
			// An error that does not say what to do next leaves the operator
			// with a session they cannot use and no way forward.
			if !strings.Contains(err.Error(), "waldo up") {
				t.Errorf("error does not say how to recover: %v", err)
			}
		})
	}
}

// Nothing recorded is different from a recorded name this build cannot honour.
// posix is the floor: it installs nothing and needs only a shell.
func TestLoadTreatsAnAbsentTierAsPOSIX(t *testing.T) {
	withTempHome(t)
	doc := rawSession("bare", "")
	delete(doc, "tier")
	writeRawSession(t, "bare", doc)

	s, err := Load("bare")
	if err != nil {
		t.Fatalf("a session with no recorded tier should load: %v", err)
	}
	if s.Tier != waldo.TierPOSIX {
		t.Errorf("tier is %v, want posix", s.Tier)
	}
}

// A session that will not load is still configured in somebody's harness.
// Dropping it from the listing prints "no waldo sessions" to an operator whose
// agent is at that moment pointed at one.
func TestListReportsUnloadableSessions(t *testing.T) {
	withTempHome(t)
	if err := newTestSession(t, "good").Save(); err != nil {
		t.Fatal(err)
	}
	writeRawSession(t, "stale", rawSession("stale", "sftp"))

	got, broken, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "good" {
		t.Errorf("List returned %d usable sessions, want just \"good\"", len(got))
	}
	if len(broken) != 1 || broken[0].Name != "stale" {
		t.Fatalf("List reported %+v as broken, want just \"stale\"", broken)
	}
	if !strings.Contains(broken[0].Err.Error(), "sftp") {
		t.Errorf("broken entry does not say why: %v", broken[0].Err)
	}
}

// Round-tripping must not lose the version, or every load-modify-save cycle
// would rewrite the document as if it were brand new.
func TestVersionSurvivesARoundTrip(t *testing.T) {
	withTempHome(t)
	if err := newTestSession(t, "rt").Save(); err != nil {
		t.Fatal(err)
	}
	s, err := Load("rt")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load("rt")
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != SchemaVersion || again.Tier != s.Tier {
		t.Errorf("round trip changed the session: version %d tier %v, want %d %v",
			again.Version, again.Tier, SchemaVersion, s.Tier)
	}
}
