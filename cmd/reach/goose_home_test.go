package main

import (
	"strings"
	"testing"
)

func TestManagedGooseConfig_EmptyBase(t *testing.T) {
	cfg := managedGooseConfig("")
	if !strings.Contains(cfg, "extensions:") {
		t.Error("expected extensions: block in output")
	}
	if !strings.Contains(cfg, "available_tools:") {
		t.Error("expected available_tools: in output")
	}
	if !strings.Contains(cfg, "- shell") {
		t.Error("expected - shell in available_tools")
	}
	if strings.Contains(cfg, "file_read") || strings.Contains(cfg, "file_write") {
		t.Error("file tools should not appear in managed config")
	}
}

func TestManagedGooseConfig_PreservesProviderKeys(t *testing.T) {
	realCfg := "GOOSE_PROVIDER: anthropic\nGOOSE_MODEL: claude-opus\n"
	cfg := managedGooseConfig(realCfg)
	if !strings.Contains(cfg, "GOOSE_PROVIDER: anthropic") {
		t.Error("expected GOOSE_PROVIDER to be copied from real config")
	}
	if !strings.Contains(cfg, "GOOSE_MODEL: claude-opus") {
		t.Error("expected GOOSE_MODEL to be copied from real config")
	}
	if !strings.Contains(cfg, "available_tools:") {
		t.Error("expected available_tools: in output")
	}
}

func TestManagedGooseConfig_StripsExtensionsBlock(t *testing.T) {
	realCfg := `GOOSE_PROVIDER: openai
GOOSE_MODEL: gpt-4o
extensions:
  developer:
    enabled: true
    name: developer
    type: builtin
GOOSE_SOMETHING: other
`
	cfg := managedGooseConfig(realCfg)
	// The original extensions block is gone; reach's replacement is there.
	if strings.Contains(cfg, "type: builtin") {
		t.Error("old extensions block should be stripped")
	}
	if !strings.Contains(cfg, "type: platform") {
		t.Error("new managed extensions block should use type: platform")
	}
	// Non-extension keys are preserved.
	if !strings.Contains(cfg, "GOOSE_PROVIDER: openai") {
		t.Error("GOOSE_PROVIDER should be preserved")
	}
	if !strings.Contains(cfg, "GOOSE_SOMETHING: other") {
		t.Error("non-extensions top-level keys should be preserved")
	}
}

func TestManagedGooseConfig_IdempotentOnAlreadyManaged(t *testing.T) {
	cfg := managedGooseConfig("")
	cfg2 := managedGooseConfig(cfg)
	if cfg != cfg2 {
		t.Errorf("managedGooseConfig should be idempotent on already-managed input:\nfirst:\n%s\nsecond:\n%s", cfg, cfg2)
	}
}

func TestManagedGooseConfig_OnlyShellInAvailableTools(t *testing.T) {
	cfg := managedGooseConfig("")
	// Available tools must list exactly shell and nothing else.
	idx := strings.Index(cfg, "available_tools:")
	if idx < 0 {
		t.Fatal("no available_tools: found")
	}
	rest := cfg[idx:]
	// Find the "- shell" entry.
	if !strings.Contains(rest, "- shell") {
		t.Error("shell should be in available_tools")
	}
	// No other tools should appear.
	for _, tool := range []string{"file_read", "file_write", "file_edit", "tree", "read_image", "write", "edit"} {
		if strings.Contains(rest[:strings.Index(rest, "\n\n")+1], tool) {
			t.Errorf("unexpected tool %q in available_tools section", tool)
		}
	}
}
