package execserver

import (
	"encoding/json"
	"sync"
)

// A sequencer keeps requests that address the same thing in the order the
// client sent them.
//
// Requests are dispatched concurrently here, and they have to be: process/read
// long-polls while other requests must still be answered, so a serial dispatch
// would deadlock against unified_exec sessions. But concurrency is not the same
// as arbitrary order. A client that sends fs/writeFile and then fs/readFile for
// one path without waiting for the first response has stated an order, and
// answering the read from before the write is a wrong answer, not a slow one.
//
// A mutex per path would not fix this. Mutual exclusion says the two never run
// at once; it says nothing about which goes first, so the read could still win.
// Order has to be taken where the order is known — in the loop reading the
// connection — and that is what enter does. Each request takes the previous
// one's completion channel as its predecessor and installs its own as the new
// tail, so a queue forms in wire order and each waiter blocks on exactly one
// other request.
//
// Only requests that address the same thing queue. Different paths, different
// processes and different handles proceed in parallel, which is what the
// handler pool exists to make worthwhile.
type sequencer struct {
	mu    sync.Mutex
	tails map[string]chan struct{}
}

func newSequencer() *sequencer {
	return &sequencer{tails: map[string]chan struct{}{}}
}

// slot is one request's place in the queue for its key. A nil slot is a request
// that needs no ordering, and every method here tolerates one.
type slot struct {
	q     *sequencer
	key   string
	after chan struct{}
	own   chan struct{}
}

// enter reserves this request's place.
//
// It must be called from the goroutine reading the connection, before the
// request is dispatched. Called from the dispatched goroutine it would record
// the order the scheduler happened to produce, which is the thing it exists to
// stop mattering.
func (q *sequencer) enter(key string) *slot {
	if key == "" {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	own := make(chan struct{})
	s := &slot{q: q, key: key, after: q.tails[key], own: own}
	q.tails[key] = own
	return s
}

// wait blocks until the request in front of this one has been answered.
func (s *slot) wait() {
	if s == nil || s.after == nil {
		return
	}
	<-s.after
}

// done releases whatever is queued behind this request.
func (s *slot) done() {
	if s == nil {
		return
	}
	s.q.mu.Lock()
	// Still the tail means nothing queued behind us, so the key can go rather
	// than accumulating one entry per path an agent ever touched.
	if s.q.tails[s.key] == s.own {
		delete(s.q.tails, s.key)
	}
	s.q.mu.Unlock()
	close(s.own)
}

// orderKey names what a request addresses, or "" for one that needs no
// ordering.
//
// process/read is deliberately absent. It long-polls, so queueing anything
// behind it would stall that process's writes for the length of the poll, and
// it changes nothing — ordering two reads of a stream that is explicitly
// addressed by sequence number buys nothing. process/signal and
// process/terminate are absent for the opposite reason: they are the escape
// hatch, and an escape hatch queued behind a write that is blocked because the
// process stopped reading is no escape at all.
//
// fs/walk is absent because it addresses a tree rather than a path, and a
// traversal is not something a client can expect to be atomic in the first
// place.
func orderKey(method string, params json.RawMessage) string {
	var field, prefix string
	switch method {
	case "fs/readFile", "fs/writeFile", "fs/createDirectory", "fs/getMetadata",
		"fs/canonicalize", "fs/readDirectory", "fs/remove":
		field, prefix = "path", "path:"
	case "fs/copy":
		// The destination is what another request could observe. Two copies
		// out of one source do not interfere.
		field, prefix = "destinationPath", "path:"
	case "fs/open", "fs/readBlock", "fs/close":
		// Keyed on the handle rather than the path, because at this point the
		// handle may not have been registered yet — which is exactly the race
		// worth closing: a readBlock overtaking its own open is answered
		// "unknown handle".
		field, prefix = "handleId", "handle:"
	case "process/write":
		// Two chunks written to one stdin in the order the client sent them.
		// Reordering these corrupts the input of anything interactive.
		field, prefix = "processId", "process:"
	default:
		return ""
	}
	value := stringField(params, field)
	if value == "" {
		return ""
	}
	return prefix + value
}

// stringField pulls one string out of a params object without committing to a
// shape. The handler parses these properly; this only needs enough to know what
// the request is about, and a request it cannot read is one it does not order.
func stringField(raw json.RawMessage, name string) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	value, ok := fields[name]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return ""
	}
	return s
}
