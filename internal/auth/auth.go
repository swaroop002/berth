// Package auth decides who is calling and what they may touch. A token
// belongs to a team; a team sees and changes only its own environments. The
// admin token sees everything. Without any token configured the API is open
// (development only) and every caller is admin.
package auth

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

var ErrUnauthenticated = errors.New("auth: missing or unknown token")

// Principal is who the request is.
type Principal struct {
	Team  string
	Admin bool
}

// CanAccess reports whether the principal may read or change an environment of that team.
func (p Principal) CanAccess(team string) bool { return p.Admin || p.Team == team }

// Resolver maps tokens to principals.
type Resolver struct {
	teams map[string]string // token -> team
	admin string
	open  bool
}

// New takes "team=token" pairs and an admin token. Tokens must be at least 16
// characters. With nothing configured the resolver is open.
func New(pairs []string, admin string) (*Resolver, error) {
	r := &Resolver{teams: map[string]string{}, admin: admin}
	for _, p := range pairs {
		team, tok, ok := strings.Cut(p, "=")
		if !ok || team == "" || len(tok) < 16 {
			return nil, errors.New("auth: tokens must be team=token with at least 16 characters")
		}
		r.teams[tok] = team
	}
	if admin != "" && len(admin) < 16 {
		return nil, errors.New("auth: the admin token must be at least 16 characters")
	}
	r.open = len(r.teams) == 0 && admin == ""
	return r, nil
}

// Open reports development mode.
func (r *Resolver) Open() bool { return r.open }

// Resolve identifies the caller.
func (r *Resolver) Resolve(req *http.Request) (Principal, error) {
	if r.open {
		return Principal{Admin: true}, nil
	}
	tok, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return Principal{}, ErrUnauthenticated
	}
	if r.admin != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(r.admin)) == 1 {
		return Principal{Admin: true}, nil
	}
	for t, team := range r.teams {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return Principal{Team: team}, nil
		}
	}
	return Principal{}, ErrUnauthenticated
}
