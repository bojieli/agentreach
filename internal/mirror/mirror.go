// Package mirror materialises individual remote files as real local files, on
// demand, so a harness's native file tools operate on remote content.
//
// This is deliberately not a filesystem sync engine. Nothing is mirrored until
// a tool actually asks for it, there is no background reconciliation, and no
// attempt is made to track deletions or resolve divergence. A sync engine would
// have to answer questions reach has no good answer to — what to do when both
// sides changed, whether a missing file was deleted or never fetched — and
// getting those wrong loses the operator's work. Fetching exactly the file a
// tool is about to touch, at the moment it touches it, has none of those
// questions.
package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/reach"
)

// Mirror maps target paths to local paths and moves content between them.
type Mirror struct {
	root string
	fo   fileops.FileOps
}

// New builds a mirror rooted at root.
func New(root string, fo fileops.FileOps) *Mirror { return &Mirror{root: root, fo: fo} }

// Root returns the local mirror root.
func (m *Mirror) Root() string { return m.root }

// Local returns the local path standing in for a target path.
//
// The target's absolute path is reproduced beneath the mirror root, so the
// mapping is total, reversible, and obvious when inspected by hand.
//
// The path is cleaned before joining. Without that, a target path containing
// ".." would escape the mirror root once filepath.Join normalised it, and
// reach would read or write an arbitrary local file on behalf of whatever
// supplied the path. Since file paths can originate in content read from an
// untrusted target, that is a real attack path and not a theoretical one.
func (m *Mirror) Local(targetPath string) string {
	clean := path.Clean("/" + strings.TrimPrefix(path.Clean(targetPath), "/"))
	return filepath.Join(m.root, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
}

// checkContained is a belt-and-braces guard that the computed local path really
// is inside the mirror root, independent of how it was derived.
func (m *Mirror) checkContained(local string) error {
	rootAbs, err := filepath.Abs(m.root)
	if err != nil {
		return err
	}
	localAbs, err := filepath.Abs(local)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, localAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to touch %s: it is outside the mirror root %s", localAbs, rootAbs)
	}
	return nil
}

// Target reverses Local. It returns ok=false for a path outside the mirror.
func (m *Mirror) Target(localPath string) (string, bool) {
	rel, err := filepath.Rel(m.root, localPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return "/" + filepath.ToSlash(rel), true
}

// Fetch copies a target file into the mirror and records the digest it had.
func (m *Mirror) Fetch(ctx context.Context, targetPath string) (string, error) {
	data, err := m.fo.Read(ctx, targetPath, 0, 0)
	if err != nil {
		return "", err
	}
	local := m.Local(targetPath)
	if err := m.checkContained(local); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(local, data, 0o600); err != nil {
		return "", err
	}
	if err := m.recordDigest(targetPath, digestOf(data)); err != nil {
		return "", err
	}
	return local, nil
}

// Prepare readies the mirror for a file that may not exist on the target yet,
// which is the case for a fresh Write.
func (m *Mirror) Prepare(ctx context.Context, targetPath string) (string, error) {
	local := m.Local(targetPath)
	if err := m.checkContained(local); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o700); err != nil {
		return "", err
	}
	if data, err := m.fo.Read(ctx, targetPath, 0, 0); err == nil {
		if err := os.WriteFile(local, data, 0o600); err != nil {
			return "", err
		}
		return local, m.recordDigest(targetPath, digestOf(data))
	}
	// Absent on the target: record the absence so Push can distinguish
	// "created behind our back" from "we knew it did not exist".
	_ = os.Remove(local)
	return local, m.recordDigest(targetPath, "")
}

// Push writes the mirrored file back to the target.
//
// Before overwriting, it verifies the target still holds the content the mirror
// was fetched from. Without that check, a file changed on the target between
// fetch and push — by a build, a deploy, another session — would be silently
// overwritten from a stale base, destroying work with no error anywhere. A
// refusal the agent can see is always better than a quiet loss.
func (m *Mirror) Push(ctx context.Context, targetPath string) error {
	local := m.Local(targetPath)
	if err := m.checkContained(local); err != nil {
		return err
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return fmt.Errorf("read mirrored file: %w", err)
	}

	if expected, known := m.expectedDigest(targetPath); known {
		current, readErr := m.fo.Read(ctx, targetPath, 0, 0)
		if readErr == nil {
			if got := digestOf(current); got != expected {
				if expected == "" {
					return fmt.Errorf("refusing to overwrite %s: it did not exist when this edit began, "+
						"but something else has created it since. Re-read the file and redo the change.", targetPath)
				}
				return fmt.Errorf("refusing to overwrite %s: it changed on the target since it was read. "+
					"Something else modified it. Re-read the file and redo the change.", targetPath)
			}
		} else {
			var nf *reach.NotFoundError
			if !errors.As(readErr, &nf) {
				return fmt.Errorf("verify %s before writing: %w", targetPath, readErr)
			}
			if expected != "" {
				return fmt.Errorf("refusing to write %s: it was deleted on the target since it was read.", targetPath)
			}
		}
	}

	if err := m.fo.Write(ctx, targetPath, data, 0o644); err != nil {
		return err
	}
	return m.recordDigest(targetPath, digestOf(data))
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Digests are stored one file per mirrored path, not in a single document.
//
// A shared JSON map was the obvious design and was wrong: a harness issues tool
// calls in parallel, each hook is its own process, and each did a
// load-modify-write of the whole map. The last writer won and the others' entries
// vanished — measured at one surviving entry out of twenty concurrent fetches.
//
// A lost digest is not a lost optimisation. Push treats "no recorded digest" as
// "nothing to verify against" and writes anyway, so the guarantee that a write
// cannot overwrite a file that changed on the target since it was read silently
// stopped holding, in exactly the concurrent case where two tools are most
// likely to be touching the same tree.
//
// One file per path makes every record independent: no shared mutable document,
// no lock, and each update is a single atomic rename.

// digestDir holds the per-path digest records.
func (m *Mirror) digestDir() string { return filepath.Join(m.root, ".reach-digests") }

// digestRecordPath names a target path's record. The name is a digest of the
// path rather than the path itself, so it is flat, fixed-length, and cannot
// collide with, escape from, or be confused for mirrored content.
func (m *Mirror) digestRecordPath(targetPath string) string {
	sum := sha256.Sum256([]byte(targetPath))
	return filepath.Join(m.digestDir(), hex.EncodeToString(sum[:]))
}

// recordDigest stores the content digest a target path had when it was fetched.
//
// An empty digest is meaningful: it records that the file did not exist, which
// Push distinguishes from having no record at all.
func (m *Mirror) recordDigest(targetPath, digest string) error {
	if err := os.MkdirAll(m.digestDir(), 0o700); err != nil {
		return err
	}
	// The record carries the path it belongs to as well as the digest, so the
	// directory can be read by a human debugging a refused write.
	body := digest + "\n" + targetPath + "\n"

	final := m.digestRecordPath(targetPath)
	tmp, err := os.CreateTemp(m.digestDir(), "tmp-")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// expectedDigest returns the digest recorded at fetch time. known is false when
// reach has no record, which Push must treat as "cannot verify" rather than as
// "the file was absent".
func (m *Mirror) expectedDigest(targetPath string) (digest string, known bool) {
	data, err := os.ReadFile(m.digestRecordPath(targetPath))
	if err != nil {
		return "", false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	return line, true
}
