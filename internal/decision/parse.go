package decision

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
)

// Parse reads a System One request body. It rejects what brout cannot
// route: malformed JSON, unknown fields, and images or box questions (D7).
func Parse(body []byte) (Request, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return Request{}, invalid("the body is not a JSON object: %v", err)
	}
	if top == nil {
		return Request{}, invalid("the body is not a JSON object: null")
	}
	for _, k := range sortedKeys(top) {
		switch k {
		case "model", "state", "questions":
		case "assets", "images":
			return Request{}, unsupported("brout v1 routes text and JSON state only; the request has %q", k)
		default:
			return Request{}, invalid("unknown field %q; a request has model, state and questions", k)
		}
	}

	var req Request
	if raw, ok := top["model"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return Request{}, invalid("model must be a string, got %s", raw)
		}
		if req.Model == "auto" {
			req.Model = ""
		}
	}

	state, ok := top["state"]
	if !ok || isNull(state) {
		return Request{}, invalid("state is required")
	}
	if c := state[0]; c != '"' && c != '{' && c != '[' {
		return Request{}, invalid("state must be a string, object or array, got %s", state)
	}
	req.State = state

	raw, ok := top["questions"]
	if !ok || isNull(raw) {
		return Request{}, invalid("questions is required")
	}
	var questions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &questions); err != nil {
		return Request{}, invalid("questions must be an object of named questions: %v", err)
	}
	req.Questions = make(map[string]Question, len(questions))
	for _, name := range sortedKeys(questions) {
		q, err := parseQuestion(name, questions[name])
		if err != nil {
			return Request{}, err
		}
		req.Questions[name] = q
	}
	return req, nil
}

func parseQuestion(name string, raw json.RawMessage) (Question, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Question{}, invalid("question %q must be an object", name)
	}
	for _, k := range sortedKeys(fields) {
		switch k {
		case "type", "instructions", "criteria":
		default:
			return Question{}, invalid("question %q has unknown field %q; a question has type, instructions and criteria", name, k)
		}
	}

	var q Question
	if err := json.Unmarshal(fields["type"], &q.Type); err != nil {
		return Question{}, invalid("question %q needs a type: noul, choice or score", name)
	}
	if raw := fields["instructions"]; !isNull(raw) {
		q.Instructions = raw
	}
	criteria := fields["criteria"]
	switch q.Type {
	case Noul:
		if isNull(criteria) {
			break
		}
		if err := json.Unmarshal(criteria, &q.TrueFalse); err != nil || q.TrueFalse == nil {
			return Question{}, invalid(`noul question %q: criteria must be an object with "true" and "false"`, name)
		}
		for k := range q.TrueFalse {
			if k != "true" && k != "false" {
				return Question{}, invalid(`noul question %q: criteria has %q; it takes only "true" and "false"`, name, k)
			}
		}
	case Choice:
		opts, err := orderedObject(criteria)
		if err != nil {
			return Question{}, invalid("choice question %q: criteria must be an object of choice names to descriptions", name)
		}
		q.Choices = opts
	case Score:
		if err := json.Unmarshal(criteria, &q.Levels); err != nil || q.Levels == nil {
			return Question{}, invalid("score question %q: criteria must be an array of level descriptions", name)
		}
	case "box":
		return Question{}, unsupported("brout v1 does not route box questions; question %q is a box", name)
	default:
		return Question{}, invalid("question %q has type %q; brout routes noul, choice and score", name, q.Type)
	}
	return q, nil
}

// orderedObject reads a JSON object's members in body order.
func orderedObject(raw json.RawMessage) ([]Option, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, io.ErrUnexpectedEOF
	}
	opts := []Option{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var desc json.RawMessage
		if err := dec.Decode(&desc); err != nil {
			return nil, err
		}
		if isNull(desc) {
			desc = nil
		}
		opts = append(opts, Option{Name: t.(string), Description: desc})
	}
	return opts, nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || strings.TrimSpace(string(raw)) == "null"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
