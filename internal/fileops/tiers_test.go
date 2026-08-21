package fileops_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/fileops/fileopstest"
	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/transport"
)

// TestTierConformance runs the shared suite against every tier reachable
// without a network.
//
// The local transport is a real transport, not a mock: the same shell quoting,
// the same exit-status protocol, the same base64 framing. What it cannot cover
// is ssh's second round of shell interpretation, which is what test/integration
// exists for.
func TestTierConformance(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier reach.Tier
	}{
		{"posix", reach.TierPOSIX},
		{"pipe", reach.TierPipe},
		{"helper", reach.TierHelper},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fileopstest.Run(t, root, func(t *testing.T) fileops.FileOps {
				tr := localTransport(t)
				caps, err := fileops.Probe(context.Background(), tr)
				if err != nil {
					t.Fatalf("probe: %v", err)
				}
				if ok, why := caps.Qualifies(tc.tier); !ok {
					t.Skipf("this machine does not qualify for tier %s: %s", tc.tier, why)
				}
				sel, err := fileops.New(context.Background(), tc.tier, tr, caps, true, nil)
				if err != nil {
					if tc.tier == reach.TierHelper && os.Getenv("CI") == "" {
						t.Skipf("helper tier unavailable here: %v", err)
					}
					t.Fatalf("build tier %s: %v", tc.tier, err)
				}
				if sel.Effective != tc.tier {
					t.Fatalf("asked for tier %s, got %s — a pinned tier must never be substituted",
						tc.tier, sel.Effective)
				}
				t.Cleanup(func() { _ = sel.Ops.Close() })
				return sel.Ops
			})
		})
	}
}

// TestPinnedTierIsNeverSubstituted is the honesty property: an operator who
// names a tier gets that tier or an error, never a quiet downgrade that leaves
// `reach status` reporting something untrue.
func TestPinnedTierIsNeverSubstituted(t *testing.T) {
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// A tier the target cannot support must fail rather than hand back tier 0.
	// The helper tier needs a platform reach has a build for; an unknown uname
	// is exactly the case where a pin cannot be honoured.
	impossible := *caps
	impossible.Uname = "Plan9 unknown-arch"
	if _, err := fileops.New(context.Background(), reach.TierHelper, tr, &impossible, true, nil); err == nil {
		t.Fatal("pinning an impossible tier succeeded; it must fail loudly")
	}
}

// TestAutonegotiationDegradesVisibly is the other half: when reach chose the
// tier itself, it may step down — but never silently.
func TestAutonegotiationDegradesVisibly(t *testing.T) {
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// Ask for a tier this target cannot build, without pinning it.
	degraded := *caps
	degraded.Python3 = false
	var warnings []string
	sel, err := fileops.New(context.Background(), reach.TierPipe, tr, &degraded, false,
		func(msg string) { warnings = append(warnings, msg) })
	if err != nil {
		t.Fatalf("autonegotiation should have fallen back, not failed: %v", err)
	}
	defer func() { _ = sel.Ops.Close() }()

	if sel.Effective >= reach.TierPipe {
		t.Fatalf("effective tier = %s, expected a fallback below pipe", sel.Effective)
	}
	if !sel.Degraded() {
		t.Error("Degraded() is false after a fallback")
	}
	if sel.Reason == "" {
		t.Error("a degraded selection carries no reason")
	}
	if len(warnings) == 0 {
		t.Error("degradation happened without warning the operator")
	}
	if sel.Ops.Tier() != sel.Effective {
		t.Errorf("Ops.Tier() = %s but Effective = %s; a strategy must report what it is",
			sel.Ops.Tier(), sel.Effective)
	}
}

// localTransport builds a local:// transport, skipping when this machine cannot
// be a target. A local target needs a POSIX shell, which Windows only has when
// Git for Windows or MSYS2 supplied one — and a Windows machine is never a
// supported target, so skipping is the honest outcome rather than a failure.
func localTransport(t *testing.T) transport.Transport {
	t.Helper()
	tr, err := transport.NewLocal()
	if err != nil {
		t.Skipf("this machine cannot host a local:// target: %v", err)
	}
	return tr
}

// Every tier writes through a same-directory temporary named `.reach.tmp.*`.
// internal/reach/tempfile.go states the rule and explains why it cannot be
// shared as code: the pipe handler is Python and the helper is a separate
// binary, so the prefix is a contract between three implementations rather than
// one constant.
//
// A contract nothing checks is a comment. The pipe handler spelled the prefix
// `.waldo.tmp.` — reach's former name — which made its leftovers unattributable
// to reach and, worse, invisible to the conformance suite's own "nothing may be
// left behind" assertion, which looks for `.reach.tmp.`. That assertion could
// never have failed for the tier reach negotiates by default.
func TestEveryTierUsesTheAgreedTemporaryPrefix(t *testing.T) {
	for _, src := range []string{
		"handler.py",
		"../../cmd/reach-helper/main.go",
	} {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("read %s: %v", src, err)
		}
		body := string(data)
		if !strings.Contains(body, `".reach.tmp.`) {
			t.Errorf("%s does not build its temporary name from the agreed `.reach.tmp.` prefix", src)
		}
		// The former name must not come back, in the handler or anywhere it
		// might be copied from.
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, `".waldo.tmp.`) {
				t.Errorf("%s still writes the pre-rename temporary name: %s", src, strings.TrimSpace(line))
			}
		}
	}
}
