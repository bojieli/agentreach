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
		if _, err := w.consumeLine(line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// consumeLine handles one line, extracting the marker wherever it appears.
//
// The marker is searched for anywhere in the line, not just at its start. A
// command whose stderr does not end in a newline would otherwise have the
// marker appended directly onto its last line — which both leaks waldo's
// bookkeeping into the agent's view of stderr and loses the working directory,
// so `cd` would stop persisting for exactly those commands.
func (w *cwdCapturingWriter) consumeLine(line string) (bool, error) {
	idx := strings.Index(line, w.marker)
	if idx < 0 {
		_, err := w.out.Write([]byte(line))
		return false, err
	}
	w.captured = strings.TrimSpace(strings.TrimRight(line[idx+len(w.marker):], "\r\n"))
	if idx > 0 {
		// Preserve whatever the command wrote before the marker.
		if _, err := w.out.Write([]byte(line[:idx])); err != nil {
			return true, err
		}
	}
	return true, nil
}

// Captured returns the working directory the command ended in, and flushes any
// trailing partial line.
func (w *cwdCapturingWriter) Captured() string {
	if w.pending.Len() > 0 {
		rest := w.pending.String()
		w.pending.Reset()
		_, _ = w.consumeLine(rest)
	}
	return w.captured
}
