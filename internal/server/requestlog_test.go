package server_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/victorarias/brout/internal/requestlog"
)

// logLines returns the lines of one day's log file, decoded.
func logLines(t *testing.T, dir, day string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, day+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("line is not JSON: %s", line)
		}
		lines = append(lines, m)
	}
	return lines
}

func compact(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLogWritesOneLinePerOutcome(t *testing.T) {
	cases := map[string]struct {
		upstream              func() *fakeProvider
		body                  []byte
		status                int
		requestedModel, model any
		attempts              string
		request               string
	}{
		"success": {
			upstream: func() *fakeProvider { return newFake(t, replies(200, liveAnswer)) },
			body:     request(`"typesafe/jev-latest"`), status: 200,
			requestedModel: "typesafe/jev-latest", model: "typesafe/jev-1.14.0",
			attempts: `[{"duration_ms":0,"model":"typesafe/jev-latest","status":200}]`,
		},
		"rejection": {
			upstream: func() *fakeProvider {
				return newFake(t, replies(400, `{"detail":{"error_type":"max_tokens_exceeded"}}`))
			},
			body: request(""), status: 400, requestedModel: "auto", model: nil,
			attempts: `[{"duration_ms":0,"error":"typesafe/jev-latest answered 400: max_tokens_exceeded","model":"typesafe/jev-latest","status":400}]`,
		},
		"unavailable": {
			upstream: func() *fakeProvider { return newFake(t, replies(529, `{"detail":"overloaded"}`)) },
			body:     request(""), status: 503, requestedModel: "auto", model: nil,
			attempts: `[{"duration_ms":0,"error":"typesafe/jev-latest answered 529: overloaded","model":"typesafe/jev-latest","status":529}]`,
		},
		"parse failure": {
			upstream: func() *fakeProvider { return newFake(t, answers("x")) },
			body:     []byte(`{"state": `), status: 422, requestedModel: nil, model: nil,
			attempts: `[]`, request: `"{\"state\": "`,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			g := newGateway(t, setup{typesafe: c.upstream(), routing: []string{"typesafe/jev-latest"}, logDir: dir})
			r := g.post(t, c.body)
			if r.status != c.status {
				t.Fatalf("status %d: %s", r.status, r.raw)
			}

			lines := logLines(t, dir, "2026-10-25")
			if len(lines) != 1 {
				t.Fatalf("%d lines, want 1", len(lines))
			}
			e := lines[0]
			if e["v"] != 1.0 || e["id"] != r.header.Get("Brout-Request-Id") || e["time"] != "2026-10-25T15:30:12Z" ||
				e["duration_ms"] != 0.0 || e["status"] != float64(c.status) || e["replay_of"] != nil {
				t.Errorf("entry %v", e)
			}
			if e["requested_model"] != c.requestedModel || e["model"] != c.model {
				t.Errorf("requested_model %v, model %v", e["requested_model"], e["model"])
			}
			if got := compact(t, e["attempts"]); got != c.attempts {
				t.Errorf("attempts\n got %s\nwant %s", got, c.attempts)
			}
			wantRequest := c.request
			if wantRequest == "" {
				var v any
				json.Unmarshal(c.body, &v)
				wantRequest = compact(t, v)
			}
			if got := compact(t, e["request"]); got != wantRequest {
				t.Errorf("request\n got %s\nwant %s", got, wantRequest)
			}
			var sent any
			json.Unmarshal(r.raw, &sent)
			if compact(t, e["response"]) != compact(t, sent) {
				t.Errorf("response\n got %v\nwant %s", e["response"], r.raw)
			}
		})
	}
}

func TestLogCrossesUTCMidnight(t *testing.T) {
	dir := t.TempDir()
	c := &fakeClock{}
	g := newGateway(t, setup{typesafe: newFake(t, answers("jev-1.14.0")), routing: []string{"typesafe/jev-latest"}, clock: c, logDir: dir})

	c.set(time.Date(2026, 10, 25, 23, 59, 59, 900e6, time.UTC))
	before := g.post(t, request("")).header.Get("Brout-Request-Id")
	c.set(time.Date(2026, 10, 26, 0, 0, 0, 100e6, time.UTC))
	after := g.post(t, request("")).header.Get("Brout-Request-Id")

	for day, id := range map[string]string{"2026-10-25": before, "2026-10-26": after} {
		lines := logLines(t, dir, day)
		if len(lines) != 1 || lines[0]["id"] != id {
			t.Errorf("%s holds %v, want only %s", day, lines, id)
		}
		if e, err := requestlog.Find(dir, id); err != nil || e.ID != id {
			t.Errorf("Find(%s) = %v, %v", id, e.ID, err)
		}
	}
	if !strings.HasPrefix(before, "20261025-235959-") || !strings.HasPrefix(after, "20261026-000000-") {
		t.Errorf("ids %s and %s", before, after)
	}
}

func TestLogRecordsReplayOf(t *testing.T) {
	dir := t.TempDir()
	g := newGateway(t, setup{typesafe: newFake(t, answers("jev-1.14.0")), routing: []string{"typesafe/jev-latest"}, logDir: dir})
	g.post(t, request(""), "Brout-Replay-Of", "20261025-120000-abcdef")
	if lines := logLines(t, dir, "2026-10-25"); lines[0]["replay_of"] != "20261025-120000-abcdef" {
		t.Errorf("replay_of %v", lines[0]["replay_of"])
	}
}

func TestLogFailureDoesNotFailRequest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	os.WriteFile(dir, nil, 0o600)
	g := newGateway(t, setup{typesafe: newFake(t, answers("jev-1.14.0")), routing: []string{"typesafe/jev-latest"}, logDir: dir})
	r := g.post(t, request(""))
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.raw)
	}
	if want := r.header.Get("Brout-Request-Id") + " request log: "; !strings.Contains(g.stderr.String(), want) {
		t.Errorf("stderr %q, want a line starting %q", g.stderr.String(), want)
	}
}
