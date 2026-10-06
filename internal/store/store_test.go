package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/swaroop002/berth/internal/spec"
)

func env(name string) *spec.Environment {
	return &spec.Environment{APIVersion: spec.APIVersion, Kind: spec.KindEnv,
		Metadata: spec.Metadata{Name: name, Team: "t", Owner: "o"},
		Spec:     spec.Spec{Size: "small", Region: "r1"}}
}

func TestApplyGenerations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "berth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, changed, err := s.Apply(env("a"))
	if err != nil || !changed || a.Metadata.Generation != 1 || a.Status.Phase != spec.PhasePending {
		t.Fatalf("%v %v %+v", err, changed, a)
	}
	if _, changed, _ := s.Apply(env("a")); changed {
		t.Fatal("unchanged spec must not bump generation")
	}
	s.UpdateStatus("a", spec.Status{Phase: spec.PhaseReady, ObservedGeneration: 1})
	e2 := env("a")
	e2.Spec.Size = "large"
	a, changed, _ = s.Apply(e2)
	if !changed || a.Metadata.Generation != 2 || a.Status.Phase != spec.PhaseReady || a.Status.ObservedGeneration != 1 {
		t.Fatalf("%+v", a)
	}
	if _, _, err := s.Apply(&spec.Environment{}); !errors.Is(err, spec.ErrInvalid) {
		t.Fatal("invalid spec must be rejected")
	}
}

func TestDeleteFlowAndEvents(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "berth.db"))
	defer s.Close()
	s.Apply(env("a"))
	d, err := s.MarkDeleting("a")
	if err != nil || d.Metadata.DeletionTimestamp == nil || d.Status.Phase != spec.PhaseDeleting {
		t.Fatalf("%v %+v", err, d)
	}
	if _, _, err := s.Apply(env("a")); !errors.Is(err, ErrConflict) {
		t.Fatal("applying over a deleting env must conflict")
	}
	for i := 0; i < 3; i++ {
		s.Record(Event{Env: "a", Type: "Reconciling", Message: "x"})
	}
	s.Record(Event{Env: "ab", Type: "Other"}) // prefix "a/" must not match "ab/"
	evs, _ := s.Events("a", 2)
	if len(evs) != 2 || evs[0].Env != "a" {
		t.Fatalf("%+v", evs)
	}
	s.Remove("a")
	if _, err := s.Get("a"); !errors.Is(err, ErrNotFound) {
		t.Fatal("removed")
	}
	if l, _ := s.List(); len(l) != 0 {
		t.Fatal("list after remove")
	}
}
