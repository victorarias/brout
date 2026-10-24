// Package systemone is the adapter for providers that serve System One's
// POST /v1/systemone: TypeSafe and SubQ share one wire and differ only in
// base URL and limits (F1).
package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/victorarias/brout/internal/decision"
	"github.com/victorarias/brout/internal/provider"
)

// Known providers that speak System One, with their documented limits (F3).
var (
	TypeSafe = Spec{
		BaseURL: "https://api.typesafe.ai",
		Limits:  provider.Limits{ChoiceOptions: provider.Range{Min: 1, Max: 255}, ScoreLevels: provider.Range{Min: 1, Max: 10}},
	}
	SubQ = Spec{
		BaseURL: "https://api.subq.ai",
		Limits: provider.Limits{
			Questions:     provider.Range{Min: 1, Max: 128},
			ChoiceOptions: provider.Range{Min: 2, Max: 255},
			ScoreLevels:   provider.Range{Min: 2, Max: 10},
			NameLength:    100,
		},
	}
)

// Spec is what differs between System One providers.
type Spec struct {
	BaseURL string
	Limits  provider.Limits
}

// Adapter calls one System One provider.
type Adapter struct {
	name   string // provider name, such as "typesafe"; prefixes model ids in messages
	spec   Spec
	apiKey string
	client *http.Client
}

func New(name string, spec Spec, apiKey string, client *http.Client) *Adapter {
	spec.BaseURL = strings.TrimRight(spec.BaseURL, "/")
	return &Adapter{name: name, spec: spec, apiKey: apiKey, client: client}
}

func (a *Adapter) Limits() provider.Limits { return a.spec.Limits }

// errorBodyLimit is a tripwire on how much of a provider's error body brout
// reads. Error bodies seen from TypeSafe are under 300 bytes.
const errorBodyLimit = 1 << 20

func (a *Adapter) Decide(ctx context.Context, model string, req decision.Request) (decision.Response, error) {
	modelID := a.name + "/" + model
	body, err := json.Marshal(struct {
		Model     string                       `json:"model"`
		State     json.RawMessage              `json:"state"`
		Questions map[string]decision.Question `json:"questions"`
	}{model, req.State, req.Questions})
	if err != nil {
		return decision.Response{}, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.spec.BaseURL+"/v1/systemone", bytes.NewReader(body))
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
			Message:     fmt.Sprintf("%s answered %d: %s", modelID, resp.StatusCode, detail(raw)),
		}
	}
	var res decision.Response
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return decision.Response{}, &provider.Error{Unavailable: true, Message: fmt.Sprintf("%s answered 200 with a body brout cannot read: %v", modelID, err)}
	}
	return res, nil
}

// detail quotes a provider's error body. TypeSafe and SubQ send "detail" as
// {error_type, message}, as a list of {loc, msg, type}, or as a string.
func detail(raw []byte) string {
	var body struct{ Detail json.RawMessage }
	if json.Unmarshal(raw, &body) != nil || len(body.Detail) == 0 {
		return strings.TrimSpace(string(raw))
	}
	var shaped struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(body.Detail, &shaped) == nil {
		switch {
		case shaped.ErrorType != "" && shaped.Message != "":
			return shaped.ErrorType + ": " + shaped.Message
		case shaped.ErrorType != "":
			return shaped.ErrorType // TypeSafe's max_tokens_exceeded has no message
		}
	}
	var text string
	if json.Unmarshal(body.Detail, &text) == nil {
		return text
	}
	var buf bytes.Buffer
	if json.Compact(&buf, body.Detail) == nil {
		return buf.String()
	}
	return string(body.Detail)
}
