package execserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/bojieli/agentreach/internal/audit"
	"github.com/bojieli/agentreach/internal/transport"
	"github.com/bojieli/agentreach/internal/reach"
)

// Every fs/* method maps the PathUri codex sent onto a target path and runs
// the corresponding file-operation tier method there. Nothing in this file
// touches the local filesystem; a fileops failure becomes a JSON-RPC error
// value, never a panic.

type fsPathParams struct {
	Path string `json:"path"`
}

func (s *Server) targetPath(raw json.RawMessage, method string) (string, *rpcError) {
	var p fsPathParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", invalidParams("%s: %v", method, err)
	}
	return s.mapURI(p.Path)
}

// fsError renders a fileops failure as a JSON-RPC internal error. Codex's own
// server maps target I/O failures the same way; the message carries the
// target's own explanation, which is what the agent reasons about.
func fsError(err error) *rpcError {
	return internalError("%v", err)
}

func (s *Server) handleFsReadFile(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	target, rerr := s.targetPath(raw, "fs/readFile")
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	data, err := s.ops.Read(opCtx, target, 0, 0)
	entry := audit.Entry{Action: "read", Path: target, Bytes: len(data)}
	if err != nil {
		entry.Error = err.Error()
		s.record(entry)
		return nil, fsError(err)
	}
	s.record(entry)
	return map[string]any{"dataBase64": base64.StdEncoding.EncodeToString(data)}, nil
}

type fsWriteFileParams struct {
	Path       string `json:"path"`
	DataBase64 string `json:"dataBase64"`
}

func (s *Server) handleFsWriteFile(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsWriteFileParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/writeFile: %v", err)
	}
	target, rerr := s.mapURI(p.Path)
	if rerr != nil {
		return nil, rerr
	}
	data, err := base64.StdEncoding.DecodeString(p.DataBase64)
	if err != nil {
		return nil, invalidParams("fs/writeFile: dataBase64 is not valid base64: %v", err)
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	err = s.ops.Write(opCtx, target, data, 0o644)
	entry := audit.Entry{Action: "write", Path: target, Bytes: len(data)}
	if err != nil {
		entry.Error = err.Error()
		s.record(entry)
		return nil, fsError(err)
	}
	s.record(entry)
	return map[string]any{}, nil
}

type fsCreateDirectoryParams struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
}

func (s *Server) handleFsCreateDirectory(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsCreateDirectoryParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/createDirectory: %v", err)
	}
	target, rerr := s.mapURI(p.Path)
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	// The tier's Mkdir already creates missing parents, which satisfies both
	// recursive values; codex's non-recursive call only promises the parent
	// exists, not that mkdir -p must fail when it does.
	err := s.ops.Mkdir(opCtx, target, 0o755)
	entry := audit.Entry{Action: "mkdir", Path: target}
	if err != nil {
		entry.Error = err.Error()
		s.record(entry)
		return nil, fsError(err)
	}
	s.record(entry)
	return map[string]any{}, nil
}

func (s *Server) handleFsGetMetadata(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	target, rerr := s.targetPath(raw, "fs/getMetadata")
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	info, err := s.ops.Stat(opCtx, target)
	if err != nil {
		return nil, fsError(err)
	}
	isFile := !info.IsDir && !info.IsLink
	if info.IsLink {
		// A symlink's target type is what codex means by isFile/isDirectory;
		// the tiers resolve cheap links when they can.
		if info.LinkTarget != "" {
			resolved := info.LinkTarget
			if !strings.HasPrefix(resolved, "/") {
				resolved = path2Dir(target) + "/" + resolved
			}
			if li, lerr := s.ops.Stat(opCtx, resolved); lerr == nil {
				isFile = !li.IsDir
			}
		}
	}
	return map[string]any{
		"isDirectory":  info.IsDir,
		"isFile":       isFile,
		"isSymlink":    info.IsLink,
		"size":         uint64(max(info.Size, 0)),
		"createdAtMs":  0, // the tiers do not carry birth time; 0 is the honest value
		"modifiedAtMs": info.ModTime.UnixMilli(),
	}, nil
}

func path2Dir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

func (s *Server) handleFsCanonicalize(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	target, rerr := s.targetPath(raw, "fs/canonicalize")
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	res, err := s.t.Run(opCtx, reach.ExecRequest{
		Command:   "readlink -f " + transport.ShellQuote(target),
		MaxOutput: 64 << 10,
	})
	if err != nil {
		return nil, internalError("canonicalize %s: %v", target, err)
	}
	if res.Code != 0 {
		return nil, internalError("canonicalize %s: %s", target, strings.TrimSpace(string(res.Stderr)))
	}
	canon := strings.TrimRight(string(res.Stdout), "\n")
	if canon == "" {
		return nil, internalError("canonicalize %s: the target answered with nothing", target)
	}
	return map[string]any{"path": pathToURI(canon)}, nil
}

func (s *Server) handleFsReadDirectory(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	target, rerr := s.targetPath(raw, "fs/readDirectory")
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	infos, err := s.ops.List(opCtx, target)
	if err != nil {
		return nil, fsError(err)
	}
	entries := make([]map[string]any, 0, len(infos))
	for _, fi := range infos {
		entries = append(entries, map[string]any{
			"fileName":    fi.Name,
			"isDirectory": fi.IsDir,
			"isFile":      !fi.IsDir,
		})
	}
	return map[string]any{"entries": entries}, nil
}

// --- fs/walk ---

type walkOptions struct {
	MaxDepth                int  `json:"maxDepth"`
	MaxDirectories          int  `json:"maxDirectories"`
	MaxEntries              int  `json:"maxEntries"`
	FollowDirectorySymlinks bool `json:"followDirectorySymlinks"`
	PruneHiddenDirectories  bool `json:"pruneHiddenDirectories"`
}

type fsWalkParams struct {
	Path    string      `json:"path"`
	Options walkOptions `json:"options"`
}

func (s *Server) handleFsWalk(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsWalkParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/walk: %v", err)
	}
	root, rerr := s.mapURI(p.Path)
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()

	w := &walker{
		s:         s,
		ctx:       opCtx,
		opts:      p.Options,
		entries:   []map[string]any{},
		errs:      []map[string]any{},
		dirCount:  1, // the root itself counts, as in codex's walker
	}
	w.walk(root, 0)
	return map[string]any{
		"entries":   w.entries,
		"errors":    w.errs,
		"truncated": w.truncated,
	}, nil
}

type walker struct {
	s         *Server
	ctx       context.Context
	opts      walkOptions
	entries   []map[string]any
	errs      []map[string]any
	dirCount  int
	examined  int
	truncated bool
}

func (w *walker) walk(dir string, depth int) {
	if w.truncated || w.opts.MaxDepth > 0 && depth >= w.opts.MaxDepth {
		return
	}
	infos, err := w.s.ops.List(w.ctx, dir)
	if err != nil {
		w.errs = append(w.errs, map[string]any{"path": pathToURI(dir), "message": err.Error()})
		return
	}
	for _, fi := range infos {
		if w.opts.MaxEntries > 0 && w.examined >= w.opts.MaxEntries {
			w.truncated = true
			return
		}
		w.examined++
		full := dir + "/" + fi.Name
		isDir := fi.IsDir
		if fi.IsLink && w.opts.FollowDirectorySymlinks {
			// Only follow directory symlinks when asked; a target-side symlink
			// loop is the target's problem then, as it would be for find -L.
			if li, lerr := w.s.ops.Stat(w.ctx, full); lerr == nil && li.IsDir {
				isDir = true
			}
		}
		kind := "file"
		if isDir {
			kind = "directory"
		}
		w.entries = append(w.entries, map[string]any{"path": pathToURI(full), "kind": kind})
		if !isDir {
			continue
		}
		if w.opts.PruneHiddenDirectories && strings.HasPrefix(fi.Name, ".") {
			continue
		}
		if fi.IsLink && !w.opts.FollowDirectorySymlinks {
			continue
		}
		if w.opts.MaxDirectories > 0 && w.dirCount >= w.opts.MaxDirectories {
			w.truncated = true
			return
		}
		w.dirCount++
		w.walk(full, depth+1)
		if w.truncated {
			return
		}
	}
}

// --- fs/remove, fs/copy ---

type fsRemoveParams struct {
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
}

func (s *Server) handleFsRemove(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsRemoveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/remove: %v", err)
	}
	target, rerr := s.mapURI(p.Path)
	if rerr != nil {
		return nil, rerr
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	recursive := p.Recursive != nil && *p.Recursive
	err := s.ops.Remove(opCtx, target, recursive)
	entry := audit.Entry{Action: "remove", Path: target}
	if err != nil {
		entry.Error = err.Error()
		s.record(entry)
		return nil, fsError(err)
	}
	s.record(entry)
	return map[string]any{}, nil
}

type fsCopyParams struct {
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
	Recursive       bool   `json:"recursive"`
}

func (s *Server) handleFsCopy(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsCopyParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/copy: %v", err)
	}
	src, rerr := s.mapURI(p.SourcePath)
	if rerr != nil {
		return nil, rerr
	}
	dst, rerr := s.mapURI(p.DestinationPath)
	if rerr != nil {
		return nil, rerr
	}
	// The tiers have no copy, and reading the content across the wire to write
	// it back is exactly the inefficiency reach exists to avoid: one cp on the
	// target does it without the bytes ever leaving the machine.
	cmd := "cp"
	if p.Recursive {
		cmd += " -R"
	}
	cmd += " " + transport.ShellQuote(src) + " " + transport.ShellQuote(dst)
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	res, err := s.t.Run(opCtx, reach.ExecRequest{Command: cmd, MaxOutput: 16 << 10})
	entry := audit.Entry{Action: "write", Path: dst}
	switch {
	case err != nil:
		entry.Error = err.Error()
		s.record(entry)
		return nil, internalError("copy %s to %s: %v", src, dst, err)
	case res.Code != 0:
		entry.Error = strings.TrimSpace(string(res.Stderr))
		s.record(entry)
		return nil, internalError("copy %s to %s: %s", src, dst, strings.TrimSpace(string(res.Stderr)))
	}
	s.record(entry)
	return map[string]any{}, nil
}

// --- fs/open, fs/readBlock, fs/close ---
//
// Handles are stateless: a handle id is the mapped target path, and each
// readBlock is an offset read through the tier. Codex uses these for streamed
// file reads; nothing about them needs an open file on the target.

type fsOpenParams struct {
	HandleID string `json:"handleId"`
	Path     string `json:"path"`
}

func (s *Server) handleFsOpen(raw json.RawMessage) (any, *rpcError) {
	var p fsOpenParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/open: %v", err)
	}
	if p.HandleID == "" {
		return nil, invalidParams("fs/open: handleId must not be empty")
	}
	target, rerr := s.mapURI(p.Path)
	if rerr != nil {
		return nil, rerr
	}
	s.mu.Lock()
	s.handles[p.HandleID] = target
	s.mu.Unlock()
	return map[string]any{"handleId": p.HandleID}, nil
}

type fsReadBlockParams struct {
	HandleID string `json:"handleId"`
	Offset   uint64 `json:"offset"`
	Len      int64  `json:"len"`
}

func (s *Server) handleFsReadBlock(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var p fsReadBlockParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/readBlock: %v", err)
	}
	s.mu.Lock()
	target, ok := s.handles[p.HandleID]
	s.mu.Unlock()
	if !ok {
		return nil, invalidRequest("fs/readBlock: unknown handle %q", p.HandleID)
	}
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	data, err := s.ops.Read(opCtx, target, int64(p.Offset), p.Len)
	if err != nil {
		return nil, fsError(err)
	}
	return map[string]any{
		"chunk": base64.StdEncoding.EncodeToString(data),
		"eof":   int64(len(data)) < p.Len,
	}, nil
}

type fsCloseParams struct {
	HandleID string `json:"handleId"`
}

func (s *Server) handleFsClose(raw json.RawMessage) (any, *rpcError) {
	var p fsCloseParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("fs/close: %v", err)
	}
	s.mu.Lock()
	delete(s.handles, p.HandleID)
	s.mu.Unlock()
	return map[string]any{}, nil
}

// --- capabilityRoots/discoverV1 ---

type capabilityRootsParams struct {
	Roots []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"roots"`
}

// handleCapabilityDiscover answers codex's plugin/skill discovery with empty
// discoveries in the valid shape: reach does not scan the target for plugin
// manifests, and an empty result is the honest answer from a seam whose job is
// running the agent's commands, not managing its extensions.
func (s *Server) handleCapabilityDiscover(raw json.RawMessage) (any, *rpcError) {
	var p capabilityRootsParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, invalidParams("capabilityRoots/discoverV1: %v", err)
	}
	roots := make([]map[string]any, 0, len(p.Roots))
	for _, r := range p.Roots {
		roots = append(roots, map[string]any{
			"id":                 r.ID,
			"path":               r.Path,
			"plugin":             nil,
			"skills":             []any{},
			"namespaceManifests": []any{},
			"warnings":           []any{},
			"error":              nil,
		})
	}
	return map[string]any{"roots": roots}, nil
}
