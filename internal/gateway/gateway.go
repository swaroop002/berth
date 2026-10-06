// Package gateway is the Route step made real: an in-process, host-based
// reverse proxy. Each environment with a route gets its hostname registered
// here; a request whose Host matches is authenticated (if the route asks for
// it) with the same team tokens the API uses, then proxied to the
// environment's upstream. Without an upstream (the fake provider's compute
// has no address to answer on) the route serves a status page, so the whole
// path from manifest to a working URL can be exercised on a laptop.
//
// A real deployment would put this behind a wildcard DNS record and TLS
// termination; what berth adds is the routing table being owned by the
// reconciler, so a route exists exactly when its environment does.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swaroop002/berth/internal/auth"
	"github.com/swaroop002/berth/internal/provider"
	"github.com/swaroop002/berth/internal/spec"
)

// UpstreamLabel on an environment names where its route should proxy to,
// for example http://localhost:3000. Without it the route serves a status page.
const UpstreamLabel = "berth.dev/upstream"

// Route is one entry of the table.
type Route struct {
	Env, Team string
	Upstream  *url.URL // nil: status page
	Auth      bool
	Since     time.Time
}

// Gateway is the proxy and its table.
type Gateway struct {
	mu     sync.RWMutex
	routes map[string]Route // host -> route
	auth   *auth.Resolver
	log    *slog.Logger
	// OnRequest, if set, is told about every request served.
	OnRequest func(host string, code int)
}

// New builds an empty gateway.
func New(a *auth.Resolver, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	return &Gateway{routes: map[string]Route{}, auth: a, log: log}
}

// Set installs or replaces a route.
func (g *Gateway) Set(host string, r Route) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.Since.IsZero() {
		r.Since = time.Now()
	}
	g.routes[strings.ToLower(host)] = r
}

// Remove drops a host; removing an unknown host is fine.
func (g *Gateway) Remove(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.routes, strings.ToLower(host))
}

// Lookup returns the route for a host.
func (g *Gateway) Lookup(host string) (Route, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	r, ok := g.routes[strings.ToLower(host)]
	return r, ok
}

// View is a route as the API shows it.
type View struct {
	Host     string    `json:"host"`
	Env      string    `json:"environment"`
	Team     string    `json:"team"`
	Upstream string    `json:"upstream,omitempty"`
	Auth     bool      `json:"auth"`
	Since    time.Time `json:"since"`
}

// Hosts lists the table, sorted by host, for status.
func (g *Gateway) Hosts() []View {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]View, 0, len(g.routes))
	for h, r := range g.routes {
		v := View{Host: h, Env: r.Env, Team: r.Team, Auth: r.Auth, Since: r.Since}
		if r.Upstream != nil {
			v.Upstream = r.Upstream.String()
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{ResponseWriter: w, code: http.StatusOK}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	defer func() {
		if g.OnRequest != nil {
			g.OnRequest(host, rec.code)
		}
	}()
	route, ok := g.Lookup(host)
	if !ok {
		http.Error(rec, "no environment serves "+host, http.StatusNotFound)
		return
	}
	if route.Auth && g.auth != nil && !g.auth.Open() {
		p, err := g.auth.Resolve(r)
		if err != nil {
			rec.Header().Set("WWW-Authenticate", `Bearer realm="berth"`)
			http.Error(rec, "this route requires a team token", http.StatusUnauthorized)
			return
		}
		if !p.CanAccess(route.Team) {
			http.Error(rec, "this environment belongs to team "+route.Team, http.StatusForbidden)
			return
		}
	}
	if route.Upstream == nil {
		rec.Header().Set("Content-Type", "application/json")
		json.NewEncoder(rec).Encode(map[string]any{
			"environment": route.Env, "team": route.Team, "host": host, "authenticated": route.Auth,
			"message": fmt.Sprintf("route is live; %s has no upstream to proxy to (set the %s label)", route.Env, UpstreamLabel),
		})
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(route.Upstream)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = route.Upstream.Host
		req.Header.Set("X-Forwarded-Host", host)
		req.Header.Set("X-Berth-Environment", route.Env)
		req.Header.Del("Authorization") // the gateway authenticated; the token stays here
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		g.log.Warn("upstream failed", "env", route.Env, "upstream", route.Upstream.String(), "err", err)
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(rec, r)
}

type recorder struct {
	http.ResponseWriter
	code int
}

func (r *recorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }

// Provider makes the Route step real by wrapping any other provider: Route
// is handled here, every other step is passed through.
type Provider struct {
	provider.Provider
	gw     *Gateway
	public string // how clients reach the gateway, e.g. http://localhost:8081
}

// Wrap returns a provider whose Route step lives in the gateway.
func Wrap(inner provider.Provider, gw *Gateway, public string) *Provider {
	return &Provider{Provider: inner, gw: gw, public: public}
}

func (p *Provider) Name() string { return p.Provider.Name() + "+gateway" }

func routeID(host string) string { return "route-" + strings.ToLower(host) }

func upstreamOf(env *spec.Environment) (*url.URL, error) {
	raw := env.Metadata.Labels[UpstreamLabel]
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("label %s must be an http(s) URL", UpstreamLabel)
	}
	return u, nil
}

func (p *Provider) Ensure(ctx context.Context, step string, env *spec.Environment, prior string) (provider.Resource, error) {
	if step != "Route" {
		return p.Provider.Ensure(ctx, step, env, prior)
	}
	if env.Spec.DNS == nil || env.Spec.Route == nil {
		return provider.Resource{}, fmt.Errorf("route needs dns.hostname")
	}
	host := strings.ToLower(env.Spec.DNS.Hostname)
	up, err := upstreamOf(env)
	if err != nil {
		return provider.Resource{}, err // permanent: the manifest is wrong
	}
	if prior != "" && prior != routeID(host) {
		p.gw.Remove(strings.TrimPrefix(prior, "route-")) // hostname changed: old entry goes
	}
	p.gw.Set(host, Route{Env: env.Metadata.Name, Team: env.Metadata.Team, Upstream: up, Auth: env.Spec.Route.Auth})
	res := provider.Resource{ID: routeID(host), Endpoint: fmt.Sprintf("%s (Host: %s)", p.public, host),
		Attrs: map[string]string{"auth": fmt.Sprint(env.Spec.Route.Auth), "port": fmt.Sprint(env.Spec.Route.Port)}}
	if up != nil {
		res.Attrs["upstream"] = up.String()
	}
	return res, nil
}

func (p *Provider) Check(ctx context.Context, step string, env *spec.Environment, id string) (bool, error) {
	if step != "Route" {
		return p.Provider.Check(ctx, step, env, id)
	}
	host := strings.TrimPrefix(id, "route-")
	r, ok := p.gw.Lookup(host)
	if !ok || r.Env != env.Metadata.Name || env.Spec.Route == nil || r.Auth != env.Spec.Route.Auth {
		return false, nil
	}
	want, err := upstreamOf(env)
	if err != nil {
		return false, nil
	}
	return (want == nil) == (r.Upstream == nil) && (want == nil || want.String() == r.Upstream.String()), nil
}

func (p *Provider) Destroy(ctx context.Context, step string, id string) error {
	if step != "Route" {
		return p.Provider.Destroy(ctx, step, id)
	}
	p.gw.Remove(strings.TrimPrefix(id, "route-"))
	return nil
}

// Restore rebuilds the table from stored environments after a restart, so
// no route flaps while the controller catches up.
func (p *Provider) Restore(envs []*spec.Environment) int {
	n := 0
	for _, e := range envs {
		if e.Spec.Route == nil || e.Spec.DNS == nil || e.Status.Resources["Route"] == "" || e.Metadata.DeletionTimestamp != nil {
			continue
		}
		up, err := upstreamOf(e)
		if err != nil {
			continue
		}
		p.gw.Set(e.Spec.DNS.Hostname, Route{Env: e.Metadata.Name, Team: e.Metadata.Team, Upstream: up, Auth: e.Spec.Route.Auth})
		n++
	}
	return n
}
