package provider

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"github.com/swaroop002/forge/internal/spec"
)

// Fake is an in-memory provider with knobs for latency and failure, so the
// reconciler's behaviour under a flaky cloud can be tested deterministically.
type Fake struct {
	mu          sync.Mutex
	inventory   map[string]Resource // id -> resource
	calls       map[string]int      // "Ensure:Compute" etc.
	ensureCount int
	// Knobs
	Latency   time.Duration
	FailEvery int             // every Nth Ensure fails transiently (0 = never)
	FailStep  string          // a step that always fails (permanent)
	DriftIDs  map[string]bool // ids Check reports as drifted
}

func NewFake() *Fake {
	return &Fake{inventory: map[string]Resource{}, calls: map[string]int{}, DriftIDs: map[string]bool{}}
}

func (f *Fake) Name() string { return "fake" }

func (f *Fake) Ensure(ctx context.Context, step string, env *spec.Environment, prior string) (Resource, error) {
	if err := f.wait(ctx); err != nil {
		return Resource{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Ensure:"+step]++
	f.ensureCount++
	if f.FailStep == step {
		return Resource{}, fmt.Errorf("%s: quota exceeded in %s", step, env.Spec.Region)
	}
	if f.FailEvery > 0 && f.ensureCount%f.FailEvery == 0 {
		return Resource{}, fmt.Errorf("%w: %s api timeout", ErrTransient, step)
	}
	if prior != "" {
		if r, ok := f.inventory[prior]; ok {
			return r, nil // already exists: idempotent
		}
	}
	id := fmt.Sprintf("%s-%s-%08x", lower(step), env.Metadata.Name, hash(env.Metadata.Name+step))
	r := Resource{ID: id, Attrs: map[string]string{"region": env.Spec.Region, "size": env.Spec.Size}}
	switch step {
	case "Compute":
		r.Endpoint = fmt.Sprintf("10.%d.%d.%d", hash(id)%200+10, hash(id+"b")%250, hash(id+"c")%250)
	case "DNS":
		r.Endpoint = env.Spec.DNS.Hostname
	case "Route":
		r.Endpoint = fmt.Sprintf("https://%s", env.Spec.DNS.Hostname)
	case "Storage":
		r.Attrs["sizeGB"] = fmt.Sprint(env.Spec.Storage.SizeGB)
	case "Telemetry":
		r.Endpoint = fmt.Sprintf("pulse://tenant/%s", env.Spec.Telemetry.Tenant)
	}
	f.inventory[id] = r
	return r, nil
}

func (f *Fake) Check(ctx context.Context, step string, env *spec.Environment, id string) (bool, error) {
	if err := f.wait(ctx); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Check:"+step]++
	_, exists := f.inventory[id]
	return exists && !f.DriftIDs[id], nil
}

func (f *Fake) Destroy(ctx context.Context, step string, id string) error {
	if err := f.wait(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Destroy:"+step]++
	delete(f.inventory, id)
	return nil
}

func (f *Fake) wait(ctx context.Context) error {
	if f.Latency == 0 {
		return nil
	}
	select {
	case <-time.After(f.Latency):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Inventory lists live resource ids, sorted.
func (f *Fake) Inventory() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.inventory))
	for id := range f.inventory {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Calls returns how often each operation ran.
func (f *Fake) Calls() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]int, len(f.calls))
	for k, v := range f.calls {
		out[k] = v
	}
	return out
}

// Lose deletes a resource behind forge's back (someone in the console).
func (f *Fake) Lose(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inventory, id)
}

func hash(s string) uint32 { h := fnv.New32a(); h.Write([]byte(s)); return h.Sum32() }
func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
