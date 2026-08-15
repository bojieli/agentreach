// Package mirror materialises individual remote files as real local files, on
// demand, so a harness's native file tools operate on remote content.
//
// This is deliberately not a filesystem sync engine. Nothing is mirrored until
// a tool actually asks for it, there is no background reconciliation, and no
// attempt is made to track deletions or resolve divergence. A sync engine would
// have to answer questions waldo has no good answer to — what to do when both
// sides changed, whether a missing file was deleted or never fetched — and
// getting those wrong loses the operator's work. Fetching exactly the file a
// tool is about to touch, at the moment it touches it, has none of those
// questions.
package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/waldo"
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
// waldo would read or write an arbitrary local file on behalf of whatever
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
			var nf *waldo.NotFoundError
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

func (m *Mirror) digestFile() string { return filepath.Join(m.root, ".waldo-digests.json") }

func (m *Mirror) loadDigests() map[string]string {
	out := map[string]string{}
	if data, err := os.ReadFile(m.digestFile()); err == nil {
		_ = json.Unmarshal(data, &out)
	}
	return out
}

func (m *Mirror) recordDigest(targetPath, digest string) error {
	d := m.loadDigests()
	d[targetPath] = digest
	if err := os.MkdirAll(m.root, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", m.digestFile(), os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.digestFile())
}

func (m *Mirror) expectedDigest(targetPath string) (string, bool) {
	v, ok := m.loadDigests()[targetPath]
	return v, ok
}
