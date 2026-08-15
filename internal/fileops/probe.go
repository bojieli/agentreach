package fileops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// Capabilities records what a target's userland actually provides.
//
// waldo probes once per session instead of guessing per command. Targets are
// not uniform — GNU coreutils, BSD, busybox and toybox differ on exactly the
// utilities file operations depend on, and a wrong guess produces corrupted
// data rather than a clean error. One round trip buys deterministic behaviour
// for the rest of the session, and `waldo doctor` prints the result so a
// surprising host is visible rather than mysterious.
type Capabilities struct {
	// StatFlavor is "gnu", "bsd" or "" when no usable stat exists.
	StatFlavor string
	// Base64Decode is the command that decodes base64 from stdin. GNU uses
	// -d, BSD/macOS uses -D, and openssl is the fallback when neither exists.
	Base64Decode string
	// Base64Encode is the command that encodes stdin.
	Base64Encode string
	// HasFind, HasXargs gate the listing and glob strategies.
	HasFind  bool
	HasXargs bool
	// Ripgrep is the rg binary name if present. Search is dramatically faster
	// and more accurate with it.
	Ripgrep string
	// SHA256 is the command producing a sha256 digest on stdin.
	SHA256 string
	// Python3 gates the pipe tier.
	Python3 bool
	// FindPrintf reports GNU find's -printf support, which allows NUL-safe
	// directory listing. Without it, listing cannot represent filenames
	// containing newlines.
	FindPrintf bool
	// GrepSkipBinary is the flag that makes the target's grep ignore binary
	// files, or empty when it has none. GNU, BSD and busybox all accept -I;
	// only GNU and BSD accept --binary-files=without-match, and busybox rejects
	// the whole command when given it.
	GrepSkipBinary string
	// CacheDir is where the target keeps per-user caches, resolved once so the
	// helper tier does not spend a round trip asking on every tool call.
	CacheDir string
	// HelperPath and HelperDigest record a helper binary this session has
	// already verified, so the verification costs one round trip per session
	// rather than one per tool call.
	HelperPath   string
	HelperDigest string
	// RawStdin and RawStdout report that binary content survives the transport
	// unencoded, in each direction, proven by a round trip rather than assumed.
	// When they hold, file content skips base64 and costs a third less to move.
	RawStdin  bool
	RawStdout bool
	// LoginPath is the PATH the target's own login shell would give the
	// operator, when it differs from the one a non-interactive command gets.
	// Empty means they matched and nothing needs overriding.
	LoginPath string
	// Shell is the target's /bin/sh identity, informational.
	Uname string
}

// probeScript is intentionally a single POSIX-sh program with no pipelines
// that depend on the very utilities it is testing for. It prints KEY=VALUE
// lines and must never fail: an absent tool is a value, not an error.
const probeScript = `
w_has() { command -v "$1" >/dev/null 2>&1; }
printf 'UNAME=%s\n' "$(uname -sm 2>/dev/null || echo unknown)"

if stat -c '%s' / >/dev/null 2>&1; then printf 'STAT=gnu\n'
elif stat -f '%z' / >/dev/null 2>&1; then printf 'STAT=bsd\n'
else printf 'STAT=\n'; fi

if w_has base64; then
  if printf 'eA==' | base64 -d >/dev/null 2>&1; then printf 'B64D=base64 -d\nB64E=base64\n'
  elif printf 'eA==' | base64 -D >/dev/null 2>&1; then printf 'B64D=base64 -D\nB64E=base64\n'
  else printf 'B64D=\nB64E=\n'; fi
elif w_has openssl; then printf 'B64D=openssl base64 -d -A\nB64E=openssl base64 -A\n'
else printf 'B64D=\nB64E=\n'; fi

w_has find  && printf 'FIND=1\n'  || printf 'FIND=0\n'
if find . -maxdepth 0 -printf '' >/dev/null 2>&1; then printf 'FINDPF=1\n'; else printf 'FINDPF=0\n'; fi
w_has xargs && printf 'XARGS=1\n' || printf 'XARGS=0\n'
w_has python3 && printf 'PY3=1\n' || printf 'PY3=0\n'

if w_has rg; then printf 'RG=rg\n'; else printf 'RG=\n'; fi

printf 'CACHE=%s\n' "${XDG_CACHE_HOME:-$HOME/.cache}"

# Which flag suppresses binary files? busybox grep accepts -I but rejects
# --binary-files=..., and rejects the *entire command* when it sees it — which
# turned every search on an Alpine target into a confident "no matches".
if printf 'x\n' | grep -I x >/dev/null 2>&1; then printf 'GREPI=-I\n'
else printf 'GREPI=\n'; fi

if w_has sha256sum; then printf 'SHA=sha256sum\n'
elif w_has shasum; then printf 'SHA=shasum -a 256\n'
elif w_has openssl; then printf 'SHA=openssl dgst -sha256 -r\n'
else printf 'SHA=\n'; fi
`

// loginPathScript asks the target what PATH the operator would actually have.
//
// `ssh host command` runs a non-interactive shell, which on Debian and Ubuntu
// returns from ~/.bashrc before reaching anything that edits PATH. So waldo saw
// /usr/bin and friends while the operator's own shell had ~/.local/bin, ~/bin
// and ~/.cargo/bin as well — measured on a real host, where the two differed by
// five directories.
//
// That gap is not cosmetic. `cargo install ripgrep` is how most people get rg,
// and it lands in ~/.cargo/bin, so waldo would report "no ripgrep on the
// target" and quietly fall back to grep on a machine that has it. Reporting a
// capability as absent when it is present, and degrading on that basis, is the
// failure this project exists to avoid.
//
// The login shell is asked once, with a timeout, and its answer is used for
// every command afterwards — detection and execution together, so waldo can
// never find a tool during the probe that it then cannot run.
const loginPathScript = `
w_login_path() {
  # Prefer the operator's own shell; fall back to sh. Either may be absent or
  # may refuse to be a login shell, and neither is worth failing the probe over.
  for s in "$SHELL" /bin/bash /bin/sh; do
    [ -n "$s" ] && [ -x "$s" ] || continue
    p=$("$s" -lc 'printf %s "$PATH"' 2>/dev/null) || continue
    [ -n "$p" ] && { printf '%s' "$p"; return 0; }
  done
  return 1
}
printf 'LOGINPATH=%s\n' "$(w_login_path 2>/dev/null)"
`

// Probe inspects a target's userland.
func Probe(ctx context.Context, t transport.Transport) (*Capabilities, error) {
	loginPath := detectLoginPath(ctx, t)
	// Detection runs under the login PATH, so what waldo finds is what the
	// operator would find.
	res, err := t.Run(ctx, waldo.ExecRequest{Command: probeScript, Env: pathEnv(loginPath)})
	if err != nil {
		return nil, fmt.Errorf("probe target: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("probe target: shell exited %d: %s", res.Code, strings.TrimSpace(string(res.Stderr)))
	}
	c := &Capabilities{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "UNAME":
			c.Uname = v
		case "STAT":
			c.StatFlavor = v
		case "B64D":
			c.Base64Decode = v
		case "B64E":
			c.Base64Encode = v
		case "FIND":
			c.HasFind = v == "1"
		case "FINDPF":
			c.FindPrintf = v == "1"
		case "XARGS":
			c.HasXargs = v == "1"
		case "PY3":
			c.Python3 = v == "1"
		case "RG":
			c.Ripgrep = v
		case "GREPI":
			c.GrepSkipBinary = v
		case "CACHE":
			c.CacheDir = v
		case "SHA":
			c.SHA256 = v
		}
	}
	if c.Base64Decode == "" || c.Base64Encode == "" {
		return c, fmt.Errorf(
			"target has no base64 and no openssl: waldo cannot move file content safely.\n"+
				"Binary-safe transfer needs one of them; without it, content would have to be\n"+
				"passed through the shell unencoded, which corrupts any file containing NUL or\n"+
				"invalid UTF-8. Target reports: %s", c.Uname)
	}
	c.LoginPath = loginPath
	c.RawStdin, c.RawStdout = probeRawIO(ctx, t, c)
	return c, nil
}

// rawProbeBlob is every byte value, plus the sequences a transport is most
// likely to mangle: a lone CR, a lone LF, CRLF, and a trailing newline that a
// naive `$(...)` capture would eat.
func rawProbeBlob() []byte {
	b := make([]byte, 0, 300)
	for i := 0; i < 256; i++ {
		b = append(b, byte(i))
	}
	return append(b, '\r', '\n', '\r', '\n', 0x00, 0xff, '\n')
}

// probeRawIO asks whether binary content survives the transport unencoded.
//
// waldo has always base64-framed file content, which is unconditionally safe
// and costs a third of the bandwidth in both directions. Whether it is
// *necessary* is a property of the transport, and transports differ: an ssh
// session with no pty is 8-bit clean, while a pty translates newlines, and
// Windows ssh clients have historically translated on their own. So it is
// measured per target instead of assumed either way.
//
// Neither direction writes anything to the target. Stdin fidelity is checked by
// piping the blob into the target's own digest command and comparing; stdout
// fidelity by having the target print the blob and comparing bytes. A target
// that garbles either simply keeps base64.
func probeRawIO(ctx context.Context, t transport.Transport, c *Capabilities) (stdin, stdout bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	blob := rawProbeBlob()

	if c.SHA256 != "" {
		sum := sha256.Sum256(blob)
		res, err := t.Run(ctx, waldo.ExecRequest{
			Command:   c.SHA256,
			Stdin:     blob,
			Env:       pathEnv(c.LoginPath),
			MaxOutput: 4 << 10,
		})
		if err == nil && res.Code == 0 {
			fields := strings.Fields(string(res.Stdout))
			stdin = len(fields) > 0 &&
				strings.TrimPrefix(fields[0], "\\") == hex.EncodeToString(sum[:])
		}
	}

	// printf with octal escapes is POSIX and needs nothing installed.
	var esc strings.Builder
	for _, b := range blob {
		fmt.Fprintf(&esc, "\\%03o", b)
	}
	res, err := t.Run(ctx, waldo.ExecRequest{
		Command:   "printf " + transport.ShellQuote(esc.String()),
		Env:       pathEnv(c.LoginPath),
		MaxOutput: 64 << 10,
	})
	if err == nil && res.Code == 0 {
		stdout = bytes.Equal(res.Stdout, blob)
	}
	return stdin, stdout
}

// detectLoginPath returns the login shell's PATH when it adds anything, or ""
// when it matches what a plain command already gets.
func detectLoginPath(ctx context.Context, t transport.Transport) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	res, err := t.Run(ctx, waldo.ExecRequest{Command: loginPathScript, MaxOutput: 64 << 10})
	if err != nil || res.Code != 0 {
		return "" // a target that will not answer keeps the default PATH
	}
	var login string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "LOGINPATH="); ok {
			login = v
		}
	}
	if login == "" || !strings.Contains(login, "/") {
		return ""
	}
	// A login shell that adds nothing is not worth overriding anything for.
	plain, err := t.Run(ctx, waldo.ExecRequest{Command: `printf %s "$PATH"`, MaxOutput: 64 << 10})
	if err == nil && strings.TrimSpace(string(plain.Stdout)) == login {
		return ""
	}
	return login
}

// pathEnv renders a PATH override, or nothing when there is none to apply.
func pathEnv(loginPath string) map[string]string {
	if loginPath == "" {
		return nil
	}
	return map[string]string{"PATH": loginPath}
}

// Env returns the environment every command on this target should carry.
func (c *Capabilities) Env() map[string]string {
	if c == nil {
		return nil
	}
	return pathEnv(c.LoginPath)
}

// Qualifies reports whether a target can support a tier, and why not when it
// cannot. The reason is shown by `waldo doctor`, so an operator can see that a
// host is on tier 0 because it lacks python3 rather than because waldo decided
// so for no visible reason.
func (c *Capabilities) Qualifies(tier waldo.Tier) (bool, string) {
	switch tier {
	case waldo.TierPOSIX:
		if c.Base64Decode == "" || c.Base64Encode == "" {
			return false, "no base64 and no openssl"
		}
		if c.StatFlavor == "" {
			return false, "no usable stat command"
		}
		return true, ""
	case waldo.TierPipe:
		if !c.Python3 {
			return false, "no python3"
		}
		return true, ""
	case waldo.TierHelper:
		if _, _, err := platformOf(c.Uname); err != nil {
			return false, err.Error()
		}
		return true, "writes a binary to the target; never chosen automatically"
	}
	return false, "unknown tier"
}

// negotiationOrder is the order autonegotiation prefers tiers in, most
// preferred first.
//
// Every remaining tier answers one file operation in one network round trip:
// the shell tier sends a command and reads its output, the pipe and helper
// tiers send a request and read a response. That is the property that decides
// this list, and the reason a since-removed SFTP tier is not on it: SFTP hands
// out a handle before it will read, so its floor was two.
//
// Between the survivors it is round trips and startup cost. Measured against a
// host 171 ms away, median of three runs:
//
//	                15x1KiB read   8MiB read   8MiB write
//	posix                  5.83s       6.39s        6.79s
//	pipe                   4.08s       5.19s        5.43s
//	helper                 3.93s       5.16s        5.53s
//
// pipe and helper are close, because they are the same protocol with a
// different program on the far end. posix trails on bulk because it pipes
// content through a shell, and on small reads because it spawns processes per
// operation — a gap that widens sharply on a macOS target, where process
// creation is expensive.
//
// TierHelper is deliberately absent, and no longer for performance reasons —
// it is now the fastest tier for small reads. It is absent because it writes a
// binary to a machine the operator may not own, and that is a decision waldo
// does not make for them. Speed is not the argument that would justify it.
var negotiationOrder = []waldo.Tier{waldo.TierPipe}

// BestTier reports the tier autonegotiation would choose for this target.
func (c *Capabilities) BestTier() waldo.Tier {
	for _, tier := range negotiationOrder {
		if ok, _ := c.Qualifies(tier); ok {
			return tier
		}
	}
	return waldo.TierPOSIX
}
