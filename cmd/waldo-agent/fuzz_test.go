package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

// FuzzReadFrame feeds arbitrary bytes to the agent's framing layer.
//
// The agent runs on someone else's machine with no supervisor. A panic here is
// not a crash report — it is the session's entire file-access capability
// disappearing mid-task, with nothing to restart it.
func FuzzReadFrame(f *testing.F) {
	var good []byte
	hdr := []byte(`{"id":1,"op":"ping"}`)
	good = binary.BigEndian.AppendUint32(good, uint32(len(hdr)))
	good = append(good, hdr...)
	good = binary.BigEndian.AppendUint32(good, 0)
	f.Add(good)
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255})
	f.Add([]byte{0, 0, 0, 2, '{', '}'})

	f.Fuzz(func(t *testing.T, data []byte) {
		req, payload, err := readFrame(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return
		}
		if len(payload) > maxFrame {
			t.Fatalf("accepted a %d-byte payload above the %d-byte limit", len(payload), maxFrame)
		}
		_ = req
	})
}
