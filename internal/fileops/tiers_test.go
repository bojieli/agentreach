package fileops_test

import (
	"context"
	"os"
	"testing"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/fileops/fileopstest"
	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
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
		tier waldo.Tier
	}{
		{"posix", waldo.TierPOSIX},
		{"pipe", waldo.TierPipe},
		{"helper", waldo.TierHelper},
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
					if tc.tier == waldo.TierHelper && os.Getenv("CI") == "" {
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
// `waldo status` reporting something untrue.
func TestPinnedTierIsNeverSubstituted(t *testing.T) {
	tr := localTransport(t)
	caps, err := fileops.Probe(context.Background(), tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	// A tier the target cannot support must fail rather than hand back tier 0.
	// The helper tier needs a platform waldo has a build for; an unknown uname
	// is exactly the case where a pin cannot be honoured.
	impossible := *caps
	impossible.Uname = "Plan9 unknown-arch"
	if _, err := fileops.New(context.Background(), waldo.TierHelper, tr, &impossible, true, nil); err == nil {
		t.Fatal("pinning an impossible tier succeeded; it must fail loudly")
	}
}

// TestAutonegotiationDegradesVisibly is the other half: when waldo chose the
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
	sel, err := fileops.New(context.Background(), waldo.TierPipe, tr, &degraded, false,
		func(msg string) { warnings = append(warnings, msg) })
	if err != nil {
		t.Fatalf("autonegotiation should have fallen back, not failed: %v", err)
	}
	defer func() { _ = sel.Ops.Close() }()

	if sel.Effective >= waldo.TierPipe {
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
