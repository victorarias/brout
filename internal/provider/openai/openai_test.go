package openai_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/victorarias/brout/internal/provider"
	"github.com/victorarias/brout/internal/provider/openai"
	"github.com/victorarias/brout/internal/provider/systemone"
	"github.com/victorarias/brout/internal/router"
	"github.com/victorarias/brout/internal/server"
)

// The request whose translation was sent live to /v1/decisions on
// 2026-10-25; testdata holds what OpenAI answered.
const threeQuestions = `{
  "model": %s,
  "state": %s,
  "questions": {
    "positive": {"type": "noul", "instructions": "Is the user happy?"},
    "topic": {"type": "choice", "instructions": "Topic?", "criteria": {"praise": "praising", "complaint": null}},
    "stars": {"type": "score", "instructions": "How satisfied?", "criteria": ["not", "somewhat", "very"]}
  }
}`

const (
	textState = `"The user wrote: I love this product, it works great!"`
	jsonState = `{"review": "I love this product, it works great!", "stars_given": 5}`
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fake is an upstream provider: an httptest server that records what brout
// sent and answers with h.
type fake struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
}

func newFake(t *testing.T, path string, h http.HandlerFunc) *fake {
	t.Helper()
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != path {
			http.Error(w, "fake serves POST "+path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// sent decodes the only request brout sent.
func (f *fake) sent(t *testing.T) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 {
		t.Fatalf("%d calls, want 1", len(f.bodies))
	}
	var body map[string]any
	if err := json.Unmarshal(f.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func replies(status int, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	}
}

// gateway is brout's HTTP handler with openai, and typesafe when given.
func gateway(t *testing.T, oa, typesafe *fake, routing ...string) string {
	t.Helper()
	providers := map[string]provider.Provider{"openai": openai.New(oa.URL, "key-openai", http.DefaultClient)}
	if typesafe != nil {
		spec := systemone.TypeSafe
		spec.BaseURL = typesafe.URL
		providers["typesafe"] = systemone.New("typesafe", spec, "key-typesafe", http.DefaultClient)
	}
	hs := httptest.NewServer(server.New(server.Options{
		Router: router.New(providers, routing, 4*time.Second, router.RealClock{}),
		Now:    time.Now,
		Stderr: io.Discard,
	}))
	t.Cleanup(hs.Close)
	return hs.URL
}

type reply struct {
	status int
	body   map[string]any
	raw    []byte
}

func post(t *testing.T, url, body string) reply {
	t.Helper()
	resp, err := http.Post(url+"/v1/systemone", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: raw}
	if err := json.Unmarshal(raw, &r.body); err != nil {
		t.Fatalf("body is not JSON: %s", raw)
	}
	return r
}

func (r reply) errorOf(t *testing.T) (string, string) {
	t.Helper()
	detail, ok := r.body["detail"].(map[string]any)
	if !ok {
		t.Fatalf("not brout's error shape: %s", r.raw)
	}
	return detail["error_type"].(string), detail["message"].(string)
}

// sameRequest compares two /v1/decisions bodies; question order is free.
func sameRequest(t *testing.T, got map[string]any, want []byte) {
	t.Helper()
	var w map[string]any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	for _, body := range []map[string]any{got, w} {
		qs := body["questions"].([]any)
		slices.SortFunc(qs, func(a, b any) int {
			return strings.Compare(a.(map[string]any)["name"].(string), b.(map[string]any)["name"].(string))
		})
	}
	if !reflect.DeepEqual(got, w) {
		g, _ := json.MarshalIndent(got, "", "  ")
		t.Errorf("brout sent\n%s\nwant\n%s", g, want)
	}
}

func answer(t *testing.T, r reply, name string) map[string]any {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.raw)
	}
	a, ok := r.body["answers"].(map[string]any)[name].(map[string]any)
	if !ok {
		t.Fatalf("no answer %q: %s", name, r.raw)
	}
	return a
}

func asJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// F2: a string state becomes input, and each question type translates to
// OpenAI's; the answers come back in System One's shape.
func TestTextState(t *testing.T) {
	oa := newFake(t, "/v1/decisions", replies(200, fixture(t, "text-state.response.json")))
	url := gateway(t, oa, nil)

	r := post(t, url, fmt.Sprintf(threeQuestions, `"openai/gpt-6-luna"`, textState))
	sameRequest(t, oa.sent(t), fixture(t, "text-state.request.json"))
	if got := oa.auth[0]; got != "Bearer key-openai" {
		t.Errorf("Authorization %q", got)
	}

	if r.body["model"] != "openai/gpt-6-luna" {
		t.Errorf("model %v", r.body["model"])
	}
	if got := asJSON(r.body["usage"]); got != `{"input_tokens":380,"output_tokens":0}` {
		t.Errorf("usage %s", got)
	}
	for name, want := range map[string]string{
		"positive": `{"noul":1,"type":"noul"}`,
		"topic":    `{"choice":"praise","confidence":1,"probabilities":{"complaint":0,"praise":1},"type":"choice"}`,
		"stars":    `{"confidence":1,"legend":{"0":"not","1":"somewhat","2":"very"},"probabilities":{"0":0,"1":0,"2":1},"score":2,"type":"score"}`,
	} {
		if got := asJSON(answer(t, r, name)); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

// F2: an object state goes on as input_json, byte for byte. An array goes
// wrapped as {"state": array}: OpenAI answers a bare array of strings with 400.
func TestJSONState(t *testing.T) {
	oa := newFake(t, "/v1/decisions", replies(200, fixture(t, "json-state.response.json")))
	url := gateway(t, oa, nil)

	r := post(t, url, fmt.Sprintf(threeQuestions, `"openai/gpt-6-luna"`, jsonState))
	sameRequest(t, oa.sent(t), fixture(t, "json-state.request.json"))
	if got := asJSON(answer(t, r, "positive")); got != `{"noul":0.99,"type":"noul"}` {
		t.Errorf("positive: %s", got)
	}

	oa = newFake(t, "/v1/decisions", replies(200, fixture(t, "json-state.response.json")))
	url = gateway(t, oa, nil)
	post(t, url, fmt.Sprintf(threeQuestions, `"openai/gpt-6-luna"`, `[1, "two"]`))
	if got := asJSON(oa.sent(t)["input"]); got != `[{"content":[{"data":{"state":[1,"two"]},"type":"input_json"}],"role":"user"}]` {
		t.Errorf("input %s", got)
	}
}

// F2: a predicate has no criteria, so noul criteria are appended to the
// instructions; instructions and descriptions that are not text go as
// compact JSON; absent instructions go as "", which OpenAI requires.
func TestQuestionText(t *testing.T) {
	oa := newFake(t, "/v1/decisions", replies(200, fixture(t, "text-state.response.json")))
	url := gateway(t, oa, nil)
	post(t, url, `{"model": "openai/gpt-6-luna", "state": "x", "questions": {
	  "positive": {"type": "noul", "instructions": "Is the user happy?", "criteria": {"true": "they say so", "false": {"tone": "angry"}}},
	  "topic": {"type": "choice", "instructions": {"ask": "Topic?", "lang": ["en"]}, "criteria": {"praise": ["kind", "words"], "complaint": null}},
	  "stars": {"type": "score", "criteria": ["not", {"at": "least"}, null]}
	}}`)

	want := `[` +
		`{"instructions":"Is the user happy?\n\ntrue means: they say so\n\nfalse means: {\"tone\":\"angry\"}","name":"positive","type":"predicate"},` +
		`{"instructions":"","levels":[{"description":"not","label":"0"},{"description":"{\"at\":\"least\"}","label":"1"},{"label":"2"}],"name":"stars","type":"score"},` +
		`{"choices":[{"description":"[\"kind\",\"words\"]","value":"praise"},{"value":"complaint"}],"instructions":"{\"ask\":\"Topic?\",\"lang\":[\"en\"]}","name":"topic","type":"choice"}` +
		`]`
	if got := asJSON(oa.sent(t)["questions"]); got != want {
		t.Errorf("questions\n%s\nwant\n%s", got, want)
	}

	oa = newFake(t, "/v1/decisions", replies(200, fixture(t, "text-state.response.json")))
	url = gateway(t, oa, nil)
	post(t, url, `{"model": "openai/gpt-6-luna", "state": "x", "questions": {
	  "positive": {"type": "noul", "criteria": {"false": "they complain"}}
	}}`)
	if got := asJSON(oa.sent(t)["questions"]); got != `[{"instructions":"false means: they complain","name":"positive","type":"predicate"}]` {
		t.Errorf("questions %s", got)
	}
}

// D12: a refusal is a rejection that names the question, and an auto request
// does not fail over past it.
func TestRefusal(t *testing.T) {
	oa := newFake(t, "/v1/decisions", replies(200, fixture(t, "refusal.response.json")))
	ts := newFake(t, "/v1/systemone", replies(200, []byte(`{}`)))
	url := gateway(t, oa, ts, "openai/gpt-6-luna", "typesafe/jev-latest")

	for _, model := range []string{`"openai/gpt-6-luna"`, `"auto"`} {
		r := post(t, url, fmt.Sprintf(`{"model": %s, "state": "x", "questions": {"weapon": {"type": "noul", "instructions": "?"}}}`, model))
		typ, msg := r.errorOf(t)
		if r.status != 422 || typ != "rejected" || msg != `openai/gpt-6-luna refused to answer question "weapon"` {
			t.Errorf("%s: %d %s %q", model, r.status, typ, msg)
		}
	}
	if ts.calls() != 0 {
		t.Errorf("a refusal failed over to typesafe")
	}

	// Only the refused question is named when others were answered.
	oa = newFake(t, "/v1/decisions", replies(200, []byte(`{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"fine","probability":0.5},{"type":"refusal","name":"weapon"}],"usage":{"input_tokens":1,"output_tokens":0}}`)))
	url = gateway(t, oa, nil)
	r := post(t, url, `{"model": "openai/gpt-6-luna", "state": "x", "questions": {"fine": {"type": "noul"}, "weapon": {"type": "noul"}}}`)
	if _, msg := r.errorOf(t); r.status != 422 || msg != `openai/gpt-6-luna refused to answer question "weapon"` {
		t.Errorf("%d %q", r.status, msg)
	}
}

// D8 on OpenAI's statuses, with OpenAI's error message quoted (D9).
func TestErrors(t *testing.T) {
	t.Run("400 is a rejection", func(t *testing.T) {
		oa := newFake(t, "/v1/decisions", replies(400, fixture(t, "error-400.response.json")))
		ts := newFake(t, "/v1/systemone", replies(200, []byte(`{}`)))
		url := gateway(t, oa, ts, "openai/gpt-6-luna", "typesafe/jev-latest")
		r := post(t, url, fmt.Sprintf(threeQuestions, `"auto"`, textState))
		typ, msg := r.errorOf(t)
		if r.status != 400 || typ != "rejected" || !strings.Contains(msg, `openai/gpt-6-luna answered 400: invalid_value: Invalid value: 'bogus'.`) {
			t.Errorf("%d %s %q", r.status, typ, msg)
		}
		if ts.calls() != 0 {
			t.Errorf("a rejection failed over")
		}
	})
	t.Run("401 fails over", func(t *testing.T) {
		oa := newFake(t, "/v1/decisions", replies(401, fixture(t, "error-401.response.json")))
		ts := newFake(t, "/v1/systemone", replies(200, []byte(`{"model":"jev-1.14.0","answers":{"a":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`)))
		url := gateway(t, oa, ts, "openai/gpt-6-luna", "typesafe/jev-latest")
		r := post(t, url, `{"state": "x", "questions": {"a": {"type": "noul", "instructions": "?"}}}`)
		if r.status != 200 || r.body["model"] != "typesafe/jev-1.14.0" {
			t.Errorf("%d: %s", r.status, r.raw)
		}

		r = post(t, url, `{"model": "openai/gpt-6-luna", "state": "x", "questions": {"a": {"type": "noul", "instructions": "?"}}}`)
		typ, msg := r.errorOf(t)
		if r.status != 401 || typ != "unavailable" || !strings.Contains(msg, "openai/gpt-6-luna answered 401: invalid_api_key: Incorrect API key provided") {
			t.Errorf("%d %s %q", r.status, typ, msg)
		}
	})
	t.Run("529 from the first model fails over to openai", func(t *testing.T) {
		oa := newFake(t, "/v1/decisions", replies(200, fixture(t, "text-state.response.json")))
		ts := newFake(t, "/v1/systemone", replies(529, []byte(`{"detail":"overloaded"}`)))
		url := gateway(t, oa, ts, "typesafe/jev-latest", "openai/gpt-6-luna")
		r := post(t, url, fmt.Sprintf(threeQuestions, `"auto"`, textState))
		if r.status != 200 || r.body["model"] != "openai/gpt-6-luna" || ts.calls() != 1 {
			t.Errorf("%d: %s", r.status, r.raw)
		}
	})
}

// F3: OpenAI's limits, measured live, are checked before any call.
func TestLimits(t *testing.T) {
	oa := newFake(t, "/v1/decisions", replies(500, nil))
	url := gateway(t, oa, nil)

	var many bytes.Buffer
	for i := range 401 {
		if i > 0 {
			many.WriteByte(',')
		}
		fmt.Fprintf(&many, `"q%d": {"type": "noul"}`, i)
	}
	cases := map[string]struct{ questions, want string }{
		"questions": {many.String(), "openai/gpt-6-luna accepts at most 400 questions; the request has 401"},
		"choices":   {`"c": {"type": "choice", "criteria": {"only": null}}`, `openai/gpt-6-luna needs at least 2 choice options; question "c" has 1`},
		"levels":    {`"s": {"type": "score", "criteria": [0,1,2,3,4,5,6,7,8,9,10]}`, `openai/gpt-6-luna accepts at most 10 score levels; question "s" has 11`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := post(t, url, `{"model": "openai/gpt-6-luna", "state": "x", "questions": {`+c.questions+`}}`)
			typ, msg := r.errorOf(t)
			if r.status != 422 || typ != "limit" || msg != c.want {
				t.Errorf("%d %s %q", r.status, typ, msg)
			}
		})
	}
	if oa.calls() != 0 {
		t.Errorf("a request over the limits reached openai")
	}
}
