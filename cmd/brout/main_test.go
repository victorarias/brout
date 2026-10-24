package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServeRefusesUnresolvedKey(t *testing.T) {
	path := writeConfig(t, `
routing = ["typesafe/jev-latest"]
[providers.typesafe]
api_key_env = "BROUT_TEST_UNSET_KEY"
`)
	err := run(context.Background(), []string{"serve", "--config", path}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "providers.typesafe.api_key_env names BROUT_TEST_UNSET_KEY, which is not set") ||
		!strings.Contains(err.Error(), "put the key in the config as api_key") {
		t.Errorf("err = %v", err)
	}
}

func TestServeConfigErrors(t *testing.T) {
	cases := map[string]struct{ config, want string }{
		"routing names an unconfigured provider": {`
routing = ["openai/gpt-6-luna"]
[providers.typesafe]
api_key = "k"
`, `routing entry "openai/gpt-6-luna" names provider "openai", which has no [providers.openai] section`},
		"routing entry is not a model id": {`
routing = ["jev-latest"]
[providers.typesafe]
api_key = "k"
`, `routing entry "jev-latest" is not a model id`},
		"unknown field":    {"listen = \"127.0.0.1:0\"\nretries = 3\n", "unknown field retries"},
		"no key":           {"[providers.typesafe]\nbase_url = \"http://x\"\n", "providers.typesafe needs api_key or api_key_env"},
		"unknown provider": {"[providers.acme]\napi_key = \"k\"\n", "[providers.acme] is not a provider brout knows"},
		"bad timeout":      {"attempt_timeout = \"soon\"\n", `attempt_timeout "soon" is not a positive duration`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), []string{"serve", "--config", writeConfig(t, c.config)}, io.Discard, io.Discard, nil)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestServeWithoutConfigPrintsSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.toml")
	err := run(context.Background(), []string{"serve", "--config", path}, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "no config at "+path) || !strings.Contains(err.Error(), `[providers.typesafe]`) {
		t.Errorf("err = %v", err)
	}
}

func TestServeAnswers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer from-env" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"model":"jev-1.14.0","answers":{"a":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	t.Setenv("BROUT_TEST_KEY", "from-env")
	path := writeConfig(t, `
listen = "127.0.0.1:0"
routing = ["typesafe/jev-latest"]
[providers.typesafe]
api_key_env = "BROUT_TEST_KEY"
base_url = "`+upstream.URL+`"
`)

	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", path}, io.Discard, io.Discard, func(addr string) { addrc <- addr })
	}()
	var addr string
	select {
	case addr = <-addrc:
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	}

	resp, err := http.Post("http://"+addr+"/v1/systemone", "application/json",
		bytes.NewReader([]byte(`{"state": "x", "questions": {"a": {"type": "noul", "instructions": "?"}}}`)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"model":"typesafe/jev-1.14.0"`) {
		t.Errorf("status %d: %s", resp.StatusCode, body)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve returned %v after shutdown", err)
	}
}
