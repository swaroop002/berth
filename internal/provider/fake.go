package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/swaroop002/berth/internal/spec"
)

// Fake is an in-memory provider with knobs for latency and failure, so the
// reconciler's behaviour under a flaky cloud can be tested deterministically.
type Fake struct {
	mu          sync.Mutex
	inventory   map[string]Resource // id -> resource
	calls       map[string]int      // "Ensure:Compute" etc.
	ensureCount int
	path        string // when set, inventory is saved here after every change
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

// Persist loads the inventory from path (if it exists) and saves it after
// every change, so the fake cloud survives a berth restart the way a real
// one would. Returns how many resources were restored.
func (f *Fake) Persist(path string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.path = path
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if err := json.Unmarshal(b, &f.inventory); err != nil {
		return 0, fmt.Errorf("fake inventory %s: %w", path, err)
	}
	return len(f.inventory), nil
}

// save writes the inventory; caller holds the lock.
func (f *Fake) save() {
	if f.path == "" {
		return
	}
	b, _ := json.MarshalIndent(f.inventory, "", "  ")
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, f.path)
	}
}

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
	f.save()
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
	f.save()
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

// Lose deletes a resource behind berth's back (someone in the console).
func (f *Fake) Lose(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inventory, id)
	f.save()
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
