package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// replaceProcess hands control of the terminal to a harness and returns its
// exit status.
//
// The preferred mechanism is execve: replacing this process means signals, job
// control, the terminal and the exit status all belong to the harness directly,
// with no wrapper in between to get them subtly wrong. Ctrl-C reaches the agent
// rather than killing waldo and orphaning it.
//
// But execve is not universally available. Go stubs syscall.Exec on Windows to
// return EWINDOWS unconditionally, and it can fail on Unix too — ETXTBSY when
// the binary is being written, or under a sandbox that forbids it. Two of the
// three call sites used to print that error and exit, which turned "this
// platform has no execve" into "waldo is broken" with no route forward. Falling
// back to a child process is worse in the ways described above, and enormously
// better than not starting.
func replaceProcess(ctx context.Context, path string, argv []string, env []string) int {
	if err := execve(path, argv, env); err != nil && !execUnsupported(err) {
		// On a platform that has execve, a failure is worth reporting: it means
		// something unusual, and the fallback's differences in signal handling
		// may matter to whoever is debugging it.
		fmt.Fprintf(os.Stderr, "waldo: could not replace this process (%v); running %s as a child instead\n",
			err, path)
	}

	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The harness owns the terminal while it runs, so waldo must not react to
	// the signals the user aims at it. Without this, Ctrl-C would kill waldo and
	// leave the agent running with no parent.
	cmd.Cancel = func() error { return nil }

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	return 0
}

// execve is a variable so the fallback path can be tested. Everywhere else it
// is syscall.Exec, which does not return on success and therefore cannot be
// exercised from inside a test process without ending it.
var execve = syscall.Exec
