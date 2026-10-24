// Package provider defines what brout needs from an upstream decision-model API.
package provider

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/victorarias/brout/internal/decision"
)

// Provider is one upstream API, such as typesafe or subq.
type Provider interface {
	Limits() Limits
	// Decide sends req to the provider's model, the part of the model id
	// after "<provider>/". A failed call returns an *Error.
	Decide(ctx context.Context, model string, req decision.Request) (decision.Response, error)
}

// Limits are the request limits a provider documents (F3).
type Limits struct {
	Questions, ChoiceOptions, ScoreLevels Range // a zero Range is unknown and unchecked
}

// Range is an inclusive count range.
type Range struct{ Min, Max int }

// Check returns why modelID cannot take req, or "" when it can.
func (l Limits) Check(modelID string, req decision.Request) string {
	if msg := l.Questions.check(modelID, "questions", "the request has", len(req.Questions)); msg != "" {
		return msg
	}
	names := make([]string, 0, len(req.Questions))
	for name := range req.Questions {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		q := req.Questions[name]
		var msg string
		switch q.Type {
		case decision.Choice:
			msg = l.ChoiceOptions.check(modelID, "choice options", fmt.Sprintf("question %q has", name), len(q.Choices))
		case decision.Score:
			msg = l.ScoreLevels.check(modelID, "score levels", fmt.Sprintf("question %q has", name), len(q.Levels))
		}
		if msg != "" {
			return msg
		}
	}
	return ""
}

func (r Range) check(modelID, what, subject string, n int) string {
	switch {
	case r == Range{}:
		return ""
	case n < r.Min:
		return fmt.Sprintf("%s needs at least %d %s; %s %d", modelID, r.Min, what, subject, n)
	case r.Max > 0 && n > r.Max:
		return fmt.Sprintf("%s accepts at most %d %s; %s %d", modelID, r.Max, what, subject, n)
	}
	return ""
}

// Error is a failed call to a provider.
type Error struct {
	Unavailable bool   // D8: true sends an auto request on to the next model
	Status      int    // the provider's HTTP status; 0 for a transport error or timeout
	Message     string // names the model and quotes what the provider said
}

func (e *Error) Error() string { return e.Message }

// Unavailable reports whether a provider status sends an auto request on to
// the next model (D8). Every other 4xx is a rejection: a caller bug that a
// second provider would hide. 401 and 403 mean brout's key is wrong.
func Unavailable(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, 529:
		return true
	}
	return status >= 500
}
