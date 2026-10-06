package provider

import (
	"context"
	"testing"

	"github.com/swaroop002/forge/internal/spec"
)

func TestFakeIsIdempotentAndCanLoseThings(t *testing.T) {
	f := NewFake()
	e := &spec.Environment{Metadata: spec.Metadata{Name: "x"}, Spec: spec.Spec{Size: "small", Region: "r"}}
	r1, err := f.Ensure(context.Background(), "Compute", e, "")
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := f.Ensure(context.Background(), "Compute", e, r1.ID)
	if r1.ID != r2.ID || len(f.Inventory()) != 1 {
		t.Fatal("ensure with prior id must return the same resource")
	}
	if ok, _ := f.Check(context.Background(), "Compute", e, r1.ID); !ok {
		t.Fatal("should exist")
	}
	f.Lose(r1.ID)
	if ok, _ := f.Check(context.Background(), "Compute", e, r1.ID); ok {
		t.Fatal("lost resource must fail the check")
	}
	if err := f.Destroy(context.Background(), "Compute", r1.ID); err != nil {
		t.Fatal("destroying something gone is fine")
	}
}
