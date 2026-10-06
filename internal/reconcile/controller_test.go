package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swaroop002/forge/internal/provider"
	"github.com/swaroop002/forge/internal/spec"
	"github.com/swaroop002/forge/internal/store"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func setup(t *testing.T) (*store.Store, *provider.Fake, *Controller) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pv := provider.NewFake()
	c := New(st, pv, Options{Logger: quiet(), MinBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond, Resync: time.Hour})
	return st, pv, c
}

func full(name string) *spec.Environment {
	return &spec.Environment{APIVersion: spec.APIVersion, Kind: spec.KindEnv,
		Metadata: spec.Metadata{Name: name, Team: "ds", Owner: "alice"},
		Spec: spec.Spec{Size: "medium", Region: "ap-south-1",
			Storage: &spec.StorageSpec{SizeGB: 50}, DNS: &spec.DNSSpec{Hostname: name + ".dev.example.com"},
			Route: &spec.RouteSpec{Port: 8888, Auth: true}, Telemetry: &spec.TelemetrySpec{Tenant: name}}}
}

func types(evs []store.Event) string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return strings.Join(out, ",")
}

func TestConvergeIsOrderedAndIdempotent(t *testing.T) {
	st, pv, c := setup(t)
	st.Apply(full("a"))
	if _, err := c.Reconcile(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	e, _ := st.Get("a")
	if e.Status.Phase != spec.PhaseReady || e.Status.ObservedGeneration != 1 || len(e.Status.Resources) != 5 {
		t.Fatalf("%+v", e.Status)
	}
	if e.Status.Endpoints["Route"] != "https://a.dev.example.com" {
		t.Fatalf("endpoints %v", e.Status.Endpoints)
	}
	evs, _ := st.Events("a", 0)
	if got := types(evs); got != "Reconciling,StepReady,StepReady,StepReady,StepReady,StepReady,Ready" {
		t.Fatalf("events: %s", got)
	}
	var order []string
	for _, ev := range evs {
		if ev.Type == "StepReady" {
			order = append(order, strings.SplitN(ev.Message, " ", 2)[0])
		}
	}
	if strings.Join(order, ",") != "Compute,Storage,DNS,Route,Telemetry" {
		t.Fatalf("order: %v", order)
	}
	// Idempotent: a second pass checks, mutates nothing, records nothing.
	before := pv.Calls()
	c.Reconcile(context.Background(), "a")
	after := pv.Calls()
	for k := range after {
		if strings.HasPrefix(k, "Ensure:") && after[k] != before[k] {
			t.Fatalf("second reconcile called %s again", k)
		}
	}
	if evs2, _ := st.Events("a", 0); len(evs2) != len(evs) {
		t.Fatal("a converged reconcile must not spam events")
	}
	if len(pv.Inventory()) != 5 {
		t.Fatal("inventory grew")
	}
}

func TestTransientFailureBacksOffThenRecovers(t *testing.T) {
	st, pv, c := setup(t)
	pv.FailEvery = 2 // every second Ensure times out
	st.Apply(full("b"))
	var attempts int
	for i := 0; i < 20; i++ {
		_, err := c.Reconcile(context.Background(), "b")
		e, _ := st.Get("b")
		if err == nil && e.Status.Phase == spec.PhaseReady {
			break
		}
		if err != nil && errors.Is(err, ErrPermanent) {
			t.Fatal("timeouts must not be permanent")
		}
		attempts++
		if e.Status.Phase != spec.PhasePending || e.Status.Condition("Ready").Reason != "Transient" {
			t.Fatalf("after a transient error: %+v", e.Status)
		}
	}
	e, _ := st.Get("b")
	if e.Status.Phase != spec.PhaseReady || attempts == 0 || e.Status.Attempts != 0 {
		t.Fatalf("attempts %d status %+v", attempts, e.Status)
	}
	if len(pv.Inventory()) != 5 {
		t.Fatalf("retries must not leak resources: %v", pv.Inventory())
	}
}

func TestPermanentFailureParksAndSpecChangeUnparks(t *testing.T) {
	st, pv, c := setup(t)
	pv.FailStep = "Storage"
	st.Apply(full("c"))
	_, err := c.Reconcile(context.Background(), "c")
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("want permanent, got %v", err)
	}
	e, _ := st.Get("c")
	if e.Status.Phase != spec.PhaseFailed || e.Status.Condition("Compute").Status != "True" || e.Status.Condition("Storage").Status != "False" {
		t.Fatalf("%+v", e.Status.Conditions)
	}
	// The user drops the storage: generation 2, converges without it.
	fixed := full("c")
	fixed.Spec.Storage = nil
	st.Apply(fixed)
	pv.FailStep = ""
	if _, err := c.Reconcile(context.Background(), "c"); err != nil {
		t.Fatal(err)
	}
	e, _ = st.Get("c")
	if e.Status.Phase != spec.PhaseReady || e.Status.ObservedGeneration != 2 || e.Status.Condition("Storage") != nil {
		t.Fatalf("%+v", e.Status)
	}
}

func TestDriftIsDetectedAndRepaired(t *testing.T) {
	st, pv, c := setup(t)
	st.Apply(full("d"))
	c.Reconcile(context.Background(), "d")
	e, _ := st.Get("d")
	lost := e.Status.Resources["DNS"]
	pv.Lose(lost) // someone deleted the record in the console
	c.Reconcile(context.Background(), "d")
	e2, _ := st.Get("d")
	if e2.Status.Resources["DNS"] == "" || e2.Status.Phase != spec.PhaseReady {
		t.Fatalf("%+v", e2.Status)
	}
	evs, _ := st.Events("d", 0)
	if !strings.Contains(types(evs), "Drift") {
		t.Fatalf("drift not recorded: %s", types(evs))
	}
	if len(pv.Inventory()) != 5 {
		t.Fatal("repaired inventory wrong")
	}
}

func TestRemovingAStepPrunesItsResource(t *testing.T) {
	st, pv, c := setup(t)
	st.Apply(full("e"))
	c.Reconcile(context.Background(), "e")
	slim := full("e")
	slim.Spec.Route, slim.Spec.Telemetry = nil, nil
	st.Apply(slim)
	c.Reconcile(context.Background(), "e")
	e, _ := st.Get("e")
	if len(e.Status.Resources) != 3 || e.Status.Endpoints["Route"] != "" || len(pv.Inventory()) != 3 {
		t.Fatalf("%+v inv=%v", e.Status, pv.Inventory())
	}
}

func TestDeletionTearsDownInReverseAndRemoves(t *testing.T) {
	st, pv, c := setup(t)
	st.Apply(full("f"))
	c.Reconcile(context.Background(), "f")
	st.MarkDeleting("f")
	if _, err := c.Reconcile(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get("f"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("object must be removed after teardown")
	}
	if len(pv.Inventory()) != 0 {
		t.Fatalf("leaked: %v", pv.Inventory())
	}
	evs, _ := st.Events("f", 0)
	var order []string
	for _, ev := range evs {
		if ev.Type == "StepDestroyed" {
			order = append(order, strings.Fields(ev.Message)[0])
		}
	}
	if strings.Join(order, ",") != "Telemetry,Route,DNS,Storage,Compute" {
		t.Fatalf("teardown order: %v", order)
	}
}

func TestTTLExpiryDeletes(t *testing.T) {
	st, pv, c := setup(t)
	e := full("g")
	e.Spec.TTL = time.Hour
	st.Apply(e)
	c.Reconcile(context.Background(), "g")
	now := time.Now().Add(2 * time.Hour)
	c.opts.Now = func() time.Time { return now }
	c.Reconcile(context.Background(), "g") // marks deleting
	c.Reconcile(context.Background(), "g") // tears down
	if _, err := st.Get("g"); !errors.Is(err, store.ErrNotFound) || len(pv.Inventory()) != 0 {
		t.Fatal("ttl expiry must delete everything")
	}
}

func TestManyEnvironmentsWithWorkers(t *testing.T) {
	st, pv, c := setup(t)
	pv.Latency = 2 * time.Millisecond
	pv.FailEvery = 7
	const n = 60
	for i := 0; i < n; i++ {
		st.Apply(full(fmt.Sprintf("env-%02d", i)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	ready := map[string]bool{}
	c.opts.Hooks.Reconciled = func(name, result string, _ time.Duration) {
		if result == "ok" {
			e, _ := st.Get(name)
			if e != nil && e.Status.Phase == spec.PhaseReady {
				mu.Lock()
				ready[name] = true
				if len(ready) == n {
					cancel()
				}
				mu.Unlock()
			}
		}
	}
	c.opts.Workers = 8
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("did not converge in time")
	}
	if len(pv.Inventory()) != n*5 {
		t.Fatalf("inventory %d, want %d", len(pv.Inventory()), n*5)
	}
}

func TestImmediateAddPreemptsDelayedResync(t *testing.T) {
	q := newQueue(time.Millisecond, time.Second)
	q.AddAfter("x", time.Hour) // the periodic resync, far away
	q.Add("x")                 // a user deletes it now
	done := make(chan struct{})
	go func() { q.Get(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("an immediate add must not be blocked by a pending delayed add")
	}
	q.Close()
}

func TestQueueDedupesAndBacksOff(t *testing.T) {
	q := newQueue(time.Millisecond, 8*time.Millisecond)
	q.Add("x")
	q.Add("x")
	q.Add("y")
	if q.Len() != 2 {
		t.Fatalf("len %d", q.Len())
	}
	k, _ := q.Get()
	q.Add(k) // requested while active -> runs once more after Done
	q.Done(k, false, 0)
	k2, _ := q.Get()
	k3, _ := q.Get()
	if (k2 != "y" && k3 != "y") || (k2 != "x" && k3 != "x") {
		t.Fatalf("got %s %s", k2, k3)
	}
	q.Done(k2, true, 0)
	q.Done(k3, true, 0)
	start := time.Now()
	q.Get()
	if time.Since(start) < time.Millisecond/2 {
		t.Fatal("backoff not applied")
	}
	q.Close()
	if _, ok := q.Get(); ok {
		t.Fatal("closed queue must report !ok")
	}
}
