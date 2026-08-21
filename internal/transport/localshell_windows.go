//go:build windows

package transport

import (
	"fmt"
	"os"
)

// localShell resolves the shell used by a local:// target.
//
// A local:// target means "this machine is the target", and reach's floor is a
// POSIX shell — so on Windows this asks for something the platform does not
// have. It is deliberately not satisfied by whatever bash happens to be on
// PATH: a stray Git for Windows installation would quietly turn a Windows
// machine into a target, where MSYS path translation makes every absolute path
// mean something different from what the caller wrote.
//
// Setting REACH_LOCAL_SHELL makes it a deliberate act by someone who knows what
// they are pointing reach at. The ordinary Windows arrangement — reach and the
// agent on Windows, driving a remote POSIX host — needs none of this.
func localShell() (string, error) {
	if explicit := os.Getenv("REACH_LOCAL_SHELL"); explicit != "" {
		return explicit, nil
	}
	return "", fmt.Errorf(
		"a local:// target needs a POSIX shell, and Windows is not a supported target.\n" +
			"reach on Windows is meant to drive a *remote* POSIX host: point it at\n" +
			"ssh://host/path instead. If you genuinely want this machine to be the target\n" +
			"and have Git for Windows or MSYS2, set REACH_LOCAL_SHELL to its bash.exe —\n" +
			"expect MSYS path translation to change what absolute paths mean")
}
