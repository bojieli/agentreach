package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// shimName is the logical name waldo answers to when acting as a harness shell
// shim.
//
// Harnesses accept a *program path* for their shell hook, not a command line:
// Claude Code stats the value of CLAUDE_CODE_SHELL_PREFIX directly, so
// "waldo shell-prefix" is looked up as a single filename and fails. Dispatching
// on argv[0] through an alias of the binary gives a bare path that works, and
// costs nothing at run time — no wrapper script, no extra process per tool call.
//
// The name is logical because Windows decorates it: see programName.
const shimName = "waldo-shell-prefix"

// isShimInvocation reports whether this process was started through the shim
// alias rather than as `waldo <command>`.
func isShimInvocation() bool {
	return programBase(os.Args[0]) == shimName
}

// binDir is where waldo keeps the shim alias.
func binDir() (string, error) {
	return waldoSubdir("bin")
}

// waldoSubdir resolves a directory beneath waldo's state directory, creating it.
func waldoSubdir(name string) (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// selfPath resolves the running binary, following symlinks.
func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		// Deliberately not an error. A binary whose path cannot be resolved
		// through symlinks is still perfectly runnable — this happens on
		// filesystems that do not support them, and on Windows where the check
		// is meaningless — and the unresolved path makes a slightly worse alias
		// than a resolved one, which is a great deal better than refusing to
		// start.
		//nolint:nilerr // the unresolved path is a valid answer, not a failure
		return self, nil
	}
	return resolved, nil
}

// ensureShim creates or refreshes the shim alias and returns its path.
//
// It is checked on every use rather than created once: waldo may have been
// upgraded, moved, or reinstalled at a different path since the alias was made,
// and a stale one would fail at the worst possible moment — inside a tool call,
// where the harness reports it as a broken shell rather than as a waldo problem.
func ensureShim() (string, error) {
	self, err := selfPath()
	if err != nil {
		return "", err
	}
	dir, err := binDir()
	if err != nil {
		return "", err
	}
	alias := filepath.Join(dir, programName(shimName))

	if programAliasIsCurrent(alias, self) {
		return alias, nil
	}
	if err := installProgramAlias(self, alias); err != nil {
		return "", fmt.Errorf("install the shell shim at %s: %w", alias, err)
	}
	return alias, nil
}
