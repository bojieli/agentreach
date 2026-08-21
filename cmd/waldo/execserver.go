package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bojieli/waldo/internal/execserver"
	"github.com/bojieli/waldo/internal/session"
)

// cmdExecServer is the entrypoint codex spawns when environments.toml names
// waldo as a remote environment. It speaks codex's exec-server JSON-RPC
// protocol on stdin/stdout, backing every method with the session named by
// WALDO_SESSION (or --session). Stderr is free for logging; stdout carries
// only protocol frames.
//
// This is an internal command, like shell-prefix and hook: codex launches it,
// people do not.
func cmdExecServer(ctx context.Context, args []string) int {
	fs := newFlagSet("exec-server")
	name := fs.String("session", "", "session name (default $WALDO_SESSION)")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	sessName := sessionNameFromEnv(*name)
	s, err := session.Load(sessName)
	if err != nil {
		// Fail loudly. Codex would otherwise see a hang or a protocol error it
		// cannot attribute; the operator reading this knows exactly what to do.
		fmt.Fprintln(os.Stderr, "waldo exec-server:", err)
		return 1
	}

	// The workspace is the local directory codex was launched in; paths under
	// it map onto the session's root on the target. Codex spawns this process
	// with its own cwd, so the working directory is the default and the
	// variable an explicit override.
	workspace := os.Getenv("WALDO_EXEC_WORKSPACE")

	srv, err := execserver.New(ctx, s, workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo exec-server:", err)
		return 1
	}
	defer func() { _ = srv.Close() }()

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "waldo exec-server:", err)
		return 1
	}
	return 0
}
