package harnessprobe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Claude Code probe replaces HOME with a throwaway directory. The shell
// prefix it launches resolves the session store from REACH_HOME, falling back
// to $HOME/.reach, so without REACH_HOME pinned to the operator's store it
// looks under the throwaway home, finds no session, and the probe reports an
// inconclusive verdict for a seam that works.

func envValues(env []string, key string) []string {
	var vals []string
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			vals = append(vals, v)
		}
	}
	return vals
}

func TestClaudeCodeEnvPinsReachHome(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	t.Setenv("REACH_HOME", "")
	os.Unsetenv("REACH_HOME")

	env := claudeCodeEnv("probe", "/bin/prefix", t.TempDir(), "http://127.0.0.1:1")

	want := filepath.Join(realHome, ".reach")
	if got := envValues(env, "REACH_HOME"); len(got) != 1 || got[0] != want {
		t.Fatalf("REACH_HOME = %q, want exactly [%q]", got, want)
	}
	if got := envValues(env, "HOME"); len(got) != 1 || got[0] == realHome {
		t.Fatalf("HOME = %q, want one throwaway home, not %q", got, realHome)
	}
}

func TestClaudeCodeEnvKeepsOperatorReachHome(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("REACH_HOME", custom)

	env := claudeCodeEnv("probe", "/bin/prefix", t.TempDir(), "http://127.0.0.1:1")

	if got := envValues(env, "REACH_HOME"); len(got) != 1 || got[0] != custom {
		t.Fatalf("REACH_HOME = %q, want exactly [%q]", got, custom)
	}
}
