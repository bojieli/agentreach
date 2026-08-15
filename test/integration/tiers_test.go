//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/waldo/internal/fileops"
	"github.com/bojieli/waldo/internal/fileops/fileopstest"
	"github.com/bojieli/waldo/internal/waldo"
)

// TestTierConformanceOverSSH runs the shared file-operation suite against every
// tier over a real SSH connection.
//
// This is where the interchangeability claim is actually settled. The unit
// tests run the same suite over the local transport, which cannot cover ssh's
// second round of shell interpretation, and cannot cover the SFTP tier at all
// because a local shell has no subsystem channel.
func TestTierConformanceOverSSH(t *testing.T) {
	for _, tier := range []waldo.Tier{
		waldo.TierPOSIX, waldo.TierSFTP, waldo.TierPipe, waldo.TierAgent,
	} {
		t.Run(tier.String(), func(t *testing.T) {
			tr := newTransport(t)
			ctx := context.Background()
			caps, err := fileops.Probe(ctx, tr)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if ok, why := caps.Qualifies(tier); !ok {
				t.Fatalf("this target does not qualify for tier %s: %s\n"+
					"The integration suite exists to test every tier; a tier that cannot run "+
					"here is a gap in coverage, not a reason to skip.", tier, why)
			}
			sel, err := fileops.New(ctx, tier, tr, caps, true, nil)
			if err != nil {
				t.Fatalf("build tier %s: %v", tier, err)
			}
			t.Cleanup(func() { _ = sel.Ops.Close() })
			if sel.Effective != tier {
				t.Fatalf("pinned tier %s was substituted with %s", tier, sel.Effective)
			}

			root := filepath.Join(workspace, "conformance-"+tier.String())
			if err := sel.Ops.Mkdir(ctx, root, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", root, err)
			}
			t.Cleanup(func() { _ = sel.Ops.Remove(context.Background(), root, true) })

			fileopstest.Run(t, root, func(t *testing.T) fileops.FileOps { return sel.Ops })
		})
	}
}

// TestTiersAgreeByteForByte pins the property that makes tiers substitutable:
// the same file written through one tier must read back identically through
// every other. A disagreement here would mean an operator's results depend on
// which tier happened to be negotiated, which is precisely the invisible
// wrong-answer failure waldo is built to prevent.
func TestTiersAgreeByteForByte(t *testing.T) {
	tr := newTransport(t)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	tiers := []waldo.Tier{waldo.TierPOSIX, waldo.TierSFTP, waldo.TierPipe, waldo.TierAgent}
	ops := map[waldo.Tier]fileops.FileOps{}
	for _, tier := range tiers {
		sel, err := fileops.New(ctx, tier, tr, caps, true, nil)
		if err != nil {
			t.Fatalf("build tier %s: %v", tier, err)
		}
		t.Cleanup(func() { _ = sel.Ops.Close() })
		ops[tier] = sel.Ops
	}

	dir := filepath.Join(workspace, "cross-tier")
	if err := ops[waldo.TierPOSIX].Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ops[waldo.TierPOSIX].Remove(context.Background(), dir, true) })

	content := []byte("cross-tier\x00\xff\xfe\r\n世界\n")
	for _, writer := range tiers {
		p := filepath.Join(dir, "by-"+writer.String())
		if err := ops[writer].Write(ctx, p, content, 0o644); err != nil {
			t.Fatalf("%s write: %v", writer, err)
		}
		for _, reader := range tiers {
			got, err := ops[reader].Read(ctx, p, 0, 0)
			if err != nil {
				t.Fatalf("%s read of a file written by %s: %v", reader, writer, err)
			}
			if string(got) != string(content) {
				t.Errorf("%s read of a %s write: got % x, want % x", reader, writer, got, content)
			}
			digest, err := ops[reader].Hash(ctx, p)
			if err != nil {
				t.Fatalf("%s hash: %v", reader, err)
			}
			if want, _ := ops[waldo.TierPOSIX].Hash(ctx, p); digest != want {
				t.Errorf("%s hash = %s, but tier posix says %s", reader, digest, want)
			}
		}
	}
}
