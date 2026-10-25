package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/victorarias/brout/internal/requestlog"
)

// echoModel is a System One provider that answers every request as the model
// it was asked for.
func echoModel(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Model string }
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]any{
			"model": req.Model + "-resolved",
			"answers": map[string]any{
				"happy": map[string]any{"type": "noul", "noul": 0.75},
				// No confidence: replay still shows the choice.
				"topic": map[string]any{"type": "choice", "choice": "praise"},
			},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// startServe runs brout serve with config until the test ends, and returns
// its address.
func startServe(t *testing.T, config string) string {
	t.Helper()
	path := writeConfig(t, config)
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", path}, io.Discard, io.Discard, func(addr string) { addrc <- addr })
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve returned %v after shutdown", err)
		}
	})
	select {
	case addr := <-addrc:
		return addr
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
		return ""
	}
}

func post(t *testing.T, addr, body string) string {
	t.Helper()
	resp, err := http.Post("http://"+addr+"/v1/systemone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header.Get("Brout-Request-Id")
}

// brout runs a client command and returns its stdout.
func brout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, stderr, err := broutStderr(t, args...)
	if stderr != "" {
		t.Logf("stderr: %s", stderr)
	}
	return stdout, err
}

func broutStderr(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err = run(context.Background(), args, &out, &errOut, nil)
	return out.String(), errOut.String(), err
}

const happyRequest = `{"model": "typesafe/jev-latest", "state": "I love it", "questions": {
  "happy": {"type": "noul", "instructions": "Happy?"},
  "topic": {"type": "choice", "instructions": "Topic?", "criteria": {"praise": null, "complaint": null}}}}`

func TestLogsShowAndReplay(t *testing.T) {
	upstream := echoModel(t)
	logDir := filepath.Join(t.TempDir(), "logs")
	config := `
listen = "127.0.0.1:0"
routing = ["typesafe/jev-latest"]
[providers.typesafe]
api_key = "k"
base_url = "` + upstream.URL + `"
[log]
dir = "` + logDir + `"
`
	addr := startServe(t, config)
	id := post(t, addr, happyRequest)
	configPath := writeConfig(t, config)

	out, err := brout(t, "logs", "--config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 2 ||
		!strings.HasPrefix(lines[1], id+"  ") || !strings.Contains(lines[1], "  typesafe/jev-latest-resolved  200  ") ||
		!strings.HasSuffix(lines[1], "  happy, topic") {
		t.Errorf("logs:\n%s", out)
	}

	out, err = brout(t, "logs", "show", id, "--config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "{\n  \"v\": 1,\n  \"id\": \""+id+"\",") || !strings.Contains(out, `"state": "I love it"`) {
		t.Errorf("logs show:\n%s", out)
	}

	out, err = brout(t, "replay", id, "--model", "typesafe/jev-next", "--config", configPath, "--url", "http://"+addr)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	entries, _ := requestlog.Latest(logDir, 1, io.Discard)
	replayID := entries[0].ID
	want := "replayed " + id + " as " + replayID + "\n\n" +
		"QUESTION  BEFORE: typesafe/jev-latest-resolved  AFTER: typesafe/jev-next-resolved\n" +
		"happy     noul 0.750                            noul 0.750\n" +
		"topic     choice praise                         choice praise\n"
	if out != want {
		t.Errorf("replay:\n%s\nwant:\n%s", out, want)
	}
	if e := entries[0]; e.ReplayOf == nil || *e.ReplayOf != id || *e.RequestedModel != "typesafe/jev-next" {
		t.Errorf("replay entry %s", e.Raw)
	}

	out, _ = brout(t, "logs", "--config", configPath, "--limit", "1")
	if !strings.Contains(out, replayID) || !strings.HasSuffix(strings.TrimSpace(out), "happy, topic (replay of "+id+")") || strings.Contains(out, id+"  ") {
		t.Errorf("logs --limit 1:\n%s", out)
	}
}

func TestReplayFindsServerFromConfigListen(t *testing.T) {
	logDir := t.TempDir()
	providers := `
[providers.typesafe]
api_key = "k"
base_url = "` + echoModel(t).URL + `"
[log]
dir = "` + logDir + `"
`
	addr := startServe(t, "listen = \"127.0.0.1:0\"\n"+providers)
	id := post(t, addr, happyRequest)
	// The client's config names the address serve bound.
	client := writeConfig(t, "listen = \""+addr+"\"\n"+providers)
	out, err := brout(t, "replay", id, "--config", client)
	if err != nil || !strings.HasPrefix(out, "replayed "+id+" as ") {
		t.Errorf("replay: %v\n%s", err, out)
	}
}

func TestReadersSkipBadLines(t *testing.T) {
	logDir := t.TempDir()
	path := writeConfig(t, "[log]\ndir = \""+logDir+"\"\n")
	good := func(id, request string) string {
		return `{"v":1,"id":"` + id + `","time":"2026-10-25T15:30:12Z","duration_ms":1,"requested_model":"auto","model":null,"status":422,"attempts":[],"replay_of":null,"request":` + request + `,"response":{"detail":{"error_type":"invalid_request","message":"m"}}}` + "\n"
	}
	day := filepath.Join(logDir, "2026-10-25.jsonl")
	os.WriteFile(day, []byte(good("20261025-153012-aaaaaa", `{"questions":{"a":{}}}`)+
		`{"v":2,"id":"20261025-153013-bbbbbb","future":true}`+"\n"+
		good("20261025-153014-cccccc", "null")+
		`{"v":1,"id":"20261025-153015-dddddd","request":{"sta`), 0o600)

	out, stderr, err := broutStderr(t, "logs", "--config", path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 3 ||
		!strings.HasPrefix(lines[1], "20261025-153014-cccccc") || !strings.HasPrefix(lines[2], "20261025-153012-aaaaaa") {
		t.Errorf("logs:\n%s", out)
	}
	if stderr != day+":2: skipped: entry version 2; this brout reads version 1\n"+day+":4: skipped: not a log entry: unexpected end of JSON input\n" {
		t.Errorf("stderr:\n%s", stderr)
	}

	if out, err := brout(t, "logs", "show", "20261025-153014-cccccc", "--config", path); err != nil || !strings.Contains(out, `"id": "20261025-153014-cccccc"`) {
		t.Errorf("logs show past a bad line: %v\n%s", err, out)
	}
	// Find stops at the entry, so the torn line after it goes unread.
	if _, stderr, _ := broutStderr(t, "logs", "show", "20261025-153012-aaaaaa", "--config", path); stderr != "" {
		t.Errorf("logs show read past its entry: %s", stderr)
	}
	if _, err := brout(t, "replay", "20261025-153014-cccccc", "--config", path); err == nil || err.Error() != "request 20261025-153014-cccccc has no logged body to replay" {
		t.Errorf("replay of a null request: %v", err)
	}
}

func TestReplayWithoutServer(t *testing.T) {
	upstream := echoModel(t)
	logDir := t.TempDir()
	config := `
listen = "127.0.0.1:0"
[providers.typesafe]
api_key = "k"
base_url = "` + upstream.URL + `"
[log]
dir = "` + logDir + `"
`
	addr := startServe(t, config)
	id := post(t, addr, happyRequest)
	_, err := brout(t, "replay", id, "--config", writeConfig(t, config), "--url", "http://127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "running brout serve at http://127.0.0.1:1, which did not answer") {
		t.Errorf("err = %v", err)
	}
}

func TestLogDisabledWritesNothing(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "logs")
	addr := startServe(t, `
listen = "127.0.0.1:0"
[providers.typesafe]
api_key = "k"
base_url = "`+echoModel(t).URL+`"
[log]
enabled = false
dir = "`+logDir+`"
`)
	if id := post(t, addr, happyRequest); id == "" {
		t.Fatal("no Brout-Request-Id")
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("log dir exists: %v", err)
	}
}

func TestLogsWithoutKeyInEnvironment(t *testing.T) {
	logDir := t.TempDir()
	path := writeConfig(t, `
[providers.typesafe]
api_key_env = "BROUT_TEST_UNSET_KEY"
[log]
dir = "`+logDir+`"
`)
	out, err := brout(t, "logs", "--config", path)
	if err != nil || out != "no requests logged in "+logDir+"\n" {
		t.Errorf("logs: %q, %v", out, err)
	}
	if _, err := brout(t, "logs", "show", "20261025-153012-7f3a9c", "--config", path); err == nil ||
		!strings.Contains(err.Error(), "no request 20261025-153012-7f3a9c: "+filepath.Join(logDir, "2026-10-25.jsonl")+" does not exist") {
		t.Errorf("logs show: %v", err)
	}
	if _, err := brout(t, "logs", "show", "../etc", "--config", path); err == nil || !strings.Contains(err.Error(), `"../etc" is not a brout request id`) {
		t.Errorf("logs show bad id: %v", err)
	}
}
