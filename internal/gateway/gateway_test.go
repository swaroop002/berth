package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/swaroop002/berth/internal/auth"
	"github.com/swaroop002/berth/internal/provider"
	"github.com/swaroop002/berth/internal/spec"
)

func env(name, team, host string, authed bool, upstream string) *spec.Environment {
	e := &spec.Environment{Metadata: spec.Metadata{Name: name, Team: team, Labels: map[string]string{}},
		Spec: spec.Spec{Size: "small", Region: "x", DNS: &spec.DNSSpec{Hostname: host}, Route: &spec.RouteSpec{Port: 80, Auth: authed}}}
	if upstream != "" {
		e.Metadata.Labels[UpstreamLabel] = upstream
	}
	return e
}

func get(t *testing.T, srv *httptest.Server, host, token string) (int, string) {
	req, _ := http.NewRequest("GET", srv.URL+"/hello?x=1", nil)
	req.Host = host
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRouteStepRegistersAuthenticatesAndProxies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("upstream saw " + r.URL.Path + " for " + r.Header.Get("X-Forwarded-Host") +
			" auth=" + r.Header.Get("Authorization")))
	}))
	defer upstream.Close()
	a, _ := auth.New([]string{"ds=0123456789abcdef", "platform=fedcba9876543210"}, "")
	gw := New(a, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()
	p := Wrap(provider.NewFake(), gw, srv.URL)

	open := env("open", "ds", "open.dev.local", false, "")
	locked := env("locked", "ds", "locked.dev.local", true, upstream.URL)
	for _, e := range []*spec.Environment{open, locked} {
		res, err := p.Ensure(context.Background(), "Route", e, "")
		if err != nil || !strings.HasPrefix(res.ID, "route-") {
			t.Fatalf("%v %+v", err, res)
		}
		if ok, _ := p.Check(context.Background(), "Route", e, res.ID); !ok {
			t.Fatal("freshly ensured route must check ok")
		}
	}
	if code, _ := get(t, srv, "nobody.dev.local", ""); code != 404 {
		t.Fatalf("unknown host: %d", code)
	}
	if code, body := get(t, srv, "open.dev.local:8081", ""); code != 200 || !strings.Contains(body, `"environment":"open"`) {
		t.Fatalf("open route without upstream must serve its status page: %d %s", code, body)
	}
	if code, _ := get(t, srv, "locked.dev.local", ""); code != 401 {
		t.Fatalf("auth route without token: %d", code)
	}
	if code, _ := get(t, srv, "locked.dev.local", "fedcba9876543210"); code != 403 {
		t.Fatalf("another team's token: %d", code)
	}
	code, body := get(t, srv, "locked.dev.local", "0123456789abcdef")
	if code != 200 || body != "upstream saw /hello for locked.dev.local auth=" {
		t.Fatalf("proxied: %d %q", code, body)
	}
	// spec change is drift for the route; destroy removes it
	locked.Spec.Route.Auth = false
	if ok, _ := p.Check(context.Background(), "Route", locked, "route-locked.dev.local"); ok {
		t.Fatal("auth flag change must be detected")
	}
	p.Destroy(context.Background(), "Route", "route-locked.dev.local")
	if code, _ := get(t, srv, "locked.dev.local", "0123456789abcdef"); code != 404 {
		t.Fatalf("destroyed route still served: %d", code)
	}
	// other steps pass through to the inner provider
	if res, err := p.Ensure(context.Background(), "Compute", open, ""); err != nil || !strings.HasPrefix(res.ID, "compute-") {
		t.Fatalf("passthrough: %v %+v", err, res)
	}
	bad := env("bad", "ds", "bad.dev.local", false, "ftp://nope")
	if _, err := p.Ensure(context.Background(), "Route", bad, ""); err == nil {
		t.Fatal("a non-http upstream label must be rejected")
	}
}

func TestRestoreRebuildsTable(t *testing.T) {
	gw := New(nil, nil)
	p := Wrap(provider.NewFake(), gw, "http://gw")
	e := env("a", "ds", "a.dev.local", false, "")
	e.Status.Resources = map[string]string{"Route": "route-a.dev.local"}
	gone := env("b", "ds", "b.dev.local", false, "")
	if n := p.Restore([]*spec.Environment{e, gone}); n != 1 {
		t.Fatalf("restored %d", n)
	}
	if _, ok := gw.Lookup("A.DEV.LOCAL"); !ok {
		t.Fatal("host lookup is case-insensitive")
	}
}
