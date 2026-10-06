// Package metrics is berth watching its own control loop.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	reg          *prometheus.Registry
	Reconciles   *prometheus.CounterVec // result
	Duration     prometheus.Histogram
	QueueDepth   prometheus.Gauge
	Environments *prometheus.GaugeVec   // phase
	HourlyCost   *prometheus.GaugeVec   // team
	Gateway      *prometheus.CounterVec // host, code
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		reg: reg,
		Reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "berth_reconciles_total",
			Help: "Reconcile passes by result (ok, error)."}, []string{"result"}),
		Duration: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "berth_reconcile_duration_seconds",
			Help: "Time spent in one reconcile pass.", Buckets: prometheus.ExponentialBuckets(0.005, 2, 12)}),
		QueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{Name: "berth_queue_depth",
			Help: "Environments waiting or scheduled for reconciliation."}),
		Environments: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "berth_environments",
			Help: "Environments by phase."}, []string{"phase"}),
		HourlyCost: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "berth_hourly_cost_usd",
			Help: "Current run rate of all environments of a team."}, []string{"team"}),
		Gateway: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "berth_gateway_requests_total",
			Help: "Requests through the route gateway by host and status code."}, []string{"host", "code"}),
	}
	reg.MustRegister(m.Reconciles, m.Duration, m.QueueDepth, m.Environments, m.HourlyCost, m.Gateway)
	return m
}

func (m *Metrics) Handler() http.Handler { return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{}) }

// Reconciled is the controller hook.
func (m *Metrics) Reconciled(name, result string, took time.Duration) {
	m.Reconciles.WithLabelValues(result).Inc()
	m.Duration.Observe(took.Seconds())
}

// GatewayRequest is the gateway hook.
func (m *Metrics) GatewayRequest(host string, code int) {
	m.Gateway.WithLabelValues(host, strconv.Itoa(code)).Inc()
}

// Observe refreshes the per-phase and per-team gauges from a listing.
func (m *Metrics) Observe(phases map[string]int, hourlyByTeam map[string]float64) {
	m.Environments.Reset()
	for ph, n := range phases {
		m.Environments.WithLabelValues(ph).Set(float64(n))
	}
	m.HourlyCost.Reset()
	for team, h := range hourlyByTeam {
		m.HourlyCost.WithLabelValues(team).Set(h)
	}
}
