package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// shimName is the basename waldo answers to when acting as a harness shell
// shim.
//
// Harnesses accept a *program path* for their shell hook, not a command line:
// Claude Code stats the value of CLAUDE_CODE_SHELL_PREFIX directly, so
// "waldo shell-prefix" is looked up as a single filename and fails. Dispatching
// on argv[0] through a symlink gives a bare path that works, and costs nothing
// at run time — no wrapper script, no extra process per tool call.
const shimName = "waldo-shell-prefix"

// isShimInvocation reports whether this process was started through the shim
// symlink rather than as `waldo <command>`.
func isShimInvocation() bool {
	return filepath.Base(os.Args[0]) == shimName
}

// binDir is where waldo keeps the shim symlink.
func binDir() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "bin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// ensureShim creates or refreshes the shim symlink and returns its path.
//
// It is refreshed on every use rather than created once: waldo may have been
// upgraded, moved, or reinstalled at a different path since the link was made,
// and a stale link would fail at the worst possible moment — inside a tool
// call, where the harness reports it as a broken shell rather than a waldo
// problem.
func ensureShim() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	dir, err := binDir()
	if err != nil {
		return "", err
	}
	link := filepath.Join(dir, shimName)

	if current, err := os.Readlink(link); err == nil && current == self {
		return link, nil
	}
	_ = os.Remove(link)
	if err := os.Symlink(self, link); err != nil {
		// Symlinks can be unavailable (some filesystems, some Windows setups).
		// A tiny exec wrapper is slower but keeps waldo working rather than
		// refusing to start.
		script := fmt.Sprintf("#!/bin/sh\nexec %q shell-prefix \"$@\"\n", self)
		if werr := os.WriteFile(link, []byte(script), 0o700); werr != nil {
			return "", fmt.Errorf("create shell shim at %s: %w", link, err)
		}
	}
	return link, nil
}
