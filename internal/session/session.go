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
// State lives in a file rather than a daemon. Connection reuse — the only
// thing a daemon would have bought — is already provided by SSH's
// ControlMaster, measured at ~7ms per command against ~130ms for a cold
// connect. A daemon would add a lifecycle, a socket, crash recovery and
// orphaned processes in exchange for nothing.
type Session struct {
	Name      string        `json:"name"`
	Target    *Target       `json:"target"`
	Mode      Mode          `json:"mode"`
	Tier      waldo.Tier    `json:"-"`
	TierName  string        `json:"tier"`
	Caps      *fileops.Capabilities `json:"caps"`
	Created   time.Time     `json:"created"`
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

// Transport builds the transport this session's target needs.
func (s *Session) Transport() (transport.Transport, error) {
	switch s.Target.Kind {
	case KindSSH:
		return transport.NewSSH(transport.SSHConfig{
			Host: s.Target.Host,
			User: s.Target.User,
			Port: s.Target.Port,
			// Agent forwarding is refused outright for untrusted targets: a
			// forwarded agent socket lets root on that host authenticate as
			// the operator against every other system they can reach.
			ForwardAgent: false,
			BatchMode:    true,
		})
	case KindDocker, KindPodman:
		return transport.NewContainer(transport.ContainerConfig{
			Runtime:   string(s.Target.Kind),
			Container: s.Target.Container,
		})
	case KindLocal:
		return transport.NewLocal(), nil
	}
	return nil, fmt.Errorf("unsupported target kind %q", s.Target.Kind)
}

// FileOps builds the file-operation strategy for this session's tier.
func (s *Session) FileOps(t transport.Transport) (fileops.FileOps, error) {
	switch s.Tier {
	case waldo.TierPOSIX:
		return fileops.NewPOSIX(t, s.Caps), nil
	default:
		// Higher tiers fall back rather than fail: a target that no longer
		// supports its recorded tier should degrade, not stop working.
		return fileops.NewPOSIX(t, s.Caps), nil
	}
}

// Probe connects to the target and records its capabilities.
func (s *Session) Probe(ctx context.Context) error {
	t, err := s.Transport()
	if err != nil {
		return err
	}
	defer t.Close()

	caps, err := fileops.Probe(ctx, t)
	if err != nil {
		return err
	}
	s.Caps = caps
	if s.Tier == waldo.TierPOSIX && caps.BestTier() > waldo.TierPOSIX {
		// Autonegotiation deliberately stops below TierAgent; writing a binary
		// to someone else's machine stays an explicit operator decision.
		s.Tier = waldo.TierPOSIX
	}
	return nil
}
