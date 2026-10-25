// Package requestlog writes and reads brout's request log: one JSONL file per
// UTC day, YYYY-MM-DD.jsonl, with one entry per request (D5). Only brout serve
// writes it (D10).
package requestlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/victorarias/brout/internal/server"
)

// Version is the entry version this brout writes. A later brout must read
// every version it ever wrote.
const Version = 1

// Entry is one log line.
type Entry struct {
	V              int             `json:"v"`
	ID             string          `json:"id"`
	Time           time.Time       `json:"time"`
	DurationMS     int64           `json:"duration_ms"`
	RequestedModel *string         `json:"requested_model"` // null when the body did not parse
	Model          *string         `json:"model"`           // null when nothing answered
	Status         int             `json:"status"`
	Attempts       []Attempt       `json:"attempts"`
	ReplayOf       *string         `json:"replay_of"`
	Request        json.RawMessage `json:"request"`  // the decompressed body; a string when it is not JSON
	Response       json.RawMessage `json:"response"` // the body brout answered with

	Raw []byte `json:"-"` // the line as read from the file
}

type Attempt struct {
	Model      string `json:"model"`
	Status     int    `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Writer appends entries to the files in one directory. brout serve holds one.
type Writer struct {
	dir    string
	stderr io.Writer
	mu     sync.Mutex
}

func NewWriter(dir string, stderr io.Writer) *Writer {
	return &Writer{dir: dir, stderr: stderr}
}

// Finish logs one exchange; it is server.Options.Finish. A failed append
// prints to stderr and never fails the request.
func (w *Writer) Finish(ex server.Exchange) {
	if err := w.Append(entryOf(ex)); err != nil {
		fmt.Fprintf(w.stderr, "%s request log: %v\n", ex.ID, err)
	}
}

// Append writes e as one line, with one write call, to the file of e's day.
func (w *Writer) Append(e Entry) error {
	path, err := fileOf(w.dir, e.ID)
	if err != nil {
		return err
	}
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		return fmt.Errorf("encoding the entry: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(line.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func entryOf(ex server.Exchange) Entry {
	e := Entry{
		V:              Version,
		ID:             ex.ID,
		Time:           ex.Start.UTC().Truncate(time.Millisecond),
		DurationMS:     ex.Duration.Milliseconds(),
		RequestedModel: nullable(ex.RequestedModel),
		Model:          nullable(ex.Model),
		Status:         ex.Status,
		Attempts:       []Attempt{},
		ReplayOf:       nullable(ex.ReplayOf),
		Request:        json.RawMessage("null"),
		Response:       ex.Response,
	}
	for _, a := range ex.Attempts {
		e.Attempts = append(e.Attempts, Attempt{Model: a.Model, Status: a.Status, Error: a.Error, DurationMS: a.Duration.Milliseconds()})
	}
	switch {
	case ex.Request == nil:
	case json.Valid(ex.Request):
		e.Request = ex.Request
	default:
		e.Request, _ = json.Marshal(string(ex.Request))
	}
	return e
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// fileOf names the file that holds the entry with id. An id starts with its
// UTC day, so writer and readers agree on one file per id.
func fileOf(dir, id string) (string, error) {
	day, err := time.Parse("20060102", id[:min(len(id), 8)])
	if err != nil {
		return "", fmt.Errorf("%q is not a brout request id, such as 20261025-153012-7f3a9c", id)
	}
	return filepath.Join(dir, day.Format("2006-01-02")+".jsonl"), nil
}

// Find returns the entry with id. It reads the id's file up to that entry, and
// skips lines it cannot read with a note to warn.
func Find(dir, id string, warn io.Writer) (Entry, error) {
	path, err := fileOf(dir, id)
	if err != nil {
		return Entry{}, err
	}
	var found *Entry
	err = scan(path, warn, func(e Entry) bool {
		if e.ID == id {
			found = &e
		}
		return found == nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return Entry{}, fmt.Errorf("no request %s: %s does not exist", id, path)
	}
	if err != nil {
		return Entry{}, err
	}
	if found == nil {
		return Entry{}, fmt.Errorf("no request %s in %s", id, path)
	}
	return *found, nil
}

// Latest returns up to n entries, newest first. It holds at most n entries
// while it reads, and skips lines it cannot read with a note to warn.
func Latest(dir string, n int, warn io.Writer) ([]Entry, error) {
	files, err := filepath.Glob(filepath.Join(dir, "[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9].jsonl"))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var out []Entry
	for i := len(files) - 1; i >= 0 && len(out) < n; i-- {
		// The newest entries of a file are its last lines: keep the last
		// want of them, oldest first.
		want := n - len(out)
		var last []Entry
		err := scan(files[i], warn, func(e Entry) bool {
			if len(last) == want {
				last = slices.Delete(last, 0, 1)
			}
			last = append(last, e)
			return true
		})
		if err != nil {
			return nil, err
		}
		slices.Reverse(last)
		out = append(out, last...)
	}
	return out, nil
}

// scan calls fn with each entry in the file at path, in order, until fn
// returns false. A torn line, such as a crash leaves, or an entry of a
// version this brout does not read is skipped with a note to warn, so one bad
// line does not hide the rest of the day.
func scan(path string, warn io.Writer, fn func(Entry) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f) // ReadBytes has no line limit; a line holds a body of up to 64 MiB
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			if e, why := decode(line); why != "" {
				fmt.Fprintf(warn, "%s:%d: skipped: %s\n", path, n, why)
			} else if !fn(e) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// decode reads one line, checking its version before the rest. why says why
// it could not.
func decode(line []byte) (e Entry, why string) {
	var v struct{ V int }
	if err := json.Unmarshal(line, &v); err != nil {
		return Entry{}, fmt.Sprintf("not a log entry: %v", err)
	}
	if v.V != Version {
		return Entry{}, fmt.Sprintf("entry version %d; this brout reads version %d", v.V, Version)
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return Entry{}, fmt.Sprintf("not a version %d entry: %v", Version, err)
	}
	e.Raw = line
	return e, ""
}
