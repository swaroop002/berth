package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swaroop002/berth/internal/api"
	"github.com/swaroop002/berth/internal/auth"
	"github.com/swaroop002/berth/internal/provider"
	"github.com/swaroop002/berth/internal/reconcile"
	"github.com/swaroop002/berth/internal/spec"
	"github.com/swaroop002/berth/internal/store"
)

const envYAML = `
apiVersion: berth.swaroop002.dev/v1alpha1
kind: Environment
metadata: {name: ds-alice, team: data-science, owner: alice}
spec:
  size: medium
  region: ap-south-1
  storage: {sizeGB: 50}
  dns: {hostname: alice.dev.example.com}
  route: {port: 8888, auth: true}
  telemetry: {tenant: ds-alice}
`

func TestLifecycleOverHTTP(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "berth.db"))
	defer st.Close()
	pv := provider.NewFake()
	ctrl := reconcile.New(st, pv, reconcile.Options{Workers: 2, Resync: time.Hour, MinBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)
	resolver, err := auth.New([]string{"data-science=ds-token-0123456789", "platform=pf-token-0123456789"}, "admin-token-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(st, ctrl, api.Options{Auth: resolver}).Handler())
	defer srv.Close()

	doAs := func(token, method, path, body, ctype string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	do := func(method, path, body, ctype string, authed bool) (int, map[string]any) {
		token := ""
		if authed {
			token = "admin-token-0123456789"
		}
		return doAs(token, method, path, body, ctype)
	}
	_ = func(method, path, body, ctype string, auth bool) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if auth {
			req.Header.Set("Authorization", "Bearer secret-token")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	if code, _ := do("GET", "/v1/environments", "", "", false); code != 401 {
		t.Fatalf("no token must be 401, got %d", code)
	}
	code, out := do("PUT", "/v1/environments/ds-alice", envYAML, "application/yaml", true)
	if code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := do("PUT", "/v1/environments/other", envYAML, "application/yaml", true); code != 400 {
		t.Fatalf("name mismatch must be 400: %d %v", code, out)
	}
	if code, out := do("PUT", "/v1/environments/bad", `{"apiVersion":"x"}`, "application/json", true); code != 422 {
		t.Fatalf("invalid must be 422: %d %v", code, out)
	}

	deadline := time.Now().Add(10 * time.Second)
	var env spec.Environment
	for time.Now().Before(deadline) {
		_, out = do("GET", "/v1/environments/ds-alice", "", "", true)
		b, _ := json.Marshal(out)
		json.Unmarshal(b, &env)
		if env.Status.Phase == spec.PhaseReady {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if env.Status.Phase != spec.PhaseReady || env.Status.Endpoints["Route"] != "https://alice.dev.example.com" {
		t.Fatalf("never became ready: %+v", env.Status)
	}
	if code, _ := do("PUT", "/v1/environments/ds-alice", envYAML, "application/yaml", true); code != 200 {
		t.Fatalf("re-apply must be 200, got %d", code)
	}
	_, ev := do("GET", "/v1/environments/ds-alice/events", "", "", true)
	if len(ev["items"].([]any)) < 7 {
		t.Fatalf("events: %v", ev)
	}
	_, s := do("GET", "/v1/status", "", "", true)
	if s["environments"].(float64) != 1 || s["hourlyUSD"].(float64) <= 0 {
		t.Fatalf("status: %v", s)
	}
	_, costs := do("GET", "/v1/costs", "", "", true)
	if len(costs["environments"].([]any)) != 1 || costs["teamHourlyUSD"].(map[string]any)["data-science"].(float64) <= 0 {
		t.Fatalf("costs: %v", costs)
	}

	// Team scoping: the platform team cannot see, change or delete data-science's environment.
	if code, _ := doAs("pf-token-0123456789", "GET", "/v1/environments/ds-alice", "", ""); code != 403 {
		t.Fatalf("other team's get must be 403: %d", code)
	}
	if code, _ := doAs("pf-token-0123456789", "DELETE", "/v1/environments/ds-alice", "", ""); code != 403 {
		t.Fatalf("other team's delete must be 403: %d", code)
	}
	if code, _ := doAs("pf-token-0123456789", "PUT", "/v1/environments/ds-alice", envYAML, "application/yaml"); code != 403 {
		t.Fatalf("other team's apply must be 403: %d", code)
	}
	hijack := strings.Replace(envYAML, "team: data-science", "team: platform", 1)
	if code, _ := doAs("pf-token-0123456789", "PUT", "/v1/environments/ds-alice", hijack, "application/yaml"); code != 403 {
		t.Fatalf("re-teaming someone else's environment must be 403: %d", code)
	}
	_, mine := doAs("pf-token-0123456789", "GET", "/v1/environments", "", "")
	if len(mine["items"].([]any)) != 0 {
		t.Fatalf("platform must see no environments: %v", mine)
	}
	if code, _ := doAs("ds-token-0123456789", "GET", "/v1/environments/ds-alice", "", ""); code != 200 {
		t.Fatalf("own team's get: %d", code)
	}
	if code, _ := doAs("ds-token-0123456789", "GET", "/v1/routes", "", ""); code != 403 {
		t.Fatalf("route table is admin-only: %d", code)
	}

	if code, _ := do("DELETE", "/v1/environments/ds-alice", "", "", true); code != 202 {
		t.Fatalf("delete: %d", code)
	}
	for time.Now().Before(deadline) {
		if code, _ := do("GET", "/v1/environments/ds-alice", "", "", true); code == 404 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := do("GET", "/v1/environments/ds-alice", "", "", true); code != 404 {
		t.Fatal("environment should be gone after teardown")
	}
	if len(pv.Inventory()) != 0 {
		t.Fatalf("leaked resources: %v", pv.Inventory())
	}
}
