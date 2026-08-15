package fileops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// POSIX implements FileOps using only a POSIX shell on the target.
//
// This tier is the project's floor: it works on a host where nothing may be
// installed, nothing may be written to disk, and no subsystem beyond a login
// shell is available. Everything else is an optimisation over it.
//
// Content crosses the wire base64-encoded in both directions. That costs about
// 33% bandwidth and buys binary safety: file contents containing NUL bytes,
// invalid UTF-8 or CRLF survive intact, which they would not if they were
// interpolated through a shell.
type POSIX struct {
	t    transport.Transport
	caps *Capabilities
}

// NewPOSIX builds the tier-0 strategy.
func NewPOSIX(t transport.Transport, caps *Capabilities) *POSIX {
	return &POSIX{t: t, caps: caps}
}

// Tier implements FileOps.
func (p *POSIX) Tier() waldo.Tier { return waldo.TierPOSIX }

// Close implements FileOps. Tier 0 holds nothing open — that is the entire
// point of it — so there is nothing to release.
func (p *POSIX) Close() error { return nil }

func q(s string) string { return transport.ShellQuote(s) }

// run executes a shell snippet on the target and fails on a non-zero status.
func (p *POSIX) run(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
	res, err := p.t.Run(ctx, waldo.ExecRequest{Command: cmd, Stdin: stdin})
	if err != nil {
		return nil, err
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(string(res.Stderr))
		if strings.Contains(msg, "No such file") || strings.Contains(msg, "not found") {
			return nil, &waldo.NotFoundError{Path: msg}
		}
		return nil, &waldo.ExitError{Code: res.Code, Stderr: msg}
	}
	return res.Stdout, nil
}

// readChunk is the largest slice of file content fetched in one command.
//
// Content is base64-framed, so a chunk costs about 4/3 of this on the wire.
// The chunk size is chosen well below the transport's output cap: if an
// encoded payload were ever truncated, the truncation notice would be spliced
// into the middle of the base64 stream and the decoded file would be silently
// corrupt. Chunking makes that structurally impossible rather than unlikely.
const readChunk = 1 << 20 // 1 MiB

// Read implements FileOps.
//
// Ranges use `tail -c +N | head -c M` rather than `dd bs=1`, which would issue
// one syscall per byte and make a large offset pathologically slow.
//
// Reads larger than readChunk are split across several commands and rejoined
// locally. Because each chunk is offset-addressed, a connection that drops
// mid-file resumes at the next chunk instead of restarting the transfer.
func (p *POSIX) Read(ctx context.Context, filePath string, off, n int64) ([]byte, error) {
	if off < 0 {
		off = 0
	}
	// Resolve an open-ended read to a concrete length so it can be chunked.
	if n <= 0 {
		fi, err := p.Stat(ctx, filePath)
		if err != nil {
			return nil, err
		}
		n = fi.Size - off
		if n <= 0 {
			// Confirm readability so an absent file is never reported as an
			// empty one.
			if _, err := p.readRange(ctx, filePath, off, 1); err != nil {
				var nf *waldo.NotFoundError
				if asNotFound(err, &nf) {
					return nil, err
				}
			}
			return []byte{}, nil
		}
	}

	var out []byte
	for read := int64(0); read < n; {
		want := n - read
		if want > readChunk {
			want = readChunk
		}
		part, err := p.readRange(ctx, filePath, off+read, want)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
		if int64(len(part)) < want {
			break // reached end of file
		}
		read += int64(len(part))
	}
	return out, nil
}

// readRange fetches exactly one chunk.
func (p *POSIX) readRange(ctx context.Context, filePath string, off, n int64) ([]byte, error) {
	var body string
	if off <= 0 && n <= 0 {
		body = fmt.Sprintf("%s < %s", p.caps.Base64Encode, q(filePath))
	} else if n <= 0 {
		body = fmt.Sprintf("tail -c +%d -- %s | %s", off+1, q(filePath), p.caps.Base64Encode)
	} else {
		body = fmt.Sprintf("tail -c +%d -- %s | head -c %d | %s", off+1, q(filePath), n, p.caps.Base64Encode)
	}
	// Fail loudly if the path is unreadable rather than returning empty
	// content, which an agent would misread as "the file is empty".
	cmd := fmt.Sprintf("test -r %s || { echo 'waldo: not readable' >&2; exit 66; }; %s", q(filePath), body)

	// Size the cap to this chunk's encoded length plus slack, so an oversized
	// response is an error rather than silent corruption.
	res, err := p.t.Run(ctx, waldo.ExecRequest{
		Command:   cmd,
		MaxOutput: n*4/3 + (64 << 10),
	})
	if err != nil {
		return nil, err
	}
	if res.Code == 66 {
		return nil, &waldo.NotFoundError{Path: filePath}
	}
	if res.Code != 0 {
		return nil, &waldo.ExitError{Code: res.Code, Stderr: strings.TrimSpace(string(res.Stderr))}
	}
	if res.Truncated {
		return nil, fmt.Errorf("read %s: response exceeded output cap; refusing to return possibly corrupt content", filePath)
	}
	return decodeB64(res.Stdout)
}

func asNotFound(err error, target **waldo.NotFoundError) bool {
	for err != nil {
		if nf, ok := err.(*waldo.NotFoundError); ok {
			*target = nf
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Write implements FileOps.
//
// The write goes to a temporary file in the same directory and is moved into
// place, so a reader never observes a half-written file and a failed transfer
// leaves the original intact. Same-directory placement keeps the rename on one
// filesystem, where POSIX guarantees atomicity.
func (p *POSIX) Write(ctx context.Context, filePath string, data []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	dir := path.Dir(filePath)
	tmp := path.Join(dir, fmt.Sprintf(".waldo.tmp.%d", time.Now().UnixNano()))
	enc := base64.StdEncoding.EncodeToString(data)

	cmd := fmt.Sprintf(
		"set -e; %s > %s; chmod %o %s; mv -f %s %s",
		p.caps.Base64Decode, q(tmp), mode.Perm(), q(tmp), q(tmp), q(filePath))
	// Clean up the temporary file if anything fails, so an interrupted write
	// does not litter the target with debris.
	cmd = fmt.Sprintf("{ %s; } || { rm -f %s; exit 1; }", cmd, q(tmp))

	_, err := p.run(ctx, cmd, []byte(enc))
	return err
}

// Stat implements FileOps.
func (p *POSIX) Stat(ctx context.Context, filePath string) (*waldo.FileInfo, error) {
	var cmd string
	switch p.caps.StatFlavor {
	case "gnu":
		cmd = fmt.Sprintf("stat -c '%%s|%%f|%%Y' -- %s", q(filePath))
	case "bsd":
		cmd = fmt.Sprintf("stat -f '%%z|%%p|%%m' -- %s", q(filePath))
	default:
		return nil, fmt.Errorf("target has no usable stat command (uname: %s)", p.caps.Uname)
	}
	out, err := p.run(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	fi, err := parseStatLine(strings.TrimSpace(string(out)), p.caps.StatFlavor)
	if err != nil {
		return nil, err
	}
	fi.Path = filePath
	fi.Name = path.Base(filePath)
	return fi, nil
}

// List implements FileOps.
func (p *POSIX) List(ctx context.Context, dir string) ([]waldo.FileInfo, error) {
	if p.caps.FindPrintf {
		// GNU find with a NUL record terminator is the only fully correct
		// option: it is the sole variant that can represent a filename
		// containing a newline.
		cmd := fmt.Sprintf(
			"find %s -maxdepth 1 -mindepth 1 -printf '%%s\\t%%m\\t%%T@\\t%%y\\t%%p\\0'",
			q(dir))
		out, err := p.run(ctx, cmd, nil)
		if err != nil {
			return nil, err
		}
		return parseFindPrintf(string(out))
	}

	// Portable fallback. Filenames containing newlines cannot be represented
	// here; waldo reports that limitation in `waldo doctor` rather than
	// silently mangling such entries.
	var cmd string
	switch p.caps.StatFlavor {
	case "gnu":
		cmd = fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -exec stat -c '%%s|%%f|%%Y|%%n' {} +", q(dir))
	case "bsd":
		cmd = fmt.Sprintf("find %s -maxdepth 1 -mindepth 1 -exec stat -f '%%z|%%p|%%m|%%N' {} +", q(dir))
	default:
		return nil, fmt.Errorf("target has no usable stat command (uname: %s)", p.caps.Uname)
	}
	out, err := p.run(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	var infos []waldo.FileInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		fi, err := parseStatLine(strings.Join(parts[:3], "|"), p.caps.StatFlavor)
		if err != nil {
			continue
		}
		fi.Path = parts[3]
		fi.Name = path.Base(parts[3])
		infos = append(infos, *fi)
	}
	return infos, nil
}

// Mkdir implements FileOps.
func (p *POSIX) Mkdir(ctx context.Context, dir string, mode fs.FileMode) error {
	if mode == 0 {
		mode = 0o755
	}
	// `--` goes before the mode, not after it. BSD chmod takes the mode as a
	// positional argument, so its option parsing stops there and a later `--`
	// is read as a filename — `chmod 700 -- /srv/app` fails on macOS with "no
	// such file or directory" while working fine under GNU coreutils. Putting
	// the delimiter first is accepted by both.
	_, err := p.run(ctx, fmt.Sprintf("mkdir -p -- %s && chmod -- %o %s", q(dir), mode.Perm(), q(dir)), nil)
	return err
}

// Remove implements FileOps.
func (p *POSIX) Remove(ctx context.Context, filePath string, recursive bool) error {
	flag := "-f"
	if recursive {
		flag = "-rf"
	}
	_, err := p.run(ctx, fmt.Sprintf("rm %s -- %s", flag, q(filePath)), nil)
	return err
}

// Rename implements FileOps.
func (p *POSIX) Rename(ctx context.Context, from, to string) error {
	_, err := p.run(ctx, fmt.Sprintf("mv -f -- %s %s", q(from), q(to)), nil)
	return err
}

// Hash implements FileOps. Content is fed on stdin so the digest command's
// output never contains the filename, which keeps parsing trivial and immune
// to unusual names.
func (p *POSIX) Hash(ctx context.Context, filePath string) (string, error) {
	if p.caps.SHA256 == "" {
		return "", fmt.Errorf("target has no sha256 utility (uname: %s)", p.caps.Uname)
	}
	out, err := p.run(ctx, fmt.Sprintf("%s < %s", p.caps.SHA256, q(filePath)), nil)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", fmt.Errorf("empty digest for %s", filePath)
	}
	return strings.TrimPrefix(f[0], "\\"), nil
}

// Search implements FileOps, running the search on the target.
func (p *POSIX) Search(ctx context.Context, req waldo.SearchRequest) ([]waldo.Match, error) {
	if req.Root == "" {
		req.Root = "."
	}
	max := req.MaxResults
	if max <= 0 {
		max = 1000
	}
	if p.caps.Ripgrep != "" {
		return p.searchRipgrep(ctx, req, max)
	}
	return p.searchGrep(ctx, req, max)
}

func (p *POSIX) searchRipgrep(ctx context.Context, req waldo.SearchRequest, max int) ([]waldo.Match, error) {
	args := []string{p.caps.Ripgrep, "--json"}
	if req.IgnoreCase {
		args = append(args, "-i")
	}
	if req.Literal {
		args = append(args, "-F")
	}
	if req.Glob != "" {
		args = append(args, "-g", q(req.Glob))
	}
	args = append(args, "-m", strconv.Itoa(max), "-e", q(req.Pattern), q(req.Root))
	// rg exits 1 when there are no matches; that is a legitimate empty result,
	// not a failure, so it is normalised here rather than surfaced as an error.
	cmd := strings.Join(args, " ") + " || true"

	out, err := p.run(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	var matches []waldo.Match
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Path       struct{ Text string } `json:"path"`
				Lines      struct{ Text string } `json:"lines"`
				LineNumber int                   `json:"line_number"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Type != "match" {
			continue
		}
		matches = append(matches, waldo.Match{
			Path: ev.Data.Path.Text,
			Line: ev.Data.LineNumber,
			Text: strings.TrimRight(ev.Data.Lines.Text, "\r\n"),
		})
		if len(matches) >= max {
			break
		}
	}
	return matches, nil
}

func (p *POSIX) searchGrep(ctx context.Context, req waldo.SearchRequest, max int) ([]waldo.Match, error) {
	flags := "-rn --binary-files=without-match"
	if req.IgnoreCase {
		flags += " -i"
	}
	if req.Literal {
		flags += " -F"
	}
	cmd := fmt.Sprintf("grep %s -e %s -- %s 2>/dev/null | head -n %d || true",
		flags, q(req.Pattern), q(req.Root), max)
	out, err := p.run(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	var matches []waldo.Match
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		// Format is path:line:text. A path containing a colon is ambiguous
		// here; ripgrep's JSON output avoids this entirely, which is why it is
		// preferred whenever present.
		a := strings.SplitN(line, ":", 3)
		if len(a) < 3 {
			continue
		}
		n, err := strconv.Atoi(a[1])
		if err != nil {
			continue
		}
		matches = append(matches, waldo.Match{Path: a[0], Line: n, Text: a[2]})
	}
	return matches, nil
}

// Glob implements FileOps.
func (p *POSIX) Glob(ctx context.Context, root, pattern string) ([]string, error) {
	if root == "" {
		root = "."
	}
	if !p.caps.HasFind {
		return nil, fmt.Errorf("target has no find command; glob is unavailable (uname: %s)", p.caps.Uname)
	}
	// `**` is a shell/zsh extension find does not know; collapse it to `*`
	// and rely on -path matching across separators.
	pat := strings.ReplaceAll(pattern, "**", "*")
	var cmd string
	if strings.Contains(pat, "/") {
		cmd = fmt.Sprintf("find %s -path %s -type f 2>/dev/null || true", q(root), q(path.Join(root, pat)))
	} else {
		cmd = fmt.Sprintf("find %s -name %s -type f 2>/dev/null || true", q(root), q(pat))
	}
	out, err := p.run(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

func decodeB64(out []byte) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, string(out))
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("decode target response: %w", err)
	}
	return data, nil
}

// parseStatLine turns one "size|mode|mtime" record into a FileInfo. GNU
// reports the mode in hex, BSD in octal; both are raw st_mode, so the file
// type is recovered from the S_IFMT bits.
func parseStatLine(line, flavor string) (*waldo.FileInfo, error) {
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) < 3 {
		return nil, fmt.Errorf("unparsable stat output %q", line)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("unparsable stat size %q", parts[0])
	}
	base := 16
	if flavor == "bsd" {
		base = 8
	}
	raw, err := strconv.ParseUint(strings.TrimSpace(parts[1]), base, 32)
	if err != nil {
		return nil, fmt.Errorf("unparsable stat mode %q", parts[1])
	}
	mtime, err := strconv.ParseInt(strings.TrimSpace(strings.SplitN(parts[2], ".", 2)[0]), 10, 64)
	if err != nil {
		mtime = 0
	}
	return &waldo.FileInfo{
		Size:    size,
		Mode:    fs.FileMode(raw & 0o7777),
		ModTime: time.Unix(mtime, 0),
		IsDir:   raw&0o170000 == 0o040000,
		IsLink:  raw&0o170000 == 0o120000,
	}, nil
}

// parseFindPrintf reads NUL-terminated records of "size\tmode\tmtime\ttype\tpath".
func parseFindPrintf(out string) ([]waldo.FileInfo, error) {
	var infos []waldo.FileInfo
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\t", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[0], 10, 64)
		mode, _ := strconv.ParseUint(f[1], 8, 32)
		mtime, _ := strconv.ParseFloat(f[2], 64)
		infos = append(infos, waldo.FileInfo{
			Name:    path.Base(f[4]),
			Path:    f[4],
			Size:    size,
			Mode:    fs.FileMode(mode),
			ModTime: time.Unix(int64(mtime), 0),
			IsDir:   f[3] == "d",
			IsLink:  f[3] == "l",
		})
	}
	return infos, nil
}

func asExit(err error, target **waldo.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*waldo.ExitError); ok {
			*target = ee
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var _ FileOps = (*POSIX)(nil)
