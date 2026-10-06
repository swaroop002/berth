// Package api is forge's HTTP surface: apply, get, list, delete, events, and
// a manual reconcile trigger. It is the only way state enters the system,
// so validation happens here once, and the controller is just told "look at
// this name".
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/swaroop002/forge/internal/spec"
	"github.com/swaroop002/forge/internal/store"
)

// Enqueuer is the controller as the API sees it.
type Enqueuer interface {
	Enqueue(name string)
	QueueDepth() int
}

// Server serves /v1.
type Server struct {
	st    *store.Store
	ctrl  Enqueuer
	token string // static bearer token; "" means open (development)
}

func New(st *store.Store, ctrl Enqueuer, token string) *Server {
	return &Server{st: st, ctrl: ctrl, token: token}
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/environments", s.auth(s.list))
	mux.HandleFunc("PUT /v1/environments/{name}", s.auth(s.apply))
	mux.HandleFunc("GET /v1/environments/{name}", s.auth(s.get))
	mux.HandleFunc("DELETE /v1/environments/{name}", s.auth(s.delete))
	mux.HandleFunc("GET /v1/environments/{name}/events", s.auth(s.events))
	mux.HandleFunc("POST /v1/environments/{name}/reconcile", s.auth(s.reconcile))
	mux.HandleFunc("GET /v1/status", s.auth(s.status))
	return mux
}

func (s *Server) auth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && r.Header.Get("Authorization") != "Bearer "+s.token {
			writeErr(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		fn(w, r)
	}
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot read body")
		return
	}
	var env spec.Environment
	if strings.Contains(r.Header.Get("Content-Type"), "yaml") || (len(body) > 0 && body[0] != '{') {
		err = yaml.Unmarshal(body, &env)
	} else {
		err = json.Unmarshal(body, &env)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cannot parse body: "+err.Error())
		return
	}
	if env.Metadata.Name == "" {
		env.Metadata.Name = r.PathValue("name")
	}
	if env.Metadata.Name != r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, "metadata.name does not match the URL")
		return
	}
	stored, changed, err := s.st.Apply(&env)
	switch {
	case errors.Is(err, spec.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	code := http.StatusOK
	if stored.Metadata.Generation == 1 && changed {
		code = http.StatusCreated
	}
	if changed {
		s.ctrl.Enqueue(stored.Metadata.Name)
	}
	writeJSON(w, code, stored)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	env, err := s.st.Get(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, env)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	envs, err := s.st.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if team := r.URL.Query().Get("team"); team != "" {
		var f []*spec.Environment
		for _, e := range envs {
			if e.Metadata.Team == team {
				f = append(f, e)
			}
		}
		envs = f
	}
	if envs == nil {
		envs = []*spec.Environment{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": envs})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	env, err := s.st.MarkDeleting(r.PathValue("name"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.st.Record(store.Event{Env: env.Metadata.Name, Type: "Deleting", Message: "requested via API"})
	s.ctrl.Enqueue(env.Metadata.Name)
	writeJSON(w, http.StatusAccepted, env)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	evs, err := s.st.Events(r.PathValue("name"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": evs})
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	if _, err := s.st.Get(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.ctrl.Enqueue(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// status is the one-screen summary: how many environments in which phase.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	envs, err := s.st.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	phases := map[string]int{}
	var stale int
	for _, e := range envs {
		phases[string(e.Status.Phase)]++
		if e.Status.ObservedGeneration != e.Metadata.Generation {
			stale++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"environments": len(envs), "phases": phases, "pendingChanges": stale,
		"queueDepth": s.ctrl.QueueDepth(), "time": time.Now().UTC(),
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Describe renders an environment for humans (the CLI's `get`).
func Describe(e *spec.Environment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Name:        %s\nTeam/Owner:  %s / %s\nSize/Region: %s / %s\nGeneration:  %d (observed %d)\nPhase:       %s\n",
		e.Metadata.Name, e.Metadata.Team, e.Metadata.Owner, e.Spec.Size, e.Spec.Region,
		e.Metadata.Generation, e.Status.ObservedGeneration, e.Status.Phase)
	if e.Metadata.DeletionTimestamp != nil {
		fmt.Fprintf(&b, "Deleting:    since %s\n", e.Metadata.DeletionTimestamp.Format(time.RFC3339))
	}
	if len(e.Status.Conditions) > 0 {
		b.WriteString("Conditions:\n")
		for _, c := range e.Status.Conditions {
			fmt.Fprintf(&b, "  %-10s %-5s %-10s %s\n", c.Type, c.Status, c.Reason, c.Message)
		}
	}
	if len(e.Status.Endpoints) > 0 {
		b.WriteString("Endpoints:\n")
		for k, v := range e.Status.Endpoints {
			fmt.Fprintf(&b, "  %-10s %s\n", k, v)
		}
	}
	return b.String()
}
