package spec

import (
	"strings"
	"testing"
	"time"
)

func valid() Environment {
	return Environment{APIVersion: APIVersion, Kind: KindEnv,
		Metadata: Metadata{Name: "ds-alice", Team: "data-science", Owner: "alice"},
		Spec:     Spec{Size: "medium", Region: "ap-south-1", DNS: &DNSSpec{Hostname: "alice.dev.example.com"}, Route: &RouteSpec{Port: 8888, Auth: true}}}
}

func TestValidate(t *testing.T) {
	e := valid()
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := valid()
	bad.Metadata.Name = "Bad_Name"
	bad.Spec.Size = "huge"
	bad.Spec.DNS = nil // route without dns
	err := bad.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"metadata.name", "spec.size", "spec.route needs spec.dns"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestStepsInDependencyOrder(t *testing.T) {
	e := valid()
	e.Spec.Storage = &StorageSpec{SizeGB: 20}
	e.Spec.Telemetry = &TelemetrySpec{Tenant: "ds-alice"}
	got := strings.Join(e.Steps(), ",")
	if got != "Compute,Storage,DNS,Route,Telemetry" {
		t.Fatal(got)
	}
}

func TestConditionsKeepSinceWhenUnchanged(t *testing.T) {
	var s Status
	t0 := time.Now().Add(-time.Hour)
	s.SetCondition(Condition{Type: "Compute", Status: "True", Reason: "Running", Since: t0})
	s.SetCondition(Condition{Type: "Compute", Status: "True", Reason: "Running", Message: "updated", Since: time.Now()})
	if c := s.Condition("Compute"); !c.Since.Equal(t0) || c.Message != "updated" {
		t.Fatalf("%+v", c)
	}
	s.SetCondition(Condition{Type: "Compute", Status: "False", Reason: "Stopped", Since: time.Now()})
	if c := s.Condition("Compute"); c.Since.Equal(t0) {
		t.Fatal("a status change must reset Since")
	}
}

func TestTTL(t *testing.T) {
	e := valid()
	e.Metadata.CreatedAt = time.Now().Add(-2 * time.Hour)
	e.Spec.TTL = time.Hour
	if !e.Expired(time.Now()) {
		t.Fatal("should be expired")
	}
	e.Spec.TTL = 0
	if e.Expired(time.Now()) {
		t.Fatal("zero TTL never expires")
	}
}
