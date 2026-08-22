package main

import (
	"errors"
	"os/exec"
	"path/filepath"
)

// lookHarnessPath finds a harness binary on PATH the way the operator's shell
// does, and returns an absolute path to it.
//
// This is exec.LookPath with one policy changed. Since Go 1.19 a binary found
// through a relative PATH entry comes back with the path *and* an error
// satisfying exec.ErrDot, so that a caller checking `err != nil` refuses to run
// it. That rule is a sensible default for a program that might be handed a
// hostile PATH, and it is the wrong one here: a relative entry is an ordinary
// thing for a person to put in their own PATH, bash runs it without comment,
// and reach reporting "not installed or not in PATH" for a binary the operator
// can launch by name is a lie about the one thing the message is asserting.
//
// It is also worse than it looks, because LookPath stops at the first match.
// An operator who hits the refusal and appends an absolute entry still fails:
// the relative entry is earlier, still matches first, and still returns ErrDot,
// so the fix that should obviously work does nothing.
//
// What reach does not inherit is the ambiguity. The path is resolved to an
// absolute one here, while the working directory is still the one the lookup
// assumed. A relative path that stayed relative would name a different file
// after any chdir — and reach hands this path to exec and to the seam probe,
// which is precisely where "which binary is this, really" has to have one
// answer.
//
// The lookup itself stays exec.LookPath rather than a hand-rolled PATH walk,
// deliberately: on Windows executability is PATHEXT, not a file mode, and
// re-implementing that is how a search quietly stops finding `.cmd` wrappers.
func lookHarnessPath(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return "", err
	}
	abs, absErr := filepath.Abs(p)
	if absErr != nil {
		// Only reachable if the working directory cannot be determined, in
		// which case the relative path is the better of two bad answers.
		return p, nil
	}
	return abs, nil
}
