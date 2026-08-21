// Command reach runs a coding agent's tools against a remote target while the
// agent — and the credentials it holds — stay on the local machine.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = `reach — teleoperation for coding agents

Point your agent at any box you can SSH into. The box never gets your agent.

USAGE
  reach <command> [arguments]

SESSIONS
  up <target>       bind a session to a target and probe it
  down [name]       end a session and close its connection
  status [name]     show sessions
  doctor [name]     diagnose a target: what works, what degrades, and why
  log [name]        what reach has run and changed on the target
  helper <op>       inspect or remove the optional helper binary

RUNNING
  exec [cmd...]     run a command on the target
  fs <op> ...       read, write, list, search files on the target
  shell-prefix      internal: entrypoint for CLAUDE_CODE_SHELL_PREFIX
  hook              internal: harness hook entrypoint (mirror mode)
  exec-server       internal: codex remote-environment endpoint (JSON-RPC over stdio)

HARNESSES
  claude [args...]  launch Claude Code wired to the session
  codex [args...]   launch Codex wired to the session
  kimi [args...]    launch Kimi Code wired to the session
  goose [args...]   launch Goose wired to the session
  gemini [args...]  launch Gemini CLI wired to the session
  crush [args...]   launch Crush wired to the session (server mode)
  grok [args...]    launch Grok Build wired to the session
  opencode install  install tools that shadow opencode's built-ins
  env               print the environment a harness needs
  harness verify claude|codex|kimi|goose|gemini|grok   probe whether this harness's shell routes through reach

TARGETS
  ssh://[user@]host[:port]/abs/path    a remote host over SSH
  docker://container/abs/path          a container
  local:///abs/path                    this machine (for testing)
  user@host:/abs/path                  scp-style shorthand

EXAMPLES
  reach up ssh://build-box/srv/app
  reach up ssh://client-box/srv/app --untrusted --name client
  reach claude
  reach exec -- go test ./...

Run 'reach <command> --help' for details.
`

func main() {
	// The platform check comes before everything, including the shim dispatch.
	// A shim that half-works on an unsupported platform is the worst outcome
	// this program has: the harness cannot tell its shell was not redirected, so
	// the model's commands run on the operator's own machine while the agent
	// believes they are running on the target.
	if err := platformCheck(); err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		os.Exit(2)
	}

	// The shim path is latency-sensitive and runs once per tool call, so it is
	// dispatched before anything else and does no extra work. Harnesses invoke
	// it through a symlink, which arrives as argv[0].
	if isShimInvocation() {
		os.Exit(runShellPrefix(os.Args[1:]))
	}
	if isBashShimInvocation() {
		os.Exit(runBashShim(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if os.Args[1] == "shell-prefix" {
		os.Exit(runShellPrefix(os.Args[2:]))
	}
	if os.Args[1] == "hook" {
		os.Exit(runHook(os.Args[2:]))
	}
	if os.Args[1] == "exec-server" {
		os.Exit(cmdExecServer(context.Background(), os.Args[2:]))
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
	case "fs":
		err = cmdFS(ctx, os.Args[2:])
	case "env":
		err = cmdEnv(ctx, os.Args[2:])
	case "claude":
		os.Exit(cmdClaude(ctx, os.Args[2:]))
	case "codex":
		os.Exit(cmdCodex(ctx, os.Args[2:]))
	case "kimi":
		os.Exit(cmdKimi(ctx, os.Args[2:]))
	case "goose":
		os.Exit(cmdGoose(ctx, os.Args[2:]))
	case "gemini":
		os.Exit(cmdGemini(ctx, os.Args[2:]))
	case "crush":
		os.Exit(cmdCrush(ctx, os.Args[2:]))
	case "grok":
		os.Exit(cmdGrok(ctx, os.Args[2:]))
	case "opencode":
		err = cmdOpencode(ctx, os.Args[2:])
	case "harness":
		err = cmdHarness(ctx, os.Args[2:])
	case "helper":
		err = cmdHelper(ctx, os.Args[2:])
	case "agent":
		// Renamed. Anyone with this in a script or in muscle memory deserves
		// the new name, not "unknown command".
		err = fmt.Errorf("`reach agent` is now `reach helper`.\n" +
			"It was renamed because \"agent\" already means the coding agent this tool " +
			"drives, which made `reach agent uninstall` read like it removed Claude Code")
	case "log":
		err = cmdLog(ctx, os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println(versionLine())
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "reach: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		os.Exit(1)
	}
}
