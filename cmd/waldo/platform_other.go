//go:build !windows

package main

import (
	"os"
	"path/filepath"
)

// This file and platform_windows.go carry every difference between the two
// operating systems waldo runs on. Nothing else in the program branches on
// GOOS, so the cost of supporting Windows is visible in one place instead of
// scattered through the harness adapters.

// platformCheck reports whether waldo can run here.
func platformCheck() error { return nil }

// execUnsupported reports whether an execve failure means the platform has no
// execve at all, rather than that this particular call went wrong. Unix always
// has one, so any failure is worth telling the operator about.
func execUnsupported(error) bool { return false }

// programName renders an executable's filename. Unix does not decorate them.
func programName(base string) string { return base }

// programBase recovers the logical name a program was invoked as, which is how
// waldo recognises that it is being run as a shim.
func programBase(argv0 string) string { return filepath.Base(argv0) }

// installProgramAlias makes dest another name for the running binary.
//
// A symlink is exactly right here: it costs nothing, it is obviously a link
// when an operator inspects the directory, and it cannot go stale in content —
// upgrading waldo in place changes what the link resolves to.
func installProgramAlias(self, dest string) error {
	_ = os.Remove(dest)
	return os.Symlink(self, dest)
}

// programAliasIsCurrent reports whether dest already points at self.
func programAliasIsCurrent(dest, self string) bool {
	current, err := os.Readlink(dest)
	return err == nil && current == self
}

// isExecutableFile reports whether a path can be run.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// shellCandidateNames are the filenames a real (non-shim) shell may have.
func shellCandidateNames() []string { return []string{"bash", "sh"} }

// fallbackShellPaths are searched when PATH yields no shell.
func fallbackShellPaths() []string {
	return []string{"/bin/bash", "/usr/bin/bash", "/bin/sh"}
}

// isPathEnvKey reports whether an environment key is the search path.
//
// Unix environments are case-sensitive, so PATH is PATH; a variable called
// "Path" is a different variable and must not be rewritten.
func isPathEnvKey(k string) bool { return k == "PATH" }
