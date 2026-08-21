package main

import (
	"strings"
	"testing"
)

func TestEnsureKimiFileToolDenyEmptyConfig(t *testing.T) {
	got := ensureKimiFileToolDeny("")
	for _, want := range []string{
		waldoKimiConfigMarker,
		"[permission]",
		// Skill must be denied: it reads local skill YAML files from
		// .kimi-code/skills/ and ~/.agents/skills/, bypassing the shell seam.
		`deny = ["Read", "Write", "Edit", "Glob", "Grep", "ReadMediaFile", "Skill"]`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("managed config missing %q:\n%s", want, got)
		}
	}
}

func TestEnsureKimiFileToolDenyExistingPermissionSection(t *testing.T) {
	base := "[permission]\ndeny = [\"Bash(rm *)\"]\n"
	got := ensureKimiFileToolDeny(base)
	if strings.Count(got, "[permission]") != 1 {
		t.Errorf("[permission] table repeated (invalid TOML):\n%s", got)
	}
	if !strings.Contains(got, `deny = ["Bash(rm *)"]`) {
		t.Errorf("operator's own rules were dropped:\n%s", got)
	}
	if c := strings.Count(got, "[[permission.rules]]"); c != len(kimiLocalFileTools) {
		t.Errorf("got %d [[permission.rules]] blocks, want %d:\n%s", c, len(kimiLocalFileTools), got)
	}
	if !strings.Contains(got, `pattern = "Read"`) {
		t.Errorf("Read deny rule missing:\n%s", got)
	}
}

func TestEnsureKimiFileToolDenyPreservesConfig(t *testing.T) {
	base := "default_model = \"kimi-k2\"\n\n[providers.\"managed:kimi-code\"]\nbase_url = \"https://example\"\n"
	got := ensureKimiFileToolDeny(base)
	if !strings.HasPrefix(got, base) {
		t.Errorf("original config not preserved verbatim:\n%s", got)
	}
}

func TestEnsureKimiFileToolDenyIdempotent(t *testing.T) {
	once := ensureKimiFileToolDeny("default_model = \"k\"\n")
	twice := ensureKimiFileToolDeny(once)
	if once != twice {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}

func TestResolveKimiBinaryPrefersExplicitEnv(t *testing.T) {
	t.Setenv("WALDO_KIMI_BINARY", "/nonexistent/kimi")
	if _, err := resolveKimiBinary(); err == nil {
		t.Error("expected error for non-executable WALDO_KIMI_BINARY")
	}
}
