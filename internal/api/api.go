// Package api is semd's HTTP/JSON interface, served on a Unix socket. It
// validates requests and delegates to the engine; it holds no state.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/embed"
	"github.com/msongd/kimquy/internal/engine"
	"github.com/msongd/kimquy/internal/kind"
)

const maxBody = 64 << 10

// Request and reply types shared with the client. Replies that carry
// engine values (search hits, status, failures) are encoded from the
// engine's own types; api_test checks that the client's mirror them.
type (
	SearchRequest  = client.SearchRequest
	PreviewRequest = client.PreviewRequest
	RootsRequest   = client.RootsRequest
	RootsResponse  = client.RootsResponse
	Settings       = client.Settings
	Error          = client.Error
)

// SearchResponse is the reply to POST /v1/search.
type SearchResponse struct {
	Hits   []engine.Hit `json:"hits"`
	TookMS int64        `json:"took_ms"`
}

type server struct {
	e   *engine.Engine
	log *slog.Logger
}

// Handler returns the /v1 API.
func Handler(e *engine.Engine, log *slog.Logger) http.Handler {
	s := &server{e: e, log: log.With("component", "api")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/search", s.search)
	mux.HandleFunc("POST /v1/preview", s.preview)
	mux.HandleFunc("GET /v1/thumbnail", s.thumbnail)
	mux.HandleFunc("GET /v1/roots", s.getRoots)
	mux.HandleFunc("PUT /v1/roots", s.putRoots)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) { reply(w, http.StatusOK, e.Status()) })
	mux.HandleFunc("GET /v1/failures", func(w http.ResponseWriter, r *http.Request) {
		f := e.Failures()
		if f == nil {
			f = []engine.Failure{}
		}
		reply(w, http.StatusOK, f)
	})
	mux.HandleFunc("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, Settings{PauseOnBattery: e.Config().PauseOnBattery})
	})
	mux.HandleFunc("PUT /v1/settings", s.putSettings)
	mux.HandleFunc("GET /v1/events", s.events)
	mux.HandleFunc("POST /v1/pause", func(w http.ResponseWriter, r *http.Request) {
		e.Pause()
		reply(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("POST /v1/resume", func(w http.ResponseWriter, r *http.Request) {
		e.Resume()
		reply(w, http.StatusOK, e.Status())
	})
	mux.HandleFunc("POST /v1/reindex", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Reindex(); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		reply(w, http.StatusAccepted, e.Status())
	})
	return s.recoverer(mux)
}

// recoverer turns a handler panic into a logged 500 instead of a dropped connection.
func (s *server) recoverer(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("handler panic", "path", r.URL.Path, "panic", v)
				reply(w, http.StatusInternalServerError, Error{Error: "internal error"})
			}
		}()
		h.ServeHTTP(w, r)
	})
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.K == 0 {
		req.K = 10
	}
	er := engine.SearchRequest{Q: req.Q, K: req.K, Roots: req.Roots}
	for _, n := range req.Kinds {
		k, err := kind.Parse(n)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		er.Kinds |= kind.SetOf(k)
	}
	if err := er.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	t0 := time.Now()
	hits, err := s.e.Search(r.Context(), er)
	switch {
	case errors.Is(err, engine.ErrReindexRequired), errors.Is(err, embed.ErrUnavailable):
		s.fail(w, http.StatusServiceUnavailable, err)
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if hits == nil {
		hits = []engine.Hit{}
	}
	reply(w, http.StatusOK, SearchResponse{Hits: hits, TookMS: time.Since(t0).Milliseconds()})
}

func (s *server) preview(w http.ResponseWriter, r *http.Request) {
	var req PreviewRequest
	if !s.decode(w, r, &req) {
		return
	}
	p, err := s.e.Preview(r.Context(), req.Path, req.Locator)
	if err != nil {
		s.fail(w, previewCode(err), err)
		return
	}
	reply(w, http.StatusOK, p)
}

// thumbnail replies with the image itself, not JSON.
func (s *server) thumbnail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(q) != 1 || len(q["path"]) != 1 {
		s.fail(w, http.StatusBadRequest, errors.New("want exactly one query parameter, path"))
		return
	}
	data, typ, err := s.e.Thumbnail(r.Context(), q.Get("path"))
	if err != nil {
		s.fail(w, previewCode(err), err)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func previewCode(err error) int {
	switch {
	case errors.Is(err, engine.ErrBadPreview):
		return http.StatusBadRequest
	case errors.Is(err, engine.ErrNotIndexed):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func rootsResponse(c config.Config) RootsResponse {
	return RootsResponse{Roots: c.Roots, Excludes: c.Excludes, DefaultExcludes: config.DefaultExcludes}
}

func (s *server) getRoots(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, rootsResponse(s.e.Config()))
}

func (s *server) putRoots(w http.ResponseWriter, r *http.Request) {
	var req RootsRequest
	if !s.decode(w, r, &req) {
		return
	}
	if req.Roots == nil {
		s.fail(w, http.StatusBadRequest, errors.New(`"roots" is required (use [] to remove all)`))
		return
	}
	c, err := s.e.SetRoots(req.Roots, req.Excludes)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	reply(w, http.StatusOK, rootsResponse(c))
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var req Settings
	if !s.decode(w, r, &req) {
		return
	}
	c, err := s.e.SetPauseOnBattery(req.PauseOnBattery)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	reply(w, http.StatusOK, Settings{PauseOnBattery: c.PauseOnBattery})
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		s.fail(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	bus := s.e.Events()
	sub := bus.Subscribe(256)
	defer bus.Unsubscribe(sub)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
		case ev := <-sub.C:
			if n := sub.Dropped(); n > 0 {
				fmt.Fprintf(w, "event: lagged\ndata: {\"dropped\":%d}\n\n", n)
			}
			b, err := json.Marshal(ev)
			if err != nil {
				s.log.Error("encode event", "err", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
				return
			}
		}
		fl.Flush()
	}
}

// decode reads a single JSON object, rejecting unknown fields, oversize
// bodies and trailing data.
func (s *server) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		s.fail(w, http.StatusBadRequest, errors.New("bad request body: trailing data"))
		return false
	}
	return true
}

func (s *server) fail(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		s.log.Error("request failed", "code", code, "err", err)
	}
	reply(w, code, Error{Error: err.Error()})
}

func reply(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"error":"encode response"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}
