package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/bojieli/waldo/internal/session"
)

// deniedFileTools are Claude Code's native file tools.
//
// In exec mode they must be denied, and this is a safety property rather than
// a preference. These tools have no interception seam: they would keep reading
// and writing the *local* filesystem while the agent believes it is operating
// on the target. An agent that reads a local file it thinks is remote reports
// confident nonsense; one that writes a local file it thinks is remote can
// destroy the operator's own work. Making that impossible is worth the
// ergonomic cost of routing file access through the shell.
var deniedFileTools = []string{"Read", "Edit", "Write", "NotebookEdit", "Glob", "Grep"}

const execModeGuidance = `This session is operating on a REMOTE target through waldo.

Your Bash tool runs on the remote target, not on this machine. The native file
tools (Read, Edit, Write, Glob, Grep) are disabled because they would act on
the local machine instead of the target, which would silently give you the
wrong file.

Use shell commands for all file access; they run on the target:
  read      cat -- FILE        (or sed -n 'A,Bp' FILE for a range)
  search    rg PATTERN DIR     (falls back to grep if ripgrep is absent)
  list      ls -la DIR ; find DIR -name PATTERN
  write     cat > FILE <<'EOF' ... EOF
  edit      apply a patch, or use sed -i / python3 for in-place edits

Paths are the target's own absolute paths. Do not translate them.`

func cmdClaude(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("claude", flag.ContinueOnError)
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	allowFileTools := fs.Bool("allow-local-file-tools", false,
		"do not deny Claude Code's native file tools (unsafe: they act on the LOCAL filesystem)")
	pos, perr := parseFlags(fs, args)
	if perr != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)
	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}

	shim, err := ensureShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		return 1
	}
	claudePath, err := exeLook("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: Claude Code is not installed or not in PATH")
		return 1
	}

	env := append(os.Environ(),
		"WALDO_SESSION="+sessName,
		"CLAUDE_CODE_SHELL_PREFIX="+shim,
	)

	argv := []string{claudePath}
	if s.Mode == session.ModeExec && !*allowFileTools {
		settings, err := writeDenySettings(sessName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "waldo:", err)
			return 1
		}
		argv = append(argv, "--settings", settings, "--append-system-prompt", execModeGuidance)
	}
	argv = append(argv, pos...)

	fmt.Fprintf(os.Stderr, "waldo: Claude Code -> %s (bash runs on the target)\n", s.Target.Describe())

	// Replace this process so signals, the terminal and the exit status all
	// belong to Claude Code directly, with no wrapper in between.
	if err := syscall.Exec(claudePath, argv, env); err != nil {
		// Exec only returns on failure; fall back to a child process.
		c := exec.Command(claudePath, argv[1:]...)
		c.Env, c.Stdin, c.Stdout, c.Stderr = env, os.Stdin, os.Stdout, os.Stderr
		if runErr := c.Run(); runErr != nil {
			var ee *exec.ExitError
			if ok := errorsAs(runErr, &ee); ok {
				return ee.ExitCode()
			}
			fmt.Fprintln(os.Stderr, "waldo:", runErr)
			return 1
		}
	}
	return 0
}

// writeDenySettings emits a Claude Code settings file denying the local file
// tools, and returns its path.
func writeDenySettings(sessName string) (string, error) {
	dir, err := session.Dir()
	if err != nil {
		return "", err
	}
	type perms struct {
		Deny []string `json:"deny"`
	}
	doc := map[string]any{"permissions": perms{Deny: deniedFileTools}}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, sessName+".claude-settings.json")
	if err := os.WriteFile(p, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func errorsAs(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
