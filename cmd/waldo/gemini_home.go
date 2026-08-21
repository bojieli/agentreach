package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// waldoGeminiSettingsMarker is written into settings.json so `waldo doctor`
// can confirm the file was written by waldo, not hand-edited by the user.
const waldoGeminiSettingsMarker = "waldo-managed"

// managedGeminiHome creates (or refreshes) the managed HOME directory waldo
// launches gemini with. It returns the directory path.
//
// The managed directory acts as a substitute for the operator's real home:
// Gemini CLI reads its configuration from HOME/.gemini/settings.json, so
// waldo sets HOME to this directory. The settings.json it contains excludes
// every Gemini built-in tool except run_shell_command, which routes through
// the PATH shim and executes on the session target. The full deny-list covers
// file tools, web tools, todo/skill/agent tools, interactive blockers, and
// tracker tools — every name is the canonical TOOL_NAME constant from
// packages/core/src/tools/definitions/base-declarations.ts.
//
// Credential files from the operator's real ~/.gemini are symlinked into the
// managed directory so that Gemini's API key and authentication tokens remain
// available without being copied (a symlink that drifts is still live; a copy
// that drifts is stale).
func managedGeminiHome(sessName string) (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "gemini-home", sessName)
	geminiDir := filepath.Join(dir, ".gemini")
	if err := os.MkdirAll(geminiDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", geminiDir, err)
	}

	// Write the controlled settings.json.
	if err := writeManagedGeminiSettings(geminiDir); err != nil {
		return "", err
	}

	// Symlink credential files from the real ~/.gemini so Gemini can
	// authenticate. Missing files are silently skipped; GEMINI_API_KEY in the
	// environment is the common case and needs no file.
	//
	// Use os.UserHomeDir() as the primary source — it handles all platforms and
	// the case where HOME is unset, relative, or garbage. Fall back to
	// os.Getenv("HOME") only when UserHomeDir itself fails (highly unlikely).
	realHome, err := os.UserHomeDir()
	if err != nil {
		realHome = os.Getenv("HOME") // last resort
	}
	realGeminiDir := filepath.Join(realHome, ".gemini")
	for _, f := range []string{
		"google-accounts.json",
		"installation_id",
	} {
		src := filepath.Join(realGeminiDir, f)
		dst := filepath.Join(geminiDir, f)
		if _, err := os.Lstat(src); err != nil {
			continue // source absent — skip
		}
		if _, err := os.Lstat(dst); err == nil {
			continue // already linked
		}
		_ = os.Symlink(src, dst)
	}

	return dir, nil
}

// writeManagedGeminiSettings writes a settings.json that excludes all built-in
// tools except run_shell_command, leaving only shell access (which routes
// through the PATH shim) in the model's view. It is idempotent: rewriting on
// every launch keeps the file current when this waldo binary changes what it
// excludes.
//
// All names are the canonical TOOL_NAME constants from:
// packages/core/src/tools/definitions/base-declarations.ts
// The canonical names differ from common shorthands — e.g. the file editor is
// "replace" (EDIT_TOOL_NAME), not "edit"; the lister is "list_directory"
// (LS_TOOL_NAME), not "ls"; the grep is "grep_search" (GREP_TOOL_NAME), not
// "grep"; and web search is "google_web_search" (WEB_SEARCH_TOOL_NAME), not
// "web_search". Wrong names are silently ignored by Gemini CLI, leaving the
// tools available to the model.
func writeManagedGeminiSettings(geminiDir string) error {
	settings := map[string]any{
		// Mark the file as waldo-managed so doctor can identify it.
		"_waldo": waldoGeminiSettingsMarker,
		// excludeTools removes these tool names from the set advertised to the
		// model. Covers ALL_BUILTIN_TOOL_NAMES except run_shell_command.
		"excludeTools": []string{
			// File-system tools (call Node's fs module directly — bypass the shim)
			"read_file",
			"write_file",
			"replace",        // EDIT_TOOL_NAME — was wrongly listed as "edit"
			"glob",
			"grep_search",    // GREP_TOOL_NAME — was wrongly listed as "grep"
			"list_directory", // LS_TOOL_NAME   — was wrongly listed as "ls"
			"read_many_files",
			// Web tools
			"web_fetch",
			"google_web_search", // WEB_SEARCH_TOOL_NAME — was wrongly listed as "web_search"
			// Todo tool — writes a local todos file
			"write_todos",
			// Skill / internal-doc tools — read local skill files
			"activate_skill",
			"get_internal_docs",
			// Interactive tools — block a headless (no-TTY) run
			"ask_user",
			"enter_plan_mode",
			"exit_plan_mode",
			// Planning / narration tools — not needed in shell-only mode
			"update_topic",
			"complete_task",
			// Tracker tools — local in-session state management
			"tracker_create_task",
			"tracker_update_task",
			"tracker_get_task",
			"tracker_list_tasks",
			"tracker_add_dependency",
			"tracker_visualize",
			// Sub-agent tool — would spawn agents that run locally
			"invoke_agent",
			// MCP resource tools — managed home has no MCP server config
			"read_mcp_resource",
			"list_mcp_resources",
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal gemini settings.json: %w", err)
	}
	path := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
