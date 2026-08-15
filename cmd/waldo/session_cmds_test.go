package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/waldo/internal/session"
)

func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WALDO_HOME", home)
	dir, err := session.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeSessionDoc writes a session file directly, which is the only way to
// produce documents an older or newer build would have left behind.
func writeSessionDoc(t *testing.T, dir, name string, doc map[string]any) string {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A session that cannot be loaded must still be removable.
//
// It was not: cmdDown loaded the session first and returned the error, so
// `down` refused exactly the sessions most in need of removing — one written by
// a newer waldo, one naming a tier that no longer exists. Both of those
// failures suggest `waldo down` as the way out, which made the advice a loop.
// The operator's only remaining move was deleting a file out of ~/.waldo by
// hand.
func TestDownRemovesASessionItCannotLoad(t *testing.T) {
	dir := tempHome(t)

	for _, tc := range []struct {
		name string
		doc  map[string]any
	}{
		{"newer schema", map[string]any{
			"version": 9999, "name": "future", "mode": "exec", "tier": "posix",
			"target": map[string]any{"kind": "ssh", "host": "h.invalid", "workspace": "/srv/app", "raw": "ssh://h.invalid/srv/app"},
		}},
		{"removed tier", map[string]any{
			"name": "stale", "mode": "exec", "tier": "sftp", "pinned": true,
			"target": map[string]any{"kind": "ssh", "host": "h.invalid", "workspace": "/srv/app", "raw": "ssh://h.invalid/srv/app"},
		}},
		{"corrupt json", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "broken"
			p := filepath.Join(dir, name+".json")
			if tc.doc == nil {
				if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				writeSessionDoc(t, dir, name, tc.doc)
			}

			if err := cmdDown(context.Background(), []string{name}); err != nil {
				t.Fatalf("down refused to remove an unloadable session: %v", err)
			}
			if _, err := os.Stat(p); !os.IsNotExist(err) {
				t.Errorf("%s still exists after `waldo down`", p)
			}
		})
	}
}

// Removing something that was never there is a mistake worth reporting. If
// down succeeded silently, a typo in the session name would look like a
// successful cleanup while the real session stayed live.
func TestDownReportsAMissingSession(t *testing.T) {
	tempHome(t)

	err := cmdDown(context.Background(), []string{"never-existed"})
	if err == nil {
		t.Fatal("down reported success for a session that does not exist")
	}
	if !strings.Contains(err.Error(), "never-existed") {
		t.Errorf("error does not name the session: %v", err)
	}
}

// status lists every session and takes no flags. Quietly ignoring them let
// `waldo status --name prod` print all sessions while looking like it printed
// one.
func TestStatusRejectsArguments(t *testing.T) {
	tempHome(t)

	err := cmdStatus(context.Background(), []string{"--name", "prod"})
	if err == nil {
		t.Fatal("status accepted and ignored its arguments")
	}
	if !strings.Contains(err.Error(), "no arguments") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

func TestIndentedLaysContinuationsUnderTheFirstLine(t *testing.T) {
	got := indented("first\nsecond\nthird\n")
	if got != "  first\n  second\n  third\n" {
		t.Errorf("indented() = %q", got)
	}
	if got := indented("only"); got != "  only\n" {
		t.Errorf("indented(single line) = %q", got)
	}
}
