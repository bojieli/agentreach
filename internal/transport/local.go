package transport

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/bojieli/waldo/internal/waldo"
)

// LocalTransport runs commands on the machine waldo itself runs on.
//
// It exists so the whole stack above it — file-operation tiers, the daemon,
// every harness adapter — can be exercised in tests without a network, and so
// `waldo up local:///path` is a working degenerate case rather than a special
// path through the code.
type LocalTransport struct {
	Shell string
}

// NewLocal builds a local transport.
//
// It resolves a POSIX shell rather than assuming /bin/sh, because on Windows
// there is not one. A local:// target means "the machine waldo is running on is
// the target", and every file-operation tier below the agent speaks to a target
// through a POSIX shell — so on Windows this is only usable when Git for
// Windows or MSYS2 has supplied one, and says so plainly when it has not.
func NewLocal() (*LocalTransport, error) {
	shell, err := localShell()
	if err != nil {
		return nil, err
	}
	return &LocalTransport{Shell: shell}, nil
}

// Run implements Transport.
func (t *LocalTransport) Run(ctx context.Context, req waldo.ExecRequest) (waldo.ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	sentinel := newSentinel()
	wrapped := wrapWithSentinel(BuildCommand(req), sentinel)

	start := time.Now()
	so, se, code, trunc, err := runLocalProcess(ctx, []string{t.Shell, "-c", wrapped}, req.Stdin, req.MaxOutput)
	return finishExec(start, so, se, code, trunc, sentinel, err, "local")
}

// Open implements Transport.
func (t *LocalTransport) Open(ctx context.Context, command string) (Stream, error) {
	cmd := exec.CommandContext(ctx, t.Shell, "-c", command)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Stream{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Stream{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Stream{}, err
	}
	if err := cmd.Start(); err != nil {
		return Stream{}, err
	}
	var once sync.Once
	var waitErr error
	var waitCode int
	wait := func() (int, error) {
		once.Do(func() {
			if e := cmd.Wait(); e != nil {
				var ee *exec.ExitError
				if errors.As(e, &ee) {
					waitCode = ee.ExitCode()
				} else {
					waitErr = e
				}
			}
		})
		return waitCode, waitErr
	}
	return Stream{
		Stdin: stdin, Stdout: stdout, Stderr: stderr, Wait: wait,
		Close: func() error {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_, _ = wait()
			return nil
		},
	}, nil
}

// Describe implements Transport.
func (t *LocalTransport) Describe() string { return "local://" }

// Close implements Transport.
func (t *LocalTransport) Close() error { return nil }

var _ Transport = (*LocalTransport)(nil)

// ContainerConfig configures a container transport.
type ContainerConfig struct {
	// Runtime is the CLI to drive: docker, podman, or nerdctl.
	Runtime string
	// Container is the container name or id.
	Container string
	// User optionally overrides the exec user.
	User string
	// Shell is the shell inside the container.
	Shell string
}

// ContainerTransport runs commands inside a container via the runtime CLI.
//
// This is the transport to develop against: it exercises the identical code
// path as ssh with no network in between, which makes it the honest way to
// prove transparency claims before blaming a flaky link for a bug.
type ContainerTransport struct {
	cfg ContainerConfig
}

// NewContainer builds a container transport.
func NewContainer(cfg ContainerConfig) (*ContainerTransport, error) {
	if cfg.Container == "" {
		return nil, fmt.Errorf("container transport: container is required")
	}
	if cfg.Runtime == "" {
		cfg.Runtime = "docker"
	}
	if cfg.Shell == "" {
		cfg.Shell = "/bin/sh"
	}
	return &ContainerTransport{cfg: cfg}, nil
}

func (t *ContainerTransport) execArgs(interactive bool) []string {
	args := []string{t.cfg.Runtime, "exec"}
	if interactive {
		args = append(args, "-i")
	}
	if t.cfg.User != "" {
		args = append(args, "-u", t.cfg.User)
	}
	return append(args, t.cfg.Container)
}

// Run implements Transport.
func (t *ContainerTransport) Run(ctx context.Context, req waldo.ExecRequest) (waldo.ExecResult, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	sentinel := newSentinel()
	wrapped := wrapWithSentinel(BuildCommand(req), sentinel)

	argv := append(t.execArgs(len(req.Stdin) > 0), t.cfg.Shell, "-c", wrapped)
	start := time.Now()
	so, se, code, trunc, err := runLocalProcess(ctx, argv, req.Stdin, req.MaxOutput)
	return finishExec(start, so, se, code, trunc, sentinel, err, t.cfg.Runtime+" "+t.cfg.Container)
}

// Open implements Transport.
func (t *ContainerTransport) Open(ctx context.Context, command string) (Stream, error) {
	argv := append(t.execArgs(true), t.cfg.Shell, "-c", command)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Stream{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Stream{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Stream{}, err
	}
	if err := cmd.Start(); err != nil {
		return Stream{}, err
	}
	var once sync.Once
	var waitErr error
	var waitCode int
	wait := func() (int, error) {
		once.Do(func() {
			if e := cmd.Wait(); e != nil {
				var ee *exec.ExitError
				if errors.As(e, &ee) {
					waitCode = ee.ExitCode()
				} else {
					waitErr = e
				}
			}
		})
		return waitCode, waitErr
	}
	return Stream{
		Stdin: stdin, Stdout: stdout, Stderr: stderr, Wait: wait,
		Close: func() error {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_, _ = wait()
			return nil
		},
	}, nil
}

// Describe implements Transport.
func (t *ContainerTransport) Describe() string {
	return t.cfg.Runtime + "://" + t.cfg.Container
}

// Close implements Transport.
func (t *ContainerTransport) Close() error { return nil }

var _ Transport = (*ContainerTransport)(nil)
