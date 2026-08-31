// Package api serves the localhost HTTP API — the daemon's single external
// contract (the CLI goes through it too). Authentication is a static API
// key compared in constant time; every endpoint except /v1/healthz requires
// it. Error bodies are static (no input echoed), JSON is written by the
// encoder only, and no CORS headers are ever served (RFP §2).
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nlink-jp/task-clock/internal/engine"
	"github.com/nlink-jp/task-clock/internal/store"
)

// maxRequestBody bounds every request (none of the endpoints need a body).
const maxRequestBody = 1 << 20

// TaskView is a task's engine status enriched with its latest history row.
type TaskView struct {
	engine.TaskStatus
	LastRun *store.Run `json:"last_run,omitempty"`
}

// Server wires the engine and store behind the HTTP surface.
type Server struct {
	engine  *engine.Engine
	store   *store.Store
	apiKey  string
	reload  func() error
	version string
}

// New builds a Server. apiKey must be non-empty (serve refuses to start
// without one — fail-closed); reload is invoked by POST /v1/reload.
func New(eng *engine.Engine, st *store.Store, apiKey string, reload func() error, version string) *Server {
	return &Server{engine: eng, store: st, apiKey: apiKey, reload: reload, version: version}
}

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/healthz", s.handleHealthz)
	mux.Handle("GET /v1/tasks", s.auth(s.handleTasks))
	mux.Handle("GET /v1/tasks/{name}", s.auth(s.handleTask))
	mux.Handle("POST /v1/tasks/{name}/trigger", s.auth(s.handleTrigger))
	mux.Handle("POST /v1/tasks/{name}/pause", s.auth(s.handlePause))
	mux.Handle("POST /v1/tasks/{name}/resume", s.auth(s.handleResume))
	mux.Handle("GET /v1/tasks/{name}/history", s.auth(s.handleHistory))
	mux.Handle("POST /v1/reload", s.auth(s.handleReload))
	mux.Handle("/", s.auth(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found")
	}))
	return http.MaxBytesHandler(mux, maxRequestBody)
}

// auth requires "Authorization: Bearer <key>" with a constant-time match.
// The key value itself never appears in any response or log.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.apiKey)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
}

func (s *Server) handleTasks(w http.ResponseWriter, _ *http.Request) {
	statuses := s.engine.Status()
	views := make([]TaskView, 0, len(statuses))
	for _, st := range statuses {
		views = append(views, s.view(st))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": views})
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, st := range s.engine.Status() {
		if st.Name == name {
			writeJSON(w, http.StatusOK, s.view(st))
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found")
}

func (s *Server) handleTrigger(w http.ResponseWriter, r *http.Request) {
	err := s.engine.Trigger(r.PathValue("name"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
	case errors.Is(err, engine.ErrUnknownTask):
		writeError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, engine.ErrDisabled):
		writeError(w, http.StatusConflict, "disabled")
	case errors.Is(err, engine.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, "already_running")
	default:
		writeError(w, http.StatusInternalServerError, "launch_failed")
	}
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Pause(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if err := s.engine.Resume(r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": false})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.taskExists(name) {
		writeError(w, http.StatusNotFound, "not_found")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "invalid_limit")
			return
		}
		limit = n
	}
	runs, err := s.store.History(name, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error")
		return
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": name, "runs": runs})
}

func (s *Server) handleReload(w http.ResponseWriter, _ *http.Request) {
	if err := s.reload(); err != nil {
		// The detail is the server's own config diagnosis, not echoed input.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "reload_failed",
			"detail": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) view(st engine.TaskStatus) TaskView {
	v := TaskView{TaskStatus: st}
	if last, err := s.store.LastRun(st.Name); err == nil {
		v.LastRun = last
	}
	return v
}

func (s *Server) taskExists(name string) bool {
	for _, st := range s.engine.Status() {
		if st.Name == name {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, id string) {
	writeJSON(w, code, map[string]string{"error": id})
}
