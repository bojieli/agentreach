package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
func launchWithPathShim(binary, label, sessName string, extraEnv []string, args []string) int {
	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	binPath, err := exeLook(binary)
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
	if err := syscall.Exec(binPath, argv, env); err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	return 0
}

func prependPath(env []string, dir string) []string {
	out := make([]string, 0, len(env))
	found := false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			out = append(out, "PATH="+dir+string(filepath.ListSeparator)+v)
			found = true
			continue
		}
		out = append(out, kv)
	}
	if !found {
		out = append(out, "PATH="+dir)
	}
	return out
}

// cmdCodex launches Codex against the session's target.
//
// Codex is the harness this works best on. Its file reads, writes and
// apply_patch edits all travel over its shell tool rather than through native
// file tools, so intercepting the shell redirects Codex's entire tool surface
// — no denied tools, no mirroring, no gaps.
func cmdCodex(_ context.Context, args []string) int {
	fs := newFlagSet("codex")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	fullAccess := fs.Bool("danger-full-access", false,
		"disable Codex's local sandbox entirely instead of only allowing network")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
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

	return launchWithPathShim("codex", "Codex", sessionNameFromEnv(*name),
		nil, append(sandbox, pos...))
}

// cmdKimi launches Kimi Code against the session's target.
//
// Kimi's shell tool is redirected, but its native read_file, write_file and
// multi_edit tools have no seam and still act locally. Until an adapter for
// those exists, treat Kimi like Claude Code in exec mode: use the shell for
// file access.
func cmdKimi(_ context.Context, args []string) int {
	fs := newFlagSet("kimi")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	fmt.Fprintln(os.Stderr,
		"waldo: note — Kimi's native file tools (read_file, write_file, multi_edit)\n"+
			"       still act on the LOCAL filesystem. Use shell commands for file access.")
	return launchWithPathShim("kimi", "Kimi Code", sessionNameFromEnv(*name), nil, pos)
}
