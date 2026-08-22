package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The profile is the only thing standing between the model and the operator's
// own disk, so the test is about what it removes rather than how it is spelled.
func TestGrokAgentProfileRemovesEveryLocalFileTool(t *testing.T) {
	got := grokAgentProfile("GUIDANCE MARKER")

	for _, tool := range []string{"read_file", "search_replace", "list_dir", "grep", "write"} {
		if !strings.Contains(got, "  - "+tool+"\n") {
			t.Errorf("profile does not disallow %q:\n%s", tool, got)
		}
	}
	if !strings.Contains(got, "  - Agent\n") {
		t.Error("profile does not disallow subagents")
	}
	if !strings.HasPrefix(got, "---\n") || !strings.Contains(got, "\n---\n") {
		t.Errorf("profile is not a frontmatter document:\n%s", got)
	}
	if !strings.Contains(got, "disallowedTools:") {
		t.Errorf("profile has no disallowedTools block:\n%s", got)
	}
	// The guidance has to survive into the body: if grok's profile body
	// replaces the built-in system prompt rather than adding to it, this is
	// the only copy the model sees.
	if !strings.Contains(got, "GUIDANCE MARKER") {
		t.Errorf("profile body dropped the guidance:\n%s", got)
	}
}

// `--agent` pointing at a missing file is not an error to grok — it falls back
// to the default agent, local file tools and all. reach therefore has to have
// actually written the file by the time it returns, not merely computed a path.
func TestManagedGrokAgentProfileWritesTheFile(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())

	path, err := managedGrokAgentProfile("sess", "GUIDANCE MARKER")
	if err != nil {
		t.Fatalf("managedGrokAgentProfile: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("path %q is not absolute; grok resolves it from its own cwd", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("profile was not written: %v", err)
	}
	if !strings.Contains(string(data), "read_file") {
		t.Errorf("written profile is not the rendered one:\n%s", data)
	}

	// Relaunching the same session must not fail on the file it left behind.
	if _, err := managedGrokAgentProfile("sess", "GUIDANCE MARKER"); err != nil {
		t.Fatalf("second call: %v", err)
	}
}
