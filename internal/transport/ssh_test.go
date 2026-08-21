package transport

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// These cover the arguments reach hands to ssh, which is the most consequential
// pure function in the project: everything else assumes the command reached the
// machine the operator named, under the policy they chose. None of it was
// tested, because testing it looked like testing a string.

// argValue returns the value following the last -o KEY= in args, and whether
// the option is present at all.
func argValue(args []string, key string) (string, bool) {
	found, value := false, ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-o" && strings.HasPrefix(args[i+1], key+"=") {
			found, value = true, strings.TrimPrefix(args[i+1], key+"=")
		}
	}
	return value, found
}

func newTestSSH(t *testing.T, cfg SSHConfig) *SSHTransport {
	t.Helper()
	if cfg.Host == "" {
		cfg.Host = "host.invalid"
	}
	tr, err := NewSSH(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Agent forwarding is refused unless explicitly configured, and — this is the
// part worth a test — it is refused *explicitly*, with ForwardAgent=no, not by
// leaving the option out. Omitting it would let a `ForwardAgent yes` in the
// operator's own ~/.ssh/config take effect, handing a host with a hostile root
// the ability to authenticate as the operator everywhere else they can reach.
func TestSSHArgsRefuseAgentForwardingExplicitly(t *testing.T) {
	tr := newTestSSH(t, SSHConfig{})
	got, ok := argValue(tr.baseArgs(), "ForwardAgent")
	if !ok {
		t.Fatal("ForwardAgent is not set at all; the operator's ssh_config decides, which is not reach's call to delegate")
	}
	if got != "no" {
		t.Errorf("ForwardAgent=%s by default, want no", got)
	}
}

func TestSSHArgsForwardAgentOnlyWhenAsked(t *testing.T) {
	tr := newTestSSH(t, SSHConfig{ForwardAgent: true})
	if got, _ := argValue(tr.baseArgs(), "ForwardAgent"); got != "yes" {
		t.Errorf("ForwardAgent=%s when requested, want yes", got)
	}
}

// A half-open TCP connection is the failure this project refuses: the agent
// stops getting output and has nothing to reason about. Keepalives turn it into
// a timeout instead.
func TestSSHArgsAlwaysBoundTheConnection(t *testing.T) {
	args := newTestSSH(t, SSHConfig{}).baseArgs()
	for _, key := range []string{"ConnectTimeout", "ServerAliveInterval", "ServerAliveCountMax"} {
		v, ok := argValue(args, key)
		if !ok {
			t.Errorf("%s is not set; a dead link would block instead of failing", key)
			continue
		}
		if v == "" || v == "0" {
			t.Errorf("%s=%q, which does not bound anything", key, v)
		}
	}
}

// Multiplexing is what makes reach usable over a real link — measured at 4-5x
// per command — but it is only sent when the local ssh proved it supports it.
// Win32-OpenSSH does not, and refuses the options outright.
func TestSSHArgsMultiplexOnlyWhenEnabled(t *testing.T) {
	off := newTestSSH(t, SSHConfig{Multiplex: false}).baseArgs()
	for _, key := range []string{"ControlMaster", "ControlPath", "ControlPersist"} {
		if _, ok := argValue(off, key); ok {
			t.Errorf("%s is sent with multiplexing off; a client without the feature refuses it", key)
		}
	}

	tr := newTestSSH(t, SSHConfig{Multiplex: true, ControlPersist: 90 * time.Second})
	on := tr.baseArgs()
	if v, _ := argValue(on, "ControlMaster"); v != "auto" {
		t.Errorf("ControlMaster=%q, want auto", v)
	}
	if v, _ := argValue(on, "ControlPath"); v != tr.controlPath() {
		t.Errorf("ControlPath=%q, want %q", v, tr.controlPath())
	}
	if v, _ := argValue(on, "ControlPersist"); v != "90" {
		t.Errorf("ControlPersist=%q, want 90 (seconds, not a Go duration)", v)
	}
}

// BatchMode stops ssh prompting for a passphrase. A prompt inside a tool call
// is a process that has stopped responding, which is the one failure mode this
// project treats as unacceptable.
func TestSSHArgsBatchMode(t *testing.T) {
	if v, ok := argValue(newTestSSH(t, SSHConfig{BatchMode: true}).baseArgs(), "BatchMode"); !ok || v != "yes" {
		t.Errorf("BatchMode=%q present=%v, want yes", v, ok)
	}
	if _, ok := argValue(newTestSSH(t, SSHConfig{BatchMode: false}).baseArgs(), "BatchMode"); ok {
		t.Error("BatchMode is sent when not requested")
	}
}

func TestSSHArgsPortAndUser(t *testing.T) {
	args := newTestSSH(t, SSHConfig{Port: 2222, User: "deploy"}).baseArgs()
	if i := slices.Index(args, "-p"); i < 0 || args[i+1] != "2222" {
		t.Errorf("port not passed: %q", args)
	}
	if i := slices.Index(args, "-l"); i < 0 || args[i+1] != "deploy" {
		t.Errorf("user not passed: %q", args)
	}

	// A zero port and an empty user mean "whatever ssh_config says", and must
	// not be sent as 0 or an empty string — either would override the
	// operator's own configuration with nonsense.
	bare := newTestSSH(t, SSHConfig{}).baseArgs()
	if slices.Contains(bare, "-p") || slices.Contains(bare, "-l") {
		t.Errorf("unset port/user were sent anyway: %q", bare)
	}
}

func TestSSHArgsHonourAnAlternateConfig(t *testing.T) {
	t.Setenv("REACH_SSH_CONFIG", "/tmp/alt_ssh_config")
	args := newTestSSH(t, SSHConfig{}).baseArgs()
	i := slices.Index(args, "-F")
	if i < 0 || args[i+1] != "/tmp/alt_ssh_config" {
		t.Fatalf("-F not passed: %q", args)
	}
	// It has to come before everything else, or ssh has already resolved the
	// destination against the default config by the time it is read.
	if i != 0 {
		t.Errorf("-F is at position %d, want first", i)
	}
}

// The control socket identifies a connection. Two different destinations
// sharing one would mean commands for one host travelling down a master held
// open to another — the wrong-machine failure, at the transport layer.
func TestControlPathsDifferPerDestination(t *testing.T) {
	base := SSHConfig{Host: "a.example", User: "alice", Port: 22}
	variants := map[string]SSHConfig{
		"baseline":      base,
		"another host":  {Host: "b.example", User: "alice", Port: 22},
		"another user":  {Host: "a.example", User: "bob", Port: 22},
		"another port":  {Host: "a.example", User: "alice", Port: 2222},
		"agent forward": {Host: "a.example", User: "alice", Port: 22, ForwardAgent: true},
	}

	seen := map[string]string{}
	for name, cfg := range variants {
		p, err := controlBaseFor(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if other, clash := seen[p]; clash {
			t.Errorf("%q and %q share the control path %s; commands would cross connections",
				name, other, p)
		}
		seen[p] = name
	}
}

// Same destination, same socket — otherwise every command opens its own master
// and multiplexing buys nothing.
func TestControlPathIsStableForOneDestination(t *testing.T) {
	cfg := SSHConfig{Host: "a.example", User: "alice", Port: 22}
	first, err := controlBaseFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := controlBaseFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("control path is not stable: %s then %s", first, second)
	}
}

// A unix socket path is capped at 104 bytes on macOS and 108 on Linux, and ssh
// fails opaquely when it overruns. The hash exists to bound it; this is the
// test that the bound actually holds for a destination long enough to matter.
func TestControlPathStaysWithinTheSocketLimit(t *testing.T) {
	base, err := controlBaseFor(SSHConfig{
		Host: strings.Repeat("very-long-hostname.", 20) + "example.com",
		User: strings.Repeat("u", 200),
		Port: 22,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The longest name any connection number can produce, not just the first.
	p := (&SSHTransport{controlBase: base}).controlPathAt(maxOverflow)
	if len(p) >= 104 {
		t.Errorf("control path is %d bytes (%s); the macOS limit is 104 and ssh fails opaquely past it",
			len(p), p)
	}
}

func TestSSHDescribeNamesTheTarget(t *testing.T) {
	for _, tc := range []struct {
		cfg  SSHConfig
		want string
	}{
		{SSHConfig{Host: "box.example"}, "ssh://box.example"},
		{SSHConfig{Host: "box.example", User: "deploy"}, "ssh://deploy@box.example"},
	} {
		if got := newTestSSH(t, tc.cfg).Describe(); got != tc.want {
			t.Errorf("Describe() = %q, want %q", got, tc.want)
		}
	}
}

func TestNewSSHRequiresAHost(t *testing.T) {
	if _, err := NewSSH(SSHConfig{}); err == nil {
		t.Fatal("NewSSH accepted an empty host; there would be no way to know where a command went")
	}
}

func TestNewSSHDefaults(t *testing.T) {
	tr := newTestSSH(t, SSHConfig{})
	if tr.cfg.Binary != "ssh" {
		t.Errorf("binary defaults to %q, want ssh", tr.cfg.Binary)
	}
	if tr.cfg.ConnectTimeout == 0 {
		t.Error("connect timeout defaults to 0, which never gives up")
	}
	if tr.cfg.ControlPersist == 0 {
		t.Error("control persist defaults to 0, which would close the master immediately")
	}
}

// --- containers ------------------------------------------------------------

func TestContainerExecArgs(t *testing.T) {
	tr, err := NewContainer(ContainerConfig{Container: "app", Runtime: "podman", User: "www"})
	if err != nil {
		t.Fatal(err)
	}
	got := tr.execArgs(false)
	want := []string{"podman", "exec", "-u", "www", "app"}
	if !slices.Equal(got, want) {
		t.Errorf("execArgs(false) = %q, want %q", got, want)
	}

	// -i is what lets a write's content reach the container at all; without it
	// docker closes stdin and the file arrives empty.
	if got := tr.execArgs(true); !slices.Contains(got, "-i") {
		t.Errorf("execArgs(true) = %q, missing -i", got)
	}

	// The container name has to be last: everything after it is the command.
	if got := tr.execArgs(true); got[len(got)-1] != "app" {
		t.Errorf("execArgs(true) = %q, want the container name last", got)
	}
}

func TestNewContainerDefaults(t *testing.T) {
	tr, err := NewContainer(ContainerConfig{Container: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.cfg.Runtime != "docker" {
		t.Errorf("runtime defaults to %q, want docker", tr.cfg.Runtime)
	}
	if tr.cfg.Shell != "/bin/sh" {
		t.Errorf("shell defaults to %q, want /bin/sh", tr.cfg.Shell)
	}
	if got := tr.Describe(); got != "docker://app" {
		t.Errorf("Describe() = %q, want docker://app", got)
	}

	if _, err := NewContainer(ContainerConfig{}); err == nil {
		t.Error("NewContainer accepted an empty container name")
	}
}
