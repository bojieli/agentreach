// Package sftp speaks the SFTP protocol (version 3) as a client.
//
// waldo implements this rather than importing a library for the same reason it
// shells out to the system ssh: the whole project is a security tool whose
// premise is that it puts nothing on a machine you do not control, and every
// dependency it takes on is a supply chain the operator inherits. Version 3 is
// small, frozen since 2001, and universally what stock OpenSSH speaks — the
// subset waldo needs fits in a few hundred lines it can be responsible for.
//
// SFTP is used here as a *protocol over an existing channel*, never as a mount:
// waldo asks for bytes and gets bytes or an error. Nothing is written to the
// target, and no operation can block indefinitely in uninterruptible sleep the
// way a stalled mount does.
package sftp

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Protocol version. Stock OpenSSH speaks 3; later drafts were never widely
// deployed, and negotiating down is what every real client does.
const protocolVersion = 3

// Packet types.
const (
	fxpInit     = 1
	fxpVersion  = 2
	fxpOpen     = 3
	fxpClose    = 4
	fxpRead     = 5
	fxpWrite    = 6
	fxpLstat    = 7
	fxpFstat    = 8
	fxpSetstat  = 9
	fxpOpendir  = 11
	fxpReaddir  = 12
	fxpRemove   = 13
	fxpMkdir    = 14
	fxpRmdir    = 15
	fxpRealpath = 16
	fxpStat     = 17
	fxpRename   = 18
	fxpReadlink = 19

	fxpStatus = 101
	fxpHandle = 102
	fxpData   = 103
	fxpName   = 104
	fxpAttrs  = 105
)

// Status codes.
const (
	statusOK                = 0
	statusEOF               = 1
	statusNoSuchFile        = 2
	statusPermissionDenied  = 3
	statusFailure           = 4
	statusBadMessage        = 5
	statusNoConnection      = 6
	statusConnectionLost    = 7
	statusOpUnsupported     = 8
	statusInvalidHandleFlag = 9
)

// Open flags.
const (
	flagRead   = 0x00000001
	flagWrite  = 0x00000002
	flagAppend = 0x00000004
	flagCreat  = 0x00000008
	flagTrunc  = 0x00000010
	flagExcl   = 0x00000020
)

// Attribute presence flags.
const (
	attrSize        = 0x00000001
	attrUIDGID      = 0x00000002
	attrPermissions = 0x00000004
	attrACModTime   = 0x00000008
	attrExtended    = 0x80000000
)

// maxPacket bounds a single protocol packet.
//
// A hostile or broken server must not be able to make waldo allocate without
// limit by claiming a four-gigabyte packet. OpenSSH never sends more than a few
// hundred kilobytes, so this is generous by two orders of magnitude and still
// finite.
const maxPacket = 1 << 20

// maxDataChunk is the payload size of one read or write request. 32 KiB is what
// every SFTP server accepts without negotiation; larger chunks are an
// optimisation some servers refuse, and a refusal here would look like a
// corrupt transfer rather than a protocol disagreement.
const maxDataChunk = 32 * 1024

// StatusError is a protocol-level failure reported by the server.
type StatusError struct {
	Code    uint32
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("sftp: %s (code %d)", e.Message, e.Code)
	}
	return fmt.Sprintf("sftp: %s", statusName(e.Code))
}

// IsNotFound reports whether the server said the path does not exist.
func (e *StatusError) IsNotFound() bool { return e.Code == statusNoSuchFile }

func statusName(code uint32) string {
	switch code {
	case statusOK:
		return "ok"
	case statusEOF:
		return "end of file"
	case statusNoSuchFile:
		return "no such file"
	case statusPermissionDenied:
		return "permission denied"
	case statusFailure:
		return "failure"
	case statusBadMessage:
		return "bad message"
	case statusNoConnection:
		return "no connection"
	case statusConnectionLost:
		return "connection lost"
	case statusOpUnsupported:
		return "operation unsupported"
	case statusInvalidHandleFlag:
		return "invalid handle"
	}
	return fmt.Sprintf("status %d", code)
}

// Attrs is the subset of SFTP file attributes waldo uses.
type Attrs struct {
	Flags       uint32
	Size        uint64
	UID, GID    uint32
	Permissions uint32
	ATime       uint32
	MTime       uint32
}

// HasSize reports whether the server supplied a size.
func (a Attrs) HasSize() bool { return a.Flags&attrSize != 0 }

// HasPermissions reports whether the server supplied a mode.
func (a Attrs) HasPermissions() bool { return a.Flags&attrPermissions != 0 }

// builder assembles a request payload.
type builder struct{ b []byte }

func (w *builder) uint32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *builder) uint64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *builder) bytes(v []byte)  { w.uint32(uint32(len(v))); w.b = append(w.b, v...) }
func (w *builder) string(v string) { w.bytes([]byte(v)) }
func (w *builder) attrs(a Attrs)   { w.uint32(a.Flags); w.optional(a) }
func (w *builder) payload() []byte { return w.b }

func (w *builder) optional(a Attrs) {
	if a.Flags&attrSize != 0 {
		w.uint64(a.Size)
	}
	if a.Flags&attrUIDGID != 0 {
		w.uint32(a.UID)
		w.uint32(a.GID)
	}
	if a.Flags&attrPermissions != 0 {
		w.uint32(a.Permissions)
	}
	if a.Flags&attrACModTime != 0 {
		w.uint32(a.ATime)
		w.uint32(a.MTime)
	}
}

// reader walks a response payload. Every accessor checks bounds: a truncated or
// hostile packet must produce an error, never a panic in a process that is
// holding the operator's credentials.
type reader struct {
	b   []byte
	err error
}

func (r *reader) fail() {
	if r.err == nil {
		r.err = io.ErrUnexpectedEOF
	}
}

func (r *reader) uint32() uint32 {
	if len(r.b) < 4 {
		r.fail()
		return 0
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *reader) uint64() uint64 {
	if len(r.b) < 8 {
		r.fail()
		return 0
	}
	v := binary.BigEndian.Uint64(r.b)
	r.b = r.b[8:]
	return v
}

func (r *reader) bytes() []byte {
	n := r.uint32()
	if r.err != nil {
		return nil
	}
	if uint64(n) > uint64(len(r.b)) {
		r.fail()
		return nil
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}

func (r *reader) string() string { return string(r.bytes()) }

func (r *reader) attrs() Attrs {
	var a Attrs
	a.Flags = r.uint32()
	if a.Flags&attrSize != 0 {
		a.Size = r.uint64()
	}
	if a.Flags&attrUIDGID != 0 {
		a.UID = r.uint32()
		a.GID = r.uint32()
	}
	if a.Flags&attrPermissions != 0 {
		a.Permissions = r.uint32()
	}
	if a.Flags&attrACModTime != 0 {
		a.ATime = r.uint32()
		a.MTime = r.uint32()
	}
	if a.Flags&attrExtended != 0 {
		count := r.uint32()
		// Extensions are skipped rather than rejected: a server is entitled to
		// send them, and refusing to parse a packet because it carried an
		// extension waldo does not use would break perfectly good hosts.
		for i := uint32(0); i < count && r.err == nil; i++ {
			r.bytes()
			r.bytes()
		}
	}
	return a
}
