// Package metrics defines provider-agnostic interfaces for emitting counters
// and histograms. It has no external dependencies; a concrete backend (e.g.
// Prometheus) implements MetricsProvider and is wired in by the caller.
package metrics

// Counter is a monotonically increasing metric.
type Counter interface {
	// Inc increments the counter by 1 for the given label values.
	Inc(labels ...string)
	// Add increments the counter by value for the given label values.
	Add(value float64, labels ...string)
}

// Histogram samples observations into configured buckets.
type Histogram interface {
	// Observe records a single observation for the given label values.
	Observe(value float64, labels ...string)
}

// MetricsProvider constructs metric instruments. Implementations are expected
// to be safe for concurrent use and to return the same instrument for repeated
// calls with the same name.
type MetricsProvider interface {
	// Counter returns a counter identified by name. labelNames declares the
	// label dimensions; label values passed to Counter methods must match.
	Counter(name, help string, labelNames ...string) Counter
	// Histogram returns a histogram identified by name with the given buckets.
	Histogram(name, help string, buckets []float64, labelNames ...string) Histogram
}

// Noop returns a MetricsProvider that discards all metrics. Use it as the
// default when no metrics backend is configured.
func Noop() MetricsProvider { return noopProvider{} }

type noopProvider struct{}

func (noopProvider) Counter(_, _ string, _ ...string) Counter { return noopCounter{} }

func (noopProvider) Histogram(_, _ string, _ []float64, _ ...string) Histogram {
	return noopHistogram{}
}

type noopCounter struct{}

func (noopCounter) Inc(_ ...string)            {}
func (noopCounter) Add(_ float64, _ ...string) {}

type noopHistogram struct{}

func (noopHistogram) Observe(_ float64, _ ...string) {}
