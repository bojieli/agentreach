package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/bojieli/agentreach/internal/session"
	"github.com/bojieli/agentreach/internal/transport"
)

// cmdCrush launches Crush wired to the session's target using Crush's native
// server mode.
//
// Crush has a split-process design: `crush server --host tcp://…` runs the
// agent loop and every tool call (bash, file read/write, edit, glob, grep)
// on one machine; a separate client process talks to it over TCP. reach
// exploits this directly:
//
//  1. Start `crush server --host tcp://127.0.0.1:PORT` on the session target
//     via the SSH transport's Open (a long-lived stream).
//  2. Forward that PORT to localhost through the existing SSH ControlMaster
//     socket so the crush client can reach it.
//  3. Wait until the server is accepting connections.
//  4. Exec into `crush --host tcp://127.0.0.1:PORT`, which connects to the
//     forwarded server.
//
// Every bash command and every file operation the agent issues runs on the
// server — which is the session target. Nothing touches the local filesystem.
// The PATH shim is not involved; server mode is end-to-end.
func cmdCrush(ctx context.Context, args []string) int {
	fs := newFlagSet("crush")
	name := fs.String("session", "", "session name (default $REACH_SESSION)")
	force := fs.Bool("force", false,
		"launch crush locally without a server on the target (tools will act on the LOCAL machine)")
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

	binPath, err := exec.LookPath("crush")
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach: crush is not installed or not in PATH")
		return 1
	}

	t, err := s.Transport()
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach:", err)
		return 1
	}

	// Crush server mode requires an SSH target: the server must run on the
	// remote machine and the TCP port must be forwarded through the SSH
	// tunnel. Docker and local sessions do not need the tunnel, so --force
	// launches the client locally for those callers who explicitly accept that
	// consequence.
	sshT, ok := t.(*transport.SSHTransport)
	if !ok {
		if *force {
			fmt.Fprintln(os.Stderr,
				"reach: WARNING: --force bypasses server mode. crush will run entirely\n"+
					"reach: on the LOCAL machine; its tools act on the operator's filesystem,\n"+
					"reach: not on the target.")
			env := append(os.Environ(), "REACH_SESSION="+sessName)
			argv := append([]string{binPath}, pos...)
			return replaceProcess(ctx, binPath, argv, env)
		}
		fmt.Fprintln(os.Stderr, "reach: crush server mode requires an SSH session (ssh://).")
		fmt.Fprintln(os.Stderr, "reach: Docker and local sessions are not supported.")
		fmt.Fprintln(os.Stderr, "reach: Use --force to launch crush locally (tools will act on the local machine).")
		return 1
	}

	// Pick a free local port. The same port number is used on both sides so
	// the forward spec is trivially symmetric (localPort:127.0.0.1:localPort).
	// If that port happens to be in use on the target, the server start will
	// fail visibly; the operator can re-run to get a different port.
	port, err := pickFreePort(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reach: cannot find a free local port:", err)
		return 1
	}

	serverCmd := fmt.Sprintf("crush server --host tcp://127.0.0.1:%d", port)
	fmt.Fprintf(os.Stderr,
		"reach: starting crush server on %s (port %d)...\n",
		s.Target.Describe(), port)

	// Start crush server on the target. Open keeps the SSH channel alive for
	// the server's lifetime. When this reach process is exec-replaced by the
	// crush client below, the SSH subprocess for this Open call becomes an
	// orphan; it eventually closes (SSH keepalive / ControlPersist), signalling
	// the remote crush server to terminate.
	if _, err := sshT.Open(ctx, serverCmd); err != nil {
		fmt.Fprintln(os.Stderr, "reach: cannot start crush server on the target:", err)
		return 1
	}

	// Add a local port forward to the existing ControlMaster connection. This
	// requires Multiplex=true, which reach sets when the session was created
	// with a capable ssh client. The forward is effectively instantaneous —
	// the master handles it without a new TCP connection.
	if err := sshT.ForwardLocal(ctx, port, port); err != nil {
		fmt.Fprintln(os.Stderr, "reach: cannot set up the SSH port forward:", err)
		fmt.Fprintln(os.Stderr, "reach: ensure the session was created with a multiplexing-capable ssh client.")
		return 1
	}

	// Poll until crush server is accepting connections. The server process
	// needs a moment to bind; without this wait, the crush client may
	// auto-start a local server instead of connecting to the remote one.
	if err := waitForCrushPort(ctx, port); err != nil {
		fmt.Fprintln(os.Stderr, "reach: crush server did not come up in time:", err)
		return 1
	}

	hostURL := fmt.Sprintf("tcp://127.0.0.1:%d", port)
	fmt.Fprintf(os.Stderr,
		"reach: Crush -> %s (server mode; every tool acts on the target)\n",
		s.Target.Describe())

	env := append(os.Environ(), "REACH_SESSION="+sessName)
	argv := append([]string{binPath, "--host", hostURL}, pos...)
	return replaceProcess(ctx, binPath, argv, env)
}

// pickFreePort asks the OS for an available TCP port on localhost by binding
// on port 0, reading the assigned address, and immediately releasing it.
// There is a small race between close and subsequent use, but that is
// acceptable here: the port space is not heavily contested and the window is
// measured in microseconds.
func pickFreePort(ctx context.Context) (int, error) {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("listener bound to %T, not TCP", l.Addr())
	}
	return addr.Port, nil
}

// waitForCrushPort polls 127.0.0.1:port until a TCP connection succeeds or
// the wait budget is exhausted. It is the minimum latency barrier between
// starting crush server on the target and launching the local client.
func waitForCrushPort(ctx context.Context, port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if conn, err := dialOnce(ctx, addr, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("no response on %s after 30 seconds", addr)
}

// dialOnce is one bounded connection attempt. The timeout is expressed as a
// context deadline derived from the caller's context so that cancelling the
// launch cancels the dial in progress rather than waiting it out.
func dialOnce(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	return d.DialContext(dialCtx, "tcp", addr)
}
