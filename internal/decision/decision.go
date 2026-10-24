// Package decision holds brout's System One request and response types.
//
// A request is parsed once, into these types, and every provider adapter
// works from them.
package decision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// Type is a question or answer type.
type Type string

const (
	Noul   Type = "noul"
	Choice Type = "choice"
	Score  Type = "score"
)

// Request is a parsed System One request.
type Request struct {
	Model     string          // "" for an auto request, otherwise what the caller sent
	State     json.RawMessage // sent on byte for byte
	Questions map[string]Question
}

// Question is one named question of a request.
type Question struct {
	Type         Type
	Instructions json.RawMessage            // absent, string, object or array
	TrueFalse    map[string]json.RawMessage // noul: "true" and "false" descriptions, or nil
	Choices      []Option                   // choice: in body order
	Levels       []json.RawMessage          // score: the index is the level
}

// Option is one choice of a choice question.
type Option struct {
	Name        string
	Description json.RawMessage // string, object, array or null
}

// Response is a System One answer.
type Response struct {
	Model   string            `json:"model"` // the model id that answered, such as "typesafe/jev-1.13.0"
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Answer is the answer to one question.
type Answer struct {
	Type          Type                       `json:"type"`
	Noul          *float64                   `json:"noul,omitempty"`
	Choice        *string                    `json:"choice,omitempty"`
	Score         *float64                   `json:"score,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
	Legend        map[string]json.RawMessage `json:"legend,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
}

// Error is an error brout returns to its caller, in the shape
// {"detail": {"error_type": ..., "message": ...}}.
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string { return e.Type + ": " + e.Message }

// MarshalJSON encodes the error in brout's error shape.
func (e *Error) MarshalJSON() ([]byte, error) {
	type detail struct {
		ErrorType string `json:"error_type"`
		Message   string `json:"message"`
	}
	return Encode(struct {
		Detail detail `json:"detail"`
	}{detail{e.Type, e.Message}})
}

// Encode is json.Marshal without HTML escaping, so messages keep their < and >.
func Encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func invalid(format string, args ...any) *Error {
	return &Error{http.StatusUnprocessableEntity, "invalid_request", fmt.Sprintf(format, args...)}
}

func unsupported(format string, args ...any) *Error {
	return &Error{http.StatusUnprocessableEntity, "unsupported", fmt.Sprintf(format, args...)}
}

// MarshalJSON encodes the question in System One's wire form, keeping the
// choices in their order.
func (q Question) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"type":`)
	t, _ := json.Marshal(q.Type)
	b.Write(t)
	if len(q.Instructions) > 0 {
		b.WriteString(`,"instructions":`)
		b.Write(q.Instructions)
	}
	switch q.Type {
	case Noul:
		if q.TrueFalse != nil {
			c, err := json.Marshal(q.TrueFalse)
			if err != nil {
				return nil, err
			}
			b.WriteString(`,"criteria":`)
			b.Write(c)
		}
	case Choice:
		b.WriteString(`,"criteria":{`)
		for i, o := range q.Choices {
			if i > 0 {
				b.WriteByte(',')
			}
			name, _ := json.Marshal(o.Name)
			b.Write(name)
			b.WriteByte(':')
			if len(o.Description) == 0 {
				b.WriteString("null")
			} else {
				b.Write(o.Description)
			}
		}
		b.WriteByte('}')
	case Score:
		c, err := json.Marshal(q.Levels)
		if err != nil {
			return nil, err
		}
		b.WriteString(`,"criteria":`)
		b.Write(c)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
