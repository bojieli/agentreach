package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bojieli/waldo/internal/waldo"
)

// SSHConfig configures the SSH transport.
type SSHConfig struct {
	// Host is any destination the system ssh understands, including an alias
	// defined in ~/.ssh/config. waldo deliberately delegates destination
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
	// other system you can reach. waldo exists to work with untrusted hosts,
	// so this is opt-in and loudly documented.
	ForwardAgent bool

	// BatchMode disables all interactive prompts. It is off during `waldo up`
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
// waldo uses the ssh binary rather than a Go SSH library on purpose. Users
// reach real hosts through jump hosts, certificate authorities, hardware
// tokens, 1Password/gpg agents, Kerberos and Match blocks; reimplementing that
// surface faithfully is not realistic, and getting it subtly wrong strands
// people on exactly the hosts they most need to reach. ControlMaster gives
// connection reuse without reimplementing anything.
type SSHTransport struct {
	cfg         SSHConfig
	controlPath string

	mu     sync.Mutex
	closed bool
}

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
	cp, err := controlPathFor(cfg)
	if err != nil {
		return nil, err
	}
	return &SSHTransport{cfg: cfg, controlPath: cp}, nil
}

// controlPathFor derives a short, unique control socket path.
//
// Length matters: a unix socket path is capped at 104 bytes on macOS and 108
// on Linux, and ssh fails opaquely when the control path overruns it. A hash
// keeps the name bounded no matter how long the destination is.
func controlPathFor(cfg SSHConfig) (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "waldo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create control dir: %w", err)
	}
	key := fmt.Sprintf("%s|%s|%d|%v", cfg.Host, cfg.User, cfg.Port, cfg.ForwardAgent)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, "c-"+hex.EncodeToString(sum[:6])+".sock"), nil
}

func (t *SSHTransport) baseArgs() []string {
	c := t.cfg
	args := []string{}
	// WALDO_SSH_CONFIG points ssh at an alternate config file. This keeps CI
	// and test fixtures from having to write into the operator's ~/.ssh/config,
	// and lets an operator isolate waldo's connections from their interactive
	// ones without duplicating host definitions.
	if cfgFile := os.Getenv("WALDO_SSH_CONFIG"); cfgFile != "" {
		args = append(args, "-F", cfgFile)
	}
	args = append(args,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + t.controlPath,
		"-o", "ControlPersist=" + strconv.Itoa(int(c.ControlPersist.Seconds())),
		"-o", "ConnectTimeout=" + strconv.Itoa(int(c.ConnectTimeout.Seconds())),
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
func (t *SSHTransport) Run(ctx context.Context, req waldo.ExecRequest) (waldo.ExecResult, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return waldo.ExecResult{}, fmt.Errorf("ssh transport to %s is closed", t.cfg.Host)
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
				if asExitError(e, &ee) {
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

// Close tears down the multiplexed master so no connection outlives the
// session that created it. Leaving a live master to someone else's server
// would be a surprising residue for a tool whose premise is leaving no trace.
func (t *SSHTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	args := append(t.baseArgs(), "-O", "exit", t.cfg.Host)
	_ = exec.CommandContext(ctx, t.cfg.Binary, args...).Run()
	return nil
}

// Alive reports whether the multiplexed master is currently up.
func (t *SSHTransport) Alive(ctx context.Context) bool {
	args := append(t.baseArgs(), "-O", "check", t.cfg.Host)
	return exec.CommandContext(ctx, t.cfg.Binary, args...).Run() == nil
}

var _ Transport = (*SSHTransport)(nil)
