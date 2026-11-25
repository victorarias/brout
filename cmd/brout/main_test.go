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

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"version"}, &out, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "brout dev\n" {
		t.Fatalf("version: %q", &out)
	}
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
		"no key":           {"[providers.typesafe]\nbase_url = \"http://x\"\n", "providers.typesafe needs api_key, api_key_env, headers or headers_env"},
		"unknown provider": {"[providers.acme]\napi_key = \"k\"\n", `[providers.acme] is not a provider brout knows (typesafe, subq, openai); set protocol = "openai" or "systemone"`},
		"unknown protocol": {"[providers.acme]\nprotocol = \"grpc\"\napi_key = \"k\"\n", `providers.acme.protocol "grpc" is not "openai" or "systemone"`},
		"built-in with another protocol": {"[providers.openai]\nprotocol = \"systemone\"\napi_key = \"k\"\n",
			`providers.openai.protocol "systemone" does not match the built-in openai provider, which speaks "openai"`},
		"custom systemone without base_url": {"[providers.acme]\nprotocol = \"systemone\"\napi_key = \"k\"\n",
			"providers.acme needs base_url; a custom systemone provider has no default"},
		"header repeats api key": {"[providers.openai]\napi_key = \"k\"\nheaders = { authorization = \"Bearer other\" }\n",
			`providers.openai.api_key and providers.openai.headers."authorization" both set the Authorization header`},
		"header set in two cases": {"[providers.openai]\nheaders = { x-api-key = \"a\", X-API-KEY = \"b\" }\n",
			`providers.openai.headers."X-API-KEY" and providers.openai.headers."x-api-key" both set the X-Api-Key header`},
		"header in headers and headers_env": {"[providers.openai]\nheaders = { apikey = \"a\" }\nheaders_env = { ApiKey = \"HOME\" }\n",
			`providers.openai.headers."apikey" and providers.openai.headers_env."ApiKey" both set the Apikey header`},
		"content type": {"[providers.openai]\napi_key = \"k\"\nheaders = { content-type = \"text/plain\" }\n",
			`providers.openai.headers."content-type" sets Content-Type, which brout sets itself`},
		"unset headers_env": {"[providers.openai]\nheaders_env = { x-api-key = \"BROUT_TEST_UNSET_KEY\" }\n",
			`providers.openai.headers_env."x-api-key" names BROUT_TEST_UNSET_KEY, which is not set in brout's environment`},
		"bad timeout": {"attempt_timeout = \"soon\"\n", `attempt_timeout "soon" is not a positive duration`},
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

// A custom provider speaks a built-in protocol, sends only its configured
// headers, and names itself in model ids and errors.
func TestServeCustomProviders(t *testing.T) {
	type seen struct{ path, auth, apiKey, team, contentType string }
	sent := make(chan seen, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- seen{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Team"), r.Header.Get("Content-Type")}
		switch r.URL.Path {
		case "/gw/v1/decisions":
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"bad model"}}`)
		case "/s1/v1/systemone":
			io.WriteString(w, `{"model":"jev-1.14.0","answers":{"a":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	defer upstream.Close()
	t.Setenv("BROUT_TEST_GATEWAY_KEY", "from-env")
	path := writeConfig(t, `
listen = "127.0.0.1:0"
routing = ["s1/jev-latest"]
[providers.gateway]
protocol = "openai"
base_url = "`+upstream.URL+`/gw"
headers = { x-team = "decisions" }
headers_env = { x-api-key = "BROUT_TEST_GATEWAY_KEY" }
[providers.s1]
protocol = "systemone"
base_url = "`+upstream.URL+`/s1"
api_key = "s1-key"
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
	post := func(model string) (int, string) {
		resp, err := http.Post("http://"+addr+"/v1/systemone", "application/json",
			strings.NewReader(`{"model": "`+model+`", "state": "x", "questions": {"a": {"type": "noul", "instructions": "?"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if status, body := post("gateway/gpt-6-luna"); status != 400 || !strings.Contains(body, "gateway/gpt-6-luna answered 400: bad model") {
		t.Errorf("gateway: status %d: %s", status, body)
	}
	if got := <-sent; got != (seen{"/gw/v1/decisions", "", "from-env", "decisions", "application/json"}) {
		t.Errorf("gateway sent %+v", got)
	}
	if status, body := post("auto"); status != 200 || !strings.Contains(body, `"model":"s1/jev-1.14.0"`) {
		t.Errorf("s1: status %d: %s", status, body)
	}
	if got := <-sent; got != (seen{"/s1/v1/systemone", "Bearer s1-key", "", "", "application/json"}) {
		t.Errorf("s1 sent %+v", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("serve returned %v after shutdown", err)
	}
}
