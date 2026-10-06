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

	"github.com/swaroop002/forge/internal/api"
	"github.com/swaroop002/forge/internal/provider"
	"github.com/swaroop002/forge/internal/reconcile"
	"github.com/swaroop002/forge/internal/spec"
	"github.com/swaroop002/forge/internal/store"
)

const envYAML = `
apiVersion: forge.swaroop002.dev/v1alpha1
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
	st, _ := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	defer st.Close()
	pv := provider.NewFake()
	ctrl := reconcile.New(st, pv, reconcile.Options{Workers: 2, Resync: time.Hour, MinBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)
	srv := httptest.NewServer(api.New(st, ctrl, "secret-token").Handler())
	defer srv.Close()

	do := func(method, path, body, ctype string, auth bool) (int, map[string]any) {
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
	if s["environments"].(float64) != 1 {
		t.Fatalf("status: %v", s)
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
