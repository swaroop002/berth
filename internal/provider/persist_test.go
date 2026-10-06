package provider

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/swaroop002/berth/internal/spec"
)

func TestPersistSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inv.json")
	f := NewFake()
	if n, err := f.Persist(path); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	env := &spec.Environment{Metadata: spec.Metadata{Name: "a"}, Spec: spec.Spec{Size: "small", Region: "r"}}
	res, _ := f.Ensure(context.Background(), "Compute", env, "")
	g := NewFake()
	if n, err := g.Persist(path); n != 1 || err != nil {
		t.Fatalf("restored %d %v", n, err)
	}
	if ok, _ := g.Check(context.Background(), "Compute", env, res.ID); !ok {
		t.Fatal("resource must survive the restart")
	}
	g.Destroy(context.Background(), "Compute", res.ID)
	h := NewFake()
	if n, _ := h.Persist(path); n != 0 {
		t.Fatalf("destroy must be persisted, got %d", n)
	}
}
