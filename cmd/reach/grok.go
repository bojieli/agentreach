package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bojieli/agentreach/internal/harnessprobe"
	"github.com/bojieli/agentreach/internal/session"
)

const grokExecGuidance = `This session is operating on a REMOTE target through reach.

Your run_terminal_command tool runs on the remote target, not on this machine.
Native file tools (read_file, search_replace, list_dir, grep) are denied
because they would act on the local machine instead of the target.

Use shell commands for all file access; they run on the target:
  read      cat -- FILE
  search    rg PATTERN DIR     (falls back to grep if ripgrep is absent)
  list      ls -la DIR ; find DIR -name PATTERN
  write     cat > FILE <<'EOF' ... EOF
  edit      apply a patch, or use sed -i / python3 for in-place edits

Paths are the target's own absolute paths. Do not translate them.`

// grokDeniedPermissionRules are Grok Build permission-deny prefixes for tools
// that call the local filesystem. A bare prefix matches every invocation.
// Names follow Grok's rule-matching reference (Read/Edit/Write/Grep).
var grokDeniedPermissionRules = []string{"Read", "Edit", "Write", "Grep"}

// cmdGrok launches Grok Build against the session's target.
//
// Grok 1.0.5 resolves its command shell from $SHELL (absolute path; a PATH
// shim is not consulted). reach sets SHELL — and GROK_SHELL, the config
// crate's documented override — to the PATH shim's bash so every
// run_terminal_command call is intercepted.
//
// Grok wraps each tool command in a local snapshot envelope
// (`__grok_user_cmd="$1" … -- <command>`). The shim unwraps that envelope
// and runs only the payload on the target; Grok's `-lc` environment
// snapshots stay local.
//
// File tools (read_file, search_replace, list_dir, grep) call the local
// disk. They are denied via --deny rules so the model uses the shell
// instead. Subagents are disabled because a child would inherit local
// file tools.
func cmdGrok(ctx context.Context, args []string) int {
	fs := newFlagSet("grok")
	name := fs.String("session", "", "session name (default $REACH_SESSION)")
	force := fs.Bool("force", false,
		"launch without verifying the shell seam (the agent's commands may run LOCALLY)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)

	s, err := session.Load(sessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}

	binPath, err := exec.LookPath("grok")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach: grok is not installed or not in PATH")
		return 1
	}

	shimDir, err := ensurePathShim()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}

	if *force {
		fmt.Fprintln(os.Stderr,
			"reach: WARNING: --force skips the grok seam verification.\n"+
				"reach: If this grok version ignores $SHELL, its commands will run on\n"+
				"reach: the LOCAL machine while appearing remote.")
	} else if rc := guardHarnessSeam(ctx, harnessprobe.HarnessGrok, sessName); rc != 0 {
		return rc
	}

	shimBash := filepath.Join(shimDir, bashShimName)
	cwd, _ := os.Getwd()

	env := os.Environ()
	env = replaceEnv(env, "SHELL", shimBash)
	env = append(env, "REACH_SESSION="+sessName)
	env = append(env, "GROK_SHELL="+shimBash)
	env = append(env, "GROK_SUBAGENTS=0")
	env = append(env, "GROK_SANDBOX=off")
	env = append(env, "REACH_EXEC_WORKSPACE="+cwd)
	env = prependPath(env, shimDir)

	argv := []string{binPath, "--no-subagents", "--sandbox", "off", "--rules", grokExecGuidance}
	for _, rule := range grokDeniedPermissionRules {
		argv = append(argv, "--deny", rule)
	}
	argv = append(argv, pos...)

	fmt.Fprintf(os.Stderr,
		"reach: Grok Build -> %s (shell via $SHELL; file tools denied)\n",
		s.Target.Describe())

	return replaceProcess(ctx, binPath, argv, env)
}
