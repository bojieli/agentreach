package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedGeminiHome_CreatesSettingsJSON(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("REACH_HOME", tmp)

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

	// All built-in tools except run_shell_command must be excluded.
	// Names are the canonical TOOL_NAME constants from base-declarations.ts —
	// shorthand aliases (e.g. "edit", "grep", "ls", "web_search") are NOT
	// matched by Gemini CLI and must not appear here.
	for _, want := range []string{
		// File-system tools
		"read_file", "write_file", "replace", "glob",
		"grep_search", "list_directory", "read_many_files",
		// Web tools
		"web_fetch", "google_web_search",
		// Todo / skill / internal tools
		"write_todos", "activate_skill", "get_internal_docs",
		// Interactive tools (block headless runs)
		"ask_user", "enter_plan_mode", "exit_plan_mode",
		// Planning tools
		"update_topic", "complete_task",
		// Agent tool
		"invoke_agent",
		// Tracker tools
		"tracker_create_task", "tracker_update_task", "tracker_get_task",
		"tracker_list_tasks", "tracker_add_dependency", "tracker_visualize",
		// MCP resource tools
		"read_mcp_resource", "list_mcp_resources",
	} {
		if !excluded[want] {
			t.Errorf("tool %q should be in excludeTools, but it is not", want)
		}
	}
	// Wrong aliases that were previously (incorrectly) listed — they do not
	// match any tool and if present indicate a regression.
	for _, stale := range []string{"edit", "grep", "ls", "web_search", "memory"} {
		if excluded[stale] {
			t.Errorf("tool %q is a wrong/stale name and should not be in excludeTools (use canonical names)", stale)
		}
	}
	// The shell tool must NOT be excluded — it is the only tool advertised to the model.
	if excluded["run_shell_command"] {
		t.Error("run_shell_command should not be in excludeTools: it is the routed shell tool")
	}
}

func TestManagedGeminiHome_IsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("REACH_HOME", tmp)

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
	t.Setenv("REACH_HOME", tmp)

	dir1, _ := managedGeminiHome("session-a")
	dir2, _ := managedGeminiHome("session-b")
	if dir1 == dir2 {
		t.Fatal("different sessions should have different managed home directories")
	}
}
