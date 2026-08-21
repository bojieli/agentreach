package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bojieli/waldo/internal/execserver"
	"github.com/bojieli/waldo/internal/harnessprobe"
	"github.com/bojieli/waldo/internal/session"
)

func shimContext() context.Context { return context.Background() }

func loadSessionQuiet() (*session.Session, error) {
	return session.Load(sessionNameFromEnv(""))
}

// launchWithPathShim starts a harness whose shell is intercepted by placing a
// `bash` shim earlier on PATH.
//
// This is the seam for harnesses that resolve their shell with execvp rather
// than reading it from a configuration value. It needs no fork and no support
// from the harness, but it is coarser than a dedicated hook: every `bash -c`
// the harness runs is redirected, including any it runs for its own internal
// purposes. That is usually what is wanted — the harness's own file reads go
// through the same path — but it is the reason waldo falls back to a local
// shell rather than failing when no session is bound.
func launchWithPathShim(ctx context.Context, binary, label, sessName string, extraEnv []string, args []string) int {
	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	binPath, err := exec.LookPath(binary)
	if err != nil {
		fmt.Fprintf(os.Stderr, "waldo: %s is not installed or not in PATH\n", label)
		return 1
	}
	shimDir, err := ensurePathShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	env := os.Environ()
	env = append(env, "WALDO_SESSION="+sessName)
	env = append(env, extraEnv...)
	env = prependPath(env, shimDir)

	fmt.Fprintf(os.Stderr, "waldo: %s -> %s (shell runs on the target)\n", label, s.Target.Describe())

	argv := append([]string{binPath}, args...)
	return replaceProcess(ctx, binPath, argv, env)
}

// cmdCodex launches Codex against the session's target.
//
// Codex 0.148 resolves its shell by absolute path, which no PATH shim can
// intercept — but the same release gained a remote-environment seam:
// environments.toml in CODEX_HOME names a program codex spawns and treats as
// the machine its tools act on. waldo launches codex with a managed CODEX_HOME
// whose environments.toml points at `waldo exec-server`, so every tool surface
// — shell commands, file reads and writes, grep and glob, apply_patch —
// executes on the session's target through the execserver package. The user's
// real CODEX_HOME is only ever read (auth and config are copied across), never
// written.
func cmdCodex(ctx context.Context, args []string) int {
	fs := newFlagSet("codex")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	fullAccess := fs.Bool("danger-full-access", false,
		"disable Codex's local sandbox entirely instead of only allowing network")
	force := fs.Bool("force", false,
		"launch without verifying the shell seam (the agent's commands may run LOCALLY)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)

	// Fail-closed seam guard. The verdict now measures the exec-server seam
	// end to end: the probe's codex runs against a throwaway CODEX_HOME whose
	// environments.toml points at this same waldo binary, and the canary must
	// report the target's hostname. --force is the operator's explicit escape
	// hatch, and it says so on stderr.
	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the codex seam verification.\n"+
				"waldo: If the exec-server seam fails to engage, every command the agent\n"+
				"waldo: runs will execute on the LOCAL machine while the agent believes it\n"+
				"waldo: is acting on the target.")
	} else if rc := guardHarnessSeam(ctx, harnessprobe.HarnessCodex, sessName); rc != 0 {
		return rc
	}

	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	binPath, err := exec.LookPath("codex")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: codex is not installed or not in PATH")
		return 1
	}
	codexHome, err := managedCodexHome(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	// Codex's sandbox blocks network syscalls by default. Commands run on the
	// target, so the meaningful boundary is the target itself — but a
	// restricted network policy against a remote environment requires an
	// executor-local proxy the waldo exec-server does not provide, so the
	// workspace-write sandbox keeps network access allowed.
	sandbox := []string{
		"-c", "sandbox_mode=\"workspace-write\"",
		"-c", "sandbox_workspace_write.network_access=true",
	}
	if *fullAccess {
		sandbox = []string{"-c", "sandbox_mode=\"danger-full-access\""}
	}

	env := replaceEnv(os.Environ(), "CODEX_HOME", codexHome)
	env = append(env, "WALDO_SESSION="+sessName)

	fmt.Fprintf(os.Stderr, "waldo: Codex -> %s (exec-server; every tool acts on the target)\n", s.Target.Describe())

	argv := append([]string{binPath}, append(sandbox, pos...)...)
	return replaceProcess(ctx, binPath, argv, env)
}

// managedCodexHome builds (or refreshes) the CODEX_HOME waldo launches codex
// with for a session. It contains copies of the operator's real codex auth and
// config — subscription logins keep working — and an environments.toml that
// binds codex's remote environment to this waldo binary and the session. The
// directory is waldo's, so the file is rewritten on every launch: a waldo
// upgrade or a moved binary must not leave codex pointing at a stale path.
func managedCodexHome(sessName string) (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "codex-home", sessName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}

	real := os.Getenv("CODEX_HOME")
	if real == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		real = filepath.Join(home, ".codex")
	}
	// Copies, not symlinks: codex resolves its home and a symlink that later
	// points somewhere else would be a silent change of credentials. Files
	// that are absent are simply skipped — OPENAI_API_KEY may be the whole
	// login.
	for _, f := range []string{"auth.json", "config.toml"} {
		data, err := os.ReadFile(filepath.Join(real, f))
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f), data, 0o600); err != nil {
			return "", fmt.Errorf("copy %s from %s: %w", f, real, err)
		}
	}

	waldoPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate the waldo binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(waldoPath); err == nil {
		waldoPath = resolved
	}
	toml := execserver.EnvironmentsTOML(waldoPath, sessName)
	if err := os.WriteFile(filepath.Join(dir, "environments.toml"), []byte(toml), 0o600); err != nil {
		return "", fmt.Errorf("write environments.toml: %w", err)
	}
	return dir, nil
}

// cmdGoose launches Goose against the session's target.
//
// Goose's developer extension honours GOOSE_SHELL as a first-class, documented
// env var that overrides the shell for every command it runs. waldo sets it to
// the PATH shim's bash, so every shell command the agent issues routes through
// waldo and executes on the session target.
//
// Goose's file tools (write, edit, tree, read_image — canonical names from
// crates/goose/src/agents/platform_extensions/developer/mod.rs) in the
// developer extension bypass the shell and would act on the local machine.
// waldo builds a managed GOOSE_PATH_ROOT whose config.yaml restricts the
// developer extension to the shell tool only (available_tools: [shell]),
// removing the file tools from the model's view. The agent reads and writes
// files through shell commands (cat, cp, patch, etc.) instead, which run on
// the target.
func cmdGoose(ctx context.Context, args []string) int {
	fs := newFlagSet("goose")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	force := fs.Bool("force", false,
		"launch without verifying the shell seam (the agent's commands may run LOCALLY)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)

	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	binPath, err := exec.LookPath("goose")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: goose is not installed or not in PATH")
		return 1
	}

	shimDir, err := ensurePathShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	gooseHome, err := managedGooseHome(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the goose seam verification.\n"+
				"waldo: If GOOSE_SHELL is not honoured by this goose version, its shell\n"+
				"waldo: commands will run on the LOCAL machine while appearing remote.")
	} else if rc := guardHarnessSeam(ctx, harnessprobe.HarnessGoose, sessName); rc != 0 {
		return rc
	}

	env := os.Environ()
	env = append(env, "WALDO_SESSION="+sessName)
	env = append(env, "GOOSE_PATH_ROOT="+gooseHome)
	env = append(env, "GOOSE_SHELL="+filepath.Join(shimDir, bashShimName))
	env = prependPath(env, shimDir)

	fmt.Fprintf(os.Stderr,
		"waldo: Goose -> %s (shell via GOOSE_SHELL; file tools denied)\n",
		s.Target.Describe())

	argv := append([]string{binPath}, pos...)
	return replaceProcess(ctx, binPath, argv, env)
}

// cmdGemini launches Gemini CLI against the session's target.
//
// Gemini CLI resolves its shell via getShellConfiguration(), which returns a
// bare "bash" name and walks PATH — the PATH shim intercepts it natively.
// waldo sets HOME to a managed directory whose .gemini/settings.json excludes
// every file tool (read_file, write_file, replace, glob, grep_search,
// list_directory, read_many_files, web_fetch, google_web_search, and more —
// all canonical TOOL_NAME constants from base-declarations.ts), leaving only
// run_shell_command in the model's view. Shell commands route through the PATH
// shim and execute on the session target.
func cmdGemini(ctx context.Context, args []string) int {
	fs := newFlagSet("gemini")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	force := fs.Bool("force", false,
		"launch without verifying the shell seam (the agent's commands may run LOCALLY)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)

	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	binPath, err := exec.LookPath("gemini")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: gemini is not installed or not in PATH")
		return 1
	}

	shimDir, err := ensurePathShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	geminiHome, err := managedGeminiHome(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the gemini seam verification.\n"+
				"waldo: If the PATH shim fails to intercept gemini's shell calls,\n"+
				"waldo: its commands will run on the LOCAL machine while appearing remote.")
	} else if rc := guardHarnessSeam(ctx, harnessprobe.HarnessGemini, sessName); rc != 0 {
		return rc
	}

	env := replaceEnv(os.Environ(), "HOME", geminiHome)
	env = append(env, "WALDO_SESSION="+sessName)
	env = prependPath(env, shimDir)

	fmt.Fprintf(os.Stderr,
		"waldo: Gemini CLI -> %s (shell via PATH shim; file tools excluded)\n",
		s.Target.Describe())

	argv := append([]string{binPath}, pos...)
	return replaceProcess(ctx, binPath, argv, env)
}

// replaceEnv returns env with key set to value, replacing any existing entry
// rather than appending a duplicate. The key's original spelling is dropped:
// on Windows a second PATH-like key differing only in case would leave the
// child's search path to chance.
func replaceEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

// cmdKimi launches Kimi Code against the session's target.
//
// Kimi Code 0.37.2 and earlier resolves its shell by absolute path, so no
// PATH shim can intercept its Bash tool calls natively. waldo's approach is
// two-part:
//
//  1. A patch to Kimi's npm bundle prepends $KIMI_SHELL_PATH to the shell
//     candidate list (contrib/kimi-shell-path-patch.mjs). The managed Kimi
//     binary under ~/.waldo/kimi-*/node_modules/.bin/kimi uses this patch.
//     waldo sets KIMI_SHELL_PATH to the PATH shim's bash, so every Bash tool
//     call routes through the shim and runs on the target.
//
//  2. A managed KIMI_CODE_HOME denies Kimi's native file tools (read_file,
//     write_file, edit, glob, grep, read_media_file) via config.toml, because
//     those tools bypass the shell and would act on the LOCAL machine. The
//     agent uses the shell for file access instead, which runs on the target.
//
// Kimi also wraps every Bash call with `cd '<local-cwd>' && <command>`, which
// would fail on the target. WALDO_EXEC_WORKSPACE tells the PATH shim to
// rewrite the cd prefix to the session's target workspace, so the working
// directory resolves correctly on both sides.
func cmdKimi(ctx context.Context, args []string) int {
	fs := newFlagSet("kimi")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	force := fs.Bool("force", false,
		"launch without verifying the shell seam (the agent's commands may run LOCALLY)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)

	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	// Use the waldo-managed patched kimi binary if available; fall back to
	// whatever "kimi" is on PATH. The seam guard verifies which binary was
	// chosen before launch, so the fall-back only succeeds if that binary
	// actually uses the PATH shim.
	binPath, err := resolveKimiBinary()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	shimDir, err := ensurePathShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	kimiHome, err := managedKimiHome(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	// WALDO_EXEC_WORKSPACE tells the PATH shim where the operator's project
	// root is so it can rewrite kimi's embedded `cd '<local-cwd>' && ...`
	// prefix to the session's target workspace.
	cwd, _ := os.Getwd()

	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the kimi seam verification.\n"+
				"waldo: If this kimi binary was not patched for KIMI_SHELL_PATH, every\n"+
				"waldo: command the agent runs will execute on the LOCAL machine while the\n"+
				"waldo: agent believes it is acting on the target.")
	} else if rc := guardKimiSeam(ctx, sessName, binPath, shimDir); rc != 0 {
		return rc
	}

	env := os.Environ()
	env = append(env, "WALDO_SESSION="+sessName)
	env = append(env, "KIMI_CODE_HOME="+kimiHome)
	env = append(env, "KIMI_SHELL_PATH="+filepath.Join(shimDir, bashShimName))
	env = append(env, "WALDO_EXEC_WORKSPACE="+cwd)
	env = prependPath(env, shimDir)

	fmt.Fprintf(os.Stderr,
		"waldo: Kimi Code -> %s (shell shim via KIMI_SHELL_PATH; file tools denied)\n",
		s.Target.Describe())

	argv := append([]string{binPath}, pos...)
	return replaceProcess(ctx, binPath, argv, env)
}
