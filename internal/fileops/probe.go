package fileops

import (
	"context"
	"fmt"
	"strings"

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
	// SFTP reports whether the SFTP subsystem answered. Only meaningful for
	// ssh transports.
	SFTP bool
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

// Probe inspects a target's userland.
func Probe(ctx context.Context, t transport.Transport) (*Capabilities, error) {
	res, err := t.Run(ctx, waldo.ExecRequest{Command: probeScript})
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
	// Whether the SFTP subsystem answers cannot be read out of the target's
	// userland, only by asking it. One extra channel during `waldo up` buys a
	// tier decision that is proven rather than assumed.
	c.SFTP = ProbeSFTP(ctx, t)
	return c, nil
}

// ProbeSFTP reports whether the target answers on the SFTP subsystem.
//
// This is done by completing a real handshake rather than by asking sshd what
// it is configured to do. A host can advertise the subsystem and still refuse
// it — a Match block, a forced command, a chroot without the server binary —
// and a tier that is available in theory and refused in practice would fail on
// first use instead of during negotiation.
func ProbeSFTP(ctx context.Context, t transport.Transport) bool {
	if _, ok := t.(transport.SubsystemOpener); !ok {
		return false
	}
	ops, err := NewSFTP(ctx, t, NewPOSIX(t, &Capabilities{}))
	if err != nil {
		return false
	}
	_ = ops.Close()
	return true
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
	case waldo.TierSFTP:
		if !c.SFTP {
			return false, "the SFTP subsystem did not answer"
		}
		return true, ""
	case waldo.TierPipe:
		if !c.Python3 {
			return false, "no python3"
		}
		return true, ""
	case waldo.TierAgent:
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
// It is measured, and it was measured twice, because the first measurement was
// taken in the wrong place.
//
// A benchmark with no network in it measures the target's process spawner, not
// the tiers: tier 0 spawns several processes per read, and the same measurement
// is 7x cheaper against a Linux target than a macOS one. Over a real link,
// round trips dominate and the tiers separate properly:
//
//	                15x1KiB read   8MiB read   8MiB write
//	posix                 22.42s      33.87s        8.65s
//	sftp                  36.27s      27.78s       15.43s
//	pipe                  20.35s       7.46s        6.36s
//	agent                 42.72s      12.23s       13.51s
//
// Latency, not CPU, is what a remote target actually costs, and the tiers
// differ in round trips per operation rather than in work done. The pipe and
// agent tiers answer a whole file in one request/response over a channel that
// is already open; SFTP needs several — open, fstat, read, close — each a round
// trip, and its cheap setup stops mattering the moment a round trip is not
// free.
//
// waldo exists to drive *remote* hosts, so the remote numbers decide this, and
// they agree at both 171 ms and 540 ms. sftp stays ahead of posix, and above
// both sits pipe.
//
// TierAgent is deliberately absent: that tier writes a binary to the target, and
// waldo never makes that choice on the operator's behalf. It is also the
// slowest to start, which the small-read column shows plainly.
var negotiationOrder = []waldo.Tier{waldo.TierPipe, waldo.TierSFTP}

// BestTier reports the tier autonegotiation would choose for this target.
func (c *Capabilities) BestTier() waldo.Tier {
	for _, tier := range negotiationOrder {
		if ok, _ := c.Qualifies(tier); ok {
			return tier
		}
	}
	return waldo.TierPOSIX
}
