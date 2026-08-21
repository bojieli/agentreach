// Package harnessprobe verifies, offline and without an API key, that a
// harness's shell tool calls actually travel through waldo's PATH shim to the
// session's target.
//
// The question matters because the failure is invisible from inside the
// harness: Codex ≥ 0.148 resolves the login shell from the account database
// (getpwuid_r) and spawns it by absolute path — /bin/zsh -lc, never touching
// PATH — so the shim directory waldo prepends is decoration. Every command the
// agent runs then executes on the operator's own machine while the agent
// believes, and reports, that it acted on the target. There is no flag,
// config key or hook in those versions that changes where the shell comes
// from; the only honest answer is to observe where a command actually ran and
// refuse to launch when the answer is "locally".
//
// The observation is done with a scripted conversation, not by reading codex's
// source: an embedded mock model server (StartMock) speaks just enough of the
// Responses API to instruct one shell tool call — "echo <canary>; hostname" —
// and captures the output the harness reports back. The canary proves the
// scripted command really executed; the hostname proves where. Comparing that
// hostname against the target's, obtained through the session's own transport,
// yields the verdict.
//
// Verdicts are cached per harness version (verdictCache) so the guard on
// `waldo codex` pays the probe's cost once per installed codex version, and
// the guard's decision is a pure function (Gate, GateFromProbe) so the whole
// matrix — ok, bypassed, unverified, forced, probe-error — is testable without
// a codex binary.
package harnessprobe

// Verdicts the seam probe can reach.
const (
	// VerdictOK means the scripted command ran on the session's target: the
	// harness's shell is intercepted by waldo's PATH shim.
	VerdictOK = "ok"

	// VerdictBypassed means the scripted command ran somewhere that is not
	// the target — the local machine, in practice. The harness resolves its
	// shell in a way the PATH shim cannot intercept, and launching it under
	// waldo would run the model's commands on the operator's own machine.
	VerdictBypassed = "bypassed"

	// VerdictError means the probe could not reach a conclusion: codex never
	// made the scripted tool call, would not start, or the run timed out.
	// An error is a statement about this machine at probe time — a missing
	// flag, a hung process — not about the codex version, so it is never
	// cached: caching it would turn one bad afternoon into a permanent
	// "unverifiable" for a version that never got a fair run.
	VerdictError = "error"
)

// Result is the outcome of one probe run.
type Result struct {
	Verdict string
	// Detail explains the verdict in one line: the observed hostname, or the
	// reason no verdict could be reached.
	Detail string
	// ToolOutput is the trimmed output the harness reported for the scripted
	// tool call, when one was observed. It is the raw evidence behind the
	// verdict and worth surfacing when the verdict is "bypassed".
	ToolOutput string
}

// Conclusive reports whether the verdict is a fact about the harness version
// (ok or bypassed) rather than a transient failure of the probe itself.
func (r Result) Conclusive() bool {
	return r.Verdict == VerdictOK || r.Verdict == VerdictBypassed
}
