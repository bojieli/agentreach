// Package transport reaches a target and runs commands on it. A transport says
// nothing about how file operations are performed; that is the job of the
// fileops package, which layers strategies on top of a transport.
package transport

import (
	"context"
	"io"
	"strings"

	"github.com/bojieli/agentreach/internal/reach"
)

// Transport runs commands on a target.
type Transport interface {
	// Run executes a command to completion and returns its captured output.
	// A non-zero exit status is reported in ExecResult.Code, not as an error:
	// the agent must be able to reason about a failing command as data. An
	// error means the transport itself failed and the command's fate is
	// unknown.
	Run(ctx context.Context, req reach.ExecRequest) (reach.ExecResult, error)

	// Open starts a long-lived command with piped stdio. It backs the pipe and
	// helper file-operation tiers, which keep one process alive and speak a
	// framed protocol over it.
	Open(ctx context.Context, command string) (Stream, error)

	// Describe returns a short human-readable identity, e.g. "ssh://host".
	Describe() string

	// Close releases connections. It is safe to call more than once.
	Close() error
}

// Overflower is a transport that can move onto a fresh connection to the same
// target when the current one has no room for another channel.
//
// It is an optional interface rather than part of Transport because it only
// means anything where one connection carries many channels. A local or
// container transport has no such limit, and an ssh transport without
// multiplexing already gives every command its own connection.
type Overflower interface {
	Overflow() bool
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
// Everything reach sends to a target passes through here. The empty string
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
func BuildCommand(req reach.ExecRequest) string {
	var b strings.Builder
	if req.Dir != "" {
		b.WriteString("cd ")
		b.WriteString(ShellQuote(req.Dir))
		b.WriteString(" && ")
	}
	// `export K=V; cmd` rather than `env K=V cmd`.
	//
	// env takes a *command*, and reach's commands are frequently shell
	// constructs — the tier-0 write is `{ ...; } || { ...; }` — which env
	// cannot run: the shell fails with a syntax error at the brace. The export
	// form is plain POSIX and works with anything that follows it.
	//
	// This was latent rather than wrong: nothing set Env until the login-PATH
	// work, so the broken branch had never executed.
	for _, k := range sortedKeys(req.Env) {
		b.WriteString("export ")
		b.WriteString(ShellQuote(k + "=" + req.Env[k]))
		b.WriteString("; ")
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
