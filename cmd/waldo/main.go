// Command waldo runs a coding agent's tools against a remote target while the
// agent — and the credentials it holds — stay on the local machine.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `waldo — teleoperation for coding agents

The agent stays on your machine. Its tools act somewhere else.

USAGE
  waldo <command> [arguments]

SESSIONS
  up <target>       bind a session to a target and probe it
  down [name]       end a session and close its connection
  status [name]     show sessions
  doctor [name]     diagnose a target: what works, what degrades, and why

RUNNING
  exec [cmd...]     run a command on the target
  shell-prefix      internal: entrypoint for CLAUDE_CODE_SHELL_PREFIX

HARNESSES
  claude [args...]  launch Claude Code wired to the session
  env               print the environment a harness needs

TARGETS
  ssh://[user@]host[:port]/abs/path    a remote host over SSH
  docker://container/abs/path          a container
  local:///abs/path                    this machine (for testing)
  user@host:/abs/path                  scp-style shorthand

EXAMPLES
  waldo up ssh://build-box/srv/app
  waldo up ssh://client-box/srv/app --untrusted --name client
  waldo claude
  waldo exec -- go test ./...

Run 'waldo <command> --help' for details.
`

func main() {
	// The shim path is latency-sensitive and runs once per tool call, so it is
	// dispatched before anything else and does no extra work. Harnesses invoke
	// it through a symlink, which arrives as argv[0].
	if isShimInvocation() {
		os.Exit(runShellPrefix(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "shell-prefix" {
		os.Exit(runShellPrefix(os.Args[2:]))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "up":
		err = cmdUp(ctx, os.Args[2:])
	case "down":
		err = cmdDown(ctx, os.Args[2:])
	case "status":
		err = cmdStatus(ctx, os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "exec":
		os.Exit(cmdExec(ctx, os.Args[2:]))
	case "env":
		err = cmdEnv(ctx, os.Args[2:])
	case "claude":
		os.Exit(cmdClaude(ctx, os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("waldo", version())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "waldo: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo:", err)
		os.Exit(1)
	}
}
