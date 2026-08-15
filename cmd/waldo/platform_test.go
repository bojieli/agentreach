package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run on every platform and assert the behaviour each one needs.
// They exist because the Windows differences are not cosmetic: two of them —
// the PATH spelling and the missing execute bit — fail *silently* in a way that
// ends with an agent running the model's commands on the operator's own machine
// while believing it is working on the target.

// TestShimAliasIsInstalledAndRecognised covers the whole shim round trip: waldo
// installs an alias of itself, and a process started through that alias must
// recognise the name it was invoked as.
func TestShimAliasIsInstalledAndRecognised(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WALDO_HOME", home)

	alias, err := ensureShim()
	if err != nil {
		t.Fatalf("ensureShim: %v", err)
	}
	if _, err := os.Stat(alias); err != nil {
		t.Fatalf("the shim was reported installed but is not there: %v", err)
	}

	// The harness is handed this exact path and must be able to execute it.
	if !isExecutableFile(alias) {
		t.Errorf("%s is not executable by this platform's rules; a harness would refuse it", alias)
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(alias), ".exe") {
		t.Errorf("shim is named %q; Windows will not execute a file whose extension is not in PATHEXT", alias)
	}

	// argv[0] dispatch: the alias name must map back to the logical shim name.
	if got := programBase(alias); got != shimName {
		t.Errorf("programBase(%q) = %q, want %q — waldo would not recognise its own shim", alias, got, shimName)
	}

	// A second call must be a no-op rather than a reinstall.
	again, err := ensureShim()
	if err != nil {
		t.Fatalf("second ensureShim: %v", err)
	}
	if again != alias {
		t.Errorf("shim path changed between calls: %q then %q", alias, again)
	}
}

// TestShimAliasIsRefreshedWhenStale is the property that matters after an
// upgrade. A stale shim is an old waldo running inside a tool call, which
// surfaces as the harness misbehaving rather than as a waldo problem.
func TestShimAliasIsRefreshedWhenStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WALDO_HOME", home)

	alias, err := ensureShim()
	if err != nil {
		t.Fatal(err)
	}
	self, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if !programAliasIsCurrent(alias, self) {
		t.Fatal("a freshly installed alias is not recognised as current")
	}

	// Replace it with something that is definitely not waldo.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, []byte("stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if programAliasIsCurrent(alias, self) {
		t.Error("a stale alias was reported as current; an upgrade would leave the old waldo in the tool path")
	}
	if _, err := ensureShim(); err != nil {
		t.Fatalf("ensureShim did not repair a stale alias: %v", err)
	}
	if !programAliasIsCurrent(alias, self) {
		t.Error("the alias is still stale after ensureShim")
	}
}

// TestPathShimsInstallEveryShellName covers the Codex/Kimi seam.
func TestPathShimsInstallEveryShellName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WALDO_HOME", home)

	dir, err := ensurePathShim()
	if err != nil {
		t.Fatalf("ensurePathShim: %v", err)
	}
	for _, name := range shimmedShellNames {
		p := filepath.Join(dir, programName(name))
		if !isExecutableFile(p) {
			t.Errorf("%s was not installed as an executable; the harness would find the real shell instead", p)
		}
		if got := programBase(p); got != name {
			t.Errorf("programBase(%q) = %q, want %q", p, got, name)
		}
	}
}

// TestPrependPathFindsTheRealPathVariable is the Windows bug that would have
// been invisible: the search path is conventionally spelled `Path` there, and
// matching "PATH" exactly appends a second variable instead of editing the real
// one. The harness then launches without waldo's shim directory in front, finds
// the genuine bash, and runs the model's commands locally.
func TestPrependPathFindsTheRealPathVariable(t *testing.T) {
	spellings := []string{"PATH"}
	if runtime.GOOS == "windows" {
		spellings = append(spellings, "Path", "path")
	}
	for _, spelling := range spellings {
		t.Run(spelling, func(t *testing.T) {
			env := []string{"HOME=/somewhere", spelling + "=/usr/bin", "OTHER=1"}
			got := prependPath(env, "/waldo/shim")

			var pathEntries []string
			for _, kv := range got {
				if k, v, ok := strings.Cut(kv, "="); ok && isPathEnvKey(k) {
					pathEntries = append(pathEntries, v)
				}
			}
			if len(pathEntries) != 1 {
				t.Fatalf("environment ended up with %d search-path variables (%v); a child process would pick one at random",
					len(pathEntries), got)
			}
			want := "/waldo/shim" + string(filepath.ListSeparator) + "/usr/bin"
			if pathEntries[0] != want {
				t.Errorf("path = %q, want %q", pathEntries[0], want)
			}
			if len(got) != len(env) {
				t.Errorf("environment grew from %d to %d entries", len(env), len(got))
			}
		})
	}
}

// TestSetPathEnvPreservesTheKeySpelling: adding a variable that differs only in
// case leaves Windows to choose between them, which is not a decision waldo
// should be delegating to chance.
func TestSetPathEnvPreservesTheKeySpelling(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows has case-insensitive environment variables")
	}
	got := setPathEnv([]string{"Path=C:\\bin"}, "C:\\tools")
	if len(got) != 1 || got[0] != "Path=C:\\tools" {
		t.Errorf("setPathEnv produced %v, want [Path=C:\\tools]", got)
	}
}

// TestSanitisedEnvRemovesTheShimDirectory stops the shim from recursing into
// itself, which without a guard runs the machine out of processes.
func TestSanitisedEnvRemovesTheShimDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WALDO_HOME", home)
	shimDir, err := shimBinDir()
	if err != nil {
		t.Fatal(err)
	}
	sep := string(filepath.ListSeparator)
	t.Setenv("PATH", shimDir+sep+filepath.Join(home, "real"))

	for _, kv := range sanitisedEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok && isPathEnvKey(k) {
			for _, dir := range filepath.SplitList(v) {
				if sameDir(dir, shimDir) {
					t.Fatalf("the shim directory survived sanitisation: %q", v)
				}
			}
			return
		}
	}
	t.Fatal("sanitisedEnv returned no search path at all")
}

// TestSameDirIsCaseInsensitiveOnWindows: comparing shim directories by bytes
// would fail to recognise waldo's own directory when the case differs, sending
// the shim back into itself.
func TestSameDirIsCaseInsensitiveOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("path comparison is case-sensitive on this platform")
	}
	if !sameDir(`C:\Users\Test\.waldo\shim`, `c:\users\test\.waldo\SHIM`) {
		t.Error("two spellings of one Windows directory were treated as different")
	}
}

// TestIsExecutableFileMatchesPlatformRules guards the check that decides
// whether waldo believes a harness is installed. Windows has no execute bit —
// os.Stat reports 0444 or 0666 and nothing else — so a mode test there is a
// test that always fails, and waldo would report every harness as missing.
func TestIsExecutableFileMatchesPlatformRules(t *testing.T) {
	dir := t.TempDir()

	plain := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isExecutableFile(plain) {
		t.Error("a plain data file was reported as executable")
	}

	runnable := filepath.Join(dir, programName("runnable"))
	if err := os.WriteFile(runnable, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(runnable) {
		t.Errorf("%s was not reported as executable; waldo would say the harness is not installed", runnable)
	}

	if isExecutableFile(dir) {
		t.Error("a directory was reported as executable")
	}
}

// TestProgramBaseStripsTheWindowsExtension: argv[0] arrives as `bash.exe`, and
// the shim dispatch compares against `bash`.
func TestProgramBaseStripsTheWindowsExtension(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only Windows decorates executable names")
	}
	for _, tc := range []struct{ argv0, want string }{
		{`C:\waldo\shim\bash.exe`, "bash"},
		{`C:\waldo\shim\BASH.EXE`, "BASH"},
		{`C:\waldo\bin\waldo-shell-prefix.exe`, shimName},
	} {
		if got := programBase(tc.argv0); !strings.EqualFold(got, tc.want) {
			t.Errorf("programBase(%q) = %q, want %q", tc.argv0, got, tc.want)
		}
	}
}
