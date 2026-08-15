package transport

import (
	"context"
	"os/exec"
	"time"

	"github.com/bojieli/waldo/internal/waldo"
)

// Connection multiplexing is the single largest performance difference between
// the platforms waldo runs on, so it is decided by evidence rather than by an
// assumption compiled into the binary.
//
// On Unix, OpenSSH's ControlMaster keeps one authenticated connection alive and
// runs later commands as new channels on it: ~7 ms per command against ~130 ms
// for a cold connect, and one authentication instead of one per tool call.
// waldo's whole "no daemon" argument rests on it — a daemon would buy exactly
// this and nothing else.
//
// Win32-OpenSSH does not implement it. The mux protocol passes file descriptors
// over a Unix domain socket, which Windows has no equivalent for, so the
// feature is absent rather than merely unconfigured. waldo therefore starts
// without the options on Windows and probes: it never sends a client an option
// that might make it refuse the connection, and if a future Windows OpenSSH
// gains multiplexing, waldo will find it and use it with no code change.

// muxProbeTimeout bounds the probe. Tier and capability decisions must always
// terminate: an unanswerable question is a value waldo records, not a wait.
const muxProbeTimeout = 20 * time.Second

// DetectMultiplexing reports whether the local ssh client can hold a
// multiplexed master connection to this destination, and why not when it
// cannot.
//
// It proves the answer by establishing one and asking the client to confirm it,
// rather than by inspecting a version string: a version tells you what was
// compiled in, not what a `Match` block, a hardened configuration or a
// restricted socket directory will permit at run time.
func DetectMultiplexing(ctx context.Context, cfg SSHConfig) (bool, string) {
	ctx, cancel := context.WithTimeout(ctx, muxProbeTimeout)
	defer cancel()

	cfg.Multiplex = true
	cfg.BatchMode = true
	probe, err := NewSSH(cfg)
	if err != nil {
		return false, err.Error()
	}
	// Whatever happens, do not leave a master behind that the caller did not
	// ask for and will not know to close.
	defer func() { _ = probe.Close() }()

	if _, err := probe.Run(ctx, waldo.ExecRequest{Command: "true"}); err != nil {
		return false, "the ssh client rejected the multiplexing options: " + err.Error()
	}
	if !probe.Alive(ctx) {
		return false, "the ssh client accepted the multiplexing options but no master is running"
	}
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
