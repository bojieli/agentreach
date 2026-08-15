//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// unsupportedPlatformOverride lets someone working on a Windows port run the
// binary anyway. It exists so that porting does not require patching and
// rebuilding, and it is deliberately verbose to type: nobody should reach for
// it without having read why it is there.
const unsupportedPlatformOverride = "WALDO_ALLOW_UNSUPPORTED_PLATFORM"

// platformCheck refuses to run on native Windows.
//
// waldo compiles here, which is the problem: it would start, and then fail in
// scattered ways that look like bugs rather than like an unsupported platform.
// Three things are missing, and none is a small fix:
//
//   - Go stubs syscall.Exec on Windows to return EWINDOWS, so waldo cannot hand
//     the terminal to a harness the way it does elsewhere.
//   - Win32-OpenSSH does not implement ControlMaster. Every command would pay a
//     full connection setup, and `ssh -O exit` — how waldo guarantees it leaves
//     no live connection to someone else's server — does not exist.
//   - The shell shims are symlinks, which need Developer Mode or an
//     administrator on Windows, and the script fallback is `#!/bin/sh`.
//
// The failure this project treats as unacceptable is an agent acting on the
// wrong machine while believing otherwise. A half-working shim is exactly how
// that happens: the harness cannot tell that its shell was not redirected, so
// it runs the model's commands locally. Refusing at startup, before any session
// exists, is the only honest option until the port is done and *verified* —
// this project does not claim platform support it has not tested.
//
// WSL is the working path today, and it is not a workaround: inside WSL this is
// Linux, which is a first-class supported platform.
func platformCheck() error {
	if os.Getenv(unsupportedPlatformOverride) != "" {
		return nil
	}
	return errors.New(
		"waldo does not support native Windows yet.\n\n" +
			"Use WSL: install waldo and your agent inside the WSL distribution, and run\n" +
			"them there. WSL is Linux, which waldo supports and tests on every commit.\n\n" +
			"Native Windows needs an execve replacement, shims that are not symlinks, and\n" +
			"a story for ControlMaster, which Win32-OpenSSH does not implement. Until that\n" +
			"exists and is tested, waldo refuses to start here rather than half-working:\n" +
			"a shell shim that fails silently would let your agent run commands on this\n" +
			"machine while believing it is working on the target.\n\n" +
			"Set " + unsupportedPlatformOverride + "=1 to override, if you are working on the port.")
}

// execUnsupported reports whether an execve failure is simply Windows having no
// execve. Go stubs syscall.Exec here to return EWINDOWS unconditionally, so
// there is nothing to report to the operator: falling back to a child process
// is the normal path rather than a degradation worth a warning.
func execUnsupported(err error) bool { return errors.Is(err, syscall.EWINDOWS) }
