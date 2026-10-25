// Package openai is the adapter for OpenAI's POST /v1/decisions. It
// translates brout's System One types to OpenAI's shape and back (F2).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/victorarias/brout/internal/decision"
	"github.com/victorarias/brout/internal/provider"
)

const DefaultBaseURL = "https://api.openai.com"

// Limits were measured against gpt-6-luna on 2026-10-25: 401 questions, 1 or
// 256 choices, and 1 or 11 levels each got a 400 that names the bound. A
// 300-character question name was accepted.
var Limits = provider.Limits{
	Questions:     provider.Range{Min: 1, Max: 400},
	ChoiceOptions: provider.Range{Min: 2, Max: 255},
	ScoreLevels:   provider.Range{Min: 2, Max: 10},
}

// Adapter calls OpenAI's decisions API.
type Adapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func New(baseURL, apiKey string, client *http.Client) *Adapter {
	return &Adapter{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, client: client}
}

func (a *Adapter) Limits() provider.Limits { return Limits }

// errorBodyLimit is a tripwire on how much of an error body brout reads.
// OpenAI's error bodies seen live are under 400 bytes.
const errorBodyLimit = 1 << 20

func (a *Adapter) Decide(ctx context.Context, model string, req decision.Request) (decision.Response, error) {
	modelID := "openai/" + model
	body, err := decision.Encode(translate(model, req))
	if err != nil {
		return decision.Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/v1/decisions", bytes.NewReader(body))
	if err != nil {
		return decision.Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.client.Do(hreq)
	if err != nil {
		return decision.Response{}, &provider.Error{Unavailable: true, Message: fmt.Sprintf("%s: %v", modelID, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
		return decision.Response{}, &provider.Error{
			Unavailable: provider.Unavailable(resp.StatusCode),
			Status:      resp.StatusCode,
			Message:     fmt.Sprintf("%s answered %d: %s", modelID, resp.StatusCode, errorMessage(raw)),
		}
	}
	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return decision.Response{}, &provider.Error{Unavailable: true, Message: fmt.Sprintf("%s answered 200 with a body brout cannot read: %v", modelID, err)}
	}
	if refused := out.refused(); len(refused) > 0 {
		// D12: a refusal is a rejection, a second provider would hide it.
		return decision.Response{}, &provider.Error{
			Status:  http.StatusUnprocessableEntity,
			Message: fmt.Sprintf("%s refused to answer %s", modelID, quoteAll(refused)),
		}
	}
	return out.translate(req), nil
}

// The request side of /v1/decisions.
type request struct {
	Model     string     `json:"model"`
	Input     any        `json:"input"`
	Questions []question `json:"questions"`
}

type question struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"` // required, "" is accepted
	Choices      []choice `json:"choices,omitempty"`
	Levels       []level  `json:"levels,omitempty"`
}

type choice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

type level struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type message struct {
	Role    string    `json:"role"`
	Content []content `json:"content"`
}

type content struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func translate(model string, req decision.Request) request {
	out := request{Model: model, Input: json.RawMessage(req.State)}
	switch req.State[0] {
	case '{':
		out.Input = inputJSON(req.State)
	case '[':
		// input_json data must be an object or an array of objects; an array
		// of strings is a 400. Every array goes wrapped, so the rule is one.
		out.Input = inputJSON(append(append([]byte(`{"state":`), req.State...), '}'))
	}
	names := make([]string, 0, len(req.Questions))
	for name := range req.Questions {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		q := req.Questions[name]
		oq := question{Name: name, Type: string(q.Type), Instructions: text(q.Instructions)}
		switch q.Type {
		case decision.Noul:
			oq.Type = "predicate"
			// /v1/decisions has no criteria for a predicate.
			if q.TrueFalse != nil {
				for _, v := range []string{"true", "false"} {
					if d := text(q.TrueFalse[v]); d != "" {
						oq.Instructions += fmt.Sprintf("\n\n%s means: %s", v, d)
					}
				}
				oq.Instructions = strings.TrimPrefix(oq.Instructions, "\n\n")
			}
		case decision.Choice:
			for _, o := range q.Choices {
				oq.Choices = append(oq.Choices, choice{Value: o.Name, Description: text(o.Description)})
			}
		case decision.Score:
			for i, d := range q.Levels {
				oq.Levels = append(oq.Levels, level{Label: strconv.Itoa(i), Description: text(d)})
			}
		}
		out.Questions = append(out.Questions, oq)
	}
	return out
}

func inputJSON(data json.RawMessage) []message {
	return []message{{Role: "user", Content: []content{{Type: "input_json", Data: data}}}}
}

// text renders a System One description or instructions as OpenAI's text: a
// string as is, null or absent as "", anything else as compact JSON.
func text(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) == nil {
		return b.String()
	}
	return string(raw)
}

// The response side of /v1/decisions, as captured live in testdata.
type response struct {
	Model   string   `json:"model"`
	Answers []answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type answer struct {
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Probability   *float64      `json:"probability"` // predicate
	Choice        *string       `json:"choice"`
	Score         *float64      `json:"score"`
	Confidence    *float64      `json:"confidence"`
	Probabilities []probability `json:"probabilities"`
}

type probability struct {
	Value       json.RawMessage `json:"value"` // the choice; the level index for a score
	Label       string          `json:"label"` // score only
	Probability float64         `json:"probability"`
}

func (r response) refused() []string {
	var names []string
	for _, a := range r.Answers {
		if a.Type == "refusal" {
			names = append(names, a.Name)
		}
	}
	return names
}

// translate maps the answers back to System One. An answer whose type does
// not match its question is passed on as is; the router rejects it.
func (r response) translate(req decision.Request) decision.Response {
	res := decision.Response{
		Model:   r.Model,
		Answers: make(map[string]decision.Answer, len(r.Answers)),
		Usage:   decision.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens},
	}
	for _, a := range r.Answers {
		out := decision.Answer{Type: decision.Type(a.Type), Confidence: a.Confidence}
		switch a.Type {
		case "predicate":
			out.Type = decision.Noul
			out.Noul = a.Probability
		case "choice":
			out.Choice = a.Choice
			out.Probabilities = map[string]float64{}
			for _, p := range a.Probabilities {
				var v string
				json.Unmarshal(p.Value, &v)
				out.Probabilities[v] = p.Probability
			}
		case "score":
			out.Score = a.Score
			out.Probabilities = map[string]float64{}
			for _, p := range a.Probabilities {
				out.Probabilities[p.Label] = p.Probability
			}
			// Levels are labeled by index, so the legend is the request's criteria.
			out.Legend = map[string]json.RawMessage{}
			for i, d := range req.Questions[a.Name].Levels {
				out.Legend[strconv.Itoa(i)] = d
			}
		}
		res.Answers[a.Name] = out
	}
	return res
}

// errorMessage quotes OpenAI's {"error": {"message": ...}}, or the raw body.
func errorMessage(raw []byte) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Error.Message == "" {
		return strings.TrimSpace(string(raw))
	}
	if body.Error.Code != "" {
		return body.Error.Code + ": " + body.Error.Message
	}
	return body.Error.Message
}

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = strconv.Quote(n)
	}
	if len(q) == 1 {
		return "question " + q[0]
	}
	return "questions " + strings.Join(q, ", ")
}
