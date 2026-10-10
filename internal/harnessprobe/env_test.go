package harnessprobe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClaudeCodeEnvPinsReachHome pins the session store for the Claude Code
// probe, which replaces HOME. Without REACH_HOME the shim resolved the store
// under the throwaway HOME, found no session, and `reach harness verify claude`
// reported a working seam as an error: "no reach session named ...".
func TestClaudeCodeEnvPinsReachHome(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("REACH_HOME", "")
	os.Unsetenv("REACH_HOME")

	env := claudeCodeEnv("s", "/shim", t.TempDir(), "http://127.0.0.1:1")
	if got, want := lookupEnv(env, "REACH_HOME"), filepath.Join(home, ".reach"); got != want {
		t.Fatalf("REACH_HOME = %q, want %q", got, want)
	}
}

// An operator's own REACH_HOME is inherited as is, and not shadowed by a second
// entry pointing at ~/.reach.
func TestClaudeCodeEnvKeepsOperatorReachHome(t *testing.T) {
	store := t.TempDir()
	t.Setenv("REACH_HOME", store)

	env := claudeCodeEnv("s", "/shim", t.TempDir(), "http://127.0.0.1:1")
	var n int
	for _, kv := range env {
		if strings.HasPrefix(kv, "REACH_HOME=") {
			n++
		}
	}
	if n != 1 || lookupEnv(env, "REACH_HOME") != store {
		t.Fatalf("REACH_HOME entries = %d, value %q, want one entry %q", n, lookupEnv(env, "REACH_HOME"), store)
	}
}

func lookupEnv(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
