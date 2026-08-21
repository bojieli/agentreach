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

// cacheFile is the on-disk shape: harness name, then version, then entry.
// Verdicts live in WALDO_HOME rather than beside the sessions because they
// describe the local harness installation, not any target.
type cacheFile map[string]map[string]Entry

// cachePath resolves $WALDO_HOME/harness-verdicts.json, applying the same
// WALDO_HOME-then-~/.waldo rule as the session store. Duplicated rather than
// shared because the main package's helper is not importable and the rule is
// five lines; two copies of a five-line rule beat a dependency from internal
// to main.
func cachePath() (string, error) {
	base := os.Getenv("WALDO_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".waldo")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(base, "harness-verdicts.json"), nil
}

func readCache() (cacheFile, error) {
	p, err := cachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cacheFile{}, nil
		}
		return nil, err
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		// A corrupt cache is not fatal: verdicts are derived data, re-probed on
		// demand. Treat it as empty rather than bricking every launch.
		//nolint:nilerr // a corrupt cache reads as an empty cache
		return cacheFile{}, nil
	}
	return c, nil
}

// LoadVerdict returns the cached verdict for one harness version, if any.
func LoadVerdict(harness, version string) (Entry, bool, error) {
	c, err := readCache()
	if err != nil {
		return Entry{}, false, err
	}
	e, ok := c[harness][version]
	return e, ok, nil
}

// StoreVerdict records a conclusive verdict for one harness version.
//
// The write is read-modify-write under an atomic rename. Two waldo processes
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
	if c[harness] == nil {
		c[harness] = map[string]Entry{}
	}
	c[harness][version] = Entry{Verdict: r.Verdict, When: time.Now(), Detail: r.Detail}

	p, err := cachePath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
