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
			"model":   req.Model + "-resolved",
			"answers": map[string]any{"happy": map[string]any{"type": "noul", "noul": 0.75}},
			"usage":   map[string]int{"input_tokens": 1, "output_tokens": 1},
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
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, &stdout, &stderr, nil)
	if stderr.Len() > 0 {
		t.Logf("stderr: %s", stderr.String())
	}
	return stdout.String(), err
}

const happyRequest = `{"model": "typesafe/jev-latest", "state": "I love it", "questions": {"happy": {"type": "noul", "instructions": "Happy?"}}}`

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
		!strings.HasSuffix(lines[1], "  happy") {
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
	entries, _ := requestlog.Latest(logDir, 1)
	replayID := entries[0].ID
	want := "replayed " + id + " as " + replayID + "\n\n" +
		"QUESTION  BEFORE: typesafe/jev-latest-resolved  AFTER: typesafe/jev-next-resolved\n" +
		"happy     noul 0.750                            noul 0.750\n"
	if out != want {
		t.Errorf("replay:\n%s\nwant:\n%s", out, want)
	}
	if e := entries[0]; e.ReplayOf == nil || *e.ReplayOf != id || *e.RequestedModel != "typesafe/jev-next" {
		t.Errorf("replay entry %s", e.Raw)
	}

	out, _ = brout(t, "logs", "--config", configPath, "--limit", "1")
	if !strings.Contains(out, replayID) || !strings.HasSuffix(strings.TrimSpace(out), "happy (replay of "+id+")") || strings.Contains(out, id+"  ") {
		t.Errorf("logs --limit 1:\n%s", out)
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
