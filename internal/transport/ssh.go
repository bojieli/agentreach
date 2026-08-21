package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

// SSHConfig configures the SSH transport.
type SSHConfig struct {
	// Host is any destination the system ssh understands, including an alias
	// defined in ~/.ssh/config. reach deliberately delegates destination
	// parsing to ssh so that Host, ProxyJump, IdentityFile, Match blocks and
	// hardware-token setups all keep working untouched.
	Host string
	User string
	Port int

	// ControlPersist is how long the multiplexed master outlives its last
	// channel. Zero uses a sensible default.
	ControlPersist time.Duration

	// ConnectTimeout bounds the initial TCP/handshake phase.
	ConnectTimeout time.Duration

	// ForwardAgent enables SSH agent forwarding. It defaults to false and
	// should stay false for any host you do not fully trust: a forwarded
	// agent socket lets root on that host authenticate as you against every
	// other system you can reach. reach exists to work with untrusted hosts,
	// so this is opt-in and loudly documented.
	ForwardAgent bool

	// Multiplex keeps one authenticated connection alive and runs later
	// commands as channels on it. It is the difference between ~7 ms and
	// ~130 ms per command, and between one authentication and one per tool
	// call — but Win32-OpenSSH does not implement it, so this is decided per
	// host by DetectMultiplexing rather than compiled in as a constant.
	Multiplex bool

	// BatchMode disables all interactive prompts. It is off during `reach up`
	// so first-connection password or 2FA prompts can be answered, and on
	// afterwards so an expired credential fails fast instead of hanging a
	// tool call on an invisible prompt.
	BatchMode bool

	// ExtraOptions are passed through as -o KEY=VALUE.
	ExtraOptions []string

	// Binary overrides the ssh executable.
	Binary string
}

// SSHTransport runs commands over the system ssh client with connection
// multiplexing.
//
// reach uses the ssh binary rather than a Go SSH library on purpose. Users
// reach real hosts through jump hosts, certificate authorities, hardware
// tokens, 1Password/gpg agents, Kerberos and Match blocks; reimplementing that
// surface faithfully is not realistic, and getting it subtly wrong strands
// people on exactly the hosts they most need to reach. ControlMaster gives
// connection reuse without reimplementing anything.
type SSHTransport struct {
	cfg SSHConfig
	// controlBase is the control socket path without its connection number.
	// See controlPathAt.
	controlBase string

	mu     sync.Mutex
	closed bool
	// overflow is which of this destination's connections commands currently
	// run on. It only ever moves forward, and only when the target refuses a
	// new channel on the one before it. See Overflow.
	overflow int
	// everOverflowed records that a second connection was opened, so Close
	// knows to tear down more than one master.
	everOverflowed bool
}

// maxOverflow bounds how many extra connections reach will open to one
// destination before it reports the refusal instead of working around it.
//
// Overflow exists to survive sshd's MaxSessions, which defaults to 10 channels
// per connection — an agent running eleven tool calls at once hits it. It is
// not a licence to open connections without limit: something that refuses every
// channel on every connection is not a capacity problem, and quietly opening
// connections forever would turn one bad answer into a connection flood against
// someone else's server.
const maxOverflow = 4

// NewSSH builds an SSH transport. It does not connect; the first command
// establishes the multiplexed master.
func NewSSH(cfg SSHConfig) (*SSHTransport, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("ssh transport: host is required")
	}
	if cfg.Binary == "" {
		cfg.Binary = "ssh"
	}
	if cfg.ControlPersist == 0 {
		cfg.ControlPersist = 10 * time.Minute
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	cb, err := controlBaseFor(cfg)
	if err != nil {
		return nil, err
	}
	return &SSHTransport{cfg: cfg, controlBase: cb}, nil
}

// controlBaseFor derives a short, unique control socket path for a destination,
// without the connection number appended by controlPathAt.
//
// Length matters: a unix socket path is capped at 104 bytes on macOS and 108
// on Linux, and ssh fails opaquely when the control path overruns it. A hash
// keeps the name bounded no matter how long the destination is.
func controlBaseFor(cfg SSHConfig) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "reach")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create control dir: %w", err)
	}
	key := fmt.Sprintf("%s|%s|%d|%v", cfg.Host, cfg.User, cfg.Port, cfg.ForwardAgent)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "c-"+hex.EncodeToString(sum[:6])), nil
}

// controlPathAt names the socket for connection n to this destination.
//
// Connection 0 is the one every reach process starts on, so a session, a tool
// call and a `reach doctor` all share one authentication. The rest exist only
// because the target refused a channel on the one before; see Overflow.
func (t *SSHTransport) controlPathAt(n int) string {
	return t.controlBase + "-" + strconv.Itoa(n) + ".sock"
}

// controlPath names the socket commands currently run over.
func (t *SSHTransport) controlPath() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.controlPathAt(t.overflow)
}

// Overflow moves this transport onto a fresh connection to the same target and
// reports whether it could.
//
// sshd caps concurrent channels per connection at MaxSessions, 10 by default.
// Multiplexing means every tool call reach runs at once is a channel on one
// connection, so an agent that fans out past that cap has its eleventh tool
// call refused — and the refusal arrives as ssh exit 255 with "administratively
// prohibited", which reach reported as "command did not complete". That names
// neither the cause nor anything the operator can do.
//
// A second connection is the answer, and paying one authentication per ten
// concurrent channels is the right trade: it is exactly the cost multiplexing
// avoids in the common case and the only thing that buys capacity in this one.
//
// Without multiplexing there is nothing to overflow — every command already
// opens its own connection, so it can never be refused for want of a channel.
func (t *SSHTransport) Overflow() bool {
	if !t.cfg.Multiplex {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.overflow >= maxOverflow {
		return false
	}
	t.overflow++
	t.everOverflowed = true
	return true
}

// channelOpenFailures are what the ssh client says when the target refused a
// new channel rather than the connection itself failing.
//
// The first is sshd at its MaxSessions limit; the second and third are how the
// multiplexing client reports the same refusal relayed by its master; the last
// is a target out of file descriptors or memory. Matching a message is
// unpleasant, but ssh offers no other channel: every one of these is exit 255
// with no distinguishing status.
var channelOpenFailures = []string{
	"administratively prohibited",
	"open refused by peer",
	"session request failed",
	"resource shortage",
}

// IsChannelOpenFailure reports whether ssh's output describes a target that
// refused another channel on a connection that is otherwise working.
//
// It matters that this is distinguishable from a connection failure: a refused
// channel means the command never ran, so retrying it on another connection is
// safe. A dropped connection says nothing about whether the command ran.
func IsChannelOpenFailure(s string) bool {
	lower := strings.ToLower(s)
	for _, sig := range channelOpenFailures {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

func (t *SSHTransport) baseArgs() []string {
	return t.baseArgsAt(t.controlPath())
}

// baseArgsAt builds the option list for one specific control socket, so Close
// can address a connection other than the current one.
func (t *SSHTransport) baseArgsAt(controlPath string) []string {
	c := t.cfg
	args := []string{}
	// REACH_SSH_CONFIG points ssh at an alternate config file. This keeps CI
	// and test fixtures from having to write into the operator's ~/.ssh/config,
	// and lets an operator isolate reach's connections from their interactive
	// ones without duplicating host definitions.
	if cfgFile := os.Getenv("REACH_SSH_CONFIG"); cfgFile != "" {
		args = append(args, "-F", cfgFile)
	}
	if c.Multiplex {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+controlPath,
			"-o", "ControlPersist="+strconv.Itoa(int(c.ControlPersist.Seconds())),
		)
	}
	args = append(args,
		"-o", "ConnectTimeout="+strconv.Itoa(int(c.ConnectTimeout.Seconds())),
		// Detect a dead link instead of blocking forever on a half-open TCP
		// connection. This is the difference between an agent seeing a timeout
		// it can retry and an agent hanging indefinitely.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	)
	if c.ForwardAgent {
		args = append(args, "-o", "ForwardAgent=yes")
	} else {
		args = append(args, "-o", "ForwardAgent=no")
	}
	if c.BatchMode {
		args = append(args, "-o", "BatchMode=yes")
	}
	if c.Port != 0 {
		args = append(args, "-p", strconv.Itoa(c.Port))
	}
	if c.User != "" {
		args = append(args, "-l", c.User)
	}
	for _, o := range c.ExtraOptions {
		args = append(args, "-o", o)
	}
	return args
}

// Run implements Transport.
//
// A command the target refused to give a channel to is retried on another
// connection. That retry is safe in a way retrying a failed command generally
// is not: a refused channel means the remote shell was never started, so there
// is nothing to have half-happened. Anything else — including a connection that
// dropped mid-command — is reported, because it says nothing about whether the
// command ran.
func (t *SSHTransport) Run(ctx context.Context, req reach.ExecRequest) (reach.ExecResult, error) {
	for {
		res, err := t.run(ctx, req)
		if err == nil || !IsChannelOpenFailure(err.Error()) {
			return res, err
		}
		if !t.Overflow() {
			return reach.ExecResult{}, t.refusedError(err)
		}
	}
}

// refusedError explains a refusal reach could not work around, which is the one
// case an operator has to act on themselves.
func (t *SSHTransport) refusedError(cause error) error {
	return fmt.Errorf("%w\n\n"+
		"%s refused a new channel on every connection reach opened (%d).\n"+
		"sshd caps concurrent sessions per connection at MaxSessions, 10 by default,\n"+
		"and reach runs one channel per tool call. Either raise MaxSessions on the\n"+
		"target or have the agent run fewer commands at once.",
		cause, t.cfg.Host, maxOverflow+1)
}

func (t *SSHTransport) run(ctx context.Context, req reach.ExecRequest) (reach.ExecResult, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return reach.ExecResult{}, fmt.Errorf("ssh transport to %s is closed", t.cfg.Host)
	}
	t.mu.Unlock()

	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	sentinel := newSentinel()
	remote := wrapWithSentinel(BuildCommand(req), sentinel)

	args := t.baseArgs()
	if req.PTY {
		args = append(args, "-tt")
	} else {
		args = append(args, "-T")
	}
	// The command must be exactly one argv element: ssh joins multiple
	// trailing arguments with spaces before handing them to the remote shell,
	// which would silently re-split anything containing whitespace.
	args = append(args, t.cfg.Host, remote)

	start := time.Now()
	so, se, code, trunc, err := runLocalProcess(ctx, append([]string{t.cfg.Binary}, args...), req.Stdin, req.MaxOutput)
	return finishExec(start, so, se, code, trunc, sentinel, err, "ssh "+t.cfg.Host)
}

// Open implements Transport, starting a long-lived remote process.
func (t *SSHTransport) Open(ctx context.Context, command string) (Stream, error) {
	args := append(t.baseArgs(), "-T", t.cfg.Host, command)
	cmd := exec.CommandContext(ctx, t.cfg.Binary, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Stream{}, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Stream{}, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Stream{}, fmt.Errorf("ssh stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Stream{}, fmt.Errorf("ssh start: %w", err)
	}

	var once sync.Once
	var waitErr error
	var waitCode int
	wait := func() (int, error) {
		once.Do(func() {
			e := cmd.Wait()
			if e != nil {
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
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		Wait: wait,
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
func (t *SSHTransport) Describe() string {
	if t.cfg.User != "" {
		return "ssh://" + t.cfg.User + "@" + t.cfg.Host
	}
	return "ssh://" + t.cfg.Host
}

// ForwardLocal requests the existing ControlMaster to forward localPort on
// localhost to 127.0.0.1:remotePort on the SSH target.
//
// This is the standard ssh(1) "local forward" (`-L`), issued here as a
// control-socket request (`-O forward`) so that no new TCP connection is
// opened — the port forward is added to the already-authenticated master.
// It requires Multiplex to be true; callers that need a forward on a
// non-multiplexed session must spawn their own ssh -L process.
func (t *SSHTransport) ForwardLocal(ctx context.Context, localPort, remotePort int) error {
	if !t.cfg.Multiplex {
		return fmt.Errorf("ForwardLocal requires a multiplexed SSH connection (Multiplex=true)")
	}
	spec := fmt.Sprintf("%d:127.0.0.1:%d", localPort, remotePort)
	args := append(t.baseArgs(), "-O", "forward", "-L", spec, t.cfg.Host)
	if out, err := exec.CommandContext(ctx, t.cfg.Binary, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh -O forward -L %s: %w; ssh said: %s",
			spec, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Close tears down the multiplexed master so no connection outlives the
// session that created it. Leaving a live master to someone else's server
// would be a surprising residue for a tool whose premise is leaving no trace.
//
// Without multiplexing there is nothing to tear down: each command owned its
// own connection and closed it when it exited. The guarantee holds either way,
// which is the one consolation of the slower path.
func (t *SSHTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	last := t.overflow
	t.mu.Unlock()

	if !t.cfg.Multiplex {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Every connection this transport opened, not only the one it ended on: an
	// overflow connection left running is exactly the residue on someone else's
	// server that closing the master exists to prevent.
	for n := 0; n <= last; n++ {
		args := append(t.baseArgsAt(t.controlPathAt(n)), "-O", "exit", t.cfg.Host)
		_ = exec.CommandContext(ctx, t.cfg.Binary, args...).Run()
	}
	return nil
}

var _ Transport = (*SSHTransport)(nil)
