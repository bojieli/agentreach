// Package fileops implements file access strategies layered over a transport.
//
// Strategies are tiered by what the target supports. Tier 0 (posix) needs
// nothing but a shell and writes nothing to the target; higher tiers are
// optimisations that are never required. See docs/TRANSPORTS.md.
package fileops

import (
	"context"
	"io/fs"

	"github.com/bojieli/waldo/internal/waldo"
)

// FileOps performs file operations on a target.
//
// Search and Glob are first-class operations rather than helpers derived from
// List. Running them on the target and returning only matches is the single
// biggest reason waldo is not a filesystem mount: a mount would drag every
// candidate file across the network to answer a question the target could have
// answered locally.
type FileOps interface {
	// Read returns up to n bytes from path starting at off. n <= 0 means the
	// whole file from off. Offset addressing lets an interrupted transfer
	// resume rather than restart.
	Read(ctx context.Context, path string, off, n int64) ([]byte, error)

	// Write atomically replaces path's contents.
	Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error

	// Stat describes a single path.
	Stat(ctx context.Context, path string) (*waldo.FileInfo, error)

	// List returns the immediate children of a directory.
	List(ctx context.Context, path string) ([]waldo.FileInfo, error)

	// Mkdir creates a directory and any missing parents.
	Mkdir(ctx context.Context, path string, mode fs.FileMode) error

	// Remove deletes a path, recursively when asked.
	Remove(ctx context.Context, path string, recursive bool) error

	// Rename moves a path.
	Rename(ctx context.Context, from, to string) error

	// Search runs a content search on the target and returns only matches.
	Search(ctx context.Context, req waldo.SearchRequest) ([]waldo.Match, error)

	// Glob expands a shell-style pattern under root on the target.
	Glob(ctx context.Context, root, pattern string) ([]string, error)

	// Hash returns a content digest, used by the mirror engine to decide what
	// actually changed rather than trusting timestamps.
	Hash(ctx context.Context, path string) (string, error)

	// Tier reports which strategy this is, for diagnostics.
	Tier() waldo.Tier
}
