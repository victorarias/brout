// Package server is brout's HTTP surface: POST /v1/systemone and
// GET /v1/models.
package server

import (
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/victorarias/brout/internal/decision"
	"github.com/victorarias/brout/internal/router"
)

// BodyLimit is a tripwire on the decompressed request body (F7): TypeSafe,
// the largest any provider documents, accepts 32 MiB.
const BodyLimit = 64 << 20

// Exchange is one finished request, handed to Options.Finish once the outcome
// is known and before the response is written.
type Exchange struct {
	ID             string
	Start          time.Time
	Duration       time.Duration
	RequestedModel string // "auto" for an auto request; "" when the body did not parse
	Request        []byte // the decompressed body; nil when it could not be read
	Status         int    // the status brout answered with
	Response       []byte // the body brout answered with, answer or error
	Model          string // the model id that answered; "" when none did
	Attempts       []router.Attempt
	ReplayOf       string // the Brout-Replay-Of header: the id brout replay replays; "" otherwise
}

type Options struct {
	Router *router.Router
	Now    func() time.Time
	Stderr io.Writer      // failover lines
	Finish func(Exchange) // optional
}

type Server struct {
	opts Options
	mux  *http.ServeMux
}

func New(opts Options) *Server {
	s := &Server{opts: opts, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /v1/systemone", s.systemOne)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("/", s.notFound)
	return s
}

// ServeHTTP gives every response a Brout-Request-Id.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Brout-Request-Id", s.requestID())
	s.mux.ServeHTTP(w, r)
}

func (s *Server) systemOne(w http.ResponseWriter, r *http.Request) {
	ex := Exchange{ID: w.Header().Get("Brout-Request-Id"), Start: s.opts.Now(), ReplayOf: r.Header.Get("Brout-Replay-Of")}

	body, err := readBody(r)
	ex.Request = body
	if err != nil {
		s.finish(w, &ex, err)
		return
	}
	req, err := decision.Parse(body)
	if err != nil {
		s.finish(w, &ex, err)
		return
	}
	ex.RequestedModel = req.Model
	if ex.RequestedModel == "" {
		ex.RequestedModel = "auto"
	}

	res, attempts, err := s.opts.Router.Decide(r.Context(), req)
	ex.Attempts = attempts
	if req.Model == "" {
		var derr *decision.Error
		s.printFailovers(ex.ID, attempts, errors.As(err, &derr) && derr.Type == "unavailable")
	}
	if err != nil {
		s.finish(w, &ex, err)
		return
	}
	ex.Model = res.Model
	ex.Status = http.StatusOK
	ex.Response, _ = decision.Encode(res)
	s.write(w, &ex)
}

// finish answers with err, which is a *decision.Error.
func (s *Server) finish(w http.ResponseWriter, ex *Exchange, err error) {
	var derr *decision.Error
	if !errors.As(err, &derr) {
		derr = &decision.Error{Status: http.StatusInternalServerError, Type: "internal", Message: err.Error()}
	}
	ex.Status = derr.Status
	ex.Response, _ = decision.Encode(derr)
	s.write(w, ex)
}

func (s *Server) write(w http.ResponseWriter, ex *Exchange) {
	ex.Duration = s.opts.Now().Sub(ex.Start)
	if s.opts.Finish != nil {
		s.opts.Finish(*ex)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ex.Status)
	w.Write(ex.Response)
}

// printFailovers prints a stderr line for every failover of an auto request
// (D8). allFailed reports that the last attempt failed over too.
func (s *Server) printFailovers(id string, attempts []router.Attempt, allFailed bool) {
	if s.opts.Stderr == nil {
		return
	}
	for i, a := range attempts {
		next := "no models left"
		if i+1 < len(attempts) {
			next = "trying " + attempts[i+1].Model
		} else if !allFailed {
			return
		}
		fmt.Fprintf(s.opts.Stderr, "%s %s unavailable (%s); %s\n", id, a.Model, statusText(a.Status), next)
	}
}

func statusText(status int) string {
	if status == 0 {
		return "no response"
	}
	return fmt.Sprint(status)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	type model struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	routing := s.opts.Router.Routing()
	models := []model{{"auto", "tries the routing list in order: " + strings.Join(routing, ", ")}}
	for i, id := range routing {
		models = append(models, model{id, fmt.Sprintf("routing %d", i+1)})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Models []model `json:"models"`
	}{models})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	body, _ := decision.Encode(&decision.Error{
		Type:    "not_found",
		Message: fmt.Sprintf("brout has no %s %s; it serves POST /v1/systemone and GET /v1/models", r.Method, r.URL.Path),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	w.Write(body)
}

// requestID returns an id that starts with its UTC day, such as
// 20261025-153012-7f3a9c.
func (s *Server) requestID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return s.opts.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// readBody reads the request body, decompressing gzip and zstd (F4).
func readBody(r *http.Request) ([]byte, error) {
	var body io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, invalidBody(enc, err)
		}
		defer zr.Close()
		body = zr
	case "zstd":
		zr, err := zstd.NewReader(r.Body)
		if err != nil {
			return nil, invalidBody(enc, err)
		}
		defer zr.Close()
		body = zr
	default:
		return nil, &decision.Error{
			Status:  http.StatusUnsupportedMediaType,
			Type:    "invalid_request",
			Message: fmt.Sprintf("Content-Encoding %q is not supported; brout accepts gzip, x-gzip and zstd", enc),
		}
	}
	data, err := io.ReadAll(io.LimitReader(body, BodyLimit+1))
	if err != nil {
		return nil, invalidBody(r.Header.Get("Content-Encoding"), err)
	}
	if len(data) > BodyLimit {
		return nil, &decision.Error{
			Status:  http.StatusRequestEntityTooLarge,
			Type:    "body_too_large",
			Message: fmt.Sprintf("brout accepts request bodies up to %d bytes (64 MiB) after decompression; the request is larger", BodyLimit),
		}
	}
	return data, nil
}

func invalidBody(enc string, err error) error {
	if enc == "" {
		return &decision.Error{Status: http.StatusBadRequest, Type: "invalid_request", Message: fmt.Sprintf("reading the body: %v", err)}
	}
	return &decision.Error{Status: http.StatusUnprocessableEntity, Type: "invalid_request", Message: fmt.Sprintf("the body is not valid %s: %v", enc, err)}
}
