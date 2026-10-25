package server_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/victorarias/brout/internal/provider"
	"github.com/victorarias/brout/internal/provider/systemone"
	"github.com/victorarias/brout/internal/requestlog"
	"github.com/victorarias/brout/internal/router"
	"github.com/victorarias/brout/internal/server"
)

// A request with one question of each type, as TypeSafe's docs show them.
const threeQuestions = `{
  "model": %s,
  "state": "The user wrote: I love this product, it works great!",
  "questions": {
    "positive": {"type": "noul", "instructions": "Is the user happy?"},
    "topic": {"type": "choice", "instructions": "Topic?", "criteria": {"praise": "praising", "complaint": null}},
    "stars": {"type": "score", "instructions": "How satisfied?", "criteria": ["not", "somewhat", "very"]}
  }
}`

// The answer TypeSafe gave live to threeQuestions on 2026-10-25.
const liveAnswer = `{"model":"jev-1.14.0","answers":{"positive":{"type":"noul","noul":0.98},"topic":{"type":"choice","choice":"praise","confidence":1.0,"probabilities":{"praise":1.0,"complaint":0.0}},"stars":{"type":"score","score":2.0,"confidence":1.0,"legend":{"0":"not","1":"somewhat","2":"very"},"probabilities":{"0":0.0,"1":0.0,"2":1.0}}},"usage":{"input_tokens":360,"output_tokens":64}}`

func request(model string) []byte {
	if model == "" {
		return []byte(strings.Replace(threeQuestions, `"model": %s,`, "", 1))
	}
	return fmt.Appendf(nil, threeQuestions, model)
}

var clock = time.Date(2026, 10, 25, 15, 30, 12, 0, time.UTC)

// fakeClock stands still at clock, or where set moves it. Its timers fire only
// when a test fires them.
type fakeClock struct {
	mu     sync.Mutex
	at     time.Time
	timers map[int]func()
	next   int
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() {
		return clock
	}
	return c.at
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = t
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timers == nil {
		c.timers = map[int]func(){}
	}
	id := c.next
	c.next++
	c.timers[id] = f
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, pending := c.timers[id]
		delete(c.timers, id)
		return pending
	}
}

// fire runs every pending timer.
func (c *fakeClock) fire() {
	c.mu.Lock()
	timers := c.timers
	c.timers = nil
	c.mu.Unlock()
	for _, f := range timers {
		f()
	}
}

// fakeProvider is a System One provider: an httptest server that speaks
// TypeSafe's wire.
type fakeProvider struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	auth   []string
}

func newFake(t *testing.T, h http.HandlerFunc) *fakeProvider {
	t.Helper()
	f := &fakeProvider{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" {
			http.Error(w, "fake serves POST /v1/systemone", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// answers answers every question it is asked, as model, with the first
// choice and the lowest level.
func answers(model string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Questions map[string]struct {
				Type     string
				Criteria json.RawMessage
			}
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		out := map[string]any{}
		for name, q := range req.Questions {
			switch q.Type {
			case "noul":
				out[name] = map[string]any{"type": "noul", "noul": 0.5}
			case "choice":
				dec := json.NewDecoder(bytes.NewReader(q.Criteria))
				dec.Token()
				first, _ := dec.Token()
				out[name] = map[string]any{"type": "choice", "choice": first, "confidence": 1, "probabilities": map[string]float64{first.(string): 1}}
			case "score":
				out[name] = map[string]any{"type": "score", "score": 0, "confidence": 1, "legend": map[string]any{}, "probabilities": map[string]float64{"0": 1}}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": model, "answers": out, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
	}
}

func replies(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// hangsUp closes the connection without answering.
func hangsUp(w http.ResponseWriter, r *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

// blocks never answers; it returns when brout cancels the call. arrived, if
// set, is closed when the call arrives.
func blocks(arrived chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if arrived != nil {
			close(arrived)
		}
		<-r.Context().Done()
	}
}

// timesOut never answers, and fires the attempt timeout once the call arrives.
func timesOut(c *fakeClock) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.fire()
		<-r.Context().Done()
	}
}

type gateway struct {
	url    string
	stderr *syncBuffer
	mu     sync.Mutex
	done   []server.Exchange
}

func (g *gateway) exchanges() []server.Exchange {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

type setup struct {
	typesafe, subq *fakeProvider // nil leaves the provider unconfigured
	routing        []string
	clock          *fakeClock
	finished       chan server.Exchange
	logDir         string // set, brout serve's request log writes here
}

func newGateway(t *testing.T, s setup) *gateway {
	t.Helper()
	providers := map[string]provider.Provider{}
	if s.typesafe != nil {
		spec := systemone.TypeSafe
		spec.BaseURL = s.typesafe.URL
		providers["typesafe"] = systemone.New("typesafe", spec, "key-typesafe", http.DefaultClient)
	}
	if s.subq != nil {
		spec := systemone.SubQ
		spec.BaseURL = s.subq.URL
		providers["subq"] = systemone.New("subq", spec, "key-subq", http.DefaultClient)
	}
	if s.clock == nil {
		s.clock = &fakeClock{}
	}
	g := &gateway{stderr: &syncBuffer{}}
	var log *requestlog.Writer
	if s.logDir != "" {
		log = requestlog.NewWriter(s.logDir, g.stderr)
	}
	srv := server.New(server.Options{
		Router: router.New(providers, s.routing, 4*time.Second, s.clock),
		Now:    s.clock.Now,
		Stderr: g.stderr,
		Finish: func(ex server.Exchange) {
			if log != nil {
				log.Finish(ex)
			}
			g.mu.Lock()
			g.done = append(g.done, ex)
			g.mu.Unlock()
			if s.finished != nil {
				s.finished <- ex
			}
		},
	})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	g.url = hs.URL
	return g
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
	raw    []byte
}

func (g *gateway) post(t *testing.T, body []byte, header ...string) reply {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, g.url+"/v1/systemone", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	return do(t, req)
}

func do(t *testing.T, req *http.Request) reply {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, header: resp.Header, raw: raw}
	if err := json.Unmarshal(raw, &r.body); err != nil {
		t.Fatalf("body is not JSON: %s", raw)
	}
	return r
}

// errorOf checks the error shape and returns its type and message.
func (r reply) errorOf(t *testing.T) (string, string) {
	t.Helper()
	detail, ok := r.body["detail"].(map[string]any)
	if !ok || len(r.body) != 1 || len(detail) != 2 {
		t.Fatalf("not brout's error shape: %s", r.raw)
	}
	return detail["error_type"].(string), detail["message"].(string)
}

var requestID = regexp.MustCompile(`^20261025-153012-[0-9a-f]{6}$`)

func TestNamedModelAnswers(t *testing.T) {
	ts := newFake(t, replies(200, liveAnswer))
	subq := newFake(t, answers("subq-snap-1.1"))
	g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"subq/subq-snap-1.1"}})

	r := g.post(t, request(`"typesafe/jev-latest"`))
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.raw)
	}
	if id := r.header.Get("Brout-Request-Id"); !requestID.MatchString(id) {
		t.Errorf("Brout-Request-Id %q", id)
	}
	want := strings.Replace(liveAnswer, `"jev-1.14.0"`, `"typesafe/jev-1.14.0"`, 1)
	var got, wantBody any
	json.Unmarshal(r.raw, &got)
	json.Unmarshal([]byte(want), &wantBody)
	if fmt.Sprint(got) != fmt.Sprint(wantBody) {
		t.Errorf("answer\n got %s\nwant %s", r.raw, want)
	}

	if ts.calls() != 1 || subq.calls() != 0 {
		t.Fatalf("calls: typesafe %d, subq %d", ts.calls(), subq.calls())
	}
	if ts.auth[0] != "Bearer key-typesafe" {
		t.Errorf("Authorization %q", ts.auth[0])
	}
	sent := string(ts.bodies[0])
	for _, part := range []string{
		`"model":"jev-latest"`,
		`"state":"The user wrote: I love this product, it works great!"`,
		`"criteria":{"praise":"praising","complaint":null}`, // choices keep their order
		`"criteria":["not","somewhat","very"]`,
	} {
		if !strings.Contains(sent, part) {
			t.Errorf("sent body lacks %s:\n%s", part, sent)
		}
	}

	ex := g.exchanges()
	if len(ex) != 1 {
		t.Fatalf("%d exchanges", len(ex))
	}
	e := ex[0]
	if e.ID != r.header.Get("Brout-Request-Id") || e.RequestedModel != "typesafe/jev-latest" || e.Model != "typesafe/jev-1.14.0" ||
		e.Status != 200 || !bytes.Equal(e.Response, r.raw) || !bytes.Equal(e.Request, request(`"typesafe/jev-latest"`)) {
		t.Errorf("exchange %+v", e)
	}
	if len(e.Attempts) != 1 || e.Attempts[0] != (router.Attempt{Model: "typesafe/jev-latest", Status: 200}) {
		t.Errorf("attempts %+v", e.Attempts)
	}
}

func TestAutoRequestUsesRoutingList(t *testing.T) {
	for name, model := range map[string]string{"absent": "", "auto": `"auto"`, "null": "null"} {
		t.Run(name, func(t *testing.T) {
			ts := newFake(t, answers("jev-1.14.0"))
			subq := newFake(t, answers("subq-snap-1.1"))
			g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"subq/subq-snap-1.1", "typesafe/jev-latest"}})

			r := g.post(t, request(model))
			if r.status != 200 || r.body["model"] != "subq/subq-snap-1.1" {
				t.Fatalf("status %d: %s", r.status, r.raw)
			}
			if subq.calls() != 1 || ts.calls() != 0 {
				t.Errorf("calls: subq %d, typesafe %d", subq.calls(), ts.calls())
			}
			if subq.auth[0] != "Bearer key-subq" || !strings.Contains(string(subq.bodies[0]), `"model":"subq-snap-1.1"`) {
				t.Errorf("sent %s with %s", subq.bodies[0], subq.auth[0])
			}
			if e := g.exchanges()[0]; e.RequestedModel != "auto" {
				t.Errorf("requested model %q", e.RequestedModel)
			}
			if g.stderr.String() != "" {
				t.Errorf("stderr %q", g.stderr)
			}
		})
	}
}

func questions(n int) string {
	qs := make([]string, n)
	for i := range qs {
		qs[i] = fmt.Sprintf(`"q%d": {"type": "noul", "instructions": "?"}`, i)
	}
	return `{` + strings.Join(qs, ",") + `}`
}

func gzipped(t *testing.T, data []byte) []byte {
	var b bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	zw.Write(data)
	zw.Close()
	return b.Bytes()
}

// TestErrors covers every row of the error_type table.
func TestErrors(t *testing.T) {
	fixedClock := &fakeClock{}
	longName := strings.Repeat("n", 101)

	cases := []struct {
		name     string
		body     []byte
		header   []string
		model    http.HandlerFunc // what typesafe does; subq answers
		routing  []string
		status   int
		typ      string
		message  string
		reachesT bool     // typesafe is called
		stderr   []string // failover lines, after the request id
	}{
		{name: "malformed JSON", body: []byte(`{"state": `), status: 422, typ: "invalid_request", message: "not a JSON object"},
		{name: "not an object", body: []byte(`[1]`), status: 422, typ: "invalid_request", message: "not a JSON object"},
		{name: "unknown field", body: []byte(`{"state": "x", "questions": {}, "temperature": 1}`), status: 422, typ: "invalid_request", message: `unknown field "temperature"`},
		{name: "missing state", body: []byte(`{"questions": {"a": {"type": "noul"}}}`), status: 422, typ: "invalid_request", message: "state is required"},
		{name: "number state", body: []byte(`{"state": 3, "questions": {}}`), status: 422, typ: "invalid_request", message: "state must be a string, object or array"},
		{name: "bad question type", body: []byte(`{"state": "x", "questions": {"a": {"type": "rank"}}}`), status: 422, typ: "invalid_request", message: `question "a" has type "rank"`},
		{name: "unknown question field", body: []byte(`{"state": "x", "questions": {"a": {"type": "noul", "weight": 2}}}`), status: 422, typ: "invalid_request", message: `question "a" has unknown field "weight"`},
		{name: "choice without criteria", body: []byte(`{"state": "x", "questions": {"a": {"type": "choice"}}}`), status: 422, typ: "invalid_request", message: `choice question "a"`},
		{name: "noul with other criteria", body: []byte(`{"state": "x", "questions": {"a": {"type": "noul", "criteria": {"maybe": "?"}}}}`), status: 422, typ: "invalid_request", message: `criteria has "maybe"`},
		{name: "bad gzip", body: []byte("not gzip"), header: []string{"Content-Encoding", "gzip"}, status: 422, typ: "invalid_request", message: "not valid gzip"},
		{name: "unsupported encoding", body: []byte("x"), header: []string{"Content-Encoding", "br"}, status: 415, typ: "invalid_request", message: `Content-Encoding "br"`},
		{name: "assets", body: []byte(`{"state": "x", "questions": {}, "assets": {}}`), status: 422, typ: "unsupported", message: `"assets"`},
		{name: "images", body: []byte(`{"state": "x", "questions": {}, "images": []}`), status: 422, typ: "unsupported", message: `"images"`},
		{name: "box", body: []byte(`{"state": "x", "questions": {"where": {"type": "box", "instructions": "?"}}}`), status: 422, typ: "unsupported", message: `question "where" is a box`},
		{name: "bare model name", body: request(`"jev-latest"`), status: 422, typ: "unknown_model", message: "subq/jev-latest or typesafe/jev-latest"},
		{name: "unconfigured provider", body: request(`"openai/gpt-6-luna"`), status: 422, typ: "unknown_model", message: `provider "openai", which is not configured; configured providers: subq, typesafe`},
		{
			name: "limit", body: fmt.Appendf(nil, `{"model": "subq/subq-snap-1.1", "state": "x", "questions": %s}`, questions(130)),
			status: 422, typ: "limit", message: "subq/subq-snap-1.1 accepts at most 128 questions; the request has 130",
		},
		{
			name: "no model fits", routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"},
			body:   []byte(`{"state": "x", "questions": {"s": {"type": "score", "criteria": [0,1,2,3,4,5,6,7,8,9,10]}}}`),
			status: 422, typ: "no_model_fits",
			message: `typesafe/jev-latest accepts at most 10 score levels; question "s" has 11; subq/subq-snap-1.1 accepts at most 10 score levels; question "s" has 11`,
		},
		{
			name: "rejected", body: request(`"typesafe/jev-latest"`), model: replies(400, `{"detail":{"error_type":"api_usage_error","message":"Invalid request."}}`),
			status: 400, typ: "rejected", message: "typesafe/jev-latest answered 400: api_usage_error: Invalid request.", reachesT: true,
		},
		{
			name: "rejected for too many tokens", body: request(`"typesafe/jev-latest"`), model: replies(400, `{"detail":{"error_type":"max_tokens_exceeded"}}`),
			status: 400, typ: "rejected", message: "typesafe/jev-latest answered 400: max_tokens_exceeded", reachesT: true,
		},
		{
			name: "rejected with a detail list", body: request(`"typesafe/jev-latest"`), model: replies(422, `{"detail":[{"type":"missing","loc":["body","state"],"msg":"Field required"}]}`),
			status: 422, typ: "rejected", message: `typesafe/jev-latest answered 422: [{"type":"missing","loc":["body","state"],"msg":"Field required"}]`, reachesT: true,
		},
		{
			name: "named model unavailable", body: request(`"typesafe/jev-latest"`), model: replies(529, `{"detail":"overloaded"}`),
			status: 529, typ: "unavailable", message: "typesafe/jev-latest answered 529: overloaded", reachesT: true,
		},
		{
			name: "named model connection closed", body: request(`"typesafe/jev-latest"`), model: hangsUp,
			status: 502, typ: "unavailable", message: "typesafe/jev-latest: ", reachesT: true,
		},
		{
			name: "named model timeout", body: request(`"typesafe/jev-latest"`), model: timesOut(fixedClock),
			status: 504, typ: "unavailable", message: "typesafe/jev-latest did not answer within 4s", reachesT: true,
		},
		{
			name: "every routing model unavailable", body: request(""), model: replies(503, `{"detail":"down"}`), routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"},
			status: 503, typ: "unavailable",
			message:  "every model in the routing list was unavailable: typesafe/jev-latest answered 503: down; subq/subq-snap-1.1 answered 429: rate_limit_error: slow down",
			reachesT: true,
			stderr: []string{
				"typesafe/jev-latest unavailable (503); trying subq/subq-snap-1.1",
				"subq/subq-snap-1.1 unavailable (429); no models left",
			},
		},
		{
			name: "body too large", body: tooLarge(t), header: []string{"Content-Encoding", "gzip"},
			status: 413, typ: "body_too_large", message: "up to 67108864 bytes (64 MiB)",
		},
		{
			name: "question name over the limit", body: fmt.Appendf(nil, `{"model": "subq/subq-snap-1.1", "state": "x", "questions": {%q: {"type": "noul", "instructions": "?"}}}`, longName),
			status: 422, typ: "limit", message: fmt.Sprintf("subq/subq-snap-1.1 accepts question names of at most 100 characters; question %q has 101", longName),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := c.model
			if h == nil {
				h = answers("jev-1.14.0")
			}
			ts := newFake(t, h)
			subq := newFake(t, answers("subq-snap-1.1"))
			if c.name == "every routing model unavailable" {
				subq = newFake(t, replies(429, `{"detail":{"error_type":"rate_limit_error","message":"slow down"}}`))
			}
			routing := c.routing
			if routing == nil {
				routing = []string{"typesafe/jev-latest"}
			}
			clk := &fakeClock{}
			if c.name == "named model timeout" {
				clk = fixedClock
			}
			g := newGateway(t, setup{typesafe: ts, subq: subq, routing: routing, clock: clk})

			r := g.post(t, c.body, c.header...)
			if r.status != c.status {
				t.Errorf("status %d, want %d: %s", r.status, c.status, r.raw)
			}
			typ, msg := r.errorOf(t)
			if typ != c.typ || !strings.Contains(msg, c.message) {
				t.Errorf("got %s %q\nwant %s %q", typ, msg, c.typ, c.message)
			}
			if !requestID.MatchString(r.header.Get("Brout-Request-Id")) {
				t.Errorf("Brout-Request-Id %q", r.header.Get("Brout-Request-Id"))
			}
			if (ts.calls() > 0) != c.reachesT {
				t.Errorf("typesafe called %d times", ts.calls())
			}
			ex := g.exchanges()
			if len(ex) != 1 || ex[0].Status != c.status || !bytes.Equal(ex[0].Response, r.raw) {
				t.Fatalf("exchanges %+v", ex)
			}
			var want string
			for _, line := range c.stderr {
				want += ex[0].ID + " " + line + "\n"
			}
			if g.stderr.String() != want {
				t.Errorf("stderr %q, want %q", g.stderr, want)
			}
		})
	}
}

// tooLarge is a gzip body that decompresses past server.BodyLimit: one 1 MiB
// gzip member repeated, which gzip readers decode as one stream.
func tooLarge(t *testing.T) []byte {
	member := gzipped(t, bytes.Repeat([]byte("a"), 1<<20))
	return bytes.Repeat(member, server.BodyLimit>>20+1)
}

// TestFailover sends an auto request on to the next model for each
// unavailable outcome (D8).
func TestFailover(t *testing.T) {
	cases := []struct {
		name   string
		h      http.HandlerFunc
		status int // the failed attempt's status
		line   string
	}{
		{"401", replies(401, `{"detail":"bad key"}`), 401, "(401)"},
		{"402", replies(402, `{"detail":"no credits"}`), 402, "(402)"},
		{"403", replies(403, `{"detail":"forbidden"}`), 403, "(403)"},
		{"429", replies(429, `{"detail":"slow down"}`), 429, "(429)"},
		{"500", replies(500, `oops`), 500, "(500)"},
		{"503", replies(503, `{"detail":"down"}`), 503, "(503)"},
		{"529", replies(529, `{"detail":"overloaded"}`), 529, "(529)"},
		{"connection closed", hangsUp, 0, "(no response)"},
		{"timeout", nil, 0, "(no response)"},
		{"unreadable 200", replies(200, `{"answers": "nope"}`), 0, "(no response)"},
		{"200 without answers", replies(200, `{}`), 0, "(no response)"},
		{"200 missing an answer", replies(200, strings.Replace(liveAnswer, `"positive":{"type":"noul","noul":0.98},`, "", 1)), 0, "(no response)"},
		{"200 with a wrong answer type", replies(200, strings.Replace(liveAnswer, `"positive":{"type":"noul","noul":0.98}`, `"positive":{"type":"score","score":1}`, 1)), 0, "(no response)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clk := &fakeClock{}
			h := c.h
			if h == nil {
				h = timesOut(clk)
			}
			ts := newFake(t, h)
			subq := newFake(t, answers("subq-snap-1.1"))
			g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"}, clock: clk})

			r := g.post(t, request(""))
			if r.status != 200 || r.body["model"] != "subq/subq-snap-1.1" {
				t.Fatalf("status %d: %s", r.status, r.raw)
			}
			e := g.exchanges()[0]
			if len(e.Attempts) != 2 || e.Attempts[0].Status != c.status || e.Attempts[0].Error == "" ||
				e.Attempts[1] != (router.Attempt{Model: "subq/subq-snap-1.1", Status: 200}) {
				t.Errorf("attempts %+v", e.Attempts)
			}
			want := e.ID + " typesafe/jev-latest unavailable " + c.line + "; trying subq/subq-snap-1.1\n"
			if g.stderr.String() != want {
				t.Errorf("stderr %q, want %q", g.stderr, want)
			}
		})
	}
}

func TestRejectionDoesNotFailOver(t *testing.T) {
	for _, status := range []int{400, 404, 413, 415, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ts := newFake(t, replies(status, `{"detail":"caller bug"}`))
			subq := newFake(t, answers("subq-snap-1.1"))
			g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"}})

			r := g.post(t, request(""))
			if typ, _ := r.errorOf(t); r.status != status || typ != "rejected" {
				t.Errorf("status %d: %s", r.status, r.raw)
			}
			if subq.calls() != 0 || g.stderr.String() != "" {
				t.Errorf("subq called %d times; stderr %q", subq.calls(), g.stderr)
			}
		})
	}
}

func TestNamedModelDoesNotFailOver(t *testing.T) {
	ts := newFake(t, replies(529, `{"detail":"overloaded"}`))
	subq := newFake(t, answers("subq-snap-1.1"))
	g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"}})

	r := g.post(t, request(`"typesafe/jev-latest"`))
	if typ, _ := r.errorOf(t); r.status != 529 || typ != "unavailable" {
		t.Errorf("status %d: %s", r.status, r.raw)
	}
	if subq.calls() != 0 || g.stderr.String() != "" {
		t.Errorf("subq called %d times; stderr %q", subq.calls(), g.stderr)
	}
}

func TestAutoSkipsModelsWhoseLimitsRejectRequest(t *testing.T) {
	ts := newFake(t, answers("jev-1.14.0"))
	subq := newFake(t, answers("subq-snap-1.1"))
	g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"subq/subq-snap-1.1", "typesafe/jev-latest"}})

	// SubQ takes 2 to 255 choice options; TypeSafe takes 1.
	r := g.post(t, []byte(`{"state": "x", "questions": {"c": {"type": "choice", "criteria": {"only": null}}}}`))
	if r.status != 200 || r.body["model"] != "typesafe/jev-1.14.0" || subq.calls() != 0 {
		t.Errorf("status %d, subq calls %d: %s", r.status, subq.calls(), r.raw)
	}
}

func TestAutoSkipsModelsWhoseNameLimitRejectsRequest(t *testing.T) {
	ts := newFake(t, answers("jev-1.14.0"))
	subq := newFake(t, answers("subq-snap-1.1"))
	g := newGateway(t, setup{typesafe: ts, subq: subq, routing: []string{"subq/subq-snap-1.1", "typesafe/jev-latest"}})

	r := g.post(t, fmt.Appendf(nil, `{"state": "x", "questions": {%q: {"type": "noul", "instructions": "?"}}}`, strings.Repeat("n", 101)))
	if r.status != 200 || r.body["model"] != "typesafe/jev-1.14.0" || subq.calls() != 0 {
		t.Errorf("status %d, subq calls %d: %s", r.status, subq.calls(), r.raw)
	}
}

func TestCompressedBodies(t *testing.T) {
	body := request(`"typesafe/jev-latest"`)
	var zbuf bytes.Buffer
	zw, _ := zstd.NewWriter(&zbuf)
	zw.Write(body)
	zw.Close()

	for enc, data := range map[string][]byte{"gzip": gzipped(t, body), "x-gzip": gzipped(t, body), "zstd": zbuf.Bytes()} {
		t.Run(enc, func(t *testing.T) {
			ts := newFake(t, answers("jev-1.14.0"))
			g := newGateway(t, setup{typesafe: ts, routing: []string{"typesafe/jev-latest"}})

			r := g.post(t, data, "Content-Encoding", enc)
			if r.status != 200 {
				t.Fatalf("status %d: %s", r.status, r.raw)
			}
			if !strings.Contains(string(ts.bodies[0]), `"state":"The user wrote: I love this product, it works great!"`) {
				t.Errorf("sent %s", ts.bodies[0])
			}
			if !bytes.Equal(g.exchanges()[0].Request, body) {
				t.Errorf("exchange request is not the decompressed body")
			}
		})
	}
}

func TestCallerGoesAway(t *testing.T) {
	arrived := make(chan struct{})
	ts := newFake(t, blocks(arrived))
	finished := make(chan server.Exchange, 1)
	g := newGateway(t, setup{typesafe: ts, routing: []string{"typesafe/jev-latest"}, finished: finished})

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/systemone", bytes.NewReader(request("")))
	go func() {
		<-arrived
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("request succeeded after cancel")
	}
	e := <-finished
	if e.Status != router.StatusCanceled || len(e.Attempts) != 1 || e.Attempts[0].Model != "typesafe/jev-latest" {
		t.Errorf("exchange %+v", e)
	}
}

func TestModels(t *testing.T) {
	g := newGateway(t, setup{typesafe: newFake(t, answers("x")), subq: newFake(t, answers("y")), routing: []string{"typesafe/jev-latest", "subq/subq-snap-1.1"}})
	req, _ := http.NewRequest(http.MethodGet, g.url+"/v1/models", nil)
	r := do(t, req)
	want := `{"models":[{"name":"auto","description":"tries the routing list in order: typesafe/jev-latest, subq/subq-snap-1.1"},{"name":"typesafe/jev-latest","description":"routing 1"},{"name":"subq/subq-snap-1.1","description":"routing 2"}]}`
	if r.status != 200 || strings.TrimSpace(string(r.raw)) != want {
		t.Errorf("status %d:\n got %s\nwant %s", r.status, r.raw, want)
	}
	if !requestID.MatchString(r.header.Get("Brout-Request-Id")) {
		t.Errorf("Brout-Request-Id %q", r.header.Get("Brout-Request-Id"))
	}
}

func TestErrorMessagesKeepAngleBrackets(t *testing.T) {
	g := newGateway(t, setup{typesafe: newFake(t, answers("x")), routing: []string{"typesafe/jev-latest"}})
	r := g.post(t, request(`"jev-latest"`))
	if !strings.Contains(string(r.raw), "<provider>/<model>") {
		t.Errorf("body %s", r.raw)
	}
}

func TestUnknownRoute(t *testing.T) {
	g := newGateway(t, setup{routing: nil})
	req, _ := http.NewRequest(http.MethodGet, g.url+"/v1/systemone", nil)
	r := do(t, req)
	if typ, msg := r.errorOf(t); r.status != 404 || typ != "not_found" || !strings.Contains(msg, "GET /v1/systemone") {
		t.Errorf("status %d: %s", r.status, r.raw)
	}
	if !requestID.MatchString(r.header.Get("Brout-Request-Id")) {
		t.Errorf("Brout-Request-Id %q", r.header.Get("Brout-Request-Id"))
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
