package fileops

import (
	"context"
	"fmt"

	"github.com/bojieli/waldo/internal/transport"
	"github.com/bojieli/waldo/internal/waldo"
)

// Warner receives a human-readable note about a degradation that happened.
//
// Degradation is the right behaviour — a host that cannot do tier 2 should
// still work — but an invisible degradation is not. The operator finds out that
// file operations quietly got ten times slower by noticing their agent feels
// sluggish, which is exactly the class of failure this project refuses
// elsewhere. Every fallback goes through here so it can be printed.
type Warner func(string)

// Selection records what a tier request actually produced.
type Selection struct {
	// Requested is the tier that was asked for.
	Requested waldo.Tier
	// Effective is the tier that was actually built.
	Effective waldo.Tier
	// Reason explains a difference between the two, and is empty when there is
	// none.
	Reason string
	// Ops is the strategy itself.
	Ops FileOps
}

// Degraded reports whether the effective tier is below the requested one.
func (s Selection) Degraded() bool { return s.Effective < s.Requested }

// New builds the file-operation strategy for a tier.
//
// pinned separates an operator's explicit --fileops choice from
// autonegotiation's own. A pinned tier that cannot be built is an error: giving
// an operator something other than what they asked for, while reporting the
// tier they asked for, is precisely the silent wrong-behaviour this project
// exists to make impossible. An autonegotiated tier steps down to the next one
// that works and reports it through warn.
func New(ctx context.Context, tier waldo.Tier, t transport.Transport, caps *Capabilities, pinned bool, warn Warner) (Selection, error) {
	if caps == nil {
		return Selection{}, fmt.Errorf("file operations need a capability probe; run `waldo up` again to refresh this session")
	}
	sel := Selection{Requested: tier}

	for try := tier; ; try-- {
		ops, err := build(ctx, try, t, caps)
		if err == nil {
			sel.Effective = ops.Tier()
			sel.Ops = ops
			return sel, nil
		}
		if pinned {
			return Selection{}, fmt.Errorf(
				"target cannot support --fileops=%s: %w\n"+
					"Run `waldo doctor` to see which tiers this host qualifies for, or omit\n"+
					"--fileops to let waldo negotiate the best one it can prove works.", try, err)
		}
		if try == waldo.TierPOSIX {
			// Tier 0 needs only a POSIX shell. If it cannot be built there is
			// nothing left to fall back to, and pretending otherwise would
			// hand the caller a strategy that fails on first use.
			return Selection{}, fmt.Errorf("no usable file-operation strategy for this target: %w", err)
		}
		sel.Reason = fmt.Sprintf("%s unavailable (%v); using %s", try, err, try-1)
		if warn != nil {
			warn("waldo: " + sel.Reason)
		}
	}
}

// build constructs exactly one tier, with no fallback.
func build(ctx context.Context, tier waldo.Tier, t transport.Transport, caps *Capabilities) (FileOps, error) {
	base := NewPOSIX(t, caps)
	switch tier {
	case waldo.TierPOSIX:
		if caps.Base64Decode == "" || caps.Base64Encode == "" {
			return nil, fmt.Errorf("no base64 or openssl on the target")
		}
		return base, nil
	case waldo.TierSFTP:
		return NewSFTP(ctx, t, base)
	case waldo.TierPipe:
		if !caps.Python3 {
			return nil, fmt.Errorf("no python3 on the target")
		}
		return NewPipe(ctx, t, base)
	case waldo.TierAgent:
		return NewAgent(ctx, t, base, caps)
	}
	return nil, fmt.Errorf("unknown tier %v", tier)
}
