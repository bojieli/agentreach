package waldo

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path"
	"sync/atomic"
)

// Atomic writes, and the one rule they all depend on.
//
// Every tier replaces a file the same way: write the new content to a temporary
// name in the *same directory*, then rename it over the destination. Same
// directory because POSIX only guarantees rename is atomic within one
// filesystem; rename because a reader must see the old file or the new one and
// never a half-written one, and a failed transfer must leave the original
// untouched.
//
// That idiom needs a temporary name, and the rule the name has to satisfy is
// easy to state and easy to get wrong:
//
//	Two waldo processes writing to one directory at the same time must never
//	choose the same temporary name.
//
// *Processes*, not goroutines. waldo runs one process per tool call and a
// harness issues tool calls in parallel, so concurrent writers are the ordinary
// case rather than an edge one. A tier since removed learned this the hard way: it
// numbered temporaries from a per-client counter that restarted at 1 in every
// process, so parallel writers all chose `.waldo.tmp.1` and the exclusive
// create refused all but one — three of eight writes failing, against a real
// host, with an error the operator could neither act on nor reproduce.
//
// The rule now lives here instead of being reinvented per tier. Randomness is
// what actually satisfies it: a process id is not enough, because the operator's
// pid says nothing about a remote machine and two containers can hold the same
// one. The counter only keeps names apart within a process, cheaply.
//
// The tiers that run *on* the target — the pipe handler and the agent — build
// the same shape from their own pid, which is genuinely unique there. They
// cannot import this package, so the contract is what they share, not the code.
var tempSeq atomic.Uint64

// TempPath returns a temporary path in dir for an atomic write.
//
// The `.waldo.tmp.` prefix is deliberate: anything left behind by an
// interrupted write is immediately identifiable as waldo's, and `waldo doctor`
// can say so rather than leaving an operator to wonder.
func TempPath(dir string) string {
	var r [8]byte
	n := tempSeq.Add(1)
	if _, err := rand.Read(r[:]); err != nil {
		// Randomness failing is not a reason to refuse a write. The counter and
		// the nanosecond-scale arrival time still separate this from most
		// others, and the caller retries on collision regardless.
		return path.Join(dir, fmt.Sprintf(".waldo.tmp.%d", n))
	}
	return path.Join(dir, fmt.Sprintf(".waldo.tmp.%s.%d", hex.EncodeToString(r[:]), n))
}

// TempAttempts is how many times a tier should re-draw a temporary name before
// giving up.
//
// Correct naming makes a collision vanishingly unlikely; retrying makes it
// invisible even when the naming is imperfect — a new tier, a target whose
// clock is stuck, a filesystem that truncates long names. A collision should
// cost microseconds, not an error the operator has to interpret.
const TempAttempts = 3
