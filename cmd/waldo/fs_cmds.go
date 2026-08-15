package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/session"
	"github.com/bojieli/waldo/internal/waldo"
)

const fsUsage = `waldo fs — file operations on the session's target

  read <path> [--offset N] [--limit N]   write file content to stdout
  write <path>                           read content from stdin
  ls <path> [--json]                     list a directory
  stat <path> [--json]                   describe a path
  grep <pattern> [root] [--json] [-i]    search content on the target
  glob <pattern> [root] [--json]         expand a pattern on the target
  rm <path> [--recursive]                remove a path
  mkdir <path>                           create a directory

Content is written to stdout as raw bytes, so binary files survive intact.
`

// openFileOps resolves the session and builds a file-operation strategy. The
// caller must close the returned FileOps: above tier 0 it owns a live channel
// to the target.
func openFileOps(ctx context.Context, sessionName string) (*session.Session, fileops.FileOps, error) {
	s, err := session.Load(sessionName)
	if err != nil {
		return nil, nil, err
	}
	t, err := s.Transport()
	if err != nil {
		return nil, nil, err
	}
	sel, err := s.FileOps(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	return s, sel.Ops, nil
}

// resolvePath makes a path absolute against the session's working directory,
// mirroring how a shell on the target would interpret it.
//
// The result is cleaned, so `waldo fs grep pattern .` searches "/srv/app"
// rather than "/srv/app/.", and the matches come back with ordinary paths. The
// difference is cosmetic to a person and not to an agent, which sees those
// paths in tool output and may hand them straight back in a later command.
//
// path.Clean is used rather than filepath.Clean: these are the target's paths,
// which are POSIX regardless of the operating system waldo itself runs on.
func resolvePath(s *session.Session, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	cwd := s.Cwd()
	if cwd == "" {
		cwd = s.Target.Workspace
	}
	return path.Join(cwd, p)
}

func cmdFS(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, fsUsage)
		return fmt.Errorf("expected a subcommand")
	}
	sub, rest := args[0], args[1:]

	fs := newFlagSet("fs " + sub)
	sessName := fs.String("session", "", "session name (default $WALDO_SESSION)")
	asJSON := fs.Bool("json", false, "emit JSON")
	offset := fs.Int64("offset", 0, "read: byte offset")
	limit := fs.Int64("limit", 0, "read: maximum bytes (0 = to end)")
	ignoreCase := fs.Bool("i", false, "grep: case-insensitive")
	literal := fs.Bool("literal", false, "grep: treat the pattern literally")
	globFilter := fs.String("glob", "", "grep: restrict to files matching this glob")
	maxResults := fs.Int("max", 1000, "grep: maximum matches")
	recursive := fs.Bool("recursive", false, "rm: remove directories recursively")

	pos, err := parseFlags(fs, rest)
	if err != nil {
		return err
	}
	if len(pos) == 0 && sub != "ls" {
		return fmt.Errorf("waldo fs %s: expected an argument\n\n%s", sub, fsUsage)
	}

	s, fo, err := openFileOps(ctx, sessionNameFromEnv(*sessName))
	if err != nil {
		return err
	}
	defer func() { _ = fo.Close() }()

	// Bound the operation, so an unresponsive target produces an error the
	// caller can act on rather than a command that never returns.
	ctx, cancel := s.OperationContext(ctx)
	defer cancel()

	arg := ""
	if len(pos) > 0 {
		arg = pos[0]
	}

	switch sub {
	case "read":
		data, err := fo.Read(ctx, resolvePath(s, arg), *offset, *limit)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(data)
		return err

	case "write":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return fo.Write(ctx, resolvePath(s, arg), data, 0o644)

	case "ls":
		dir := resolvePath(s, arg)
		if arg == "" {
			dir = s.Cwd()
		}
		entries, err := fo.List(ctx, dir)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(entries)
		}
		for _, e := range entries {
			kind := "-"
			if e.IsDir {
				kind = "d"
			} else if e.IsLink {
				kind = "l"
			}
			fmt.Printf("%s %10d  %s\n", kind, e.Size, e.Name)
		}
		return nil

	case "stat":
		fi, err := fo.Stat(ctx, resolvePath(s, arg))
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(fi)
		}
		fmt.Printf("path  %s\nsize  %d\nmode  %v\nmtime %s\ndir   %v\n",
			fi.Path, fi.Size, fi.Mode, fi.ModTime.Format("2006-01-02 15:04:05"), fi.IsDir)
		return nil

	case "grep":
		root := s.Cwd()
		if len(pos) > 1 {
			root = resolvePath(s, pos[1])
		}
		matches, err := fo.Search(ctx, waldo.SearchRequest{
			Pattern: arg, Root: root, Glob: *globFilter,
			IgnoreCase: *ignoreCase, Literal: *literal, MaxResults: *maxResults,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(matches)
		}
		for _, m := range matches {
			fmt.Printf("%s:%d:%s\n", m.Path, m.Line, m.Text)
		}
		return nil

	case "glob":
		root := s.Cwd()
		if len(pos) > 1 {
			root = resolvePath(s, pos[1])
		}
		paths, err := fo.Glob(ctx, root, arg)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(paths)
		}
		for _, p := range paths {
			fmt.Println(p)
		}
		return nil

	case "rm":
		return fo.Remove(ctx, resolvePath(s, arg), *recursive)

	case "mkdir":
		return fo.Mkdir(ctx, resolvePath(s, arg), 0o755)

	default:
		fmt.Fprint(os.Stderr, fsUsage)
		return fmt.Errorf("unknown fs subcommand %q", sub)
	}
}
