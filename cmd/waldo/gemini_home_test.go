package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedGeminiHome_CreatesSettingsJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("WALDO_HOME", tmp)

	dir, err := managedGeminiHome("test-session")
	if err != nil {
		t.Fatalf("managedGeminiHome: %v", err)
	}

	settingsPath := filepath.Join(dir, ".gemini", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("settings.json not created at %s: %v", settingsPath, err)
	}

	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}

	tools, ok := settings["excludeTools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatal("settings.json should have a non-empty excludeTools array")
	}

	excluded := make(map[string]bool, len(tools))
	for _, v := range tools {
		if s, ok := v.(string); ok {
			excluded[s] = true
		}
	}

	// File tools must be excluded so they never act on the local machine.
	for _, want := range []string{"read_file", "write_file", "edit", "glob", "grep", "ls", "read_many_files"} {
		if !excluded[want] {
			t.Errorf("tool %q should be in excludeTools, but it is not", want)
		}
	}
	// The shell tool must NOT be excluded — it is the only tool advertised to the model.
	if excluded["run_shell_command"] {
		t.Error("run_shell_command should not be in excludeTools: it is the routed shell tool")
	}
}

func TestManagedGeminiHome_IsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("WALDO_HOME", tmp)

	dir1, err := managedGeminiHome("test-session")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	dir2, err := managedGeminiHome("test-session")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if dir1 != dir2 {
		t.Fatalf("managedGeminiHome should return the same path on repeated calls: %q vs %q", dir1, dir2)
	}
}

func TestManagedGeminiHome_SessionIsolation(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("WALDO_HOME", tmp)

	dir1, _ := managedGeminiHome("session-a")
	dir2, _ := managedGeminiHome("session-b")
	if dir1 == dir2 {
		t.Fatal("different sessions should have different managed home directories")
	}
}
