//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/bojieli/agentreach/internal/fileops"
	"github.com/bojieli/agentreach/internal/fileops/fileopstest"
	"github.com/bojieli/agentreach/internal/reach"
	"github.com/bojieli/agentreach/internal/transport"
)

// TestTierConformanceOverSSH runs the shared file-operation suite against every
// tier over a real SSH connection.
//
// This is where the interchangeability claim is actually settled. The unit
// tests run the same suite over the local transport, which cannot cover ssh's
// second round of shell interpretation, nor whether the link is 8-bit clean —
// which is what decides whether file content is base64-framed.
func TestTierConformanceOverSSH(t *testing.T) {
	for _, tier := range []reach.Tier{
		reach.TierPOSIX, reach.TierPipe, reach.TierHelper,
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

			fileopstest.Run(t, root, func(*testing.T) fileops.FileOps { return sel.Ops })
		})
	}
}

// TestTiersAgreeByteForByte pins the property that makes tiers substitutable:
// the same file written through one tier must read back identically through
// every other. A disagreement here would mean an operator's results depend on
// which tier happened to be negotiated, which is precisely the invisible
// wrong-answer failure reach is built to prevent.
func TestTiersAgreeByteForByte(t *testing.T) {
	tr := newTransport(t)
	ctx := context.Background()
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	tiers := []reach.Tier{reach.TierPOSIX, reach.TierPipe, reach.TierHelper}
	ops := map[reach.Tier]fileops.FileOps{}
	for _, tier := range tiers {
		sel, err := fileops.New(ctx, tier, tr, caps, true, nil)
		if err != nil {
			t.Fatalf("build tier %s: %v", tier, err)
		}
		t.Cleanup(func() { _ = sel.Ops.Close() })
		ops[tier] = sel.Ops
	}

	dir := filepath.Join(workspace, "cross-tier")
	if err := ops[reach.TierPOSIX].Mkdir(ctx, dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ops[reach.TierPOSIX].Remove(context.Background(), dir, true) })

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
			if want, _ := ops[reach.TierPOSIX].Hash(ctx, p); digest != want {
				t.Errorf("%s hash = %s, but tier posix says %s", reader, digest, want)
			}
		}
	}
}

// TestWorksWithoutMultiplexing exercises the connection path Windows is
// permanently on.
//
// Win32-OpenSSH does not implement ControlMaster, so reach there opens and
// authenticates a connection per command. That is a different code path — no
// control socket, no master to tear down, a fresh authentication every time —
// and it cannot be exercised on the platform where it matters, because this
// suite cannot run a Windows target. Disabling multiplexing on Unix runs the
// identical path against a real sshd, which is the closest thing to evidence
// available without a Windows machine in the loop.
func TestWorksWithoutMultiplexing(t *testing.T) {
	tr, err := transport.NewSSH(transport.SSHConfig{
		Host: sshHostAlias, BatchMode: true, Multiplex: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx := context.Background()

	// Several commands in sequence: each one is its own connection, and the
	// exit-status protocol has to survive that just as it does on a shared one.
	for i, tc := range []struct {
		command string
		code    int
	}{
		{"echo one", 0},
		{"exit 3", 3},
		{"exit 255", 255}, // ssh's own failure code, which must not be confused
	} {
		res, err := tr.Run(ctx, reach.ExecRequest{Command: tc.command})
		if err != nil {
			t.Fatalf("command %d (%q) failed without multiplexing: %v", i, tc.command, err)
		}
		if res.Code != tc.code {
			t.Errorf("command %q exited %d, want %d", tc.command, res.Code, tc.code)
		}
	}

	// File operations too: the tier stack must not assume a shared connection.
	caps, err := fileops.Probe(ctx, tr)
	if err != nil {
		t.Fatalf("probe without multiplexing: %v", err)
	}
	sel, err := fileops.New(ctx, caps.BestTier(), tr, caps, false, nil)
	if err != nil {
		t.Fatalf("build tier without multiplexing: %v", err)
	}
	t.Cleanup(func() { _ = sel.Ops.Close() })

	p := filepath.Join(workspace, "no-mux.bin")
	content := []byte("no multiplexing\x00\xff\r\n")
	if err := sel.Ops.Write(ctx, p, content, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := sel.Ops.Read(ctx, p, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("round trip without multiplexing: got % x, want % x", got, content)
	}

	// Closing must be clean even though there is no master to shut down.
	if err := tr.Close(); err != nil {
		t.Errorf("Close without a master: %v", err)
	}
}
