// Command waldo-helper is the optional helper waldo installs on a target for
// tier 3.
//
// It is the only thing waldo ever writes to a target's disk, and only when the
// operator asks for it explicitly. It therefore does as little as possible: it
// reads framed requests on stdin, performs file operations, and writes framed
// responses on stdout. It opens no socket, forks nothing, reads no
// configuration, and holds no state that outlives the process.
//
// The protocol is byte-identical to the tier-2 Python handler's, so waldo's
// client speaks to either without knowing which it got. See
// internal/fileops/handler.py for the framing.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// version is stamped at build time. It is part of the installed path on the
// target, so an upgraded waldo installs a new agent instead of silently reusing
// a stale one.
var version = "dev"

const (
	maxFrame = 64 << 20
	chunk    = 1 << 20
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--selftest":
			// waldo runs this after uploading and compares both fields against
			// the file it just sent. A mismatch means the upload was truncated,
			// tampered with, or answered by a different binary already on the
			// target, and waldo reinstalls rather than trusting it.
			sum, err := selfDigest()
			if err != nil {
				fmt.Fprintln(os.Stderr, "waldo-helper: cannot hash self:", err)
				os.Exit(1)
			}
			fmt.Printf("waldo-helper %s %s %s/%s\n", version, sum, runtime.GOOS, runtime.GOARCH)
			return
		case "--version":
			fmt.Printf("waldo-helper %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
			return
		case "serve":
			// the default; accepted so the command line reads clearly
		default:
			fmt.Fprintf(os.Stderr, "waldo-helper: unknown argument %q\n", os.Args[1])
			os.Exit(2)
		}
	}
	if err := serve(os.Stdin, os.Stdout); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "waldo-helper:", err)
		os.Exit(1)
	}
}

func selfDigest() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type request struct {
	ID        uint32 `json:"id"`
	Op        string `json:"op"`
	Path      string `json:"path"`
	From      string `json:"from"`
	To        string `json:"to"`
	Offset    int64  `json:"offset"`
	Limit     int64  `json:"limit"`
	Mode      int    `json:"mode"`
	Recursive bool   `json:"recursive"`
	Version   string `json:"version"`
}

func serve(stdin io.Reader, stdout io.Writer) error {
	in := bufio.NewReaderSize(stdin, 1<<16)
	out := bufio.NewWriterSize(stdout, 1<<16)

	for {
		req, payload, err := readFrame(in)
		if err != nil {
			return err
		}
		hdr, body, opErr := dispatch(req, payload)
		if opErr != nil {
			// Every failure is a value the caller can reason about. An agent
			// that exits on a bad path would take the whole session's file
			// access with it.
			hdr = map[string]any{"kind": classify(opErr), "error": opErr.Error()}
			body = nil
		}
		if hdr == nil {
			hdr = map[string]any{}
		}
		hdr["id"] = req.ID
		hdr["ok"] = opErr == nil
		if err := writeFrame(out, hdr, body); err != nil {
			return err
		}
		if err := out.Flush(); err != nil {
			return err
		}
	}
}

func dispatch(req request, payload []byte) (map[string]any, []byte, error) {
	switch req.Op {
	case "ping":
		return map[string]any{"version": version, "helper": true, "os": runtime.GOOS, "arch": runtime.GOARCH}, nil, nil
	case "read":
		data, err := opRead(req)
		return nil, data, err
	case "write":
		n, err := opWrite(req, payload)
		return map[string]any{"bytes": n}, nil, err
	case "stat":
		info, err := opStat(req.Path)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"info": info}, nil, nil
	case "list":
		entries, err := opList(req.Path)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"entries": entries}, nil, nil
	case "mkdir":
		mode := req.Mode
		if mode == 0 {
			mode = 0o755
		}
		return nil, nil, os.MkdirAll(req.Path, os.FileMode(mode))
	case "remove":
		if req.Recursive {
			return nil, nil, os.RemoveAll(req.Path)
		}
		return nil, nil, os.Remove(req.Path)
	case "rename":
		return nil, nil, os.Rename(req.From, req.To)
	case "hash":
		sum, err := opHash(req.Path)
		if err != nil {
			return nil, nil, err
		}
		return map[string]any{"digest": sum}, nil, nil
	}
	return nil, nil, fmt.Errorf("unknown op %q", req.Op)
}

func opRead(req request) ([]byte, error) {
	f, err := os.Open(req.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if req.Offset > 0 {
		if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if req.Limit <= 0 {
		return io.ReadAll(io.LimitReader(f, maxFrame))
	}
	limit := req.Limit
	if limit > maxFrame {
		limit = maxFrame
	}
	buf := make([]byte, 0, limit)
	tmp := make([]byte, chunk)
	for int64(len(buf)) < limit {
		want := limit - int64(len(buf))
		if want > chunk {
			want = chunk
		}
		n, err := f.Read(tmp[:want])
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return buf, nil
}

// opWrite replaces a file's contents atomically: same-directory temporary plus
// rename, so a reader sees the old file or the new one and never a partial one.
func opWrite(req request, payload []byte) (int, error) {
	mode := req.Mode
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(req.Path)
	tmp := filepath.Join(dir, fmt.Sprintf(".waldo.tmp.%d.%d", os.Getpid(), req.ID))

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(mode))
	if err != nil {
		return 0, err
	}
	written, werr := f.Write(payload)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, os.FileMode(mode))
	}
	if werr == nil {
		werr = os.Rename(tmp, req.Path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return 0, werr
	}
	return written, nil
}

func opStat(path string) (map[string]any, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	info := infoOf(path, fi.Name(), fi)
	if fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(path); err == nil {
			info["link_target"] = target
		}
	}
	return info, nil
}

func opList(dir string) ([]map[string]any, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, infoOf(filepath.Join(dir, e.Name()), e.Name(), fi))
	}
	return out, nil
}

func infoOf(path, name string, fi os.FileInfo) map[string]any {
	return map[string]any{
		"name":    name,
		"path":    path,
		"size":    fi.Size(),
		"mode":    uint32(fi.Mode().Perm()),
		"mtime":   fi.ModTime().Unix(),
		"is_dir":  fi.IsDir(),
		"is_link": fi.Mode()&os.ModeSymlink != 0,
	}
}

func opHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func classify(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "notfound"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case errors.Is(err, os.ErrExist):
		return "exists"
	}
	return "error"
}

func readFrame(in *bufio.Reader) (request, []byte, error) {
	hdrLen, err := readUint32(in)
	if err != nil {
		return request{}, nil, err
	}
	if hdrLen > maxFrame {
		return request{}, nil, fmt.Errorf("header of %d bytes exceeds the frame limit", hdrLen)
	}
	hdr := make([]byte, hdrLen)
	if _, err := io.ReadFull(in, hdr); err != nil {
		return request{}, nil, err
	}
	var req request
	if err := json.Unmarshal(hdr, &req); err != nil {
		return request{}, nil, fmt.Errorf("parse request: %w", err)
	}
	payloadLen, err := readUint32(in)
	if err != nil {
		return request{}, nil, err
	}
	if payloadLen > maxFrame {
		return request{}, nil, fmt.Errorf("payload of %d bytes exceeds the frame limit", payloadLen)
	}
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(in, payload); err != nil {
			return request{}, nil, err
		}
	}
	return req, payload, nil
}

func readUint32(in *bufio.Reader) (uint32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(in, buf[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(buf[:]), nil
}

func writeFrame(out io.Writer, hdr map[string]any, payload []byte) error {
	body, err := json.Marshal(hdr)
	if err != nil {
		return err
	}
	var frame []byte
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(body)))
	frame = append(frame, body...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(len(payload)))
	if _, err := out.Write(frame); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := out.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
