//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Windows support. See platform_other.go for the Unix side; between them they
// hold every difference, so nothing else in waldo branches on GOOS.
//
// Three Windows facts shape this file:
//
//  1. There is no execve. Go stubs syscall.Exec to return EWINDOWS, so a
//     harness is launched as a child process instead (see launch.go).
//  2. Symlinks need Developer Mode or an administrator, so they cannot be part
//     of a tool that must work for an ordinary user. Hard links do not, and
//     they are better than copies: a hard link is the same file, so waldo can
//     tell whether a shim is current by identity rather than by guesswork.
//  3. Executability is not a file mode. Windows decides by extension, via
//     PATHEXT, which is why every shim waldo installs is named `.exe` and why
//     lookups go through exec.LookPath rather than a hand-rolled PATH walk.

// platformCheck reports whether waldo can run here.
func platformCheck() error { return nil }

// execUnsupported reports whether an execve failure is simply Windows having no
// execve. Go stubs syscall.Exec here to return EWINDOWS unconditionally, so
// there is nothing to report to the operator: launching a child is the normal
// path on this platform, not a degradation worth a warning.
func execUnsupported(err error) bool { return errors.Is(err, syscall.EWINDOWS) }

// programName renders an executable's filename.
//
// The extension is not cosmetic. Windows will not execute a file without one
// that appears in PATHEXT, and a harness that resolves its shell by name would
// simply not find `bash`.
func programName(base string) string {
	if strings.EqualFold(filepath.Ext(base), ".exe") {
		return base
	}
	return base + ".exe"
}

// programBase recovers the logical name a program was invoked as.
//
// argv[0] arrives as `bash.exe`, and the shim dispatch compares against `bash`,
// so the extension is stripped. It is compared case-insensitively because
// Windows filenames are, and a harness may spawn `BASH.EXE`.
func programBase(argv0 string) string {
	base := filepath.Base(argv0)
	if ext := filepath.Ext(base); ext != "" && strings.EqualFold(ext, ".exe") {
		return strings.TrimSuffix(base, ext)
	}
	return base
}

// installProgramAlias makes dest another name for the running binary.
//
// A hard link is tried first: it consumes no extra disk, and because it is
// literally the same file, os.SameFile can later prove a shim is current rather
// than assume it. It fails across volumes and on filesystems without link
// support, in which case waldo copies — correct, just several megabytes and in
// need of refreshing when waldo is upgraded.
func installProgramAlias(self, dest string) error {
	_ = os.Remove(dest)
	if err := os.Link(self, dest); err == nil {
		return nil
	}
	return copyProgram(self, dest)
}

func copyProgram(self, dest string) error {
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	// Write to a temporary name and rename into place, so a concurrent waldo
	// never executes a half-copied shim — which on Windows presents as a
	// baffling "not a valid Win32 application" in the middle of a tool call.
	tmp := dest + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install %s: %w", dest, err)
	}
	// Record what this copy was made from, so the next run can tell whether it
	// is stale. A hard link needs no stamp; identity answers the question.
	return writeAliasStamp(dest, self)
}

// programAliasIsCurrent reports whether dest is still a valid alias for self.
//
// Getting this wrong is not cosmetic. A stale shim is an old waldo running
// inside a tool call, which is the kind of failure that surfaces as a harness
// misbehaving rather than as a waldo problem.
func programAliasIsCurrent(dest, self string) bool {
	destInfo, err := os.Stat(dest)
	if err != nil {
		return false
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	// Hard link: same file, so it cannot be stale.
	if os.SameFile(destInfo, selfInfo) {
		return true
	}
	// Copy: compare against the stamp written when it was made.
	stamp, err := os.ReadFile(aliasStampPath(dest))
	if err != nil {
		return false
	}
	return string(stamp) == aliasStampFor(self, selfInfo)
}

func aliasStampPath(dest string) string { return dest + ".source" }

func aliasStampFor(self string, fi os.FileInfo) string {
	return fmt.Sprintf("%s\n%d\n%d\n", self, fi.Size(), fi.ModTime().UnixNano())
}

func writeAliasStamp(dest, self string) error {
	fi, err := os.Stat(self)
	if err != nil {
		return err
	}
	return os.WriteFile(aliasStampPath(dest), []byte(aliasStampFor(self, fi)), 0o600)
}

// isExecutableFile reports whether a path can be run.
//
// Windows has no execute bit — os.Stat reports 0444 or 0666 and nothing else —
// so asking for one is a test that always fails. What decides here is the
// extension.
func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return false
	}
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	for _, candidate := range strings.Split(pathext, ";") {
		if strings.EqualFold(strings.TrimSpace(candidate), ext) {
			return true
		}
	}
	return false
}

// shellCandidateNames are the filenames a real (non-shim) shell may have.
//
// On Windows this means a POSIX shell — Git for Windows or MSYS2 — because
// waldo's shim implements the `bash -c` contract and hands anything else
// straight through to the genuine article.
func shellCandidateNames() []string { return []string{"bash.exe", "sh.exe"} }

// fallbackShellPaths are searched when PATH yields no shell. These are where
// Git for Windows and MSYS2 install by default.
func fallbackShellPaths() []string {
	var out []string
	for _, root := range []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		`C:\Program Files`,
		`C:\Program Files (x86)`,
	} {
		if root == "" {
			continue
		}
		out = append(out,
			filepath.Join(root, "Git", "bin", "bash.exe"),
			filepath.Join(root, "Git", "usr", "bin", "bash.exe"),
		)
	}
	return append(out, `C:\msys64\usr\bin\bash.exe`, `C:\cygwin64\bin\bash.exe`)
}

// isPathEnvKey reports whether an environment key is the search path.
//
// Windows environment variables are case-insensitive and the search path is
// conventionally spelled `Path`, not `PATH`. Comparing exactly — which is
// correct on Unix — means waldo silently fails to put its shim directory in
// front of the harness, and the harness then finds the real shell and runs the
// model's commands on the operator's own machine.
func isPathEnvKey(k string) bool { return strings.EqualFold(k, "PATH") }
