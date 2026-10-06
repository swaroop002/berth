package auth

import (
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	if _, err := New([]string{"ds=short"}, ""); err == nil {
		t.Fatal("short token must be rejected")
	}
	r, _ := New([]string{"ds=0123456789abcdef", "platform=fedcba9876543210"}, "adminadminadminadmin")
	req := httptest.NewRequest("GET", "/", nil)
	if _, err := r.Resolve(req); err == nil {
		t.Fatal("no token must fail")
	}
	req.Header.Set("Authorization", "Bearer 0123456789abcdef")
	p, _ := r.Resolve(req)
	if p.Team != "ds" || p.Admin || !p.CanAccess("ds") || p.CanAccess("platform") {
		t.Fatalf("%+v", p)
	}
	req.Header.Set("Authorization", "Bearer adminadminadminadmin")
	if p, _ := r.Resolve(req); !p.Admin || !p.CanAccess("anything") {
		t.Fatalf("%+v", p)
	}
	open, _ := New(nil, "")
	if p, _ := open.Resolve(httptest.NewRequest("GET", "/", nil)); !open.Open() || !p.Admin {
		t.Fatal("open mode is admin")
	}
}
