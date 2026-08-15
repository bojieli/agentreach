//go:build !windows

package transport

// localShell resolves the shell used by a local:// target.
func localShell() (string, error) { return "/bin/sh", nil }
