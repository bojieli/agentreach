package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// waldoGooseConfigMarker marks waldo's managed extensions section so that
// repeated launches do not append the restriction twice.
const waldoGooseConfigMarker = "# waldo: developer extension restricted to shell tool"

// managedGooseHome builds (or refreshes) the GOOSE_PATH_ROOT waldo launches
// goose with for a session.
//
// The managed home has a config.yaml that copies the operator's provider
// settings (so their model and API key keep working) but replaces the
// extensions section with a waldo-controlled version that restricts the
// developer extension to the shell tool only via `available_tools: [shell]`.
// The other developer tools (write, edit, tree, read_image — canonical names
// from crates/goose/src/agents/platform_extensions/developer/mod.rs) are
// therefore not advertised to the model; the agent uses shell commands for
// file access instead, and those commands run on the target.
func managedGooseHome(sessName string) (string, error) {
	dir, err := waldoSubdir(filepath.Join("goose-home", sessName))
	if err != nil {
		return "", err
	}
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", fmt.Errorf("create goose config dir: %w", err)
	}

	// Read the operator's real goose config.yaml. Errors are silently ignored:
	// if the file does not exist (fresh goose install), the managed config is
	// written from scratch. Provider settings that are only in the config file
	// and not in env vars will be absent from the managed config; operators
	// who configure goose exclusively through env vars are unaffected.
	var baseYAML string
	if realPath := realGooseConfigPath(); realPath != "" {
		if data, err := os.ReadFile(realPath); err == nil {
			baseYAML = string(data)
		}
	}

	conf := managedGooseConfig(baseYAML)
	dst := filepath.Join(configDir, "config.yaml")
	if err := os.WriteFile(dst, []byte(conf), 0o600); err != nil {
		return "", fmt.Errorf("write managed goose config: %w", err)
	}
	return dir, nil
}

// realGooseConfigPath returns the path to the operator's real goose
// config.yaml on the current platform. This mirrors the path logic in
// goose's crates/goose/src/config/paths.rs, which uses etcetera's
// AppStrategy for the Block/goose application.
//
// On macOS, etcetera resolves config_dir() to
// ~/Library/Application Support/Block/goose.
// On Linux (and other XDG systems), config_dir() is ~/.config/goose
// (XDG does not include the author prefix).
func realGooseConfigPath() string {
	confBase, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(confBase, "Block", "goose", "config.yaml")
	default: // linux, etc.
		return filepath.Join(confBase, "goose", "config.yaml")
	}
}

// managedGooseConfig returns a goose config.yaml for the managed home.
//
// If the operator's real config is provided, all non-extension keys are
// copied across so that provider settings (GOOSE_PROVIDER, GOOSE_MODEL,
// API key references) keep working without requiring the operator to set
// every value as an env var. The extensions: block is replaced wholesale
// with waldo's restriction.
//
// If baseYAML is empty (no existing config), a minimal config with only
// the extension restriction is written; the operator's provider config must
// then come from env vars (GOOSE_PROVIDER, GOOSE_MODEL, API key env vars),
// which is already how most goose users configure their provider.
func managedGooseConfig(baseYAML string) string {
	if strings.Contains(baseYAML, waldoGooseConfigMarker) {
		// Already a waldo-managed config; return unchanged to avoid a second
		// append from an older run that already wrote the marker.
		return baseYAML
	}

	// Strip the extensions: block. YAML blocks are indented under their
	// top-level key; a block ends when the next non-indented, non-comment
	// line appears. This handles the structure of all known goose config.yaml
	// files (a flat set of GOOSE_* keys plus one extensions: map) without
	// importing a full YAML parser.
	var out strings.Builder
	inExtensions := false
	for _, line := range strings.Split(baseYAML, "\n") {
		isTopLevelKey := len(line) > 0 &&
			line[0] != ' ' && line[0] != '\t' &&
			line[0] != '#' && line[0] != '-' && line[0] != '\n'
		if isTopLevelKey && strings.HasPrefix(line, "extensions:") {
			inExtensions = true
			continue
		}
		if isTopLevelKey && inExtensions {
			inExtensions = false
		}
		if !inExtensions {
			out.WriteString(line)
			out.WriteString("\n")
		}
	}

	// Append waldo's controlled extensions section.
	out.WriteString(waldoGooseConfigMarker + "\n")
	out.WriteString("extensions:\n")
	out.WriteString("  developer:\n")
	out.WriteString("    name: developer\n")
	out.WriteString("    type: platform\n")
	out.WriteString("    enabled: true\n")
	out.WriteString("    available_tools:\n")
	out.WriteString("      - shell\n")
	return out.String()
}
