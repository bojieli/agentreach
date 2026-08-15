// Package transport reaches a target and runs commands on it. A transport says
// nothing about how file operations are performed; that is the job of the
// fileops package, which layers strategies on top of a transport.
package transport

import (
	"context"
	"io"
	"strings"

	"github.com/bojieli/waldo/internal/waldo"
)

// Transport runs commands on a target.
type Transport interface {
	// Run executes a command to completion and returns its captured output.
	// A non-zero exit status is reported in ExecResult.Code, not as an error:
	// the agent must be able to reason about a failing command as data. An
	// error means the transport itself failed and the command's fate is
	// unknown.
	Run(ctx context.Context, req waldo.ExecRequest) (waldo.ExecResult, error)

	// Open starts a long-lived command with piped stdio. It backs the pipe and
	// agent file-operation tiers, which keep one process alive and speak a
	// framed protocol over it.
	Open(ctx context.Context, command string) (Stream, error)

	// Describe returns a short human-readable identity, e.g. "ssh://host".
	Describe() string

	// Close releases connections. It is safe to call more than once.
	Close() error
}

// SubsystemOpener is implemented by transports that can start an SSH
// subsystem, which is how the SFTP tier reaches stock OpenSSH's sftp-server
// without running a command or installing anything.
//
// It is deliberately an optional interface rather than a method on Transport.
// A container or a local shell has no subsystem concept, and giving them a
// method that can only return "unsupported" would invite callers to treat the
// absence as a runtime failure instead of what it is: a tier this target does
// not qualify for.
type SubsystemOpener interface {
	OpenSubsystem(ctx context.Context, name string) (Stream, error)
}

// Stream is a long-lived remote process.
type Stream struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	// Wait blocks until the process exits and returns its status.
	Wait func() (int, error)
	// Close terminates the process and releases its pipes.
	Close func() error
}

// ShellQuote renders s as a single POSIX shell word.
//
// Everything waldo sends to a target passes through here. The empty string
// must still produce a word, and a single quote inside the value must not be
// able to terminate the quoting: the standard '"'"' idiom is used instead of
// backslash escaping, which is not portable inside single quotes.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, "\\'\"`${}[]()|&;<>*?!~# \t\n") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// BuildCommand wraps a command with its working directory and environment.
//
// The `cd` is chained with && so a missing directory fails loudly rather than
// silently running the command somewhere unintended, which on an untrusted
// host could mean acting on the wrong tree entirely.
func BuildCommand(req waldo.ExecRequest) string {
	var b strings.Builder
	if req.Dir != "" {
		b.WriteString("cd ")
		b.WriteString(ShellQuote(req.Dir))
		b.WriteString(" && ")
	}
	if len(req.Env) > 0 {
		b.WriteString("env")
		for _, k := range sortedKeys(req.Env) {
			b.WriteString(" ")
			b.WriteString(ShellQuote(k + "=" + req.Env[k]))
		}
		b.WriteString(" ")
	}
	b.WriteString(req.Command)
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// insertion sort keeps this dependency-free and the maps are tiny
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
