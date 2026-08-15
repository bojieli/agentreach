package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: Claude Code is not installed or not in PATH")
		return 1
	}

	env := append(os.Environ(),
		"WALDO_SESSION="+sessName,
		"CLAUDE_CODE_SHELL_PREFIX="+shim,
	)

	argv := []string{claudePath}
	switch {
	case s.Mode == session.ModeMirror:
		settings, err := writeMirrorSettings(sessName)
		if err != nil {
			fmt.Fprintln(os.Stderr, "waldo:", err)
			return 1
		}
		argv = append(argv, "--settings", settings, "--append-system-prompt", mirrorModeGuidance)
	case !*allowFileTools:
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
	return replaceProcess(ctx, claudePath, argv, env)
}

const mirrorModeGuidance = `This session is operating on a REMOTE target through waldo.

Your Bash tool runs on the target. Read, Write and Edit also act on the target:
waldo fetches each file the moment you open it and writes it back when you
change it. Use the target's own absolute paths; do not translate them.

Grep and Glob are disabled. They would search a local cache that holds only the
files you have already opened, so they would report "no matches" for code that
does exist. Search with the shell instead, which runs on the target:
  rg 'pattern' DIR        (or grep -rn if ripgrep is absent)
  find DIR -name 'glob'

If a write is refused because the file changed on the target, re-read it and
redo the change; do not retry blindly.`

// writeMirrorSettings emits a settings file wiring waldo's hook into the file
// tools, and returns its path.
func writeMirrorSettings(sessName string) (string, error) {
	dir, err := session.ConfDir()
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	hookCmd := self + " hook"
	type hookSpec struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcherSpec struct {
		Matcher string     `json:"matcher"`
		Hooks   []hookSpec `json:"hooks"`
	}
	doc := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []matcherSpec{{
				Matcher: "Read|Write|Edit|NotebookEdit|Grep|Glob",
				Hooks:   []hookSpec{{Type: "command", Command: hookCmd}},
			}},
			"PostToolUse": []matcherSpec{{
				Matcher: "Write|Edit|NotebookEdit",
				Hooks:   []hookSpec{{Type: "command", Command: hookCmd}},
			}},
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, sessName+".claude-mirror-settings.json")
	if err := os.WriteFile(p, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return p, nil
}

// writeDenySettings emits a Claude Code settings file denying the local file
// tools, and returns its path.
func writeDenySettings(sessName string) (string, error) {
	dir, err := session.ConfDir()
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
