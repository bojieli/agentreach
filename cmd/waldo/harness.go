package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

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
// Codex is the harness this works best on. Its file reads, writes and
// apply_patch edits all travel over its shell tool rather than through native
// file tools, so intercepting the shell redirects Codex's entire tool surface
// — no denied tools, no mirroring, no gaps.
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

	// Fail-closed seam guard. Codex >= 0.148 resolves its shell by absolute
	// path (getpwuid_r -> /bin/zsh -lc) instead of by name, which no PATH shim
	// can intercept: every command it ran would execute on the local machine
	// while the agent believed it was acting on the target. There is no
	// config, env or hook seam to change that, so waldo verifies the behaviour
	// once per codex version and refuses versions measured broken. --force is
	// the operator's explicit escape hatch, and it says so on stderr.
	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the codex seam verification.\n"+
				"waldo: If this codex version resolves its shell by absolute path, every\n"+
				"waldo: command the agent runs will execute on the LOCAL machine while the\n"+
				"waldo: agent believes it is acting on the target.")
	} else if rc := guardHarnessSeam(ctx, "codex", sessName); rc != 0 {
		return rc
	}

	// Codex sandboxes the commands it runs, and that sandbox blocks network
	// syscalls. waldo's shell shim has to open an SSH connection, so under the
	// default policy every command fails with "Operation not permitted".
	//
	// The narrow fix is to keep the filesystem sandbox and allow network,
	// rather than disabling isolation wholesale. The local sandbox is doing
	// less work than usual here anyway — the commands execute on the target,
	// so the meaningful boundary is the target itself — but there is no reason
	// to give up the part that still applies.
	sandbox := []string{
		"-c", "sandbox_mode=\"workspace-write\"",
		"-c", "sandbox_workspace_write.network_access=true",
	}
	if *fullAccess {
		sandbox = []string{"-c", "sandbox_mode=\"danger-full-access\""}
	}

	return launchWithPathShim(ctx, "codex", "Codex", sessName,
		nil, append(sandbox, pos...))
}

// cmdKimi launches Kimi Code against the session's target.
//
// Kimi's shell tool is redirected, but its native read_file, write_file and
// multi_edit tools have no seam and still act locally. Until an adapter for
// those exists, treat Kimi like Claude Code in exec mode: use the shell for
// file access.
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

	// The same fail-closed seam guard as codex: Kimi Code 0.37.2 spawns its
	// shell by absolute path, which no PATH shim can intercept, so every
	// command its Bash tool ran would execute on the local machine while the
	// agent believed it was acting on the target.
	if *force {
		fmt.Fprintln(os.Stderr,
			"waldo: WARNING: --force skips the kimi seam verification.\n"+
				"waldo: If this kimi version spawns its shell by absolute path, every\n"+
				"waldo: command the agent runs will execute on the LOCAL machine while the\n"+
				"waldo: agent believes it is acting on the target.")
	} else if rc := guardHarnessSeam(ctx, "kimi", sessName); rc != 0 {
		return rc
	}

	fmt.Fprintln(os.Stderr,
		"waldo: note — Kimi's native file tools (read_file, write_file, multi_edit)\n"+
			"       still act on the LOCAL filesystem. Use shell commands for file access.")
	return launchWithPathShim(ctx, "kimi", "Kimi Code", sessName, nil, pos)
}
