package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// Mode selects how much of the harness's tool surface waldo redirects.
type Mode string

const (
	// ModeExec redirects command execution only. Harnesses whose file tools
	// cannot be redirected must have those tools denied in this mode, because
	// a native Read or Write would silently act on the local filesystem while
	// the agent believes it is working on the target.
	ModeExec Mode = "exec"

	// ModeMirror additionally materialises the workspace as real local files
	// so native file tools operate at native speed.
	ModeMirror Mode = "mirror"
)

// Session is the persisted binding between a shell and a target.
//
// State lives in a file rather than a daemon. Connection reuse — the only thing
// a daemon would have bought — is already provided by SSH's ControlMaster,
// measured against real hosts at 4-5x faster per command than reconnecting
// (171ms against 772ms on one, 557ms against 2.85s on another). A daemon would
// add a lifecycle, a socket, crash recovery and orphaned processes in exchange
// for nothing.
type Session struct {
	Name     string     `json:"name"`
	Target   *Target    `json:"target"`
	Mode     Mode       `json:"mode"`
	Tier     waldo.Tier `json:"-"`
	TierName string     `json:"tier"`
	// Pinned records that the operator named this tier with --fileops. A pinned
	// tier is an instruction, not a preference: waldo fails rather than quietly
	// giving them a different one.
	Pinned bool `json:"pinned,omitempty"`
	// MultiplexNote explains why multiplexing is unavailable, and is empty when
	// it is available.
	MultiplexNote string `json:"multiplex_note,omitempty"`
	// TierReason explains a tier that is lower than the one asked for, and is
	// empty when nothing was degraded.
	TierReason string                `json:"tier_reason,omitempty"`
	Caps       *fileops.Capabilities `json:"caps"`
	Created    time.Time             `json:"created"`
	// Multiplex records whether the local ssh client proved it can hold a
	// multiplexed master to this target. It is the difference between ~7 ms and
	// ~130 ms per command, and it is recorded rather than assumed because
	// Win32-OpenSSH does not implement the feature.
	Multiplex bool `json:"multiplex"`
	// Untrusted marks a target whose operator you are not. waldo will not
	// install anything on it and will not forward an SSH agent to it.
	Untrusted bool `json:"untrusted"`
	// Timeout bounds an individual command.
	Timeout time.Duration `json:"timeout"`
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Dir returns waldo's state directory.
func Dir() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// ConfDir holds files waldo generates for harnesses, such as settings
// documents.
//
// These are kept out of the sessions directory deliberately. Session discovery
// enumerates that directory, and a generated file that happens to parse as
// JSON would otherwise be loaded as a session with no target — which crashed
// `waldo status` with a nil dereference until this separation existed.
func ConfDir() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	dir := filepath.Join(base, "conf")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

func pathFor(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

func cwdPathFor(name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".cwd"), nil
}

// Save writes the session atomically.
func (s *Session) Save() error {
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("invalid session name %q: use letters, digits, dot, dash or underscore", s.Name)
	}
	s.TierName = s.Tier.String()
	p, err := pathFor(s.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads a session by name.
func Load(name string) (*Session, error) {
	p, err := pathFor(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no waldo session named %q. Start one with:\n  waldo up ssh://host/path --name %s", name, name)
		}
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session %q is corrupt (%w); remove %s and start it again", name, err, p)
	}
	// Defence in depth: a document that parses as JSON but is not a session
	// must not produce a Session with nil fields that callers will dereference.
	if s.Target == nil || s.Name == "" {
		return nil, fmt.Errorf("%s is not a waldo session file", p)
	}
	if t, err := waldo.ParseTier(s.TierName); err == nil {
		s.Tier = t
	}
	return &s, nil
}

// List returns all known sessions.
func List() ([]*Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, err := Load(strings.TrimSuffix(e.Name(), ".json"))
		if err == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// Remove deletes a session's state.
func Remove(name string) error {
	p, err := pathFor(name)
	if err != nil {
		return err
	}
	c, _ := cwdPathFor(name)
	_ = os.Remove(c)
	return os.Remove(p)
}

// Cwd returns the session's current working directory on the target,
// defaulting to the workspace root.
func (s *Session) Cwd() string {
	p, err := cwdPathFor(s.Name)
	if err != nil {
		return s.Target.Workspace
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return s.Target.Workspace
	}
	if cwd := strings.TrimSpace(string(data)); cwd != "" {
		return cwd
	}
	return s.Target.Workspace
}

// SetCwd records the working directory.
//
// This is kept in its own small file rather than inside the session JSON: it
// changes on almost every command, and rewriting the whole session document
// each time would make concurrent commands race over unrelated fields.
func (s *Session) SetCwd(cwd string) error {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return nil
	}
	p, err := cwdPathFor(s.Name)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if err := os.WriteFile(tmp, []byte(cwd+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Transport builds the transport this session's target needs, in batch mode:
// no interactive prompts, so an expired credential fails fast instead of
// hanging a tool call on a password prompt the agent cannot see or answer.
func (s *Session) Transport() (transport.Transport, error) {
	return s.transport(true)
}

// InteractiveTransport allows ssh to prompt.
//
// The first connection to a host may legitimately need a passphrase, a
// password or a 2FA touch. That has to be possible somewhere, and `waldo up`
// is the one moment an operator is present to answer. Afterwards ControlMaster
// keeps the authenticated connection alive, so later tool calls never prompt.
func (s *Session) InteractiveTransport() (transport.Transport, error) {
	return s.transport(false)
}

func (s *Session) transport(batch bool) (transport.Transport, error) {
	switch s.Target.Kind {
	case KindSSH:
		return transport.NewSSH(transport.SSHConfig{
			Host: s.Target.Host,
			User: s.Target.User,
			Port: s.Target.Port,
			// Whether the local ssh client can multiplex was settled during
			// `waldo up` by establishing one and asking the client to confirm
			// it. Assuming it here would mean sending options that a client
			// without the feature may refuse outright.
			Multiplex: s.Multiplex,
			// Agent forwarding is refused outright for untrusted targets: a
			// forwarded agent socket lets root on that host authenticate as
			// the operator against every other system they can reach.
			ForwardAgent: false,
			BatchMode:    batch,
		})
	case KindDocker, KindPodman:
		return transport.NewContainer(transport.ContainerConfig{
			Runtime:   string(s.Target.Kind),
			Container: s.Target.Container,
		})
	case KindLocal:
		return transport.NewLocal()
	}
	return nil, fmt.Errorf("unsupported target kind %q", s.Target.Kind)
}

// defaultOperationTimeout bounds a file operation when a session predates the
// Timeout field or was written with a zero.
const defaultOperationTimeout = 2 * time.Minute

// OperationContext bounds one file operation with the session's timeout.
//
// Without this, a target that accepts a request and never answers leaves the
// tool call blocked forever, which is precisely the failure this project exists
// to eliminate: an agent cannot reason about a process that has stopped
// responding, but it can reason about a timeout. Applying it here covers every
// tier at once rather than relying on each strategy to remember.
func (s *Session) OperationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// checkWorkspace verifies the session's directory exists on the target.
func (s *Session) checkWorkspace(ctx context.Context, t transport.Transport) error {
	res, err := t.Run(ctx, waldo.ExecRequest{
		Command:   fmt.Sprintf("test -d %s", transport.ShellQuote(s.Target.Workspace)),
		MaxOutput: 4 << 10,
	})
	if err != nil {
		return err
	}
	if res.Code == 0 {
		return nil
	}
	return fmt.Errorf(
		"%s is not a directory on %s.\n"+
			"waldo will not create it: making directories on a machine you pointed at is\n"+
			"not something this tool should do uninvited. Create it there, or point waldo\n"+
			"at a path that exists.", s.Target.Workspace, s.Target.Describe())
}

// FileOps builds the file-operation strategy for this session's tier.
//
// A pinned tier — one the operator named with --fileops — is never silently
// replaced. An autonegotiated one steps down to whatever works and says so on
// stderr, because a host that stopped answering on its usual tier should keep
// working, but not without the operator being able to see that it changed.
func (s *Session) FileOps(ctx context.Context, t transport.Transport) (fileops.Selection, error) {
	if s.Tier == waldo.TierAgent && s.Untrusted {
		return fileops.Selection{}, fmt.Errorf(
			"session %q is marked --untrusted, and the agent tier installs a binary on the target.\n"+
				"Re-create the session without --untrusted, or use a tier that installs nothing.", s.Name)
	}
	warn := func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	return fileops.New(ctx, s.Tier, t, s.Caps, s.Pinned, warn)
}

// Probe connects to the target, records its capabilities, and settles which
// file-operation tier this session will use.
//
// The chosen tier is *built* here, not merely selected. Recording a tier that
// turns out to be unusable would move the failure from `waldo up`, where an
// operator is present and can act on it, into the middle of an agent's turn,
// where it surfaces as a broken tool.
func (s *Session) Probe(ctx context.Context) error {
	t, err := s.InteractiveTransport()
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()

	caps, err := fileops.Probe(ctx, t)
	if err != nil {
		return err
	}
	s.Caps = caps

	// Settle connection multiplexing now, while an operator is present to read
	// the answer, rather than discovering it inside a tool call. The probe
	// establishes a master and asks the client to confirm it, so the result
	// reflects what this client will actually do against this host.
	if s.Target.Kind == KindSSH {
		ok, why := transport.DetectMultiplexing(ctx, transport.SSHConfig{
			Host: s.Target.Host,
			User: s.Target.User,
			Port: s.Target.Port,
		})
		s.Multiplex = ok
		s.MultiplexNote = why
	}

	// Confirm the workspace is really there. Without this, `waldo up` succeeds
	// against any reachable host and then *every* command fails with a `cd`
	// error from the target — which reads as waldo being broken rather than as
	// a path being wrong, and does so once per tool call rather than once, in
	// front of the operator who typed the path.
	if err := s.checkWorkspace(ctx, t); err != nil {
		return err
	}

	if !s.Pinned {
		// Autonegotiation deliberately stops below TierAgent: that tier writes
		// a binary to the target, and waldo never makes that choice on the
		// operator's behalf.
		s.Tier = caps.BestTier()
	}

	sel, err := s.FileOps(ctx, t)
	if err != nil {
		return err
	}
	defer func() { _ = sel.Ops.Close() }()
	s.Tier = sel.Effective
	s.TierReason = sel.Reason
	return nil
}
