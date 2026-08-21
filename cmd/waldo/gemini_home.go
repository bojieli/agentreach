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
// every Gemini file tool (read_file, write_file, edit, glob, grep, ls,
// read_many_files, web_fetch, web_search), leaving only run_shell_command
// advertised to the model. Shell commands route through the PATH shim and
// execute on the session target.
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
	realGeminiDir := filepath.Join(os.Getenv("HOME"), ".gemini")
	if realGeminiDir == filepath.Join("", ".gemini") {
		// HOME was empty; try UserHomeDir.
		if h, err := os.UserHomeDir(); err == nil {
			realGeminiDir = filepath.Join(h, ".gemini")
		}
	}
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

// writeManagedGeminiSettings writes a settings.json that excludes all local
// file tools, leaving only run_shell_command (which routes through the PATH
// shim) in the model's view. It is idempotent: rewriting on every launch keeps
// the file current if this waldo binary changes what it excludes.
func writeManagedGeminiSettings(geminiDir string) error {
	settings := map[string]any{
		// Mark the file as waldo-managed so doctor can identify it.
		"_waldo": waldoGeminiSettingsMarker,
		// excludeTools removes these tool names from the set advertised to the
		// model. The names are the SHELL_TOOL_NAME / READ_FILE_TOOL_NAME etc.
		// constants from @google/genai's tool-names.ts.
		"excludeTools": []string{
			"read_file",
			"write_file",
			"edit",
			"glob",
			"grep",
			"ls",
			"read_many_files",
			"web_fetch",
			"web_search",
			"memory",
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
