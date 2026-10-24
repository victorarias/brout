// Package router picks the model for a request: the one the caller named, or
// the routing list in order, failing over when an attempt is unavailable.
package router

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/victorarias/brout/internal/decision"
	"github.com/victorarias/brout/internal/provider"
)

// StatusCanceled is the status of a request whose caller went away.
const StatusCanceled = 499

// Router decides requests.
type Router struct {
	providers map[string]provider.Provider
	routing   []string
	timeout   time.Duration
	clock     Clock
}

// Clock is the time source for attempt durations and the attempt timeout.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f after d, unless stop is called first.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) AfterFunc(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }

var errAttemptTimeout = errors.New("attempt timeout")

// Attempt is one call to one provider.
type Attempt struct {
	Model    string // model id
	Status   int    // the provider's status; 0 for a transport error or timeout
	Error    string // empty on success
	Duration time.Duration
}

// New returns a router. routing holds model ids whose providers are in
// providers; config loading checks that.
func New(providers map[string]provider.Provider, routing []string, timeout time.Duration, clock Clock) *Router {
	return &Router{providers: providers, routing: routing, timeout: timeout, clock: clock}
}

// Routing returns the routing list.
func (r *Router) Routing() []string { return r.routing }

// Decide answers req. An error is a *decision.Error.
func (r *Router) Decide(ctx context.Context, req decision.Request) (decision.Response, []Attempt, error) {
	if req.Model != "" {
		return r.decideNamed(ctx, req)
	}

	var fits, reasons []string
	for _, id := range r.routing {
		if why := r.providers[providerOf(id)].Limits().Check(id, req); why != "" {
			reasons = append(reasons, why)
			continue
		}
		fits = append(fits, id)
	}
	if len(fits) == 0 {
		msg := "the routing list is empty"
		if len(reasons) > 0 {
			msg = "no model in the routing list takes this request: " + strings.Join(reasons, "; ")
		}
		return decision.Response{}, nil, &decision.Error{Status: http.StatusUnprocessableEntity, Type: "no_model_fits", Message: msg}
	}

	var attempts []Attempt
	for _, id := range fits {
		res, a, err, _ := r.attempt(ctx, id, req)
		attempts = append(attempts, a)
		if err == nil {
			return res, attempts, nil
		}
		if ctx.Err() != nil {
			return decision.Response{}, attempts, canceled()
		}
		if !err.Unavailable {
			return decision.Response{}, attempts, &decision.Error{Status: err.Status, Type: "rejected", Message: err.Message}
		}
	}
	failed := make([]string, len(attempts))
	for i, a := range attempts {
		failed[i] = a.Error
	}
	return decision.Response{}, attempts, &decision.Error{
		Status:  http.StatusServiceUnavailable,
		Type:    "unavailable",
		Message: "every model in the routing list was unavailable: " + strings.Join(failed, "; "),
	}
}

func (r *Router) decideNamed(ctx context.Context, req decision.Request) (decision.Response, []Attempt, error) {
	id := req.Model
	name, model, ok := strings.Cut(id, "/")
	if !ok {
		return decision.Response{}, nil, &decision.Error{
			Status:  http.StatusUnprocessableEntity,
			Type:    "unknown_model",
			Message: fmt.Sprintf("model %q is not a model id; brout names models <provider>/<model>, such as %s", id, r.suggest(id)),
		}
	}
	p, ok := r.providers[name]
	if !ok || model == "" {
		return decision.Response{}, nil, &decision.Error{
			Status:  http.StatusUnprocessableEntity,
			Type:    "unknown_model",
			Message: fmt.Sprintf("model %q names provider %q, which is not configured; configured providers: %s", id, name, strings.Join(r.names(), ", ")),
		}
	}
	if why := p.Limits().Check(id, req); why != "" {
		return decision.Response{}, nil, &decision.Error{Status: http.StatusUnprocessableEntity, Type: "limit", Message: why}
	}

	res, a, err, timedOut := r.attempt(ctx, id, req)
	attempts := []Attempt{a}
	switch {
	case err == nil:
		return res, attempts, nil
	case ctx.Err() != nil:
		return decision.Response{}, attempts, canceled()
	case !err.Unavailable:
		return decision.Response{}, attempts, &decision.Error{Status: err.Status, Type: "rejected", Message: err.Message}
	}
	status := err.Status
	switch {
	case timedOut:
		status = http.StatusGatewayTimeout
	case status == 0:
		status = http.StatusBadGateway
	}
	return decision.Response{}, attempts, &decision.Error{Status: status, Type: "unavailable", Message: err.Message}
}

// attempt makes one call, bounded by the attempt timeout. timedOut reports
// that the attempt timeout ended it.
func (r *Router) attempt(ctx context.Context, id string, req decision.Request) (res decision.Response, a Attempt, perr *provider.Error, timedOut bool) {
	name, model, _ := strings.Cut(id, "/")
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stop := r.clock.AfterFunc(r.timeout, func() { cancel(errAttemptTimeout) })
	defer stop()

	start := r.clock.Now()
	res, err := r.providers[name].Decide(actx, model, req)
	a = Attempt{Model: id, Duration: r.clock.Now().Sub(start)}
	if err == nil {
		a.Status = http.StatusOK
		if res.Model == "" {
			res.Model = model
		}
		res.Model = name + "/" + res.Model
		return res, a, nil, false
	}

	if !errors.As(err, &perr) {
		perr = &provider.Error{Unavailable: true, Message: fmt.Sprintf("%s: %v", id, err)}
	}
	if perr.Status == 0 && ctx.Err() == nil && context.Cause(actx) == errAttemptTimeout {
		perr = &provider.Error{Unavailable: true, Message: fmt.Sprintf("%s did not answer within %s", id, r.timeout)}
		timedOut = true
	}
	a.Status = perr.Status
	a.Error = perr.Message
	return decision.Response{}, a, perr, timedOut
}

func canceled() *decision.Error {
	return &decision.Error{Status: StatusCanceled, Type: "canceled", Message: "the caller went away before an answer arrived"}
}

// suggest proposes model ids for a bare model name, one per provider.
func (r *Router) suggest(model string) string {
	names := r.names()
	ids := make([]string, len(names))
	for i, n := range names {
		ids[i] = n + "/" + model
	}
	if len(ids) == 0 {
		return "typesafe/" + model
	}
	return strings.Join(ids, " or ")
}

func (r *Router) names() []string {
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func providerOf(id string) string {
	name, _, _ := strings.Cut(id, "/")
	return name
}
