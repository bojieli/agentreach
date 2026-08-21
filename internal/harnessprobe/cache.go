package harnessprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Entry is one cached verdict: what the probe concluded about one version of
// one harness, when, and why.
type Entry struct {
	Verdict string    `json:"verdict"`
	When    time.Time `json:"when"`
	Detail  string    `json:"detail,omitempty"`
}

// cacheSchema versions the on-disk shape. Schema 1 was a bare
// harness→version→entry map; every verdict recorded under it measured the
// PATH-shim seam. When reach's seam for a harness changes (Codex moved to the
// exec-server protocol, Kimi to KIMI_SHELL_PATH), those verdicts describe a
// mechanism reach no longer uses, so the whole file is discarded rather than
// trusted: a stale "bypassed" would refuse a launch the new seam would pass,
// and a stale "ok" would be worse.
const cacheSchema = 2

// cacheFile is the on-disk shape: a schema marker, then harness name, then
// version, then entry. Verdicts live in REACH_HOME rather than beside the
// sessions because they describe the local harness installation, not any
// target.
type cacheFile struct {
	Schema   int                         `json:"schema"`
	Verdicts map[string]map[string]Entry `json:"verdicts"`
}

// emptyCache returns a usable zero cache at the current schema.
func emptyCache() cacheFile {
	return cacheFile{Schema: cacheSchema, Verdicts: map[string]map[string]Entry{}}
}

// cachePath resolves $REACH_HOME/harness-verdicts.json, applying the same
// REACH_HOME-then-~/.reach rule as the session store. Duplicated rather than
// shared because the main package's helper is not importable and the rule is
// five lines; two copies of a five-line rule beat a dependency from internal
// to main.
func cachePath() (string, error) {
	base := os.Getenv("REACH_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".reach")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(base, "harness-verdicts.json"), nil
}

func readCache() (cacheFile, error) {
	p, err := cachePath()
	if err != nil {
		return cacheFile{}, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emptyCache(), nil
		}
		return cacheFile{}, err
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		// A corrupt cache is not fatal: verdicts are derived data, re-probed on
		// demand. Treat it as empty rather than bricking every launch.
		//nolint:nilerr // a corrupt cache reads as an empty cache
		return emptyCache(), nil
	}
	if c.Schema != cacheSchema {
		// A cache from another schema — including a newer reach's — is not
		// evidence about the seam this build uses. Re-probe everything.
		return emptyCache(), nil
	}
	if c.Verdicts == nil {
		c.Verdicts = map[string]map[string]Entry{}
	}
	return c, nil
}

// LoadVerdict returns the cached verdict for one harness version, if any.
func LoadVerdict(harness, version string) (Entry, bool, error) {
	c, err := readCache()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := c.Verdicts[harness][version]
	return e, ok, nil
}

// StoreVerdict records a conclusive verdict for one harness version.
//
// The write is read-modify-write under an atomic rename. Two reach processes
// storing at once can lose each other's update; that is acceptable here —
// the loser simply re-probes next launch — while a half-written file is not,
// because the guard reads this cache on the path to launching an agent.
func StoreVerdict(harness, version string, r Result) error {
	if !r.Conclusive() {
		return fmt.Errorf("refusing to cache an inconclusive verdict %q", r.Verdict)
	}
	c, err := readCache()
	if err != nil {
		return err
	}
	if c.Verdicts[harness] == nil {
		c.Verdicts[harness] = map[string]Entry{}
	}
	c.Verdicts[harness][version] = Entry{Verdict: r.Verdict, When: time.Now(), Detail: r.Detail}

	p, err := cachePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Use os.CreateTemp so concurrent StoreVerdict calls within the same process
	// each get a unique file rather than racing on a shared PID-named path.
	// The write is still read-modify-write (two concurrent stores can lose each
	// other's update), but neither will corrupt the other's temp file.
	f, err := os.CreateTemp(filepath.Dir(p), ".harness-verdicts-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}
