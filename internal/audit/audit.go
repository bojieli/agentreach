// Package audit records what waldo did on a target.
//
// This exists because of the situation waldo is built for. You have pointed an
// autonomous agent at a machine you do not own — a client's server, a shared
// box, a production host someone else administers — and at some point somebody
// will ask what it did there. Without a record the honest answer is "I don't
// know", which is not an answer anyone can accept about a production host.
//
// The log is deliberately local. It is evidence for the operator, not telemetry:
// nothing is sent anywhere, and it is written to a file only the operator can
// read.
//
// It is also deliberately not a security control. A record of what waldo was
// asked to do is not a record of everything that happened on the target, and a
// compromised target can do things waldo never sees. It answers "what did my
// agent run", which is the question that actually gets asked.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one recorded action.
type Entry struct {
	Time    time.Time `json:"time"`
	Session string    `json:"session"`
	Target  string    `json:"target"`
	// Action is "exec", "read", "write", "remove", "rename" or "mkdir".
	Action string `json:"action"`
	// Command is the shell command sent to the target, for exec actions.
	Command string `json:"command,omitempty"`
	// Dir is the working directory the command ran in.
	Dir string `json:"dir,omitempty"`
	// Path is the file acted on, for file actions.
	Path string `json:"path,omitempty"`
	// Bytes is how much content moved, where that is meaningful.
	Bytes int `json:"bytes,omitempty"`
	// Code is the exit status, for exec actions.
	Code int `json:"code"`
	// Millis is how long it took.
	Millis int64 `json:"ms,omitempty"`
	// Error is set when the action failed outright.
	Error string `json:"error,omitempty"`
}

// DisableEnv turns the log off. It exists because a record of every command is
// occasionally the wrong thing to keep — a shared machine, a command line that
// will contain a secret — and that judgement belongs to the operator.
const DisableEnv = "WALDO_NO_AUDIT"

// maxField bounds any single recorded string.
//
// It keeps a record under the 4 KiB that POSIX guarantees an O_APPEND write
// delivers atomically, which is what lets concurrent tool calls append to one
// file without interleaving into corruption. It is a bound on the *record*, not
// on what waldo will run: a command too long to record in full is truncated in
// the log and executed in full.
const maxField = 1500

var warnOnce sync.Once

// Append records one action. It never returns an error, because a failure to
// write the log must not be able to fail the operation it is describing.
func Append(dir, sessionName string, e Entry) {
	if os.Getenv(DisableEnv) != "" {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.Session = sessionName
	e.Command = clip(e.Command)
	e.Path = clip(e.Path)
	e.Dir = clip(e.Dir)
	e.Error = clip(e.Error)

	line, err := json.Marshal(e)
	if err != nil {
		warn(err)
		return
	}

	f, err := os.OpenFile(Path(dir, sessionName), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		warn(err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(line, '\n')); err != nil {
		warn(err)
	}
}

// warn reports a broken audit log once per process.
//
// Silence would be wrong: an audit log that quietly stopped recording is worse
// than none at all, because the operator would still believe they had one.
// Repeating it once per command would be noise inside an agent's turn.
func warn(err error) {
	warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "waldo: cannot write the audit log (%v); actions are not being recorded\n", err)
	})
}

func clip(s string) string {
	if len(s) <= maxField {
		return s
	}
	return s[:maxField] + fmt.Sprintf("...[%d more bytes]", len(s)-maxField)
}

// Path is where a session's log lives.
func Path(dir, sessionName string) string {
	return filepath.Join(dir, sessionName+".audit.jsonl")
}

// Read returns the most recent entries, oldest first. limit <= 0 means all.
func Read(dir, sessionName string, limit int) ([]Entry, error) {
	data, err := os.ReadFile(Path(dir, sessionName))
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	out := make([]Entry, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			continue
		}
		var e Entry
		// A truncated final line — a process killed mid-write — is skipped
		// rather than allowed to fail the whole read. The rest of the record is
		// still evidence.
		if json.Unmarshal([]byte(l), &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// openForAppend is used by the tests to simulate a record cut short by a
// process that died mid-write.
func openForAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}
