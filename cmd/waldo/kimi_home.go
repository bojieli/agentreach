package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// kimiLocalFileTools are Kimi's native file tools. They act on the LOCAL
// filesystem no matter where the shell seam points, which is exactly the
// wrong-machine failure waldo exists to prevent, so the managed launch config
// denies them and the agent reaches files through its shell — which runs on
// the target. The names are the built-in tool names in both kimi engines
// (packages/agent-core*/src/**/tools: Read, Write, Edit, Glob, Grep,
// ReadMediaFile); Bash is deliberately not in the list.
var kimiLocalFileTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "ReadMediaFile"}

// waldoKimiConfigMarker marks waldo's addition to a managed kimi config so
// repeated launches do not append it twice.
const waldoKimiConfigMarker = "# waldo: local file tools denied"

// resolveKimiBinary picks the kimi executable to launch, in order:
//
//  1. WALDO_KIMI_BINARY — the operator's explicit choice.
//  2. The newest waldo-managed patched npm install under ~/.waldo/kimi-*
//     (see contrib/kimi-shell-path-patch.mjs; the stock binary spawns its
//     shell by absolute path, which no shim can intercept).
//  3. Whatever "kimi" is on PATH.
//
// The seam guard measures the chosen binary's behaviour before launch, so a
// wrong pick here fails closed rather than silently running locally.
func resolveKimiBinary() (string, error) {
	if explicit := os.Getenv("WALDO_KIMI_BINARY"); explicit != "" {
		if isExecutableFile(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("WALDO_KIMI_BINARY %q is not executable", explicit)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".waldo", "kimi-*", "node_modules", ".bin", "kimi"))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[len(matches)-1], nil
		}
	}
	return lookPathKimi()
}

// lookPathKimi is exec.LookPath for "kimi", kept nameable for tests.
var lookPathKimi = func() (string, error) {
	return exec.LookPath("kimi")
}

// managedKimiHome builds a per-session KIMI_CODE_HOME under ~/.waldo that
// shares the operator's login (auth entries are symlinks, so refreshed OAuth
// tokens flow back to the real home) but carries waldo's tool policy: the
// native file tools are denied, because they would act on the local machine.
//
// Anything not linked or copied starts fresh — sessions, logs and caches stay
// out of the operator's real history, which is also what keeps a waldo run's
// transcripts clearly separated from interactive ones.
func managedKimiHome(sessName string) (string, error) {
	dir, err := waldoSubdir(filepath.Join("kimi-home", sessName))
	if err != nil {
		return "", err
	}
	real := os.Getenv("KIMI_CODE_HOME")
	if real == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		real = filepath.Join(home, ".kimi-code")
	}
	// Auth and identity entries are linked, not copied: OAuth tokens are
	// refreshed in place, and a copied token would silently expire.
	for _, entry := range []string{"credentials", "oauth", "region", "device_id", "tui.toml"} {
		src := filepath.Join(real, entry)
		dst := filepath.Join(dir, entry)
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Symlink(src, dst); err != nil {
			return "", fmt.Errorf("link %s into the managed kimi home: %w", entry, err)
		}
	}
	// config.toml is copied, not linked: waldo's tool policy is appended to
	// the copy and must never leak into the operator's interactive config.
	srcConf := filepath.Join(real, "config.toml")
	data, err := os.ReadFile(srcConf)
	if err != nil {
		data = nil
	}
	conf := ensureKimiFileToolDeny(string(data))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(conf), 0o600); err != nil {
		return "", fmt.Errorf("write managed kimi config: %w", err)
	}
	return dir, nil
}

// ensureKimiFileToolDeny returns a config.toml that denies Kimi's native file
// tools. Kimi's permission config ( honoured by both engines) accepts either a
// [permission] table with a deny list or [[permission.rules]] entries; which
// one waldo emits depends on whether the operator already has a [permission]
// section, because TOML tables cannot repeat.
func ensureKimiFileToolDeny(conf string) string {
	if strings.Contains(conf, waldoKimiConfigMarker) {
		return conf
	}
	var b strings.Builder
	b.WriteString(conf)
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n" + waldoKimiConfigMarker + " — they act on the operator's machine, not the target; the shell runs on the target.\n")
	if strings.Contains(conf, "[permission]") {
		for _, tool := range kimiLocalFileTools {
			fmt.Fprintf(&b, "[[permission.rules]]\ndecision = \"deny\"\npattern = %q\nreason = \"waldo: local file tools act on the operator's machine, not the target\"\n", tool)
		}
		return b.String()
	}
	b.WriteString("[permission]\ndeny = [")
	for i, tool := range kimiLocalFileTools {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", tool)
	}
	b.WriteString("]\n")
	return b.String()
}
