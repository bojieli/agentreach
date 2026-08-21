package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// kimiLocalFileTools are Kimi's native tools that act on the LOCAL filesystem
// no matter where the shell seam points, which is exactly the wrong-machine
// failure reach exists to prevent. The managed launch config denies them; the
// agent reaches files through its shell tool (Bash), which runs on the target.
//
// Names are the readonly `name` constants from both kimi engines:
//
//	packages/agent-core/src/tools/builtin/file/*.ts   (v1)
//	packages/agent-core-v2/src/agent/tools/os/*.ts    (v2)
//
// Bash is deliberately absent — it is the intercept point.
//
// Skill (v1 collaboration/skill-tool.ts, v2 agent/tools/skill/skillTool.ts)
// reads local skill YAML files from .kimi-code/skills/ and ~/.agents/skills/,
// bypassing the shell seam. Denied to prevent local project data from being
// read on the operator's machine instead of the target.
var kimiLocalFileTools = []string{"Read", "Write", "Edit", "Glob", "Grep", "ReadMediaFile", "Skill"}

// reachKimiConfigMarker marks reach's addition to a managed kimi config so
// repeated launches do not append it twice.
const reachKimiConfigMarker = "# reach: local file tools denied"

// resolveKimiBinary picks the kimi executable to launch, in order:
//
//  1. REACH_KIMI_BINARY — the operator's explicit choice.
//  2. The newest reach-managed patched npm install under ~/.reach/kimi-*
//     (see contrib/kimi-shell-path-patch.mjs; the stock binary spawns its
//     shell by absolute path, which no shim can intercept).
//  3. Whatever "kimi" is on PATH.
//
// The seam guard measures the chosen binary's behaviour before launch, so a
// wrong pick here fails closed rather than silently running locally.
func resolveKimiBinary() (string, error) {
	if explicit := os.Getenv("REACH_KIMI_BINARY"); explicit != "" {
		if isExecutableFile(explicit) {
			return explicit, nil
		}
		return "", fmt.Errorf("REACH_KIMI_BINARY %q is not executable", explicit)
	}
	home, err := os.UserHomeDir()
	if err == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".reach", "kimi-*", "node_modules", ".bin", "kimi"))
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

// managedKimiHome builds a per-session KIMI_CODE_HOME under ~/.reach that
// shares the operator's login (auth entries are symlinks, so refreshed OAuth
// tokens flow back to the real home) but carries reach's tool policy: the
// native file tools are denied, because they would act on the local machine.
//
// Anything not linked or copied starts fresh — sessions, logs and caches stay
// out of the operator's real history, which is also what keeps a reach run's
// transcripts clearly separated from interactive ones.
func managedKimiHome(sessName string) (string, error) {
	dir, err := reachSubdir(filepath.Join("kimi-home", sessName))
	if err != nil {
		return "", err
	}
	realHome := os.Getenv("KIMI_CODE_HOME")
	if realHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		realHome = filepath.Join(home, ".kimi-code")
	}
	// Auth and identity entries are linked, not copied: OAuth tokens are
	// refreshed in place, and a copied token would silently expire.
	for _, entry := range []string{"credentials", "oauth", "region", "device_id", "tui.toml"} {
		src := filepath.Join(realHome, entry)
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
	// config.toml is copied, not linked: reach's tool policy is appended to
	// the copy and must never leak into the operator's interactive config.
	srcConf := filepath.Join(realHome, "config.toml")
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
// one reach emits depends on whether the operator already has a [permission]
// section, because TOML tables cannot repeat.
func ensureKimiFileToolDeny(conf string) string {
	if strings.Contains(conf, reachKimiConfigMarker) {
		return conf
	}
	var b strings.Builder
	b.WriteString(conf)
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("\n" + reachKimiConfigMarker + " — they act on the operator's machine, not the target; the shell runs on the target.\n")
	if strings.Contains(conf, "[permission]") {
		for _, tool := range kimiLocalFileTools {
			fmt.Fprintf(&b, "[[permission.rules]]\ndecision = \"deny\"\npattern = %q\nreason = \"reach: local file tools act on the operator's machine, not the target\"\n", tool)
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
