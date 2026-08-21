package harnessprobe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cache is the guard's memory: a wrong verdict read back is a wrong launch
// decision made on evidence that was never gathered. These tests pin the
// round trip, the file shape, and the rule that only conclusive verdicts may
// be stored.

func TestVerdictCacheRoundTrip(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())

	if _, ok, err := LoadVerdict("codex", "0.148.0"); err != nil || ok {
		t.Fatalf("empty cache: got ok=%v err=%v, want a clean miss", ok, err)
	}

	want := Result{Verdict: VerdictBypassed, Detail: "ran locally", ToolOutput: "marker\nbuild-box"}
	if err := StoreVerdict("codex", "0.148.0", want); err != nil {
		t.Fatalf("StoreVerdict: %v", err)
	}
	if err := StoreVerdict("codex", "0.147.0", Result{Verdict: VerdictOK, Detail: "on target"}); err != nil {
		t.Fatalf("StoreVerdict second version: %v", err)
	}
	if err := StoreVerdict("kimi", "0.148.0", Result{Verdict: VerdictOK, Detail: "on target"}); err != nil {
		t.Fatalf("StoreVerdict kimi: %v", err)
	}

	got, ok, err := LoadVerdict("codex", "0.148.0")
	if err != nil || !ok {
		t.Fatalf("LoadVerdict: ok=%v err=%v", ok, err)
	}
	if got.Verdict != want.Verdict || got.Detail != want.Detail {
		t.Fatalf("round trip changed the entry: got %+v, want verdict %q detail %q", got, want.Verdict, want.Detail)
	}
	if got.When.IsZero() || time.Since(got.When) > time.Minute {
		t.Fatalf("When should be stamped at store time, got %v", got.When)
	}

	// Storing a second version must not clobber the first.
	other, ok, err := LoadVerdict("codex", "0.147.0")
	if err != nil || !ok || other.Verdict != VerdictOK {
		t.Fatalf("the second version's entry was lost or changed: %+v ok=%v err=%v", other, ok, err)
	}

	// Verdicts are namespaced by harness: the same version number means
	// different binaries for codex and kimi, and one harness's "bypassed" must
	// never ground the other.
	kimi, ok, err := LoadVerdict("kimi", "0.148.0")
	if err != nil || !ok || kimi.Verdict != VerdictOK {
		t.Fatalf("the kimi entry was lost or changed: %+v ok=%v err=%v", kimi, ok, err)
	}
}

func TestVerdictCacheFileShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REACH_HOME", home)

	if err := StoreVerdict("codex", "0.148.0", Result{Verdict: VerdictOK}); err != nil {
		t.Fatalf("StoreVerdict: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "harness-verdicts.json"))
	if err != nil {
		t.Fatalf("the cache file is not where it is documented to be: %v", err)
	}
	// The shape is a contract with operators debugging the guard; pin it.
	if !strings.Contains(string(data), `"codex"`) ||
		!strings.Contains(string(data), `"0.148.0"`) ||
		!strings.Contains(string(data), `"verdict"`) {
		t.Fatalf("unexpected cache shape:\n%s", data)
	}
}

func TestStoreVerdictRefusesInconclusive(t *testing.T) {
	t.Setenv("REACH_HOME", t.TempDir())
	if err := StoreVerdict("codex", "0.148.0", Result{Verdict: VerdictError, Detail: "timeout"}); err == nil {
		t.Fatal("an error verdict must not be cacheable: it is a fact about probe conditions, not the codex version")
	}
}

func TestReadCacheToleratesCorruption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REACH_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "harness-verdicts.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadVerdict("codex", "0.148.0"); err != nil || ok {
		t.Fatalf("a corrupt cache must read as empty, not brick launches: ok=%v err=%v", ok, err)
	}
}

func TestReadCacheDiscardsSchema1Verdicts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("REACH_HOME", home)
	// Schema 1 was a bare harness→version→entry map, and every verdict in it
	// was measured against the PATH-shim seam. Those verdicts must not
	// survive into a build whose seam is different.
	legacy := `{"codex": {"0.148.0": {"verdict": "bypassed", "when": "2026-08-21T08:00:00Z", "detail": "ran locally"}}}`
	if err := os.WriteFile(filepath.Join(home, "harness-verdicts.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadVerdict("codex", "0.148.0"); err != nil || ok {
		t.Fatalf("a schema-1 verdict must read as a cache miss, not a refusal: ok=%v err=%v", ok, err)
	}
	// Storing after the upgrade rewrites the file at the current schema.
	if err := StoreVerdict("codex", "0.148.0", Result{Verdict: VerdictOK}); err != nil {
		t.Fatalf("StoreVerdict: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, "harness-verdicts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"schema"`) {
		t.Fatalf("rewritten cache must carry the schema marker:\n%s", data)
	}
}

func TestNormalizeVersion(t *testing.T) {
	cases := map[string]string{
		"codex-cli 0.148.0":       "0.148.0",
		"codex-cli 0.147.0-alpha": "0.147.0-alpha",
		"0.148.0":                 "0.148.0",
		"0.37.2":                  "0.37.2", // kimi's bare semver
		"kimi, version 0.37.2":    "0.37.2", // and if a banner ever wraps it
		"codex-cli 0.148.0\nmore": "0.148.0",
		"no version here":         "no version here",
		"":                        "",
	}
	for in, want := range cases {
		if got := NormalizeVersion(in); got != want {
			t.Errorf("NormalizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}
