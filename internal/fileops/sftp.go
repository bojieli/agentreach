package fileops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/waldo/internal/sftp"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// handshakeTimeout bounds the subsystem handshake.
//
// A host with the SFTP subsystem disabled usually refuses the channel and the
// stream closes immediately, but a host that accepts the channel and then never
// answers would otherwise hang `waldo up` forever. Tier negotiation must always
// terminate: an unreachable tier is a value waldo reports, not a wait.
const handshakeTimeout = 20 * time.Second

// SFTP implements FileOps over the SFTP subsystem that stock OpenSSH ships.
//
// Content operations go over the subsystem, where they cost no base64 expansion
// and can be pipelined. Search and glob do not: there is no SFTP operation for
// "find me the files containing this", and answering it client-side would mean
// dragging every candidate file across the network — the exact behaviour that
// makes a filesystem mount the wrong tool for this job. Those stay shell
// commands executed on the target, delegated to the tier-0 strategy.
//
// Nothing is written to the target. SFTP is used as a protocol, never mounted.
type SFTP struct {
	client *sftp.Client
	stream transport.Stream
	base   *POSIX

	closeOnce sync.Once
}

// NewSFTP opens the SFTP subsystem and completes the handshake.
func NewSFTP(ctx context.Context, t transport.Transport, base *POSIX) (FileOps, error) {
	opener, ok := t.(transport.SubsystemOpener)
	if !ok {
		return nil, fmt.Errorf("%s is not an SSH transport, so it has no SFTP subsystem", t.Describe())
	}

	// The subsystem outlives this call, so it must not be tied to a context
	// that is cancelled when the handshake finishes.
	stream, err := opener.OpenSubsystem(context.WithoutCancel(ctx), "sftp")
	if err != nil {
		return nil, fmt.Errorf("open sftp subsystem: %w", err)
	}

	// sshd writes subsystem errors to stderr. Draining it keeps the pipe from
	// filling and blocking the connection, and keeps the message for the error
	// waldo reports when the handshake fails.
	var errBuf strings.Builder
	go func() { _, _ = io.Copy(&limitedWriter{w: &errBuf, remaining: 4 << 10}, stream.Stderr) }()

	type result struct {
		c   *sftp.Client
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := sftp.New(stream.Stdout, stream.Stdin)
		ch <- result{c, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			_ = stream.Close()
			msg := strings.TrimSpace(errBuf.String())
			if msg != "" {
				return nil, fmt.Errorf("sftp subsystem unavailable: %w (%s)", r.err, msg)
			}
			return nil, fmt.Errorf("sftp subsystem unavailable: %w", r.err)
		}
		return &SFTP{client: r.c, stream: stream, base: base}, nil
	case <-time.After(handshakeTimeout):
		_ = stream.Close()
		return nil, fmt.Errorf("sftp subsystem did not answer within %s", handshakeTimeout)
	}
}

// Tier implements FileOps.
func (s *SFTP) Tier() waldo.Tier { return waldo.TierSFTP }

// Close implements FileOps.
func (s *SFTP) Close() error {
	s.closeOnce.Do(func() {
		_ = s.client.Close()
		_ = s.stream.Close()
	})
	return nil
}

// Read implements FileOps.
func (s *SFTP) Read(ctx context.Context, filePath string, off, n int64) ([]byte, error) {
	data, err := s.client.ReadFile(ctx, filePath, off, n)
	return data, s.translate(err, filePath)
}

// Write implements FileOps.
func (s *SFTP) Write(ctx context.Context, filePath string, data []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	return s.translate(s.client.WriteFile(ctx, filePath, data, uint32(mode.Perm())), filePath)
}

// Stat implements FileOps.
func (s *SFTP) Stat(ctx context.Context, filePath string) (*waldo.FileInfo, error) {
	a, err := s.client.Lstat(ctx, filePath)
	if err != nil {
		return nil, s.translate(err, filePath)
	}
	fi := infoFromAttrs(path.Base(filePath), filePath, a)
	if fi.IsLink {
		if target, err := s.client.Readlink(ctx, filePath); err == nil {
			fi.LinkTarget = target
		}
	}
	return &fi, nil
}

// List implements FileOps.
func (s *SFTP) List(ctx context.Context, dir string) ([]waldo.FileInfo, error) {
	entries, err := s.client.ReadDir(ctx, dir)
	if err != nil {
		return nil, s.translate(err, dir)
	}
	out := make([]waldo.FileInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, infoFromAttrs(e.Name, path.Join(dir, e.Name), e.Attrs))
	}
	return out, nil
}

// Mkdir implements FileOps, creating missing parents.
//
// SFTP has no mkdir -p, so the ancestors are walked explicitly. An "it already
// exists" failure on an ancestor is success, not an error: two sessions
// creating the same tree concurrently is ordinary, and treating the race as a
// failure would make it look like a permissions problem.
func (s *SFTP) Mkdir(ctx context.Context, dir string, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	clean := path.Clean(dir)
	if clean == "/" || clean == "." {
		return nil
	}
	var built string
	for _, part := range strings.Split(strings.TrimPrefix(clean, "/"), "/") {
		if part == "" {
			continue
		}
		built += "/" + part
		if fi, err := s.client.Stat(ctx, built); err == nil {
			if fi.HasPermissions() && fi.Permissions&0o170000 != 0o040000 {
				return fmt.Errorf("cannot create %s: %s exists and is not a directory", dir, built)
			}
			continue
		}
		if err := s.client.Mkdir(ctx, built, uint32(mode.Perm())); err != nil {
			if _, statErr := s.client.Stat(ctx, built); statErr == nil {
				continue // created concurrently
			}
			return s.translate(err, built)
		}
	}
	return nil
}

// Remove implements FileOps.
//
// Recursive removal is delegated to the shell. SFTP would need a full
// client-side tree walk with one round trip per entry, while the target can do
// it in a single command — and the exec channel is always available, because a
// transport that could not run a command could not have been probed at all.
func (s *SFTP) Remove(ctx context.Context, filePath string, recursive bool) error {
	if recursive {
		return s.base.Remove(ctx, filePath, true)
	}
	a, err := s.client.Lstat(ctx, filePath)
	if err != nil {
		return s.translate(err, filePath)
	}
	if a.HasPermissions() && a.Permissions&0o170000 == 0o040000 {
		return s.translate(s.client.Rmdir(ctx, filePath), filePath)
	}
	return s.translate(s.client.Remove(ctx, filePath), filePath)
}

// Rename implements FileOps.
func (s *SFTP) Rename(ctx context.Context, from, to string) error {
	return s.translate(s.client.Rename(ctx, from, to), from)
}

// Search implements FileOps by running the search on the target. See the type
// comment for why this is not an SFTP operation.
func (s *SFTP) Search(ctx context.Context, req waldo.SearchRequest) ([]waldo.Match, error) {
	return s.base.Search(ctx, req)
}

// Glob implements FileOps on the target, for the same reason as Search.
func (s *SFTP) Glob(ctx context.Context, root, pattern string) ([]string, error) {
	return s.base.Glob(ctx, root, pattern)
}

// Hash implements FileOps.
//
// SFTP has no digest operation in the version stock OpenSSH speaks, and hashing
// client-side would mean transferring the whole file to learn whether it
// changed — which is exactly the transfer the digest exists to avoid. The
// target computes it.
func (s *SFTP) Hash(ctx context.Context, filePath string) (string, error) {
	return s.base.Hash(ctx, filePath)
}

// translate converts protocol errors into waldo's vocabulary, so callers can
// distinguish "the file is not there" from "the connection broke" without
// knowing anything about SFTP.
func (s *SFTP) translate(err error, filePath string) error {
	if err == nil {
		return nil
	}
	var se *sftp.StatusError
	if errors.As(err, &se) {
		if se.IsNotFound() {
			return &waldo.NotFoundError{Path: filePath}
		}
	}
	return err
}

func infoFromAttrs(name, full string, a sftp.Attrs) waldo.FileInfo {
	fi := waldo.FileInfo{
		Name: name,
		Path: full,
		Size: sftp.ClampSize(a.Size),
	}
	if a.HasPermissions() {
		fi.Mode = fs.FileMode(a.Permissions & 0o7777)
		fi.IsDir = a.Permissions&0o170000 == 0o040000
		fi.IsLink = a.Permissions&0o170000 == 0o120000
	}
	if a.MTime != 0 {
		fi.ModTime = time.Unix(int64(a.MTime), 0)
	}
	return fi
}

// limitedWriter keeps at most a bounded prefix of what is written to it, so a
// chatty or hostile server cannot grow waldo's memory through a diagnostic
// buffer.
type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.remaining <= 0 {
		return len(p), nil
	}
	if len(p) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.w.Write(p)
	l.remaining -= n
	return len(p), err
}

var _ FileOps = (*SFTP)(nil)
