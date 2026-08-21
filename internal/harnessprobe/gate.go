package harnessprobe

import (
	"fmt"
	"strings"
)

// Decision is what the launch guard should do about one harness version.
type Decision struct {
	// Allow permits the launch. The guard fails closed: Allow is true only
	// for a verified-ok seam, for a probe that could not run at all (with a
	// loud warning), or for --force, which never consults Gate at all.
	Allow bool
	// RunProbe asks the caller to run Verify now and re-decide with
	// GateFromProbe. Set when there is no trustworthy cached verdict.
	RunProbe bool
	// Message is for stderr. Empty means silent — a verified seam is the
	// expected state and should not chatter on every launch.
	Message string
}

// Gate decides from the cache alone whether this harness version may launch.
//
// The asymmetry is the entire point of the guard: a cached "bypassed" refuses,
// because that is a measured fact about the version, while a missing or
// unrecognised verdict only triggers a probe — absence of evidence is not
// evidence of a broken seam.
func Gate(harness, version string, cached *Entry) Decision {
	if cached == nil {
		return Decision{RunProbe: true}
	}
	switch cached.Verdict {
	case VerdictOK:
		return Decision{Allow: true}
	case VerdictBypassed:
		return Decision{Message: refusalMessage(harness, version, "on "+cached.When.Format("2006-01-02"), cached.Detail)}
	default:
		// A verdict this build does not recognise — written by a newer reach,
		// say — is not evidence either way. Re-probe.
		return Decision{RunProbe: true}
	}
}

// GateFromProbe decides from a fresh probe result.
//
// A probe error fails open, deliberately: the probe not running proves nothing
// about the seam, and refusing to launch because reach could not check would
// break every user whose machine cannot run the probe. The warning is loud
// because the operator is now flying without the guard. Only a measured
// "bypassed" — where the danger is known, not hypothetical — refuses.
func GateFromProbe(harness, version string, r Result) Decision {
	switch r.Verdict {
	case VerdictOK:
		return Decision{Allow: true, Message: fmt.Sprintf(
			"reach: %s %s seam verified — shell commands route to the target\n", harness, version)}
	case VerdictBypassed:
		return Decision{Message: refusalMessage(harness, version, "just now", r.Detail)}
	default:
		return Decision{Allow: true, Message: fmt.Sprintf(
			"reach: WARNING: could not verify whether %s's shell is redirected: %s\n"+
				"reach: Launching anyway. If this %s version resolves its shell by absolute\n"+
				"reach: path, every command it runs will execute LOCALLY while the agent\n"+
				"reach: believes it is acting on the target. Run `reach harness verify %s`\n"+
				"reach: to diagnose.\n", harness, r.Detail, harness, harness)}
	}
}

// harnessMessage holds the per-harness prose of the refusal: how the bypass
// works, and what the operator can do instead. These are facts about each
// harness, not derivable from the verdict, so they live in a table.
type harnessMessage struct {
	// whyBypass explains the mechanism in one paragraph.
	whyBypass string
	// remediation lists the operator's options, one per line.
	remediation []string
	// caveat is an extra warning worth repeating even in a refusal.
	caveat string
}

var harnessMessages = map[string]harnessMessage{
	HarnessCodex: {
		whyBypass: "Codex 0.148+ runs its whole tool surface through an exec-server remote\n" +
			"environment, which reach implements (`reach exec-server`). The seam probe measured\n" +
			"the scripted command running somewhere other than the session's target.",
		remediation: []string{
			"Run `reach doctor` to check the session target, then re-probe with:\n      reach harness verify codex",
			"Report the failure with the probe detail above — the seam is measured, so this\n    verdict means the routing broke rather than that the version is unsupported.",
			"Re-run with `reach codex --force` if you accept that every command the agent\n    runs will execute on the local machine.",
		},
	},
	HarnessKimi: {
		whyBypass: "Kimi Code spawns its shell by absolute path instead of by name, so the PATH\n" +
			"shim reach installs can never intercept it.",
		remediation: []string{
			"Run kimi on the target itself, without reach in between.",
			"Track upstream and re-check after upgrading with:\n      reach harness verify kimi",
			"Re-run with `reach kimi --force` if you accept that every command the agent\n    runs will execute on the local machine.",
		},
		// Even a *working* shell seam leaves Kimi's native file tools acting
		// locally; a refusal that omits this would let the operator walk into
		// the subtler version of the same trap.
		caveat: "Note: Kimi's native read_file, write_file and multi_edit tools act on the\n" +
			"LOCAL filesystem even when the shell seam works — use shell commands for file\n" +
			"access.",
	},
}

// refusalMessage is the fail-closed message for a version whose seam is
// measured broken. It says what happens, how reach knows, and what the
// operator's options are, because a refusal without a route forward reads as
// the tool being broken rather than as the tool refusing to break the operator.
func refusalMessage(harness, version, when, detail string) string {
	msg, ok := harnessMessages[harness]
	if !ok {
		msg = harnessMessage{
			whyBypass: "This harness spawns its shell by absolute path instead of by name,\n" +
				"so the PATH shim reach installs can never intercept it.",
			remediation: []string{
				fmt.Sprintf("Re-run with `reach %s --force` if you accept that every command the agent\n    runs will execute on the local machine.", harness),
			},
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "reach: refusing to launch %s %s: this version bypasses reach's shell redirection.\n\n", harness, version)
	b.WriteString(msg.whyBypass + "\n")
	b.WriteString("Everything it ran would execute on THIS machine while the agent believed —\n" +
		"and reported — that it was acting on the target. That is the failure reach exists\n" +
		"to prevent, so reach will not launch this combination.\n\n")
	fmt.Fprintf(&b, "Verified %s by reach's seam probe", when)
	if detail != "" {
		fmt.Fprintf(&b, ": %s", detail)
	}
	b.WriteString(".\n\nWhat you can do instead:\n")
	for _, r := range msg.remediation {
		b.WriteString("  - " + r + "\n")
	}
	if msg.caveat != "" {
		b.WriteString("\n" + msg.caveat + "\n")
	}
	return b.String()
}
