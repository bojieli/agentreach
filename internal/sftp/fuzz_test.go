package sftp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Fuzzing belongs here more than almost anywhere else in waldo: every byte this
// package parses comes from the other end of a connection, in a process holding
// the operator's credentials. A malformed packet must produce an error — never a
// panic, and never a plausible-looking wrong answer.

// FuzzReadPacket feeds arbitrary bytes to the framing layer.
func FuzzReadPacket(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, fxpVersion})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{255, 255, 255, 255, 1, 2, 3})
	f.Add([]byte{0, 0, 0, 5, fxpStatus, 0, 0, 0, 2})

	f.Fuzz(func(t *testing.T, data []byte) {
		typ, payload, err := readPacket(bytes.NewReader(data))
		if err != nil {
			return
		}
		// A packet that parsed must be within the declared bound; anything else
		// means the size check can be walked past.
		if len(payload)+1 > maxPacket {
			t.Fatalf("accepted a %d-byte packet above the %d-byte limit", len(payload)+1, maxPacket)
		}
		_ = typ
	})
}

// FuzzAttrs feeds arbitrary bytes to the attribute parser, which walks
// length-prefixed fields whose counts the server chooses.
func FuzzAttrs(f *testing.F) {
	var b builder
	b.attrs(Attrs{Flags: attrSize | attrPermissions, Size: 4096, Permissions: 0o100644})
	f.Add(b.payload())
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{128, 0, 0, 0, 255, 255, 255, 255}) // extended, with an absurd count

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &reader{b: data}
		a := r.attrs()
		// Whatever it decodes, a size must never come back negative through the
		// clamp — a negative length changes the meaning of every read using it.
		if ClampSize(a.Size) < 0 {
			t.Fatalf("clamped size is negative for %d", a.Size)
		}
	})
}

// FuzzStatusResponse covers the response every request can receive.
func FuzzStatusResponse(f *testing.F) {
	payload := make([]byte, 0, 16)
	payload = binary.BigEndian.AppendUint32(payload, statusNoSuchFile)
	payload = binary.BigEndian.AppendUint32(payload, 3)
	payload = append(payload, 'a', 'b', 'c')
	f.Add(payload)
	f.Add([]byte{})

	// The assertion is the absence of a panic: statusFrom walks two
	// length-prefixed strings whose lengths the server chose.
	f.Fuzz(func(_ *testing.T, data []byte) {
		_ = statusFrom(data)
	})
}
