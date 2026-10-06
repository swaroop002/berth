// Package cost puts a price on an environment: a run rate from its size and
// attachments, and what it has accrued since it was created. Numbers are
// illustrative rates in USD per hour, configurable; the point is that every
// environment shows what it costs, so a forgotten one is visible.
package cost

import (
	"sort"
	"time"

	"github.com/swaroop002/berth/internal/spec"
)

// Rates per hour.
type Rates struct {
	Compute   map[string]float64 // size -> $/h
	StorageGB float64            // $/GB-hour
	DNS       float64
	Route     float64
	Telemetry float64
}

// Default is roughly a mid-size cloud's on-demand pricing in ap-south-1.
var Default = Rates{
	Compute:   map[string]float64{"small": 0.021, "medium": 0.083, "large": 0.333},
	StorageGB: 0.00011, DNS: 0.0007, Route: 0.03, Telemetry: 0.01,
}

// Hourly is the run rate of an environment as specified.
func (r Rates) Hourly(e *spec.Environment) float64 {
	h := r.Compute[e.Spec.Size]
	if e.Spec.Storage != nil {
		h += float64(e.Spec.Storage.SizeGB) * r.StorageGB
	}
	if e.Spec.DNS != nil {
		h += r.DNS
	}
	if e.Spec.Route != nil {
		h += r.Route
	}
	if e.Spec.Telemetry != nil {
		h += r.Telemetry
	}
	return h
}

// Accrued is what the environment has cost from creation until now (or until
// its deletion started). It uses the current spec for the whole span, which
// is honest enough for a budget view and simple enough to explain.
func (r Rates) Accrued(e *spec.Environment, now time.Time) float64 {
	if e.Metadata.CreatedAt.IsZero() {
		return 0
	}
	end := now
	if e.Metadata.DeletionTimestamp != nil && e.Metadata.DeletionTimestamp.Before(now) {
		end = *e.Metadata.DeletionTimestamp
	}
	hours := end.Sub(e.Metadata.CreatedAt).Hours()
	if hours < 0 {
		hours = 0
	}
	return r.Hourly(e) * hours
}

// Line is one row of the cost report.
type Line struct {
	Name     string  `json:"name"`
	Team     string  `json:"team"`
	Owner    string  `json:"owner"`
	Hourly   float64 `json:"hourlyUSD"`
	Accrued  float64 `json:"accruedUSD"`
	AgeHours float64 `json:"ageHours"`
	Phase    string  `json:"phase"`
}

// Report prices every environment and sums by team.
type Report struct {
	Lines      []Line             `json:"environments"`
	TeamHourly map[string]float64 `json:"teamHourlyUSD"`
	TeamTotal  map[string]float64 `json:"teamAccruedUSD"`
	Hourly     float64            `json:"hourlyUSD"`
	Accrued    float64            `json:"accruedUSD"`
}

func (r Rates) Build(envs []*spec.Environment, now time.Time) Report {
	rep := Report{TeamHourly: map[string]float64{}, TeamTotal: map[string]float64{}}
	for _, e := range envs {
		h, a := r.Hourly(e), r.Accrued(e, now)
		rep.Lines = append(rep.Lines, Line{Name: e.Metadata.Name, Team: e.Metadata.Team, Owner: e.Metadata.Owner,
			Hourly: h, Accrued: a, AgeHours: now.Sub(e.Metadata.CreatedAt).Hours(), Phase: string(e.Status.Phase)})
		rep.TeamHourly[e.Metadata.Team] += h
		rep.TeamTotal[e.Metadata.Team] += a
		rep.Hourly += h
		rep.Accrued += a
	}
	sort.Slice(rep.Lines, func(i, j int) bool { return rep.Lines[i].Accrued > rep.Lines[j].Accrued })
	return rep
}
