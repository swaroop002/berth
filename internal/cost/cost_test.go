package cost

import (
	"math"
	"testing"
	"time"

	"github.com/swaroop002/berth/internal/spec"
)

func TestRatesAndReport(t *testing.T) {
	now := time.Now()
	a := &spec.Environment{Metadata: spec.Metadata{Name: "a", Team: "ds", CreatedAt: now.Add(-10 * time.Hour)},
		Spec: spec.Spec{Size: "medium", Storage: &spec.StorageSpec{SizeGB: 100}, DNS: &spec.DNSSpec{Hostname: "x.y.z"}, Route: &spec.RouteSpec{Port: 1}}}
	b := &spec.Environment{Metadata: spec.Metadata{Name: "b", Team: "platform", CreatedAt: now.Add(-2 * time.Hour)},
		Spec: spec.Spec{Size: "small"}}
	del := now.Add(-time.Hour)
	b.Metadata.DeletionTimestamp = &del // accrual stops at deletion
	want := 0.083 + 100*0.00011 + 0.0007 + 0.03
	if h := Default.Hourly(a); math.Abs(h-want) > 1e-9 {
		t.Fatalf("hourly %v want %v", h, want)
	}
	if acc := Default.Accrued(b, now); math.Abs(acc-0.021*1) > 1e-9 {
		t.Fatalf("accrual must stop at deletion: %v", acc)
	}
	rep := Default.Build([]*spec.Environment{b, a}, now)
	if rep.Lines[0].Name != "a" || rep.TeamTotal["ds"] < rep.TeamTotal["platform"] || len(rep.TeamHourly) != 2 {
		t.Fatalf("%+v", rep)
	}
}
