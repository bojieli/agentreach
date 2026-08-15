package main

import (
	"bytes"
	"io"
	"strings"
)

// cwdCapturingWriter passes stderr through while extracting and removing the
// working-directory line waldo appends to every command.
//
// The directory is reported on stderr rather than stdout because stdout is the
// command's own data: a file read, a JSON payload, a diff. Appending anything
// to it would corrupt the very content the agent asked for. stderr is already
// diagnostic, and the marker line is stripped before the agent sees it.
type cwdCapturingWriter struct {
	out      io.Writer
	marker   string
	pending  bytes.Buffer
	captured string
}

func (w *cwdCapturingWriter) Write(p []byte) (int, error) {
	w.pending.Write(p)
	// Emit only complete lines, so a marker split across two reads is never
	// half-forwarded to the user.
	for {
		data := w.pending.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx+1])
		w.pending.Next(idx + 1)
		if rest, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), w.marker); ok {
			w.captured = strings.TrimSpace(rest)
			continue
		}
		if _, err := w.out.Write([]byte(line)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Captured returns the working directory the command ended in, and flushes any
// trailing partial line.
func (w *cwdCapturingWriter) Captured() string {
	if w.pending.Len() > 0 {
		rest := w.pending.String()
		w.pending.Reset()
		if v, ok := strings.CutPrefix(strings.TrimRight(rest, "\r\n"), w.marker); ok {
			w.captured = strings.TrimSpace(v)
		} else {
			_, _ = w.out.Write([]byte(rest))
		}
	}
	return w.captured
}
