// Package session owns reach's per-session state: which target a shell is
// bound to, where it is working, and what that target's userland supports.
package session

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Kind identifies a target family.
type Kind string

// The target families reach can reach.
const (
	KindSSH    Kind = "ssh"
	KindDocker Kind = "docker"
	KindPodman Kind = "podman"
	KindLocal  Kind = "local"
)

// Target is a parsed target specification.
type Target struct {
	Kind Kind   `json:"kind"`
	Host string `json:"host,omitempty"`
	User string `json:"user,omitempty"`
	Port int    `json:"port,omitempty"`
	// Container is the container name for container kinds.
	Container string `json:"container,omitempty"`
	// Workspace is the absolute directory on the target that the session
	// operates in.
	Workspace string `json:"workspace"`
	// Raw is the original specification, kept for diagnostics.
	Raw string `json:"raw"`
}

// ParseTarget accepts the forms:
//
//	ssh://[user@]host[:port]/abs/path
//	ssh://alias/abs/path            (alias resolved by the user's ssh config)
//	docker://container/abs/path
//	podman://container/abs/path
//	local:///abs/path
//	user@host:/abs/path             (scp-style shorthand)
//
// Host is deliberately passed through to the ssh client untouched, so entries
// in ~/.ssh/config — ProxyJump, IdentityFile, Match blocks, hardware tokens —
// keep working exactly as they do outside reach.
func ParseTarget(spec string) (*Target, error) {
	if spec == "" {
		return nil, fmt.Errorf("empty target")
	}

	// scp-style shorthand, e.g. root@box:/srv/app
	if !strings.Contains(spec, "://") {
		host, wsPath, ok := strings.Cut(spec, ":")
		if !ok {
			return nil, fmt.Errorf("target %q: expected scheme://... or [user@]host:/path", spec)
		}
		t := &Target{Kind: KindSSH, Raw: spec, Workspace: wsPath}
		if u, h, has := strings.Cut(host, "@"); has {
			t.User, t.Host = u, h
		} else {
			t.Host = host
		}
		return t, validate(t)
	}

	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", spec, err)
	}
	t := &Target{Raw: spec, Workspace: u.Path}

	switch u.Scheme {
	case "ssh":
		t.Kind = KindSSH
		t.Host = u.Hostname()
		if u.User != nil {
			t.User = u.User.Username()
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("target %q: bad port %q", spec, p)
			}
			t.Port = n
		}
	case "docker", "podman":
		t.Kind = Kind(u.Scheme)
		t.Container = u.Host
	case "local":
		t.Kind = KindLocal
	default:
		return nil, fmt.Errorf("target %q: unsupported scheme %q (want ssh, docker, podman or local)", spec, u.Scheme)
	}
	return t, validate(t)
}

func validate(t *Target) error {
	switch t.Kind {
	case KindSSH:
		if t.Host == "" {
			return fmt.Errorf("target %q: missing host", t.Raw)
		}
	case KindDocker, KindPodman:
		if t.Container == "" {
			return fmt.Errorf("target %q: missing container name", t.Raw)
		}
	}
	if t.Workspace == "" {
		return fmt.Errorf("target %q: missing workspace path; specify the directory to work in, e.g. ssh://host/srv/app", t.Raw)
	}
	if !path.IsAbs(t.Workspace) {
		return fmt.Errorf("target %q: workspace %q must be an absolute path on the target", t.Raw, t.Workspace)
	}
	return nil
}

// Describe renders a short identity for diagnostics.
func (t *Target) Describe() string {
	switch t.Kind {
	case KindSSH:
		h := t.Host
		if t.User != "" {
			h = t.User + "@" + h
		}
		if t.Port != 0 {
			h = fmt.Sprintf("%s:%d", h, t.Port)
		}
		return "ssh://" + h + t.Workspace
	case KindDocker, KindPodman:
		return string(t.Kind) + "://" + t.Container + t.Workspace
	default:
		return "local://" + t.Workspace
	}
}
