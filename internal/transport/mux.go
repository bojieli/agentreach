package transport

import (
	"context"
	"os/exec"
	"time"

	"github.com/bojieli/agentreach/internal/reach"
)

// Connection multiplexing is the single largest performance difference between
// the platforms reach runs on, so it is decided by evidence rather than by an
// assumption compiled into the binary.
//
// On Unix, OpenSSH's ControlMaster keeps one authenticated connection alive and
// runs later commands as new channels on it: ~7 ms per command against ~130 ms
// for a cold connect, and one authentication instead of one per tool call.
// reach's whole "no daemon" argument rests on it — a daemon would buy exactly
// this and nothing else.
//
// Win32-OpenSSH does not implement it. The mux protocol passes file descriptors
// over a Unix domain socket, which Windows has no equivalent for, so the
// feature is absent rather than merely unconfigured. reach therefore starts
// without the options on Windows and probes: it never sends a client an option
// that might make it refuse the connection, and if a future Windows OpenSSH
// gains multiplexing, reach will find it and use it with no code change.

// muxProbeTimeout bounds the probe. Tier and capability decisions must always
// terminate: an unanswerable question is a value reach records, not a wait.
const muxProbeTimeout = 20 * time.Second

// interactiveProbeTimeout bounds a probe that may put a prompt in front of an
// operator. Twenty seconds is generous for a machine and short for a person
// finding a hardware token.
const interactiveProbeTimeout = 3 * time.Minute

// DetectMultiplexing reports whether the local ssh client can hold a
// multiplexed master connection to this destination, and why not when it
// cannot.
//
// It proves the answer by establishing one and asking the client to confirm it,
// rather than by inspecting a version string: a version tells you what was
// compiled in, not what a `Match` block, a hardened configuration or a
// restricted socket directory will permit at run time.
func DetectMultiplexing(ctx context.Context, cfg SSHConfig) (bool, string) {
	// cfg.BatchMode is the caller's to set. The probe used to force it on, which
	// made it unanswerable on exactly the hosts multiplexing matters most for:
	// a password or a hardware token cannot be supplied in batch mode, so the
	// probe failed, reach recorded "no multiplexing", and every later tool call
	// opened its own connection — in batch mode, and so failed too. `reach up`
	// and `reach doctor` both run with an operator present, and both let the
	// probe prompt.
	timeout := muxProbeTimeout
	if !cfg.BatchMode {
		timeout = interactiveProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cfg.Multiplex = true
	probe, err := NewSSH(cfg)
	if err != nil {
		return false, err.Error()
	}
	// A master that could not be proved working is torn down; one that was is
	// kept. It is this destination's connection, now authenticated, and every
	// connection after this one runs in batch mode — so closing it would throw
	// away the one authentication an operator was present to complete, and hand
	// the first tool call a reconnect it cannot perform. `reach down` ends it,
	// which is what `reach down` is for.
	keep := false
	defer func() {
		if !keep {
			_ = probe.Close()
		}
	}()

	if _, err := probe.Run(ctx, reach.ExecRequest{Command: "true"}); err != nil {
		return false, "the ssh client rejected the multiplexing options: " + err.Error()
	}
	if !probe.Alive(ctx) {
		return false, "the ssh client accepted the multiplexing options but no master is running"
	}
	keep = true
	return true, ""
}

// Alive reports whether the multiplexed master is currently up.
func (t *SSHTransport) Alive(ctx context.Context) bool {
	if !t.cfg.Multiplex {
		return false
	}
	args := append(t.baseArgs(), "-O", "check", t.cfg.Host)
	return exec.CommandContext(ctx, t.cfg.Binary, args...).Run() == nil
}
