// Package api is the HTTP layer for the three endpoints in
// docs/protocol.md §10. Standard library only — no router.
//
// POST /events is the hot path: it validates, applies one transition in
// memory, and returns. No disk, no network, no logging on the accepted
// path (PRD.md §7). Malformed input returns an error rather than
// panicking, since a panic would take the process down and break
// fail-open at the process level, not just the logical one.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"trafficlight/internal/aggregation"
	"trafficlight/internal/auth"
	"trafficlight/internal/config"
	"trafficlight/internal/events"
	"trafficlight/internal/sessions"
	"trafficlight/internal/types"
)

// maxBodyBytes bounds POST /events. The envelope is five short strings;
// anything larger is a bug or an abuse, and reading it would be work
// done on the hot path.
const maxBodyBytes = 4 << 10

type Server struct {
	cfg     config.Config
	mgr     *sessions.Manager
	token   string
	log     *slog.Logger
	started time.Time
	now     func() time.Time
}

func New(cfg config.Config, mgr *sessions.Manager, token string, log *slog.Logger) *Server {
	return NewWithClock(cfg, mgr, token, log, time.Now)
}

func NewWithClock(cfg config.Config, mgr *sessions.Manager, token string, log *slog.Logger, now func() time.Time) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{
		cfg:     cfg,
		mgr:     mgr,
		token:   token,
		log:     log,
		started: now(),
		now:     now,
	}
}

// Handler wires the routes. /health is unauthenticated (a trivial
// liveness check); /events and /state require the bearer token.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /events", auth.Require(s.token, http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /state", auth.Require(s.token, http.HandlerFunc(s.handleState)))
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ev, err := events.Parse(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	out := s.mgr.Apply(ev)

	// Logging happens after Apply returns, outside the session lock, and
	// only for outcomes worth a line — never for an ordinary accepted
	// transition.
	switch {
	case out.Unexpected:
		s.log.Warn("unexpected transition -> unknown",
			"tool", out.Tool, "session", out.SessionID,
			"event", ev.Event, "from", out.From)
	case out.NoHistory:
		s.log.Info("event for untracked session; derived from event alone",
			"tool", out.Tool, "session", out.SessionID,
			"event", ev.Event, "to", out.To)
	case out.Ignored:
		s.log.Debug("session_ended for untracked session; ignored",
			"tool", out.Tool, "session", out.SessionID)
	}

	// The body is deliberately empty: hooks are fail-open and ignore the
	// response, and echoing state back would invite a client to trust it
	// over GET /state.
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	recs, updatedAt := s.mgr.Snapshot()
	resp := aggregation.Build(recs, updatedAt, s.now(), s.cfg.WaitingTooLong)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, types.HealthResponse{
		Status:        "ok",
		Version:       config.Version,
		UptimeSeconds: int64(s.now().Sub(s.started).Seconds()),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
