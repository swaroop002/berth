// Package api is berth's HTTP surface: apply, get, list, delete, events, a
// manual reconcile trigger, a cost report and /metrics. It is the only way
// state enters the system, so validation happens here once, and the
// controller is just told "look at this name".
//
// Authorisation is by team: a team's token sees and changes only that
// team's environments; the admin token sees everything (internal/auth).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/swaroop002/berth/internal/auth"
	"github.com/swaroop002/berth/internal/cost"
	"github.com/swaroop002/berth/internal/spec"
	"github.com/swaroop002/berth/internal/store"
)

// Enqueuer is the controller as the API sees it.
type Enqueuer interface {
	Enqueue(name string)
	QueueDepth() int
}

// Options for the server.
type Options struct {
	Auth    *auth.Resolver // nil: open, every caller is admin (development)
	Rates   cost.Rates     // zero value: cost.Default
	Metrics http.Handler   // served at /metrics when set
	Routes  func() any     // gateway table for /v1/routes, optional
}

// Server serves /v1.
type Server struct {
	st   *store.Store
	ctrl Enqueuer
	opts Options
}

func New(st *store.Store, ctrl Enqueuer, opts Options) *Server {
	if opts.Auth == nil {
		opts.Auth, _ = auth.New(nil, "")
	}
	if opts.Rates.Compute == nil {
		opts.Rates = cost.Default
	}
	return &Server{st: st, ctrl: ctrl, opts: opts}
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	if s.opts.Metrics != nil {
		mux.Handle("GET /metrics", s.opts.Metrics)
	}
	mux.HandleFunc("GET /v1/environments", s.auth(s.list))
	mux.HandleFunc("PUT /v1/environments/{name}", s.auth(s.apply))
	mux.HandleFunc("GET /v1/environments/{name}", s.auth(s.get))
	mux.HandleFunc("DELETE /v1/environments/{name}", s.auth(s.delete))
	mux.HandleFunc("GET /v1/environments/{name}/events", s.auth(s.events))
	mux.HandleFunc("POST /v1/environments/{name}/reconcile", s.auth(s.reconcile))
	mux.HandleFunc("GET /v1/status", s.auth(s.status))
	mux.HandleFunc("GET /v1/costs", s.auth(s.costs))
	mux.HandleFunc("GET /v1/routes", s.auth(s.routes))
	return mux
}

type ctxKey struct{}

func (s *Server) auth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.opts.Auth.Resolve(r)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "missing or unknown bearer token")
			return
		}
		fn(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func who(r *http.Request) auth.Principal {
	p, _ := r.Context().Value(ctxKey{}).(auth.Principal)
	return p
}

// visible loads an environment the caller may see, or writes the error.
func (s *Server) visible(w http.ResponseWriter, r *http.Request, name string) (*spec.Environment, bool) {
	env, err := s.st.Get(name)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return nil, false
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if !who(r).CanAccess(env.Metadata.Team) {
		writeErr(w, http.StatusForbidden, "environment "+name+" belongs to team "+env.Metadata.Team)
		return nil, false
	}
	return env, true
}

// listVisible is every environment the caller may see.
func (s *Server) listVisible(r *http.Request) ([]*spec.Environment, error) {
	envs, err := s.st.List()
	if err != nil {
		return nil, err
	}
	p := who(r)
	team := r.URL.Query().Get("team")
	out := make([]*spec.Environment, 0, len(envs))
	for _, e := range envs {
		if p.CanAccess(e.Metadata.Team) && (team == "" || e.Metadata.Team == team) {
			out = append(out, e)
		}
	}
	return out, nil
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
	p := who(r)
	if !p.CanAccess(env.Metadata.Team) {
		writeErr(w, http.StatusForbidden, "your token belongs to team "+p.Team+", not "+env.Metadata.Team)
		return
	}
	if cur, err := s.st.Get(env.Metadata.Name); err == nil && !p.CanAccess(cur.Metadata.Team) {
		writeErr(w, http.StatusForbidden, "environment "+env.Metadata.Name+" belongs to team "+cur.Metadata.Team)
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
	if env, ok := s.visible(w, r, r.PathValue("name")); ok {
		writeJSON(w, http.StatusOK, env)
	}
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	envs, err := s.listVisible(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": envs})
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.visible(w, r, r.PathValue("name")); !ok {
		return
	}
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
	if _, ok := s.visible(w, r, r.PathValue("name")); !ok {
		return
	}
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
	if _, ok := s.visible(w, r, r.PathValue("name")); !ok {
		return
	}
	s.ctrl.Enqueue(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// status is the one-screen summary: how many environments in which phase,
// and what they cost per hour.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	envs, err := s.listVisible(r)
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
	rep := s.opts.Rates.Build(envs, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"environments": len(envs), "phases": phases, "pendingChanges": stale,
		"queueDepth": s.ctrl.QueueDepth(), "hourlyUSD": round2(rep.Hourly), "accruedUSD": round2(rep.Accrued),
		"time": time.Now().UTC(),
	})
}

// costs prices every visible environment: run rate, accrued, by team.
func (s *Server) costs(w http.ResponseWriter, r *http.Request) {
	envs, err := s.listVisible(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.opts.Rates.Build(envs, time.Now()))
}

// routes shows the gateway table (admin only: it spans teams).
func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "the route table is admin-only")
		return
	}
	if s.opts.Routes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": s.opts.Routes()})
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

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
		keys := make([]string, 0, len(e.Status.Endpoints))
		for k := range e.Status.Endpoints {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %-10s %s\n", k, e.Status.Endpoints[k])
		}
	}
	fmt.Fprintf(&b, "Cost:        $%.3f/h, $%.2f so far\n", cost.Default.Hourly(e), cost.Default.Accrued(e, time.Now()))
	return b.String()
}

// CostTable renders the cost report for humans (the CLI's `costs`).
func CostTable(rep cost.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-22s %-14s %-8s %-10s %10s %12s\n", "NAME", "TEAM", "PHASE", "AGE", "USD/H", "ACCRUED USD")
	for _, l := range rep.Lines {
		fmt.Fprintf(&b, "%-22s %-14s %-8s %-10s %10.3f %12.2f\n", l.Name, l.Team, l.Phase, fmt.Sprintf("%.1fh", l.AgeHours), l.Hourly, l.Accrued)
	}
	teams := make([]string, 0, len(rep.TeamHourly))
	for t := range rep.TeamHourly {
		teams = append(teams, t)
	}
	sort.Strings(teams)
	b.WriteString("\nBy team:\n")
	for _, t := range teams {
		fmt.Fprintf(&b, "  %-20s %10.3f/h %12.2f\n", t, rep.TeamHourly[t], rep.TeamTotal[t])
	}
	fmt.Fprintf(&b, "  %-20s %10.3f/h %12.2f\n", "total", rep.Hourly, rep.Accrued)
	return b.String()
}
