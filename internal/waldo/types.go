// Package waldo defines the core types shared by transports, file-operation
// strategies, sessions and adapters.
package waldo

import (
	"fmt"
	"io/fs"
	"time"
)

// Version is the waldo release. It is part of the remote agent cache path, so
// bumping it forces a fresh agent install rather than silent reuse of a stale
// binary.
const Version = "0.1.0"

// ExecRequest is a command to run on a target.
type ExecRequest struct {
	// Command is a shell command line, interpreted by the target's shell.
	Command string
	// Dir is the working directory. Empty means the target's default.
	Dir string
	// Env are extra environment variables. Targets may refuse to set some.
	Env map[string]string
	// Stdin is fed to the command. Nil means /dev/null; commands are never
	// left waiting on a terminal that will not arrive.
	Stdin []byte
	// Timeout bounds the whole call. Zero means the session default.
	Timeout time.Duration
	// MaxOutput caps captured stdout and stderr independently. Zero means the
	// session default. Output beyond the cap is dropped and flagged, so a
	// runaway log cannot wedge a session or blow up an agent's context.
	MaxOutput int64
	// PTY requests a pseudo-terminal. Some tools change behaviour without one.
	PTY bool
}

// ExecResult is the outcome of an ExecRequest.
type ExecResult struct {
	Stdout    []byte
	Stderr    []byte
	Code      int
	Truncated bool
	Duration  time.Duration
}

// FileInfo describes a remote directory entry. It is deliberately a plain
// struct rather than fs.FileInfo so it can cross the daemon RPC boundary.
type FileInfo struct {
	Name    string      `json:"name"`
	Path    string      `json:"path"`
	Size    int64       `json:"size"`
	Mode    fs.FileMode `json:"mode"`
	ModTime time.Time   `json:"mod_time"`
	IsDir   bool        `json:"is_dir"`
	IsLink  bool        `json:"is_link"`
	// LinkTarget is set for symlinks when the strategy can resolve it cheaply.
	LinkTarget string `json:"link_target,omitempty"`
}

// Match is a single search hit.
type Match struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SearchRequest describes a server-side content search. Executing this on the
// target and returning only matches is the operation a filesystem mount cannot
// do efficiently, and the main reason waldo is not a mount.
type SearchRequest struct {
	Pattern    string
	Root       string
	Glob       string
	IgnoreCase bool
	MaxResults int
	// Literal disables regex interpretation of Pattern.
	Literal bool
}

// Tier identifies a file-operation strategy, ordered by capability.
type Tier int

const (
	// TierPOSIX uses only a POSIX shell on the target. Universal; requires
	// nothing installed and writes nothing to the target's disk.
	TierPOSIX Tier = iota
	// TierPipe streams a stdlib-only handler over stdin. No disk footprint.
	TierPipe
	// TierHelper uses a small binary waldo installs on the target. The only
	// tier that writes anything there, and never chosen by autonegotiation.
	TierHelper
)

func (t Tier) String() string {
	switch t {
	case TierPOSIX:
		return "posix"
	case TierPipe:
		return "pipe"
	case TierHelper:
		return "helper"
	}
	return fmt.Sprintf("tier(%d)", int(t))
}

// ParseTier maps a CLI value to a Tier.
func ParseTier(s string) (Tier, error) {
	switch s {
	case "posix":
		return TierPOSIX, nil
	case "sftp":
		// Removed rather than renamed, and worth saying so: an operator who
		// pinned it deserves to know it is gone and why, not to be told the
		// name is unknown.
		return 0, fmt.Errorf("the sftp tier was removed: it could not answer a tool call in one " +
			"round trip, because SFTP hands out a handle before it will read, and it no longer " +
			"moved bytes faster than the shell tier once that tier stopped base64-encoding them. " +
			"Use posix (installs nothing), pipe (needs python3), or agent")
	case "pipe":
		return TierPipe, nil
	case "helper":
		return TierHelper, nil
	case "agent":
		// Renamed, not removed. "agent" already meant the coding agent — the
		// thing waldo exists to serve — so a tier of the same name made
		// `waldo agent uninstall` read like it removed Claude Code.
		return 0, fmt.Errorf("the agent tier is now called helper: use --fileops=helper. " +
			"It was renamed because \"agent\" already means the coding agent this tool drives")
	}
	return 0, fmt.Errorf("unknown fileops tier %q (want posix, pipe or helper)", s)
}

// ExitError reports a command that ran to completion with a non-zero status.
// It is distinct from a transport failure: the agent should see a non-zero
// exit as data to reason about, while a transport failure is an error.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("command exited %d: %s", e.Code, e.Stderr)
}

// NotFoundError reports a missing path.
type NotFoundError struct{ Path string }

func (e *NotFoundError) Error() string { return "no such file or directory: " + e.Path }
