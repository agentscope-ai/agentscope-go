// Package prometheus provides a Prometheus-backed metrics.MetricsProvider plus
// an HTTP handler for scraping. It is an optional, dependency-carrying adapter:
// the core metrics package stays dependency-free, and applications opt into
// Prometheus by wiring this provider and mounting Handler() at /metrics.
package prometheus

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	asmetrics "github.com/agentscope-ai/agentscope-go/v2/pkg/agentscope/metrics"
)

// Provider implements metrics.MetricsProvider over a Prometheus registry.
type Provider struct {
	reg *prometheus.Registry

	mu         sync.Mutex
	counters   map[string]*prometheus.CounterVec
	histograms map[string]*prometheus.HistogramVec
}

// New creates a Provider with its own registry.
func New() *Provider {
	return &Provider{
		reg:        prometheus.NewRegistry(),
		counters:   make(map[string]*prometheus.CounterVec),
		histograms: make(map[string]*prometheus.HistogramVec),
	}
}

// Registry exposes the underlying registry (e.g. to register Go/process collectors).
func (p *Provider) Registry() *prometheus.Registry { return p.reg }

// Handler returns an http.Handler that serves the registered metrics in the
// Prometheus text exposition format. Mount it at /metrics.
func (p *Provider) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{})
}

// Counter returns a Prometheus-backed counter. Repeated calls with the same name
// return the same underlying vector.
func (p *Provider) Counter(name, help string, labelNames ...string) asmetrics.Counter {
	p.mu.Lock()
	defer p.mu.Unlock()
	cv, ok := p.counters[name]
	if !ok {
		cv = prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labelNames)
		p.reg.MustRegister(cv)
		p.counters[name] = cv
	}
	return &counter{cv: cv, names: labelNames}
}

// Histogram returns a Prometheus-backed histogram.
func (p *Provider) Histogram(name, help string, buckets []float64, labelNames ...string) asmetrics.Histogram {
	p.mu.Lock()
	defer p.mu.Unlock()
	hv, ok := p.histograms[name]
	if !ok {
		opts := prometheus.HistogramOpts{Name: name, Help: help}
		if len(buckets) > 0 {
			opts.Buckets = buckets
		}
		hv = prometheus.NewHistogramVec(opts, labelNames)
		p.reg.MustRegister(hv)
		p.histograms[name] = hv
	}
	return &histogram{hv: hv, names: labelNames}
}

type counter struct {
	cv    *prometheus.CounterVec
	names []string
}

func (c *counter) Inc(labels ...string) { c.cv.WithLabelValues(align(c.names, labels)...).Inc() }
func (c *counter) Add(v float64, labels ...string) {
	c.cv.WithLabelValues(align(c.names, labels)...).Add(v)
}

type histogram struct {
	hv    *prometheus.HistogramVec
	names []string
}

func (h *histogram) Observe(v float64, labels ...string) {
	h.hv.WithLabelValues(align(h.names, labels)...).Observe(v)
}

// align returns exactly len(names) label values, truncating extras and padding
// missing values with "" so a caller/label-count mismatch cannot panic the
// Prometheus vector.
func align(names, vals []string) []string {
	out := make([]string, len(names))
	for i := range names {
		if i < len(vals) {
			out[i] = vals[i]
		}
	}
	return out
}

// Compile-time check.
var _ asmetrics.MetricsProvider = (*Provider)(nil)
