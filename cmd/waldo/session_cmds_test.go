package main

import (
	"context"
	"encoding/json"
	"io"
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

// `waldo status NAME` shows one session, which is what the help has always
// said. Accepting the argument and listing everything anyway looked like it had
// printed the one that was asked for.
func TestStatusNamesOneSession(t *testing.T) {
	dir := tempHome(t)
	writeSessionDoc(t, dir, "prod", map[string]any{
		"name": "prod", "mode": "exec", "tier": "posix",
		"target": map[string]any{"kind": "ssh", "host": "prod.invalid", "workspace": "/srv/app", "raw": "ssh://prod.invalid/srv/app"},
	})
	quiet(t)

	if err := cmdStatus(context.Background(), []string{"prod"}); err != nil {
		t.Fatalf("status prod: %v", err)
	}
	// A name that is not a session must say so rather than falling back to
	// listing everything, which would look like an answer.
	err := cmdStatus(context.Background(), []string{"no-such-session"})
	if err == nil {
		t.Fatal("status accepted a session name that does not exist")
	}
	if !strings.Contains(err.Error(), "no-such-session") {
		t.Errorf("error does not name the session: %v", err)
	}
}

// An unknown flag must not be swallowed. status has none, so `waldo status
// --name prod` used to list everything while looking like it printed one.
func TestStatusRejectsUnknownFlags(t *testing.T) {
	tempHome(t)
	quiet(t)

	if err := cmdStatus(context.Background(), []string{"--name", "prod"}); err == nil {
		t.Fatal("status accepted and ignored an unknown flag")
	}
	if err := cmdStatus(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("status accepted two session names")
	}
}

// Every command that acts on a session takes its name the same way. They did
// not: `waldo env --session prod` failed with "flag provided but not defined"
// while `waldo log --session prod` worked, so the right spelling depended on
// which command you happened to be running, and the error blamed the operator
// for a flag the tool uses everywhere else.
func TestSessionNameIsAcceptedTheSameWayEverywhere(t *testing.T) {
	dir := tempHome(t)
	writeSessionDoc(t, dir, "prod", map[string]any{
		"name": "prod", "mode": "exec", "tier": "posix",
		"target": map[string]any{"kind": "ssh", "host": "prod.invalid", "workspace": "/srv/app", "raw": "ssh://prod.invalid/srv/app"},
	})
	quiet(t)

	// Commands that only read local state, so no target is contacted.
	commands := map[string]func(context.Context, []string) error{
		"status": cmdStatus,
		"env":    cmdEnv,
		"log":    cmdLog,
		"down":   cmdDown,
	}
	for name, run := range commands {
		for _, form := range [][]string{
			{"prod"},
			{"--session", "prod"},
			{"--session=prod"},
		} {
			// The assertion is about the *name* being understood, not about the
			// command having something to report: `waldo log prod` legitimately
			// says there is no audit log yet for a session that has never run
			// anything. A rejected flag is the failure this is guarding.
			err := run(context.Background(), form)
			if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
				t.Errorf("waldo %s %v: %v", name, form, err)
			}
			// down removes the session, so put it back for the next form.
			if name == "down" {
				writeSessionDoc(t, dir, "prod", map[string]any{
					"name": "prod", "mode": "exec", "tier": "posix",
					"target": map[string]any{"kind": "ssh", "host": "prod.invalid", "workspace": "/srv/app", "raw": "ssh://prod.invalid/srv/app"},
				})
			}
		}
	}
}

// A bare `waldo status` lists everything even inside a harness's shell, where
// $WALDO_SESSION is set. Narrowing to one session there would hide the others
// at exactly the moment someone is checking what is running.
func TestStatusIgnoresTheAmbientSession(t *testing.T) {
	dir := tempHome(t)
	for _, n := range []string{"one", "two"} {
		writeSessionDoc(t, dir, n, map[string]any{
			"name": n, "mode": "exec", "tier": "posix",
			"target": map[string]any{"kind": "ssh", "host": "h.invalid", "workspace": "/srv/app", "raw": "ssh://h.invalid/srv/app"},
		})
	}
	t.Setenv("WALDO_SESSION", "one")

	out := captureStdout(t, func() {
		if err := cmdStatus(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"one", "two"} {
		if !strings.Contains(out, want) {
			t.Errorf("status omitted %q with WALDO_SESSION set:\n%s", want, out)
		}
	}
}

// captureStdout collects what a function prints.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	fn()
	os.Stdout = saved
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
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
